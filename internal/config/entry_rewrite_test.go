package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the generalised entry editor (issue #92) to the same
// contract as the settings rewriter: golden files prove that everything
// outside the one field being set — comments, sibling entries, sub-tables,
// formatting — survives byte-for-byte, across the families #92 ships
// (knowledge.feeds) and #93 will adopt (routines).

// TestSetEntryFieldGolden drives the editor over hand-written documents and
// compares byte-for-byte.
func TestSetEntryFieldGolden(t *testing.T) {
	cases := []struct {
		name   string
		family string
		entry  string
		field  string
		value  any
	}{
		// Inserting the key where none exists: lands after the entry's last
		// body key, before the comment that belongs to the next entry.
		{"disable", "knowledge.feeds", "amd", "enabled", false},
		// Replacing an existing key: the inline comment survives.
		{"reenable", "knowledge.feeds", "amd", "enabled", true},
		// A family with sub-tables ([[routines.steps]]): the key lands in the
		// entry's own body, never inside a step — the #93 shape.
		{"routine", "routines", "evening", "enabled", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, err := os.ReadFile(filepath.Join("testdata", "entry", tc.name+".input.toml"))
			if err != nil {
				t.Fatal(err)
			}
			golden, err := os.ReadFile(filepath.Join("testdata", "entry", tc.name+".golden.toml"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := SetEntryField(input, tc.family, tc.entry, tc.field, tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(golden) {
				t.Errorf("rewrite mismatch\n--- got ---\n%s\n--- want ---\n%s", got, golden)
			}
		})
	}
}

// TestSetEntryFieldIsIdempotent: writing the value that already stands
// changes nothing — the byte-preservation contract includes the no-op.
func TestSetEntryFieldIsIdempotent(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "entry", "disable.golden.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := SetEntryField(golden, "knowledge.feeds", "amd", "enabled", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(golden) {
		t.Errorf("repeat write changed the document\n--- got ---\n%s\n--- want ---\n%s", got, golden)
	}
}

// TestSetEntryFieldMatchesNamesCaseInsensitively: the same rule every family
// uses for name uniqueness applies to addressing.
func TestSetEntryFieldMatchesNamesCaseInsensitively(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("testdata", "entry", "disable.input.toml"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := SetEntryField(input, "knowledge.feeds", "  AMD ", "enabled", false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseBytes(out)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Knowledge.Feeds[0].IsEnabled() {
		t.Error("the case-insensitively addressed feed was not disabled")
	}
	if !cfg.Knowledge.Feeds[1].IsEnabled() {
		t.Error("the sibling feed was disabled too")
	}
}

// TestSetEntryFieldRefusals: an unknown entry, an unknown family, and a
// document that does not parse each refuse without writing.
func TestSetEntryFieldRefusals(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("testdata", "entry", "disable.input.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SetEntryField(input, "knowledge.feeds", "nvda", "enabled", false); err == nil ||
		!strings.Contains(err.Error(), `named "nvda"`) {
		t.Errorf("unknown entry error = %v, want it named", err)
	}
	if _, err := SetEntryField(input, "scripts", "amd", "enabled", false); err == nil {
		t.Error("unknown family must refuse — there is no entry to edit")
	}
	if _, err := SetEntryField([]byte("not = toml ["), "knowledge.feeds", "amd", "enabled", false); err == nil ||
		!strings.Contains(err.Error(), "fix it by hand") {
		t.Errorf("unparsable document error = %v, want the hand-fix pointer", err)
	}
}

// TestSetEntryFieldOtherScalars: the editor is not an enabled-only tool — the
// value side takes what encodeTOMLValue takes, which is what lets #93 reuse
// it beyond the switch.
func TestSetEntryFieldOtherScalars(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("testdata", "entry", "disable.input.toml"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := SetEntryField(input, "knowledge.feeds", "weather", "ttl_sec", 1200)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseBytes(out)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Knowledge.Feeds[1].TTLSec != 1200 {
		t.Errorf("ttl_sec = %d, want the written 1200", cfg.Knowledge.Feeds[1].TTLSec)
	}
	if !strings.Contains(string(out), "# watches the AMD price") {
		t.Error("a hand comment vanished")
	}
}
