package undo

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// This file is the disk half of the account: one TOML document under the XDG
// state dir, written atomically with the fsync-and-rename discipline every
// durable store here keeps (ADR 0011), private to the user (0600 in a 0700
// directory).
//
// TOML for the reminder store's reason, with one of its own. The user owns
// this file: "what did Jarvix do?" must be answerable with a text editor and
// no daemon running, and deleting a stanza must be a way of saying "I would
// rather not keep a record of that" — the conversation archive's stance
// (ADR 0027), applied to actions. The store is small, capped, rewritten whole
// on every change, and read far more often than written, which is the shape
// TOML wins at.
//
// One consequence is worth stating rather than discovering. A file restore
// carries the previous bytes of the file it would put back, and TOML quotes a
// string containing newlines as one escaped line. So a [action.file] stanza's
// `previous` is long and ugly to read. That is the price of byte-exactness,
// and byte-exactness is the whole point: the config editor deliberately
// preserves comments, key order and spacing that no re-serialisation would
// reproduce, and an undo that lost the user's comments would be a second edit
// wearing a reversal's name.

// document is the on-disk shape.
type document struct {
	Version int `toml:"version"`
	// NextID is the id high-water mark, persisted so an id is never reused —
	// not across a restart, and not after the record holding it fell off the
	// cap or was deleted by hand. A conversation that once named an action
	// must never come to name a different one.
	NextID int `toml:"next_id"`
	// Forgotten counts the records the cap has evicted over this file's
	// lifetime. It is the disclosure's arithmetic: "the last 100 actions, and
	// 37 older ones I no longer keep" is a true sentence; "the last 100
	// actions" alone is a true sentence that reads like a complete one.
	Forgotten int            `toml:"forgotten,omitzero"`
	Actions   []actionRecord `toml:"action"`
}

type actionRecord struct {
	ID         string    `toml:"id"`
	At         time.Time `toml:"at"`
	Tool       string    `toml:"tool"`
	Summary    string    `toml:"summary"`
	Target     string    `toml:"target,omitempty"`
	Job        string    `toml:"job,omitempty"`
	Kind       string    `toml:"kind"`
	Because    string    `toml:"because,omitempty"`
	Provenance []string  `toml:"provenance,omitempty"`
	UndoneBy   string    `toml:"undone_by,omitempty"`
	// UndoneAt is a pointer so an action that still stands writes no key at
	// all: BurntSushi renders a zero time.Time as 0001-01-01, which would put
	// a date on every row that has not been undone and read as one that had.
	UndoneAt *time.Time   `toml:"undone_at,omitempty"`
	File     *fileRecord  `toml:"file,omitempty"`
	Window   *windowState `toml:"window,omitempty"`
}

type fileRecord struct {
	Path        string `toml:"path"`
	Existed     bool   `toml:"existed"`
	Previous    string `toml:"previous"`
	AfterDigest string `toml:"after_digest,omitempty"`
}

type windowState struct {
	Address       string `toml:"address"`
	StableID      string `toml:"stable_id,omitempty"`
	Class         string `toml:"class"`
	PID           int    `toml:"pid,omitzero"`
	Describe      string `toml:"describe,omitempty"`
	Workspace     int    `toml:"workspace"`
	WorkspaceName string `toml:"workspace_name,omitempty"`
	Floating      bool   `toml:"floating"`
	Fullscreen    bool   `toml:"fullscreen"`
	X             int    `toml:"x,omitzero"`
	Y             int    `toml:"y,omitzero"`
	Width         int    `toml:"width,omitzero"`
	Height        int    `toml:"height,omitzero"`
}

// documentVersion is bumped when the shape changes incompatibly; an
// unrecognised version is treated like corruption (warn, serve an empty
// account, never overwrite) rather than guessed at.
const documentVersion = 1

// header is written at the top of every save — the file's own documentation,
// discoverable by the person it belongs to without reading source.
//
// It discloses the bound in the file itself, not only on the listing, because
// the file is one of the places a person goes looking for an action that is
// no longer there.
const header = `# What Jarvix did in your name, and what would put it back.
#
# One [[action]] per thing Jarvix changed on this machine. kind says how it can
# be reversed: "file" restores the previous bytes of a file, "window" puts a
# window back where it was, "none" means it genuinely cannot be undone and
# because says why. A shell command you approved is always "none": it ran, and
# nothing here can un-run it.
#
# Only the last 100 actions are kept — the count of the ones that fell off is
# in forgotten, so this file never quietly shortens its own memory. undone_by
# names the action that reversed this one; a reversal is an action too, so it
# gets a row of its own.
#
# Edit freely; Jarvix picks up changes without a restart. Deleting an [[action]]
# stanza deletes it from the account, which is the right thing to do with a
# command whose text you would rather not keep — nothing is tombstoned, and the
# id is not reissued. An [action.file] "previous" holds the exact bytes of the
# file before the change, escaped onto one line; removing it leaves a record of
# what happened with nothing to restore, which Jarvix will say plainly.
#
# Jarvix rewrites this file whenever it acts; comments are not preserved.

`

