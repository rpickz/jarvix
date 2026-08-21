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
	for _, m := range doc.Messages {
		messages = append(messages, ai.Message{Role: ai.Role(m.Role), Content: m.Content})
	}
	return messages, doc.LastTurn, nil
}

// Save implements Store.
func (f *File) Save(messages []ai.Message, lastTurn time.Time) error {
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
	return nil
}

// Clear implements Store.
func (f *File) Clear() error {
	err := os.Remove(f.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove history: %w", err)
	}
	return nil
}
