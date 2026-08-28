package approvals

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// This file is the disk half of the approval ledger: one TOML document under
// the XDG state dir, written atomically with the fsync-and-rename discipline
// of conversation history (ADR 0011), private to the user (0600 in a 0700
// directory) — the reminder store's storage contract, applied to approval
// history (issue #162, ADR 0053).
//
// Deliberately NOT config.toml, and the distinction matters more here than
// anywhere else in the project. config.toml holds what Jarvix MAY DO; this
// file holds what Jarvix HAS DONE. Mixing them would mean a firing count
// rewriting the permission file several times a minute — churning the very
// document whose diffs are the user's record of what they granted, and
// putting a machine-written counter next to a human-written rule where a bad
// write could damage both.

// document is the on-disk shape.
type document struct {
	Version   int           `toml:"version"`
	Approvals []entryRecord `toml:"approval"`
}

type entryRecord struct {
	Pattern string `toml:"pattern"`
	Source  string `toml:"source"`
	// Added is when the card wrote the rule; omitted for a hand-added one,
	// because a zero timestamp read back as 0001-01-01 would be a date the
	// Approvals view then has to explain.
	Added    time.Time `toml:"added,omitempty"`
	Uses     int       `toml:"uses,omitempty"`
	LastUsed time.Time `toml:"last_used,omitempty"`
}

// documentVersion is bumped when the shape changes incompatibly; an
// unrecognised version is treated like corruption (warn, serve empty, never
// overwrite) rather than guessed at.
const documentVersion = 1

// header is written at the top of every save — the file's own documentation,
// the reminder store's rule.
const header = `# Jarvix's approval history — when each pre-approved command pattern was
# agreed to, and how often it has since let a command run without asking.
#
# This file is a RECORD, not a permission. The patterns themselves live in
# config.toml under [tools.policy] shell_allow, and that file alone decides
# what runs without asking: deleting this one loses the history and changes
# nothing about what Jarvix may do. Revoke a pattern with
# "jarvix approvals forget <pattern>", the window's Approvals tab, or by
# editing config.toml directly.
# Jarvix rewrites this file whenever an approval fires; comments not preserved.

`

// readLedger loads the ledger file.
func readLedger(path string) (map[string]*record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc document
	md, err := toml.Decode(string(data), &doc)
	if err != nil {
		return nil, fmt.Errorf("parse approval ledger: %w", err)
	}
	// An unknown key is most likely a hand-edit typo. Refusing loudly gets
	// the documented degradation — a warning and an empty history — rather
	// than silently dropping whatever it held.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("approval ledger has unknown key %q", undecoded[0].String())
	}
	if doc.Version != documentVersion {
		return nil, fmt.Errorf("approval ledger version %d is not supported", doc.Version)
	}
	out := make(map[string]*record, len(doc.Approvals))
	for _, e := range doc.Approvals {
		key := normalise(e.Pattern)
		if key == "" {
			continue
		}
		source := e.Source
		if source != SourceCard {
			source = SourceHand
		}
		out[key] = &record{source: source, added: e.Added, uses: e.Uses, lastUsed: e.LastUsed}
	}
	return out, nil
}

// isNotExist reports whether err is the ordinary "no ledger yet". A first run
// is not a warning.
func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// writeLedger persists the ledger atomically: temp file in the same
// directory, fsync, rename, fsync the directory — a crash mid-write leaves
// the old ledger or the new one, never a torn file.
func writeLedger(path string, records map[string]*record) error {
	doc := document{Version: documentVersion, Approvals: make([]entryRecord, 0, len(records))}
	for _, key := range sortedKeys(records) {
		r := records[key]
		doc.Approvals = append(doc.Approvals, entryRecord{
			Pattern:  key,
			Source:   r.source,
			Added:    r.added.UTC(),
			Uses:     r.uses,
			LastUsed: r.lastUsed.UTC(),
		})
	}
	var buf bytes.Buffer
	buf.WriteString(header)
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return fmt.Errorf("encode approval ledger: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	// MkdirAll applies its mode only to directories it creates; assert the
	// 0700 on every write rather than hope.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".approvals-*.tmp")
	if err != nil {
		return fmt.Errorf("write approval ledger: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write approval ledger: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write approval ledger: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write approval ledger: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write approval ledger: %w", err)
	}
	// CreateTemp asks for 0600 but the umask can clear bits; reassert.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure approval ledger: %w", err)
	}
	return syncDir(dir)
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
