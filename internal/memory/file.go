package memory

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// This file is the disk half of the Book: one TOML document under the XDG
// state dir, written atomically with the same fsync-and-rename discipline as
// conversation history (ADR 0011), private to the user (0600 in a 0700
// directory).
//
// TOML rather than JSONL, and the reason is the owner. The store's contract
// is that the *user* edits it — correcting a fact with a text editor is a
// first-class operation, not a recovery procedure — and TOML is the dialect
// this project already asks users to hand-edit (config.toml), with bare
// multi-word strings, native datetimes, and no escaping puzzles. JSONL wins
// for append-only machine logs, which is exactly what this store is not: it
// is small, curated, and rewritten whole on every change.

// document is the on-disk shape.
type document struct {
	Version int `toml:"version"`
	// NextID is the id high-water mark. Persisted so an id is never reused
	// even across forgets and restarts: a supersede trail or a conversation
	// that once named "m2" must never come to describe a different fact.
	NextID int          `toml:"next_id"`
	Facts  []factRecord `toml:"fact"`
}

type factRecord struct {
	ID       string           `toml:"id"`
	Content  string           `toml:"content"`
	Stored   time.Time        `toml:"stored"`
	Updated  time.Time        `toml:"updated"`
	Source   string           `toml:"source,omitempty"`
	Previous []revisionRecord `toml:"previous,omitempty"`
}

type revisionRecord struct {
	Content    string    `toml:"content"`
	Stored     time.Time `toml:"stored"`
	Superseded time.Time `toml:"superseded"`
}

// documentVersion is bumped when the shape changes incompatibly; an
// unrecognised version is treated like corruption (warn, serve empty, never
// overwrite) rather than guessed at.
const documentVersion = 1

// header is written at the top of every save. It is the file's own
// documentation — the format must be discoverable by the person the file
// belongs to, without them ever reading Jarvix's source.
const header = `# Jarvix's remembered facts — the knowledge base behind "remember that ...".
#
# Edit freely; Jarvix picks up changes without a restart. Each [[fact]] needs
# a content; ids and timestamps are filled in when missing. [[fact.previous]]
# entries are the values a fact held before it was corrected. Jarvix rewrites
# this file whenever a fact changes, and comments are not preserved.

`

// readStore loads the fact file. A parse failure, unknown version, or
// unknown key is an error the Book downgrades to a warning plus an empty
// memory; content never travels inside the error.
func readStore(path string) ([]Fact, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("read memory store: %w", err)
	}
	var doc document
	md, err := toml.Decode(string(data), &doc)
	if err != nil {
		return nil, 0, fmt.Errorf("parse memory store: %w", err)
	}
	// An unknown key is most likely a hand-edit typo ("contnet"), and
	// silently dropping the value it holds would look exactly like Jarvix
	// forgetting. Refusing loudly gets the documented degradation instead:
	// a warning, an empty memory, and a file that is never overwritten.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, 0, fmt.Errorf("memory store has unknown key %q", undecoded[0].String())
	}
	if doc.Version != documentVersion {
		return nil, 0, fmt.Errorf("memory store version %d is not supported", doc.Version)
	}
	facts := make([]Fact, 0, len(doc.Facts))
	for _, r := range doc.Facts {
		f := Fact{ID: r.ID, Content: r.Content, Stored: r.Stored, Updated: r.Updated, Source: r.Source}
		for _, p := range r.Previous {
			f.Previous = append(f.Previous, Revision(p))
		}
		facts = append(facts, f)
	}
	return facts, doc.NextID, nil
}

// writeStore persists facts atomically: temp file in the same directory,
// fsync, rename, fsync the directory — a crash mid-write leaves the old
// store or the new one, never a torn file, and the rename is durable rather
// than merely atomic (the ADR 0011 discipline, applied verbatim).
func writeStore(path string, facts []Fact, nextID int) error {
	doc := document{Version: documentVersion, NextID: nextID,
		Facts: make([]factRecord, 0, len(facts))}
	for _, f := range facts {
		r := factRecord{ID: f.ID, Content: f.Content, Stored: f.Stored.UTC(), Updated: f.Updated.UTC(), Source: f.Source}
		for _, p := range f.Previous {
			r.Previous = append(r.Previous, revisionRecord{
				Content: p.Content, Stored: p.Stored.UTC(), Superseded: p.Superseded.UTC()})
		}
		doc.Facts = append(doc.Facts, r)
	}
	var buf bytes.Buffer
	buf.WriteString(header)
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return fmt.Errorf("encode memory store: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	// MkdirAll applies its mode only to directories it creates; an existing
	// state dir keeps whatever modes it had. The content is the user's life,
	// so the 0700 requirement is asserted on every write, not hoped for.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".memory-*.tmp")
	if err != nil {
		return fmt.Errorf("write memory store: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write memory store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write memory store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write memory store: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write memory store: %w", err)
	}
	// CreateTemp asks for 0600 but the umask can clear bits, and the rename
	// carries whatever the temp file ended up with. Reassert rather than hope.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure memory store: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("write memory store: %w", err)
	}
	return nil
}

// syncDir fsyncs a directory so entries renamed into it survive a crash —
// atomic is not durable until the directory entry itself is on disk.
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
