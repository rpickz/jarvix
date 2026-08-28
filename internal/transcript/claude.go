package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The Claude Code adapter. Claude Code writes one JSONL file per session
// under <ClaudeDir>/projects/<slugged-cwd>/, appending a line per event as
// the session runs: user messages, assistant content blocks (one line per
// block — text, thinking, tool_use), tool results (as user-role lines), and
// bookkeeping lines (mode, file-history-snapshot, ai-title, …) that carry no
// conversation at all. The newest file's mtime moves with every event, which
// is what makes "newest .jsonl" the live session and the freshness gate
// meaningful.

// claudeSlug maps a working directory to Claude Code's project directory
// name: every byte outside [A-Za-z0-9] becomes a dash, case preserved.
// Pinned by test against a real observed mapping — if Claude Code ever
// changes its slugging, the fixture fails before a user ever gets a silent
// wrong-directory lookup.
func claudeSlug(cwd string) string {
	var b strings.Builder
	b.Grow(len(cwd))
	for _, r := range cwd {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// readClaude reads the newest Claude Code transcript for cwd. Absence at any
// step — no project dir, no .jsonl files, nothing fresh — is ErrNoSession;
// past that point a failure is a real error the recap must admit, because a
// session provably exists and could not be read.
func (f *Finder) readClaude(ctx context.Context, cwd string) (Tail, error) {
	if f.ClaudeDir == "" {
		return Tail{}, ErrNoSession
	}
	dir := filepath.Join(f.ClaudeDir, "projects", claudeSlug(cwd))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return Tail{}, ErrNoSession
	}
	if err != nil {
		return Tail{}, fmt.Errorf("the Claude session directory could not be read: %w", err)
	}
	var newest string
	var newestAt time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // vanished between listing and stat — absence, not failure
		}
		if newest == "" || info.ModTime().After(newestAt) {
			newest, newestAt = filepath.Join(dir, e.Name()), info.ModTime()
		}
	}
	if newest == "" || !f.fresh(newestAt) {
		return Tail{}, ErrNoSession
	}
	if err := ctx.Err(); err != nil {
		return Tail{}, err
	}
	data, err := readFileTail(newest, f.maxTailBytes())
	if err != nil {
		return Tail{}, fmt.Errorf("the Claude transcript could not be read: %w", err)
	}
	events := parseClaudeTail(data)
	if len(events) == 0 {
		// A transcript that yields no conversation at all is not a shape this
		// adapter recognises — found but unreadable, never silently invented.
		return Tail{}, fmt.Errorf("the Claude transcript held no readable exchanges")
	}
	return Tail{
		Text:   f.renderClaude(events),
		State:  classifyClaude(events[len(events)-1]),
		Source: "claude",
	}, nil
}

// readFileTail reads at most maxBytes from the end of a file. When the read
// starts mid-file the first line is almost certainly torn, so everything up
// to the first newline is dropped — a half JSON line is noise, not data.
func readFileTail(path string, maxBytes int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	// Read-only: a Close failure can lose nothing.
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	offset := info.Size() - int64(maxBytes)
	if offset < 0 {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)))
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}
	return data, nil
}

// claudeEvent is one conversational line of the transcript, reduced to what
// classification and rendering need. Bookkeeping lines never become one.
type claudeEvent struct {
	role       string // "user" or "assistant"
	apiError   bool   // an assistant line Claude Code marked as an API error
	stopReason string // the message's stop_reason, "" when null
	toolUse    string // the tool a tool_use block invoked
	toolResult bool   // a user-role line carrying a tool's result
	text       string // the message's text content, "" when none
}

// claudeLine is the wire shape of one transcript line — only the fields the
// adapter reads; everything else is ignored so format growth cannot break
// parsing.
type claudeLine struct {
	Type              string `json:"type"`
	IsAPIErrorMessage bool   `json:"isApiErrorMessage"`
	Message           struct {
		Role       string          `json:"role"`
		StopReason string          `json:"stop_reason"`
		Content    json.RawMessage `json:"content"`
	} `json:"message"`
}

