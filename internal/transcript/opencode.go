package transcript

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The opencode adapter, over opencode's JSON file storage:
//
//	<OpencodeDir>/storage/project/<id>.json     — {id, worktree, …}
//	<OpencodeDir>/storage/session/<projectID>/  — ses_*.json {id, directory, time{updated}}
//	<OpencodeDir>/storage/message/<sessionID>/  — msg_*.json {role, time{created,completed}, finish, error}
//	<OpencodeDir>/storage/part/<messageID>/     — prt_*.json {type, text, tool, …}
//
// Discovery goes worktree → project → newest session → trailing messages →
// their parts, every step bounded. Newer opencode releases have moved this
// record into a sqlite database (opencode.db); reading that would mean a
// driver dependency, so it is a follow-up adapter behind this same seam —
// against a sqlite-only install every path below reports ErrNoSession and
// the recap falls back to the title capture exactly as designed (ADR 0047).

// opencodeProject is a project index entry; worktree is the repository root
// the project was opened on.
type opencodeProject struct {
	ID       string `json:"id"`
	Worktree string `json:"worktree"`
}

// opencodeSession is one session record; directory is where it actually ran
// (a worktree's subdirectory keeps the project's id but its own directory).
type opencodeSession struct {
	ID        string `json:"id"`
	Directory string `json:"directory"`
	Time      struct {
		Updated int64 `json:"updated"` // unix milliseconds
	} `json:"time"`
}

// opencodeMessage is one message record. Completed is zero while the
// assistant is still generating; Error is present when the turn failed.
type opencodeMessage struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Finish string          `json:"finish"`
	Error  json.RawMessage `json:"error"`

	parts []opencodePart // loaded separately, carried alongside
}

// opencodePart is one content part of a message.
type opencodePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Tool string `json:"tool"`
}

// readOpencode reads the newest opencode session for cwd. As with the Claude
// adapter, absence anywhere is ErrNoSession; once a fresh session is in hand
// a failure to read its messages is a real error the recap must admit.
func (f *Finder) readOpencode(ctx context.Context, cwd string) (Tail, error) {
	if f.OpencodeDir == "" {
		return Tail{}, ErrNoSession
	}
	storage := filepath.Join(f.OpencodeDir, "storage")
	project, err := f.opencodeProjectFor(storage, cwd)
	if err != nil {
		return Tail{}, err
	}
	session, err := f.opencodeNewestSession(storage, project, cwd)
	if err != nil {
		return Tail{}, err
	}
	if err := ctx.Err(); err != nil {
		return Tail{}, err
	}
	messages, err := f.opencodeTrailingMessages(storage, session.ID)
	if err != nil {
		return Tail{}, err
	}
	if len(messages) == 0 {
		return Tail{}, fmt.Errorf("the opencode session %s held no readable messages", session.ID)
	}
	return Tail{
		Text:   f.renderOpencode(messages),
		State:  classifyOpencode(messages[len(messages)-1]),
		Source: "opencode",
	}, nil
}

// opencodeProjectFor finds the project whose worktree is cwd. The match is
// exact: guessing "close enough" directories is how a recap ends up speaking
// about the neighbouring repository.
func (f *Finder) opencodeProjectFor(storage, cwd string) (opencodeProject, error) {
	dir := filepath.Join(storage, "project")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return opencodeProject{}, ErrNoSession
	}
	if err != nil {
		return opencodeProject{}, fmt.Errorf("the opencode project index could not be read: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var p opencodeProject
		if !readJSONFile(filepath.Join(dir, e.Name()), &p) {
			continue // one corrupt index entry must not hide the others
		}
		if p.Worktree == cwd && p.ID != "" {
			return p, nil
		}
	}
	return opencodeProject{}, ErrNoSession
}

// opencodeNewestSession picks the project's freshest session, preferring one
// that ran in cwd itself: a project spans its worktrees, and the session in
// *this* directory is the one the anchored terminal is looking at.
func (f *Finder) opencodeNewestSession(storage string, project opencodeProject, cwd string) (opencodeSession, error) {
	dir := filepath.Join(storage, "session", project.ID)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return opencodeSession{}, ErrNoSession
	}
	if err != nil {
		return opencodeSession{}, fmt.Errorf("the opencode sessions could not be read: %w", err)
	}
	var best opencodeSession
	var bestHere bool
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var s opencodeSession
		if !readJSONFile(filepath.Join(dir, e.Name()), &s) || s.ID == "" {
			continue
		}
		here := s.Directory == cwd
		// A session in cwd beats any session elsewhere; among equals, newest
		// wins.
		switch {
		case here && !bestHere:
		case here == bestHere && s.Time.Updated > best.Time.Updated:
		default:
			continue
		}
		best, bestHere = s, here
	}
	if best.ID == "" || !f.fresh(time.UnixMilli(best.Time.Updated)) {
		return opencodeSession{}, ErrNoSession
	}
	return best, nil
}