// persisted is everything one read or write of the account carries.
type persisted struct {
	records   []Record
	nextID    int
	forgotten int
}

// readStore loads the file. A parse failure, unknown version, or unknown key
// is an error the Store downgrades to a warning plus an empty account;
// content never travels inside the error.
func readStore(path string) (persisted, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return persisted{}, fmt.Errorf("read action account: %w", err)
	}
	var doc document
	md, err := toml.Decode(string(data), &doc)
	if err != nil {
		return persisted{}, fmt.Errorf("parse action account: %w", err)
	}
	// An unknown key is most likely a hand-edit typo ("summry"), and silently
	// dropping the value it holds would look exactly like Jarvix forgetting
	// what it did. Refusing loudly gets the documented degradation: a warning,
	// an empty account, and a file that is never overwritten.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return persisted{}, fmt.Errorf("action account has unknown key %q", undecoded[0].String())
	}
	if doc.Version != documentVersion {
		return persisted{}, fmt.Errorf("action account version %d is not supported", doc.Version)
	}
	p := persisted{nextID: doc.NextID, forgotten: doc.Forgotten}
	for _, r := range doc.Actions {
		rec := Record{
			ID: r.ID, At: r.At, UndoneBy: r.UndoneBy,
			Action: Action{
				Tool: r.Tool, Summary: r.Summary, Target: r.Target, Job: r.Job,
				Provenance: append([]string(nil), r.Provenance...),
				Restore:    Restore{Kind: Kind(r.Kind), Because: r.Because},
			},
		}
		if r.UndoneAt != nil {
			rec.UndoneAt = *r.UndoneAt
		}
		if r.File != nil {
			rec.Restore.File = &FileRestore{
				Path: r.File.Path, Existed: r.File.Existed,
				Previous: r.File.Previous, AfterDigest: r.File.AfterDigest,
			}
		}
		if r.Window != nil {
			w := *r.Window
			rec.Restore.Window = &WindowState{
				Address: w.Address, StableID: w.StableID, Class: w.Class, PID: w.PID,
				Describe: w.Describe, Workspace: w.Workspace, WorkspaceName: w.WorkspaceName,
				Floating: w.Floating, Fullscreen: w.Fullscreen,
				X: w.X, Y: w.Y, Width: w.Width, Height: w.Height,
			}
		}
		p.records = append(p.records, rec)
	}
	return p, nil
}

// ValidateFile reports whether the account at path would load: parseable
// TOML, no unknown keys, a supported schema version. `jarvix restore` (ADR
// 0045) proves a staged archive with it before swapping anything into place.
func ValidateFile(path string) error {
	_, err := readStore(path)
	return err
}

// writeStore persists the account atomically: temp file in the same
// directory, fsync, rename, fsync the directory — a crash mid-write leaves
// the old account or the new one, never a torn file, and the rename is
// durable rather than merely atomic (the ADR 0011 discipline, verbatim).
func writeStore(path string, p persisted) error {
	doc := document{
		Version:   documentVersion,
		NextID:    p.nextID,
		Forgotten: p.forgotten,
		Actions:   make([]actionRecord, 0, len(p.records)),
	}
	for _, r := range p.records {
		out := actionRecord{
			ID: r.ID, At: r.At.UTC(), Tool: r.Tool, Summary: r.Summary,
			Target: r.Target, Job: r.Job, Kind: string(r.Restore.Kind),
			Because: r.Restore.Because, Provenance: append([]string(nil), r.Provenance...),
			UndoneBy: r.UndoneBy,
		}
		if !r.UndoneAt.IsZero() {
			at := r.UndoneAt.UTC()
			out.UndoneAt = &at
		}
		if f := r.Restore.File; f != nil {
			out.File = &fileRecord{
				Path: f.Path, Existed: f.Existed,
				Previous: f.Previous, AfterDigest: f.AfterDigest,
			}
		}
		if w := r.Restore.Window; w != nil {
			out.Window = &windowState{
				Address: w.Address, StableID: w.StableID, Class: w.Class, PID: w.PID,
				Describe: w.Describe, Workspace: w.Workspace, WorkspaceName: w.WorkspaceName,
				Floating: w.Floating, Fullscreen: w.Fullscreen,
				X: w.X, Y: w.Y, Width: w.Width, Height: w.Height,
			}
		}
		doc.Actions = append(doc.Actions, out)
	}

	var buf bytes.Buffer
	buf.WriteString(header)
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return fmt.Errorf("encode action account: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	// MkdirAll applies its mode only to directories it creates, and this file
	// sits beside stores that are private, so 0700 is asserted on every write.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".undo-*.tmp")
	if err != nil {
		return fmt.Errorf("write action account: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write action account: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write action account: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write action account: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write action account: %w", err)
	}
	// CreateTemp asks for 0600 but the umask can clear bits, and the rename
	// carries whatever the temp file ended up with. Reassert rather than hope:
	// this file holds the previous contents of the user's configuration.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure action account: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("write action account: %w", err)
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
