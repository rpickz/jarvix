// Package history persists conversation memory across daemon restarts.
//
// The session engine keeps a rolling history of prior exchanges in memory
// (ADR 0010); this package is the disk behind it (ADR 0011), so a follow-up
// asked after a restart still has its context. The content is user speech,
// so the file is private to the user (0600 in a 0700 directory), written
// atomically, and bounded in size. Callers treat every failure as
// degradation, never as fatal: a daemon that cannot persist still converses.
package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/statehold"
)

// Store persists the rolling conversation history. Implementations must
// tolerate concurrent processes badly behaved enough to delete the file
// between calls; only one daemon writes at a time.
type Store interface {
	// Load returns the persisted history and the time of its last exchange.
	// A missing file is an empty history, not an error; a corrupt file is an
	// error the caller downgrades to a warning plus an empty history.
	Load() (messages []ai.Message, lastTurn time.Time, err error)
	// Save replaces the persisted history with the given exchanges.
	Save(messages []ai.Message, lastTurn time.Time) error
	// Clear removes the persisted history. Clearing an absent history is a
	// no-op, not an error.
	Clear() error
}

// MaxFileBytes caps the serialized history file. Save drops the oldest
// exchanges until the document fits, so the file on disk can never grow
// without bound even if the turn cap is configured very large.
const MaxFileBytes = 1 << 20 // 1 MiB

// File is a Store backed by one JSON file, written atomically via a temp
// file and rename so a crash mid-write leaves either the old history or the
// new one — never a torn file.
type File struct {
	// Path is the history file, conventionally under $XDG_STATE_HOME/jarvix.
	Path string
	// Gate is the backup write barrier (ADR 0045); nil — the CLI, tests —
	// means writes are never held. Only the daemon threads one through.
	Gate *statehold.Gate
}

// document is the on-disk shape. Only the user question and the assistant's
// final answer of each exchange are stored — matching what the engine keeps
// in memory: no tool traffic, and never the system prompt.
type document struct {
	Version  int       `json:"version"`
	LastTurn time.Time `json:"last_turn"`
	Messages []message `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// documentVersion is bumped when the shape changes incompatibly; an
// unrecognised version is treated like corruption (warn, start fresh) rather
// than guessed at.
const documentVersion = 1

// Load implements Store.
func (f *File) Load() ([]ai.Message, time.Time, error) {
	data, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read history: %w", err)
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, time.Time{}, fmt.Errorf("parse history: %w", err)
	}
	if doc.Version != documentVersion {
		return nil, time.Time{}, fmt.Errorf("history version %d is not supported", doc.Version)
	}
	messages := make([]ai.Message, 0, len(doc.Messages))
	for i, m := range doc.Messages {
		// A hand-edited or half-corrupted file can still be valid JSON while
		// carrying a role no provider understands. Loading it unchecked turns
		// the corruption into a malformed provider request mid-conversation;
		// treating it as a load error gets the documented degradation instead
		// — warn, and start from an empty history (raised in review of #16).
		if !knownRole(ai.Role(m.Role)) {
			return nil, time.Time{}, fmt.Errorf("history message %d has unknown role %q", i, m.Role)
		}
		messages = append(messages, ai.Message{Role: ai.Role(m.Role), Content: m.Content})
	}
	return messages, doc.LastTurn, nil
}

// knownRole reports whether a role read from disk is one the ai package
// defines. Every role is accepted, not just the user/assistant pair the
// engine currently persists: rejecting a role the type system knows about
// would turn a future engine change into a silent history wipe.
func knownRole(r ai.Role) bool {
	switch r {
	case ai.RoleUser, ai.RoleAssistant, ai.RoleSystem, ai.RoleTool:
		return true
	default:
		return false
	}
}

// Save implements Store.
func (f *File) Save(messages []ai.Message, lastTurn time.Time) error {
	// Entered before the first byte moves, released once the file is
	// settled: `jarvix backup` holds this gate for its coherent cut.
	defer f.Gate.Enter()()
	doc := document{Version: documentVersion, LastTurn: lastTurn}
	doc.Messages = make([]message, 0, len(messages))
	for _, m := range messages {
		doc.Messages = append(doc.Messages, message{Role: string(m.Role), Content: m.Content})
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode history: %w", err)
	}
	// Enforce the size cap by dropping whole exchanges, oldest first. History
	// arrives as user+assistant pairs, so drop two at a time.
	for len(data) > MaxFileBytes && len(doc.Messages) > 0 {
		drop := 2
		if len(doc.Messages) < drop {
			drop = len(doc.Messages)
		}
		doc.Messages = doc.Messages[drop:]
		if data, err = json.Marshal(doc); err != nil {
			return fmt.Errorf("encode history: %w", err)
		}
	}

	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	// MkdirAll applies its mode only to directories it creates, and even then
	// the umask can clear bits. A state directory that already exists — made
	// by an older build, another tool, or a permissive umask — keeps whatever
	// modes it had, so the 0700 privacy requirement (ADR 0011) was documented
	// but not enforced. Assert it on every save; the content is user speech
	// (raised in review of #16).
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure state dir: %w", err)
	}
	// Temp file in the same directory so the final rename is atomic on the
	// same filesystem. CreateTemp already creates it 0600.
	tmp, err := os.CreateTemp(dir, ".history-*.tmp")
	if err != nil {
		return fmt.Errorf("write history: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write history: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write history: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write history: %w", err)
	}
	if err := os.Rename(tmp.Name(), f.Path); err != nil {
		return fmt.Errorf("write history: %w", err)
	}
	// CreateTemp asks for 0600 but the umask can still clear bits, and the
	// rename carries whatever the temp file ended up with onto the real path.
	// Reassert it rather than hope (ADR 0011: 0600 in a 0700 directory).
	if err := os.Chmod(f.Path, 0o600); err != nil {
		return fmt.Errorf("secure history file: %w", err)
	}
	// fsync the containing directory. Rename is atomic — a reader sees the
	// old file or the new one, never a torn one — but atomic is not durable:
	// the new directory entry can still be sitting in the page cache when the
	// machine loses power, which would resurrect the previous history or lose
	// the file entirely. ADR 0011 claims fsync+rename crash safety, and this
	// is the half that was missing (raised in review of #16).
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("write history: %w", err)
	}
	return nil
}

// syncDir fsyncs a directory so entries created or renamed inside it survive
// a crash. Opening a directory read-only and calling Sync is the portable
// POSIX spelling; on Linux it is exactly what a durable rename needs.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// Clear implements Store.
func (f *File) Clear() error {
	defer f.Gate.Enter()()
	err := os.Remove(f.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove history: %w", err)
	}
	return nil
}
