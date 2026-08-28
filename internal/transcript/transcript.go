// Package transcript discovers and reads AI-CLI session transcripts for the
// AI-session recap (#137, ADR 0047). AI coding CLIs keep their sessions on
// disk — Claude Code as JSONL under ~/.claude/projects/<slugged-cwd>/,
// opencode as JSON files under its data dir — and that record is the
// session's own ground truth: what the agent actually did and what it awaits,
// where the window title (#124's capture) can only imply.
//
// The package is pure discovery and parsing, three rules deep:
//
//   - Bounded. Only the newest transcript's tail is ever read (MaxTailBytes),
//     only a fixed window of it is ever rendered (MaxTextRunes, newest kept),
//     and a transcript nobody has touched in maxSessionAge is not a session —
//     it is an archive, and resurrecting it would put a confident, stale
//     sentence in the user's ear.
//   - Deterministic. The working / needs-you / done classification is
//     computed from the transcript's structure — the last event's type —
//     with no model call, and StateUnknown never guesses: an unreadable or
//     unrecognised shape classifies as nothing at all.
//   - Honest about absence. ErrNoSession means "this directory hosts no
//     current AI session" — the caller falls back to the title capture
//     silently, because there was nothing to fail at. Any other error means
//     a session was found and could not be read, which the recap must admit.
//
// Nothing here touches a compositor, a provider, or the network; the daemon
// wires a Finder behind the focus package's Capture seam (the richer gatherer
// slot ADR 0043 left open), and redaction stays the daemon's job — this
// package returns what the transcript says, the caller decides what may
// travel.
package transcript

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// State is the deterministic session classification (#137): what the
// transcript's structure says the session is doing right now. The overlay dot
// (#127) renders it; StateUnknown renders as no dot at all.
type State string

const (
	// StateWorking: the agent is mid-task — a tool is running or was just
	// answered, a reply is mid-stream, or the user has spoken and the agent
	// has yet to finish responding.
	StateWorking State = "working"
	// StateNeedsYou: the session stopped on the user — the agent asked a
	// question, or errored and cannot continue without a decision.
	StateNeedsYou State = "needs_you"
	// StateDone: the agent delivered a final message and awaits nothing.
	StateDone State = "done"
	// StateUnknown is the honest empty: no transcript, or a shape the rules
	// do not recognise. It is never guessed into one of the other three.
	StateUnknown State = ""
)

// Tail is one bounded read of the newest session transcript for a working
// directory: the rendered last exchanges and the structural classification.
type Tail struct {
	// Text is the session's last exchanges rendered for a prompt, oldest
	// kept line first, bounded to MaxTextRunes with the newest content
	// winning. User text, assistant text, and tool-run notes only — never
	// tool output and never chain-of-thought.
	Text string
	// State is the deterministic classification of the session's last event.
	State State
	// Source names the adapter that served the read: "claude" or "opencode".
	Source string
}

// ErrNoSession reports that a directory hosts no current AI session: no
// transcript dir, no transcript files, or nothing fresh enough to still be a
// session. The recap falls back to the title capture silently — this is
// absence, not failure.
var ErrNoSession = errors.New("no AI session transcript")

// The bounds. Tail bytes cap the disk read; text runes cap what a prompt can
// carry (matching the desktop-context capture bound, so the transcript path
// can never widen what #124 allowed); entry runes keep one long paste from
// crowding every other exchange out of the window.
const (
	// DefaultMaxTailBytes is how much of the newest transcript is read.
	DefaultMaxTailBytes = 64 * 1024
	// DefaultMaxTextRunes bounds the rendered exchanges.
	DefaultMaxTextRunes = 2000
	// maxEntryRunes bounds one rendered message inside the window.
	maxEntryRunes = 600
	// maxSessionAge is how stale a transcript may be and still count as a
	// session. Generous on purpose — an agent's question should survive the
	// user's evening and their morning coffee — but bounded, so a directory
	// last visited weeks ago cannot recap as if the work were live.
	maxSessionAge = 48 * time.Hour
	// maxRenderMessages bounds how many trailing messages the adapters parse
	// into the render window; the rune budget usually cuts first.
	maxRenderMessages = 12
)

// Finder resolves working directories to session transcripts. The zero value
// is unusable; NewFinder builds the production one, and tests build their own
// with every root and the clock pointed at fixtures — no test ever reads a
// real CLI's state or the real /proc.
type Finder struct {
	// ClaudeDir is Claude Code's state root (production: ~/.claude).
	ClaudeDir string
	// OpencodeDir is opencode's data root (production:
	// ~/.local/share/opencode, honouring XDG_DATA_HOME).
	OpencodeDir string
	// ProcDir is the procfs root for window-cwd resolution (production:
	// /proc).
	ProcDir string
	// MaxTailBytes and MaxTextRunes override the default bounds; zero keeps
	// them.
	MaxTailBytes int
	MaxTextRunes int
	// Now is the clock the freshness gate reads; nil means time.Now.
	Now func() time.Time
}