// opencodeTrailingMessages loads the session's last messages with their
// parts, oldest first. Only the newest maxRenderMessages files are parsed —
// the bound that keeps a thousand-message session from costing a thousand
// reads per recap.
func (f *Finder) opencodeTrailingMessages(storage, sessionID string) ([]opencodeMessage, error) {
	dir := filepath.Join(storage, "message", sessionID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		// The session record exists; its messages must too. This is the
		// found-but-unreadable case whatever the errno.
		return nil, fmt.Errorf("the opencode messages could not be read: %w", err)
	}
	// Message ids are time-ordered, so the filenames sort chronologically —
	// take the trailing window by name, then order what parses by created
	// time in case an id scheme ever changes underneath.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) > maxRenderMessages {
		names = names[len(names)-maxRenderMessages:]
	}
	var messages []opencodeMessage
	for _, name := range names {
		var m opencodeMessage
		if !readJSONFile(filepath.Join(dir, name), &m) || m.Role == "" {
			continue
		}
		m.parts = f.opencodeParts(storage, m.ID)
		messages = append(messages, m)
	}
	sort.SliceStable(messages, func(i, j int) bool {
		return messages[i].Time.Created < messages[j].Time.Created
	})
	return messages, nil
}

// opencodeParts loads one message's parts in id order. A message without a
// parts directory simply has none — user messages sometimes carry their text
// elsewhere, and rendering treats missing parts as nothing to say.
func (f *Finder) opencodeParts(storage, messageID string) []opencodePart {
	dir := filepath.Join(storage, "part", messageID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	parts := make([]opencodePart, 0, len(names))
	for _, name := range names {
		var p opencodePart
		if readJSONFile(filepath.Join(dir, name), &p) {
			parts = append(parts, p)
		}
	}
	return parts
}

// classifyOpencode reads the session's state off its last message — the same
// deterministic table as the Claude adapter, expressed in opencode's shapes:
//
//   - a message that recorded an error: the turn failed and the session
//     waits on the user — needs_you.
//   - a user message: input the agent has not yet answered — working.
//   - an assistant message not yet completed, or finished on tool-calls: the
//     agent is mid-task — working.
//   - a completed assistant message ending on a question — needs_you;
//     otherwise — done.
func classifyOpencode(last opencodeMessage) State {
	if len(last.Error) > 0 && string(last.Error) != "null" {
		return StateNeedsYou
	}
	if last.Role == "user" {
		return StateWorking
	}
	if last.Role != "assistant" {
		return StateUnknown
	}
	if last.Time.Completed == 0 || last.Finish == "tool-calls" {
		return StateWorking
	}
	if endsWithQuestion(lastTextPart(last.parts)) {
		return StateNeedsYou
	}
	return StateDone
}

// lastTextPart returns the final text part's content — the words the
// assistant actually stopped on.
func lastTextPart(parts []opencodePart) string {
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i].Type == "text" {
			return parts[i].Text
		}
	}
	return ""
}

// renderOpencode turns the trailing messages into the prompt window on the
// Claude renderer's terms: user text, assistant text, tool-run notes;
// reasoning parts dropped (chain-of-thought never travels), tool output
// dropped (the noisiest, most secret-prone content the store holds).
func (f *Finder) renderOpencode(messages []opencodeMessage) string {
	var lines []renderedLine
	for _, m := range messages {
		prefix := "Assistant: "
		if m.Role == "user" {
			prefix = "User: "
		}
		for _, p := range m.parts {
			switch p.Type {
			case "text":
				if text := strings.TrimSpace(p.Text); text != "" {
					lines = append(lines, entryLine(prefix, text))
				}
			case "tool":
				if p.Tool != "" {
					lines = append(lines, entryLine("Assistant ran ", p.Tool+"."))
				}
			}
		}
		if len(m.Error) > 0 && string(m.Error) != "null" {
			lines = append(lines, entryLine("Session error", "."))
		}
	}
	return f.renderTail(lines)
}

// readJSONFile decodes one JSON file into out, reporting success. Failures
// are absence by design at every call site: a single corrupt record must
// degrade one entry, never the whole read.
func readJSONFile(path string, out any) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(data, out) == nil
}
