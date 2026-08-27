package vocabulary

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// This file is the disk half of the Store: one TOML document under the XDG
// state dir, written atomically with the same fsync-and-rename discipline as
// the memory book and conversation history (ADR 0011), private to the user
// (0600 in a 0700 directory).
//
// TOML for the memory book's reason, verbatim: the store's contract is that
// the *user* edits it — correcting a meaning with a text editor is a
// first-class operation — and TOML is the dialect this project already asks
// users to hand-edit, with bare multi-word strings, native datetimes, and no
// escaping puzzles. The store is small, curated, and rewritten whole on
// every change, which is exactly the shape TOML wins at.

// document is the on-disk shape.
type document struct {
	Version int `toml:"version"`
	// NextID is the id high-water mark. Persisted so an id is never reused
	// even across forgets and restarts: a supersede trail or a conversation
	// that once named "w2" must never come to describe a different phrase.
	NextID  int           `toml:"next_id"`
	Entries []entryRecord `toml:"entry"`
}

type entryRecord struct {
	ID      string    `toml:"id"`
	Phrase  string    `toml:"phrase"`
	Meaning string    `toml:"meaning"`
	Note    string    `toml:"note,omitempty"`
	Taught  time.Time `toml:"taught"`
	Updated time.Time `toml:"updated"`
	Source  string    `toml:"source,omitempty"`
	// HardToHear is omitted when false so an unflagged entry writes exactly
	// the lines it would have written before the flag existed — hand-edit
	// diffs stay clean, the factRecord precedent.
	HardToHear bool             `toml:"hard_to_hear,omitempty"`
	Previous   []revisionRecord `toml:"previous,omitempty"`
}

type revisionRecord struct {
	Phrase     string    `toml:"phrase"`
	Meaning    string    `toml:"meaning"`
	Note       string    `toml:"note,omitempty"`
	Taught     time.Time `toml:"taught"`
	Superseded time.Time `toml:"superseded"`
}

// documentVersion is bumped when the shape changes incompatibly; an
// unrecognised version is treated like corruption (warn, serve empty, never
// overwrite) rather than guessed at.
const documentVersion = 1

// header is written at the top of every save — the file's own documentation,
// discoverable by the person the file belongs to without reading source.
const header = `# Jarvix's taught vocabulary — the words behind "when I say X I mean Y".
#
# Edit freely; Jarvix picks up changes without a restart. Each [[entry]]
# needs a phrase and a meaning; ids and timestamps are filled in when
# missing. hard_to_hear = true also biases speech recognition toward the
# phrase (the list is small — Jarvix caps it). [[entry.previous]] entries are
# the meanings a phrase held before it was re-taught. Jarvix rewrites this
# file whenever an entry changes; comments are not preserved.

`

// readStore loads the vocabulary file. A parse failure, unknown version, or
// unknown key is an error the Store downgrades to a warning plus an empty
// vocabulary; content never travels inside the error.
func readStore(path string) ([]Entry, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("read vocabulary store: %w", err)
	}
	var doc document
	md, err := toml.Decode(string(data), &doc)
	if err != nil {
		return nil, 0, fmt.Errorf("parse vocabulary store: %w", err)
	}
	// An unknown key is most likely a hand-edit typo ("meening"), and
	// silently dropping the value it holds would look exactly like Jarvix
	// forgetting a word. Refusing loudly gets the documented degradation:
	// a warning, an empty vocabulary, and a file that is never overwritten.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, 0, fmt.Errorf("vocabulary store has unknown key %q", undecoded[0].String())
	}
	if doc.Version != documentVersion {
		return nil, 0, fmt.Errorf("vocabulary store version %d is not supported", doc.Version)
	}
	entries := make([]Entry, 0, len(doc.Entries))
	for _, r := range doc.Entries {
		e := Entry{ID: r.ID, Phrase: r.Phrase, Meaning: r.Meaning, Note: r.Note,
			Taught: r.Taught, Updated: r.Updated, Source: r.Source, HardToHear: r.HardToHear}
		for _, p := range r.Previous {
			e.Previous = append(e.Previous, Revision(p))
		}
		entries = append(entries, e)
	}
	return entries, doc.NextID, nil
}

// writeStore persists entries atomically: temp file in the same directory,
// fsync, rename, fsync the directory — a crash mid-write leaves the old
// store or the new one, never a torn file (the ADR 0011 discipline, applied
// verbatim as the memory book applies it).
func writeStore(path string, entries []Entry, nextID int) error {
	doc := document{Version: documentVersion, NextID: nextID,
		Entries: make([]entryRecord, 0, len(entries))}
	for _, e := range entries {
		r := entryRecord{ID: e.ID, Phrase: e.Phrase, Meaning: e.Meaning, Note: e.Note,
			Taught: e.Taught.UTC(), Updated: e.Updated.UTC(),
			Source: e.Source, HardToHear: e.HardToHear}
		for _, p := range e.Previous {
			r.Previous = append(r.Previous, revisionRecord{
				Phrase: p.Phrase, Meaning: p.Meaning, Note: p.Note,
				Taught: p.Taught.UTC(), Superseded: p.Superseded.UTC()})
		}
		doc.Entries = append(doc.Entries, r)
	}
	var buf bytes.Buffer
	buf.WriteString(header)
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return fmt.Errorf("encode vocabulary store: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	// MkdirAll applies its mode only to directories it creates; the content
	// is the user's own language, so 0700 is asserted on every write.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".vocabulary-*.tmp")
	if err != nil {
		return fmt.Errorf("write vocabulary store: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write vocabulary store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write vocabulary store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write vocabulary store: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write vocabulary store: %w", err)
	}
	// CreateTemp asks for 0600 but the umask can clear bits, and the rename
	// carries whatever the temp file ended up with. Reassert rather than hope.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure vocabulary store: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("write vocabulary store: %w", err)
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
