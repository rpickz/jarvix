// Package setup implements the first-run wizard behind `jarvix setup`.
//
// The wizard is a sequence of idempotent steps (TTS engine, activation, AI
// provider, advisor CLIs, verification). Each step detects its current state
// before doing anything, so re-running the wizard shows finished steps as
// done and skips them; detection and side effects are injected so tests run
// hermetically with fakes and stub binaries.
package setup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// File is a minimal, comment-preserving editor for config.toml. The wizard
// only ever touches the exact keys it owns, byte-preserving everything else,
// so a hand-edited config keeps its comments and layout. It intentionally
// supports just what the wizard writes: string values in named tables.
type File struct {
	path  string
	lines []string
	dirty bool
}

// LoadFile reads the config file at path. A missing file is not an error;
// Save will create it.
func LoadFile(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &File{path: path}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return &File{path: path, lines: strings.Split(string(data), "\n")}, nil
}

// Path returns the file's location on disk.
func (f *File) Path() string { return f.path }

// tableRange returns the line span [start, end) of the section body under
// the exact header [name], with start pointing just past the header line.
// found is false when the header does not exist.
func (f *File) tableRange(name string) (start, end int, found bool) {
	header := "[" + name + "]"
	for i, line := range f.lines {
		trimmed := strings.TrimSpace(line)
		if !found {
			if trimmed == header {
				start, found = i+1, true
			}
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			return start, i, true
		}
	}
	if found {
		return start, len(f.lines), true
	}
	return 0, 0, false
}

// keyLine finds the line index of "key = ..." inside [start, end), or -1.
func (f *File) keyLine(start, end int, key string) int {
	for i := start; i < end; i++ {
		trimmed := strings.TrimSpace(f.lines[i])
		if rest, ok := strings.CutPrefix(trimmed, key); ok {
			rest = strings.TrimSpace(rest)
			if strings.HasPrefix(rest, "=") {
				return i
			}
		}
	}
	return -1
}

// Get returns the value of table.key as written in the file. Only simple
// values are understood (quoted strings, bare scalars); that covers every
// key the wizard owns.
func (f *File) Get(table, key string) (string, bool) {
	start, end, ok := f.tableRange(table)
	if !ok {
		return "", false
	}
	i := f.keyLine(start, end, key)
	if i < 0 {
		return "", false
	}
	_, raw, _ := strings.Cut(f.lines[i], "=")
	return parseValue(raw), true
}

// parseValue extracts a simple TOML value: a basic quoted string, or a bare
// scalar with any trailing comment stripped.
func parseValue(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, `"`) {
		if end := strings.Index(raw[1:], `"`); end >= 0 {
			return raw[1 : 1+end]
		}
	}
	if i := strings.Index(raw, "#"); i >= 0 {
		raw = strings.TrimSpace(raw[:i])
	}
	return raw
}

// HasTablePrefix reports whether any table header [prefix...] exists, e.g.
// HasTablePrefix("advisors.") matches [advisors.claude].
func (f *File) HasTablePrefix(prefix string) bool {
	for _, line := range f.lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "["+prefix) && strings.HasSuffix(trimmed, "]") {
			return true
		}
	}
	return false
}

// TablesWithPrefix lists the table names under a prefix, e.g. advisor names
// for prefix "advisors.".
func (f *File) TablesWithPrefix(prefix string) []string {
	var names []string
	for _, line := range f.lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "["+prefix) && strings.HasSuffix(trimmed, "]") {
			names = append(names, strings.TrimSuffix(strings.TrimPrefix(trimmed, "["+prefix), "]"))
		}
	}
	return names
}

// Set writes table.key = "value" (TOML basic string), replacing the existing
// key line in place or appending to the table; a missing table is appended
// at the end of the file. All other lines are preserved byte-for-byte.
func (f *File) Set(table, key, value string) {
	line := fmt.Sprintf("%s = %q", key, value)
	start, end, ok := f.tableRange(table)
	if !ok {
		if n := len(f.lines); n > 0 && strings.TrimSpace(f.lines[n-1]) != "" {
			f.lines = append(f.lines, "")
		}
		f.lines = append(f.lines, "["+table+"]", line)
		f.dirty = true
		return
	}
	if i := f.keyLine(start, end, key); i >= 0 {
		if f.lines[i] == line {
			return
		}
		f.lines[i] = line
		f.dirty = true
		return
	}
	// Insert after the last non-blank line of the section, keeping any
	// trailing blank separator where it was.
	insert := start
	for i := start; i < end; i++ {
		if strings.TrimSpace(f.lines[i]) != "" {
			insert = i + 1
		}
	}
	f.lines = append(f.lines[:insert], append([]string{line}, f.lines[insert:]...)...)
	f.dirty = true
}

// Save writes the file back if anything changed, creating the directory and
// file on first use. Unchanged files are left untouched.
func (f *File) Save() error {
	if !f.dirty {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return err
	}
	content := strings.Join(f.lines, "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if err := os.WriteFile(f.path, []byte(content), 0o644); err != nil {
		return err
	}
	f.dirty = false
	return nil
}
