package reminders

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// This file is the disk half of the reminder store: one TOML document under
// the XDG state dir, written atomically with the fsync-and-rename discipline
// of conversation history (ADR 0011), private to the user (0600 in a 0700
// directory) — the memory book's storage contract (ADR 0025), applied to
// one-shot reminders (#141, ADR 0046).
//
// Deliberately NOT config.toml: a reminder is throwaway state the user
// creates by voice a dozen times a day, and putting it in configuration
// would drag every "remind me at three" through the config-write
// confirmation card — the exact ceremony this feature exists to remove.

// document is the on-disk shape.
type document struct {
	Version int `toml:"version"`
	// NextID is the id high-water mark, persisted so an id is never reused
	// even across firings and restarts: a conversation that once named a
	// reminder must never come to describe a different one.
	NextID    int              `toml:"next_id"`
	Reminders []reminderRecord `toml:"reminder"`
	// Fired is the capped delivery history — what "what fired today" reads.
	Fired []firedRecord `toml:"fired"`
}

type reminderRecord struct {
	ID      string    `toml:"id"`
	Text    string    `toml:"text"`
	Due     time.Time `toml:"due"`
	Created time.Time `toml:"created"`
}

type firedRecord struct {
	ID   string    `toml:"id"`
	Text string    `toml:"text"`
	Due  time.Time `toml:"due"`
	// At is when the reminder actually left the pending list — spoken, or
	// cancelled.
	At time.Time `toml:"at"`
	// Outcome is "fired" or "cancelled".
	Outcome string `toml:"outcome"`
	// Late marks a delivery more than the grace period after its moment —
	// deferred behind a session, or fired once at boot after downtime.
	Late bool `toml:"late,omitempty"`
}

// documentVersion is bumped when the shape changes incompatibly; an
// unrecognised version is treated like corruption (warn, serve empty, never
// overwrite) rather than guessed at.
const documentVersion = 1

// header is written at the top of every save — the file's own documentation,
// the focus store's rule.
const header = `# Jarvix's one-shot reminders — the moments behind "remind me at three to …".
#
# Edit freely; Jarvix picks up changes without a restart. Each [[reminder]]
# needs text and a due time (RFC 3339); ids and created times are filled in
# when missing. [[fired]] entries are the capped delivery history behind
# "what fired today" — outcome is "fired" or "cancelled", late marks a
# delivery that was deferred or caught up at boot.
# Jarvix rewrites this file whenever a reminder changes; comments not preserved.

`

// readStore loads the reminder file. A parse failure, unknown version, or
// unknown key is an error the Service downgrades to a warning plus an empty
// store; content never travels inside the error.
func readStore(path string) (persisted, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return persisted{}, fmt.Errorf("read reminder store: %w", err)
	}
	var doc document
	md, err := toml.Decode(string(data), &doc)
	if err != nil {
		return persisted{}, fmt.Errorf("parse reminder store: %w", err)
	}
	// An unknown key is most likely a hand-edit typo, and silently dropping
	// the value it holds would look exactly like Jarvix forgetting. Refusing
	// loudly gets the documented degradation instead: a warning, an empty
	// store, a file never overwritten.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return persisted{}, fmt.Errorf("reminder store has unknown key %q", undecoded[0].String())
	}
	if doc.Version != documentVersion {
		return persisted{}, fmt.Errorf("reminder store version %d is not supported", doc.Version)
	}
	p := persisted{nextID: doc.NextID}
	for _, r := range doc.Reminders {
		p.pending = append(p.pending, Reminder(r))
	}
	for _, f := range doc.Fired {
		p.fired = append(p.fired, Fired(f))
	}
	return p, nil
}

// persisted is everything one read or write of the store carries.
type persisted struct {
	pending []Reminder
	fired   []Fired
	nextID  int
}

// writeStore persists the store atomically: temp file in the same directory,
// fsync, rename, fsync the directory — a crash mid-write leaves the old
// store or the new one, never a torn file (the ADR 0011 discipline,
// verbatim).
func writeStore(path string, p persisted) error {
	doc := document{
		Version:   documentVersion,
		NextID:    p.nextID,
		Reminders: make([]reminderRecord, 0, len(p.pending)),
		Fired:     make([]firedRecord, 0, len(p.fired)),
	}
	for _, r := range p.pending {
		r.Due, r.Created = r.Due.UTC(), r.Created.UTC()
		doc.Reminders = append(doc.Reminders, reminderRecord(r))
	}
	for _, f := range p.fired {
		f.Due, f.At = f.Due.UTC(), f.At.UTC()
		doc.Fired = append(doc.Fired, firedRecord(f))
	}
	var buf bytes.Buffer
	buf.WriteString(header)
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return fmt.Errorf("encode reminder store: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	// MkdirAll applies its mode only to directories it creates; assert the
	// 0700 on every write rather than hope (the focus store's rule).
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".reminders-*.tmp")
	if err != nil {
		return fmt.Errorf("write reminder store: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write reminder store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write reminder store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write reminder store: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write reminder store: %w", err)
	}
	// CreateTemp asks for 0600 but the umask can clear bits; reassert.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure reminder store: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("write reminder store: %w", err)
	}
	return nil
}

// syncDir fsyncs a directory so entries renamed into it survive a crash.
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
