package monitors

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
// the memory book, the taught vocabulary and the reminder store (ADR 0011),
// private to the user (0600 in a 0700 directory).
//
// TOML for the memory book's reason, verbatim: the store's contract is that
// the *user* edits it — repointing "top" at a different connector after a
// dock move is a first-class operation, and doing it in a text editor must
// work as well as saying it — and TOML is the dialect this project already
// asks users to hand-edit. The store is tiny, rewritten whole on every
// change, and read far more often than it is written, which is exactly the
// shape TOML wins at.

// document is the on-disk shape.
//
// There is deliberately no id and no next_id high-water mark, and that is a
// difference from the vocabulary store rather than an omission. A nickname's
// identity IS its name: the user says "top", the routine writes `top`, and
// nothing anywhere refers to a nickname by a handle. An id would be a second
// identity nobody uses, kept in sync by hand.
type document struct {
	Version   int              `toml:"version"`
	Nicknames []nicknameRecord `toml:"nickname"`
}

type nicknameRecord struct {
	Name      string    `toml:"name"`
	Connector string    `toml:"connector"`
	Named     time.Time `toml:"named"`
	Updated   time.Time `toml:"updated"`
}

// documentVersion is bumped when the shape changes incompatibly; an
// unrecognised version is treated like corruption (warn, serve no nicknames,
// never overwrite) rather than guessed at.
const documentVersion = 1

// header is written at the top of every save — the file's own documentation,
// discoverable by the person the file belongs to without reading source.
const header = `# What Jarvix calls your screens — the names behind "call this monitor top".
#
# Edit freely; Jarvix picks up changes without a restart. Each [[nickname]]
# needs a name (one word, yours to choose) and a connector as the window
# manager spells it — run "jarvix monitors" to see what is plugged in.
#
# Names are resolved when a routine runs, never when it is written, so moving
# a cable is one edit here rather than an edit to every routine. A connector
# that is not plugged in is not an error in this file: Jarvix says "no monitor
# is called <name> right now" at the moment it matters and carries on.
#
# Jarvix rewrites this file whenever a nickname changes; comments are not
# preserved.

`

// readStore loads the nickname file. A parse failure, unknown version, or
// unknown key is an error the Store downgrades to a warning plus an empty
// set of nicknames; content never travels inside the error.
func readStore(path string) ([]Nickname, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read monitor store: %w", err)
	}
	var doc document
	md, err := toml.Decode(string(data), &doc)
	if err != nil {
		return nil, fmt.Errorf("parse monitor store: %w", err)
	}
	// An unknown key is most likely a hand-edit typo ("conector"), and
	// silently dropping the value it holds would look exactly like Jarvix
	// losing a screen name. Refusing loudly gets the documented degradation:
	// a warning, no nicknames, and a file that is never overwritten.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("monitor store has unknown key %q", undecoded[0].String())
	}
	if doc.Version != documentVersion {
		return nil, fmt.Errorf("monitor store version %d is not supported", doc.Version)
	}
	out := make([]Nickname, 0, len(doc.Nicknames))
	for _, r := range doc.Nicknames {
		// A conversion rather than a field-by-field copy, which is only safe
		// while the two shapes stay identical — and they are meant to: the
		// record IS the nickname, with toml tags. Adding a field to one and
		// not the other stops compiling here, which is the point.
		out = append(out, Nickname(r))
	}
	return out, nil
}

// ValidateFile reports whether the store at path would load: parseable TOML,
// no unknown keys, a supported schema version. `jarvix restore` (ADR 0045)
// proves a staged archive with it before swapping anything into place.
func ValidateFile(path string) error {
	_, err := readStore(path)
	return err
}

// writeStore persists the nicknames atomically: temp file in the same
// directory, fsync, rename, fsync the directory — a crash mid-write leaves
// the old store or the new one, never a torn file (the ADR 0011 discipline,
// applied verbatim as the vocabulary store applies it).
func writeStore(path string, names []Nickname) error {
	doc := document{Version: documentVersion, Nicknames: make([]nicknameRecord, 0, len(names))}
	for _, n := range names {
		doc.Nicknames = append(doc.Nicknames, nicknameRecord{
			Name: n.Name, Connector: n.Connector,
			Named: n.Named.UTC(), Updated: n.Updated.UTC()})
	}
	var buf bytes.Buffer
	buf.WriteString(header)
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return fmt.Errorf("encode monitor store: %w", err)
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
	tmp, err := os.CreateTemp(dir, ".monitors-*.tmp")
	if err != nil {
		return fmt.Errorf("write monitor store: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write monitor store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write monitor store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write monitor store: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write monitor store: %w", err)
	}
	// CreateTemp asks for 0600 but the umask can clear bits, and the rename
	// carries whatever the temp file ended up with. Reassert rather than hope.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure monitor store: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("write monitor store: %w", err)
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