// claudeBlock is one content block of a message whose content is an array.
type claudeBlock struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Text string `json:"text"`
}

// parseClaudeTail turns the tail's lines into conversational events, in
// order, skipping bookkeeping lines and anything unparseable — one corrupt
// line must not cost the readable exchanges around it.
func parseClaudeTail(data []byte) []claudeEvent {
	var events []claudeEvent
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var parsed claudeLine
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			continue
		}
		if parsed.Type != "user" && parsed.Type != "assistant" {
			continue
		}
		ev := claudeEvent{
			role:       parsed.Type,
			apiError:   parsed.IsAPIErrorMessage,
			stopReason: parsed.Message.StopReason,
		}
		// Content is either a plain string (early user messages) or an array
		// of typed blocks; both are conversation.
		var text string
		if err := json.Unmarshal(parsed.Message.Content, &text); err == nil {
			ev.text = text
		} else {
			var blocks []claudeBlock
			if err := json.Unmarshal(parsed.Message.Content, &blocks); err != nil {
				continue
			}
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if ev.text != "" {
						ev.text += " "
					}
					ev.text += b.Text
				case "tool_use":
					ev.toolUse = b.Name
				case "tool_result":
					ev.toolResult = true
				}
				// "thinking" blocks are deliberately dropped: chain-of-thought
				// is not conversation and must never reach a prompt or a
				// spoken sentence.
			}
		}
		events = append(events, ev)
	}
	return events
}

// classifyClaude reads the session's state off its last conversational
// event — the deterministic classification (#137). Each admission of
// "working" is structural, never inferred from prose:
//
//   - an API-error line: the session stopped abnormally and cannot continue
//     without the user — needs_you.
//   - a user line (text or tool result): the agent has input it has not yet
//     finished answering — working.
//   - an assistant line that invoked a tool, or whose stop_reason is
//     tool_use: a tool is running or about to — working.
//   - an assistant line with no stop_reason yet: the reply is mid-stream —
//     working.
//   - a finished assistant line ending on a question — needs_you; otherwise
//     the agent delivered its final word — done.
func classifyClaude(last claudeEvent) State {
	if last.apiError {
		return StateNeedsYou
	}
	if last.role == "user" {
		return StateWorking
	}
	if last.toolUse != "" || last.stopReason == "tool_use" {
		return StateWorking
	}
	if last.stopReason == "" {
		return StateWorking
	}
	if endsWithQuestion(last.text) {
		return StateNeedsYou
	}
	return StateDone
}

// renderClaude turns the trailing events into the prompt window: user text,
// assistant text, and tool-run notes. Tool *results* are omitted — they are
// command output, the noisiest and most secret-prone content in the file,
// and the assistant's own words already say what came of them. Harness
// bookkeeping that travels inside user messages (XML-tagged caveats,
// command wrappers) is dropped the same way: it was never something the
// user said.
func (f *Finder) renderClaude(events []claudeEvent) string {
	start := 0
	if len(events) > maxRenderMessages {
		start = len(events) - maxRenderMessages
	}
	var lines []renderedLine
	for _, ev := range events[start:] {
		switch {
		case ev.role == "user" && !ev.toolResult:
			text := strings.TrimSpace(ev.text)
			if text == "" || strings.HasPrefix(text, "<") {
				continue
			}
			lines = append(lines, entryLine("User: ", text))
		case ev.role == "assistant" && ev.apiError:
			lines = append(lines, entryLine("Session error: ", strings.TrimSpace(ev.text)))
		case ev.role == "assistant":
			if text := strings.TrimSpace(ev.text); text != "" {
				lines = append(lines, entryLine("Assistant: ", text))
			}
			if ev.toolUse != "" {
				lines = append(lines, entryLine("Assistant ran ", ev.toolUse+"."))
			}
		}
	}
	return f.renderTail(lines)
}
