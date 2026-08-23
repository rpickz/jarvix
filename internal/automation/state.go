package automation

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// This file is the disk half of the scheduler: the last-run trail, one TOML
// document under the XDG state dir, written atomically with the
// fsync-and-rename discipline of conversation history (ADR 0011), 0600 in a
// 0700 directory like the feed cache it sits beside.
//
// The trail is deliberately small — when each schedule last fired, and which
// scheduled moment the daemon last dealt with — because that is all the
// missed-while-down policy needs (ADR 0032): schedules resume from
// configuration alone, and the trail only lets boot say "backup notes was due
// at 02:00 while I was off" instead of silently re-firing or silently
// forgetting. Deleting the file resets the trail and nothing else.

// trailDocument is the on-disk shape.
type trailDocument struct {
	Version int           `toml:"version"`
	Trail   []trailRecord `toml:"schedule"`
}

type trailRecord struct {
	Kind string `toml:"kind"`
	Name string `toml:"name"`
	// LastFired is when the schedule last actually fired a run.
	LastFired time.Time `toml:"last_fired,omitempty"`
	// LastDue is the scheduled moment the daemon most recently dealt with —
	// fired, skipped, or refused — which is what the boot-time missed check
	// compares against.
	LastDue time.Time `toml:"last_due,omitempty"`
}

// trailVersion is bumped when the shape changes incompatibly; an unrecognised
// version is a cold trail, not an error.
const trailVersion = 1

// trailHeader documents the file for the person who finds it.
const trailHeader = `# Jarvix's schedule trail — when each scheduled routine or script last
# fired, kept so a boot can report a firing missed while the daemon was off
# (it is reported, never re-fired). Machine-written: edits are overwritten,
# and deleting this file just resets the trail.

`

// entryState is everything remembered about one schedule between firings.
// running is in-memory only: whether a run is in flight is a fact about this
// daemon's goroutines, not about history.
type entryState struct {
	lastFired time.Time
	lastDue   time.Time
	running   bool
}

// entryKey identifies a schedule in the state map and the trail: kind plus
// case-folded name, because names are unique per kind (config validates) and
// every other surface resolves them case-insensitively.
func entryKey(kind Kind, name string) string {
	return string(kind) + "\x00" + strings.ToLower(strings.TrimSpace(name))
}

// readTrail loads the persisted trail. Any failure — missing file, parse
// error, unknown version — returns an empty map and, except for a missing
// file, an error for the caller to log: a cold trail only costs one boot's
// missed report, so cold is always an acceptable answer.
func readTrail(path string) (map[string]*entryState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read schedule trail: %w", err)
	}
	var doc trailDocument
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return nil, fmt.Errorf("parse schedule trail: %w", err)
	}
	if doc.Version != trailVersion {
		return nil, fmt.Errorf("schedule trail version %d is not supported", doc.Version)
	}
	states := make(map[string]*entryState, len(doc.Trail))
	for _, r := range doc.Trail {
		if r.Name == "" {
			continue
		}
		states[entryKey(Kind(r.Kind), r.Name)] = &entryState{
			lastFired: r.LastFired,
			lastDue:   r.LastDue,
		}
	}
	return states, nil
}

// writeTrail persists the trail for the configured entries, in declaration
// order — records for schedules no longer configured are dropped here, which
// is the trail's whole deletion story. Atomic with the ADR 0011 discipline:
// temp file in the same directory, fsync, rename, fsync the directory.
func writeTrail(path string, entries []Entry, states map[string]*entryState) error {
	doc := trailDocument{Version: trailVersion}
	for _, e := range entries {
		st := states[entryKey(e.Kind, e.Name)]
		if st == nil || (st.lastFired.IsZero() && st.lastDue.IsZero()) {
			continue // nothing has ever happened to this schedule
		}
		doc.Trail = append(doc.Trail, trailRecord{
			Kind:      string(e.Kind),
			Name:      e.Name,
			LastFired: st.lastFired.UTC(),
			LastDue:   st.lastDue.UTC(),
		})
	}
	var buf bytes.Buffer
	buf.WriteString(trailHeader)
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return fmt.Errorf("encode schedule trail: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	// MkdirAll applies its mode only to directories it creates; the state dir
	// holds sensitive neighbours (feeds, history), so the 0700 requirement is
	// asserted on every write, not hoped for.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".automations-*.tmp")
	if err != nil {
		return fmt.Errorf("write schedule trail: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write schedule trail: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write schedule trail: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write schedule trail: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write schedule trail: %w", err)
	}
	// CreateTemp asks for 0600 but the umask can clear bits, and the rename
	// carries whatever the temp file ended up with. Reassert rather than hope.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure schedule trail: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("write schedule trail: %w", err)
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
