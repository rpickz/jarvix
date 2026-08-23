package knowledge

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

// This file is the disk half of the feed cache: one TOML document under the
// XDG state dir, written atomically with the fsync-and-rename discipline of
// conversation history (ADR 0011), private to the user (0600 in a 0700
// directory) because feed values may be sensitive.
//
// Unlike the memory store (ADR 0025) this file is machine-written, not
// user-curated: it exists so the daemon boots warm, and every byte of it is
// reproducible by running the feed commands again. That changes the failure
// posture — an unparseable file is discarded and rewritten on the next fetch,
// never preserved for hand repair — but not the format: TOML keeps it
// readable by the person it belongs to, and deleting it is a supported way
// to reset a feed.

// valuesDocument is the on-disk shape.
type valuesDocument struct {
	Version int           `toml:"version"`
	Values  []valueRecord `toml:"value"`
}

type valueRecord struct {
	Feed      string    `toml:"feed"`
	Value     string    `toml:"value"`
	Truncated bool      `toml:"truncated,omitempty"`
	Fetched   time.Time `toml:"fetched"`
	// The failure trail travels with the value so "failing since yesterday"
	// survives a restart — doctor's answer must not reset to healthy just
	// because the daemon did.
	LastAttempt  time.Time `toml:"last_attempt,omitempty"`
	Failures     int       `toml:"failures,omitempty"`
	FailingSince time.Time `toml:"failing_since,omitempty"`
	LastError    string    `toml:"last_error,omitempty"`
}

// valuesVersion is bumped when the shape changes incompatibly; an
// unrecognised version is a cold boot, not an error.
const valuesVersion = 1

// valuesHeader documents the file for the person who finds it.
const valuesHeader = `# Jarvix's cached feed values — the latest output of each [[knowledge.feeds]]
# command, kept so the daemon boots warm. Machine-written: edits are
# overwritten on the next fetch, and deleting this file just makes every
# feed fetch fresh.

`

// readValues loads the persisted cache. Any failure — missing file, parse
// error, unknown version — returns an empty map and, except for a missing
// file, an error for the caller to log: the cache is reproducible, so cold
// is always an acceptable answer.
func readValues(path string) (map[string]*feedState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read feed values: %w", err)
	}
	var doc valuesDocument
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return nil, fmt.Errorf("parse feed values: %w", err)
	}
	if doc.Version != valuesVersion {
		return nil, fmt.Errorf("feed values version %d is not supported", doc.Version)
	}
	states := make(map[string]*feedState, len(doc.Values))
	for _, r := range doc.Values {
		if r.Feed == "" {
			continue
		}
		states[r.Feed] = &feedState{
			Value:        r.Value,
			Truncated:    r.Truncated,
			FetchedAt:    r.Fetched,
			LastAttempt:  r.LastAttempt,
			Failures:     r.Failures,
			FailingSince: r.FailingSince,
			LastErr:      r.LastError,
		}
	}
	return states, nil
}

// writeValues persists the cache for the configured feeds, in declaration
// order, atomically: temp file in the same directory, fsync, rename, fsync
// the directory — a crash mid-write leaves the old cache or the new one,
// never a torn file (the ADR 0011 discipline, applied verbatim).
func writeValues(path string, feeds []Feed, states map[string]*feedState) error {
	doc := valuesDocument{Version: valuesVersion}
	for _, f := range feeds {
		st := states[f.Name]
		if st == nil || (st.FetchedAt.IsZero() && st.LastAttempt.IsZero()) {
			continue // nothing has ever happened to this feed
		}
		doc.Values = append(doc.Values, valueRecord{
			Feed:         f.Name,
			Value:        st.Value,
			Truncated:    st.Truncated,
			Fetched:      st.FetchedAt.UTC(),
			LastAttempt:  st.LastAttempt.UTC(),
			Failures:     st.Failures,
			FailingSince: st.FailingSince.UTC(),
			LastError:    st.LastErr,
		})
	}
	var buf bytes.Buffer
	buf.WriteString(valuesHeader)
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return fmt.Errorf("encode feed values: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	// MkdirAll applies its mode only to directories it creates; the values
	// may be sensitive, so the 0700 requirement is asserted on every write,
	// not hoped for.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".feeds-*.tmp")
	if err != nil {
		return fmt.Errorf("write feed values: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write feed values: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write feed values: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write feed values: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write feed values: %w", err)
	}
	// CreateTemp asks for 0600 but the umask can clear bits, and the rename
	// carries whatever the temp file ended up with. Reassert rather than hope.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure feed values: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("write feed values: %w", err)
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