// NewFinder builds the production Finder over the real state roots. An
// unresolvable home directory returns an error and the caller runs without
// transcripts — the recap degrades to the title capture, never to a crash.
func NewFinder() (*Finder, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(home, ".local", "share")
	}
	return &Finder{
		ClaudeDir:   filepath.Join(home, ".claude"),
		OpencodeDir: filepath.Join(data, "opencode"),
		ProcDir:     "/proc",
	}, nil
}

// ReadWindow resolves a window's process to the working directories of the
// shells and agents it hosts and reads the newest session transcript among
// them. Candidates are tried shallowest first — the shell before the agent
// before the agent's tool children — because the shell's directory is where
// the user is, and a tool child may be off in a subdirectory that hosts its
// own, wrong, transcripts. The window process itself is the last resort: a
// terminal emulator's own cwd is usually its launch directory, which is the
// least likely candidate to be the session's.
func (f *Finder) ReadWindow(ctx context.Context, pid int) (Tail, error) {
	if pid <= 0 {
		return Tail{}, ErrNoSession
	}
	for _, cwd := range f.candidateCwds(pid) {
		if err := ctx.Err(); err != nil {
			return Tail{}, err
		}
		tail, err := f.Read(ctx, cwd)
		if errors.Is(err, ErrNoSession) {
			continue
		}
		return tail, err
	}
	return Tail{}, ErrNoSession
}

// Read finds the newest session transcript for one working directory and
// returns its bounded tail. Adapters are tried in a fixed order; the first
// one that finds a session answers, and only a total absence is ErrNoSession.
func (f *Finder) Read(ctx context.Context, cwd string) (Tail, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return Tail{}, ErrNoSession
	}
	tail, err := f.readClaude(ctx, cwd)
	if !errors.Is(err, ErrNoSession) {
		return tail, err
	}
	return f.readOpencode(ctx, cwd)
}

// maxTailBytes and maxTextRunes apply the defaults.
func (f *Finder) maxTailBytes() int {
	if f.MaxTailBytes > 0 {
		return f.MaxTailBytes
	}
	return DefaultMaxTailBytes
}

func (f *Finder) maxTextRunes() int {
	if f.MaxTextRunes > 0 {
		return f.MaxTextRunes
	}
	return DefaultMaxTextRunes
}

func (f *Finder) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// fresh reports whether a transcript touched at t still counts as a session.
func (f *Finder) fresh(t time.Time) bool {
	return f.now().Sub(t) <= maxSessionAge
}

// renderedLine is one line of the prompt window with its rune cost, kept so
// the tail-biased budget cut never re-counts.
type renderedLine struct {
	text  string
	runes int
}

// renderTail joins rendered lines newest-biased: lines are dropped from the
// oldest end until the whole window fits MaxTextRunes, because the newest
// exchange is the one the recap exists to speak.
func (f *Finder) renderTail(lines []renderedLine) string {
	budget := f.maxTextRunes()
	total := 0
	keep := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		// The +1 is the joining newline.
		if total+lines[i].runes+1 > budget {
			break
		}
		total += lines[i].runes + 1
		keep = i
	}
	if keep == len(lines) {
		if len(lines) == 0 {
			return ""
		}
		// Even the newest line alone exceeds the budget (a test-sized bound):
		// speak its head rather than nothing — one message has no newer part
		// to prefer.
		return clampRunes(lines[len(lines)-1].text, budget)
	}
	parts := make([]string, 0, len(lines)-keep)
	for _, l := range lines[keep:] {
		parts = append(parts, l.text)
	}
	return strings.Join(parts, "\n")
}

// entryLine builds one rendered line, clamped to maxEntryRunes so a pasted
// wall of text cannot crowd out the exchanges around it.
func entryLine(prefix, text string) renderedLine {
	text = strings.Join(strings.Fields(text), " ")
	text = clampRunes(text, maxEntryRunes)
	line := prefix + text
	return renderedLine{text: line, runes: utf8.RuneCountInString(line)}
}

// clampRunes bounds text at n runes without tearing a multi-byte character —
// the desktop truncation rule, restated here because this package cannot
// import the daemon's copy.
func clampRunes(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	kept := 0
	for i := range text {
		if kept == n {
			return text[:i]
		}
		kept++
	}
	return text
}

// endsWithQuestion reports whether prose stops on a question — the
// deterministic "awaiting your answer" signal. Trailing whitespace and
// closing quotes or brackets are looked through, so a question inside
// formatting still counts.
func endsWithQuestion(text string) bool {
	trimmed := strings.TrimRightFunc(text, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '"', '\'', ')', ']', '*', '`':
			return true
		}
		return false
	})
	return strings.HasSuffix(trimmed, "?")
}
