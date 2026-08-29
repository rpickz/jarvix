package managed

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
// the memory book, the focus threads and the screen names (ADR 0011),
// private to the user (0600 in a 0700 directory).
//
// TOML for the monitor store's reason, verbatim: the user owns this file.
// "What has Jarvix got hold of?" must be answerable with a text editor, and
// deleting a stanza must be a way of taking a window back — giving up power
// needs no ceremony, in the file as in the spoken verb. The store is tiny,
// rewritten whole on every change, and read far more often than written,
// which is the shape TOML wins at.

// document is the on-disk shape.
//
// There is deliberately no id and no high-water mark. A record's identity is
// the window itself — address, stable id, class and pid together — and an id
// would be a second identity nobody refers to, kept in sync by hand.
type document struct {
	Version int            `toml:"version"`
	Windows []windowRecord `toml:"window"`
	Claims  []claimRecord  `toml:"claim"`
}

type windowRecord struct {
	Address  string    `toml:"address"`
	StableID string    `toml:"stable_id,omitempty"`
	Class    string    `toml:"class"`
	PID      int       `toml:"pid,omitzero"`
	App      string    `toml:"app,omitempty"`
	Source   string    `toml:"source"`
	Program  string    `toml:"program,omitempty"`
	Since    time.Time `toml:"since"`
}

type claimRecord struct {
	Class   string    `toml:"class"`
	Program string    `toml:"program,omitempty"`
	Issued  time.Time `toml:"issued"`
}

// documentVersion is bumped when the shape changes incompatibly; an
// unrecognised version is treated like corruption (warn, manage nothing,
// never overwrite) rather than guessed at.
const documentVersion = 1

// header is written at the top of every save — the file's own documentation,
// discoverable by the person the file belongs to without reading source.
//
// It says what managed means AND what it does not, because that is the one
// thing a reader of this file could otherwise get wrong: seeing their
// terminal listed here, the natural conclusion is that Jarvix may run things
// in it, and the whole design says otherwise.
const header = `# The windows Jarvix manages — the ones it opened, and the ones you handed
# over by saying "take control of this terminal".
#
# Managed means Jarvix may read the window, place it, and type into it, and
# that a job may run there. It does NOT mean Jarvix may run commands: text
# typed into a managed terminal is classified and confirmed exactly as a
# shell command is, every time, and nothing in this file changes that.
#
# Edit freely; Jarvix picks up changes without a restart. Deleting a [[window]]
# stanza releases that window — giving up power needs no permission here
# either. A window is matched on all four of address, stable_id, class and pid
# together: an address on its own is a handle the window manager reuses.
#
# [[claim]] entries are launches whose window has not appeared yet; they turn
# into [[window]] entries when it does, and expire if it never does.
#
# Jarvix rewrites this file whenever what it manages changes; comments are not
# preserved.

`

// readStore loads the file. A parse failure, unknown version, or unknown key
// is an error the Store downgrades to a warning plus an empty store; content
// never travels inside the error.
func readStore(path string) ([]Record, []Claim, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read managed-window store: %w", err)
	}
	var doc document
	md, err := toml.Decode(string(data), &doc)
	if err != nil {
		return nil, nil, fmt.Errorf("parse managed-window store: %w", err)
	}
	// An unknown key is most likely a hand-edit typo ("stableid"), and
	// silently dropping the value it holds would look exactly like Jarvix
	// forgetting which window it was given. Refusing loudly gets the
	// documented degradation: a warning, nothing managed, and a file that is
	// never overwritten.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, nil, fmt.Errorf("managed-window store has unknown key %q", undecoded[0].String())
	}
	if doc.Version != documentVersion {
		return nil, nil, fmt.Errorf("managed-window store version %d is not supported", doc.Version)
	}
	records := make([]Record, 0, len(doc.Windows))
	for _, r := range doc.Windows {
		records = append(records, Record{
			Address: r.Address, StableID: r.StableID, Class: r.Class, PID: r.PID,
			App: r.App, Source: Source(r.Source), Program: r.Program, Since: r.Since,
		})
	}
	claims := make([]Claim, 0, len(doc.Claims))
	for _, c := range doc.Claims {
		claims = append(claims, Claim(c))
	}
	return records, claims, nil
}

// ValidateFile reports whether the store at path would load: parseable TOML,
// no unknown keys, a supported schema version. `jarvix restore` (ADR 0045)
// proves a staged archive with it before swapping anything into place.
func ValidateFile(path string) error {
	_, _, err := readStore(path)
	return err
}

// writeStore persists the store atomically: temp file in the same directory,
// fsync, rename, fsync the directory — a crash mid-write leaves the old store
// or the new one, never a torn file, and the rename is durable rather than
// merely atomic (the ADR 0011 discipline, applied verbatim).
func writeStore(path string, records []Record, claims []Claim) error {
	doc := document{
		Version: documentVersion,
		Windows: make([]windowRecord, 0, len(records)),
		Claims:  make([]claimRecord, 0, len(claims)),
	}
	for _, r := range records {
		doc.Windows = append(doc.Windows, windowRecord{
			Address: r.Address, StableID: r.StableID, Class: r.Class, PID: r.PID,
			App: r.App, Source: string(r.Source), Program: r.Program, Since: r.Since.UTC(),
		})
	}
	for _, c := range claims {
		doc.Claims = append(doc.Claims, claimRecord{
			Class: c.Class, Program: c.Program, Issued: c.Issued.UTC()})
	}
	var buf bytes.Buffer
	buf.WriteString(header)
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return fmt.Errorf("encode managed-window store: %w", err)
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
	tmp, err := os.CreateTemp(dir, ".managed-*.tmp")
	if err != nil {
		return fmt.Errorf("write managed-window store: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write managed-window store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write managed-window store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write managed-window store: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write managed-window store: %w", err)
	}
	// CreateTemp asks for 0600 but the umask can clear bits, and the rename
	// carries whatever the temp file ended up with. Reassert rather than hope.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure managed-window store: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("write managed-window store: %w", err)
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
