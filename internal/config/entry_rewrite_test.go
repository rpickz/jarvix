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
		// Re-enabling a routine (#93): the existing key flips in place, its
		// inline comment kept, the steps untouched.
		{"routine_reenable", "routines", "evening", "enabled", true},
		// The scripts family (#93): the key lands after the entry's own last
		// body key, before the comment that belongs to the next entry, with
		// the sibling's inline comment intact.
		{"script", "scripts", "backup notes", "enabled", false},
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

// The whole-entry editor (#99): the key orders the daemon's form surface
// uses, restated here so the goldens render in the documented shape.
var (
	routineKeyOrder = []string{"name", "phrases", "schedule", "announce", "enabled", "steps"}
	routineSubOrder = map[string][]string{
		"steps": {"app", "match", "workspace", "float", "size", "position", "tile"},
	}
	scriptKeyOrder = []string{"name", "phrases", "path", "timeout_sec", "report", "schedule", "announce", "enabled"}
	feedKeyOrder   = []string{"name", "description", "command", "mode",
		"interval_sec", "ttl_sec", "timeout_sec", "inject", "enabled"}
)

// TestUpsertEntryTOMLGolden drives the whole-entry writer over hand-written
// documents and compares byte-for-byte: an insert appends at the end with
// everything above untouched, an in-place edit replaces exactly one entry's
// block — [[routines.steps]] sub-tables rendered fresh in the draft's order —
// with both neighbours and their comments byte-identical.
func TestUpsertEntryTOMLGolden(t *testing.T) {
	cases := []struct {
		name     string
		family   string
		entry    string // "" inserts
		draft    map[string]any
		keyOrder []string
		subOrder map[string][]string
	}{
		// A new routine with two steps lands at the end of the document, after
		// the [[scripts]] table that follows the existing routines — blocks are
		// appended, never re-sorted into their family's section.
		{"routine_insert", "routines", "", map[string]any{
			"name":     "morning setup",
			"phrases":  []string{"morning setup", "start the day"},
			"schedule": "08:30 mon-fri",
			"steps": []map[string]any{
				{"app": "alacritty", "workspace": 1},
				{"app": "firefox", "match": "org.mozilla.firefox", "workspace": 2},
			},
		}, routineKeyOrder, routineSubOrder},
		// The #99 edit shape: a phrase added, the steps reordered with one
		// gaining float+size, announce switched on. Only this entry's block
		// changes; the neighbour and the hand comments outside the block stay.
		{"routine_edit", "routines", "evening", map[string]any{
			"name":     "evening",
			"phrases":  []string{"evening mode", "wind down"},
			"schedule": "19:00",
			"announce": true,
			"steps": []map[string]any{
				{"app": "spotify", "workspace": 6},
				{"app": "mpv", "workspace": 5, "float": true, "size": []int{1280, 720}},
			},
		}, routineKeyOrder, routineSubOrder},
		{"script_insert", "scripts", "", map[string]any{
			"name":        "rotate wallpaper",
			"phrases":     []string{"rotate the wallpaper"},
			"path":        "/home/me/bin/rotate-wallpaper.sh",
			"timeout_sec": 30,
			"report":      "silent",
			"enabled":     false,
		}, scriptKeyOrder, nil},
		{"script_edit", "scripts", "backup notes", map[string]any{
			"name":        "backup notes",
			"phrases":     []string{"backup my notes", "run the backup"},
			"path":        "/home/me/bin/backup-notes-v2.sh",
			"timeout_sec": 120,
			"schedule":    "03:30",
			"announce":    true,
		}, scriptKeyOrder, nil},
		// The #100 shapes, on the dotted family the editor was built for in
		// #92: a new [[knowledge.feeds]] lands at the end of the document —
		// after the [tts] table that follows the existing feeds — and an
		// in-place edit replaces exactly one feed's block, its glued header
		// comment, both neighbours, and the [tts] table byte-identical.
		{"feed_insert", "knowledge.feeds", "", map[string]any{
			"name":        "nvda",
			"description": "NVDA share price",
			"command":     []string{"/home/me/bin/nvda-price", "--short"},
			"mode":        "lazy",
			"ttl_sec":     600,
			"inject":      true,
		}, feedKeyOrder, nil},
		{"feed_edit", "knowledge.feeds", "amd", map[string]any{
			"name":         "amd",
			"description":  "AMD share price in dollars",
			"command":      []string{"/home/me/bin/amd-price", "--currency", "usd"},
			"mode":         "eager",
			"interval_sec": 120,
			"ttl_sec":      600,
			"inject":       true,
		}, feedKeyOrder, nil},
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
			got, err := UpsertEntryTOML(input, tc.family, "name", tc.entry, tc.draft, tc.keyOrder, tc.subOrder)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(golden) {
				t.Errorf("rewrite mismatch\n--- got ---\n%s\n--- want ---\n%s", got, golden)
			}
		})
	}
}

// TestUpsertEntryTOMLRoundTripsThroughEntryValue: what EntryValue reads is
// exactly what UpsertEntryTOML accepts — the form's round trip (#99): read the
// map, write it back unchanged, and the edit is a no-op except for block
// re-rendering, with every key (report, timeout_sec, sub-tables) surviving.
func TestUpsertEntryTOMLRoundTripsThroughEntryValue(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("testdata", "entry", "routine_edit.input.toml"))
	if err != nil {
		t.Fatal(err)
	}
	entry, ok, err := EntryValue(input, "routines", "name", "Evening")
	if err != nil || !ok {
		t.Fatalf("EntryValue = %v, %v", ok, err)
	}
	out, err := UpsertEntryTOML(input, "routines", "name", "evening", entry, routineKeyOrder, routineSubOrder)
	if err != nil {
		t.Fatal(err)
	}
	back, ok, err := EntryValue(out, "routines", "name", "evening")
	if err != nil || !ok || !entryMapEqual(back, entry) {
		t.Errorf("round trip changed the entry: %v vs %v (%v)", back, entry, err)
	}
	if !strings.Contains(string(out), "# the neighbour must not move") {
		t.Error("a hand comment outside the entry vanished")
	}
}

// TestUpsertEntryTOMLRefusals: an unknown addressed entry and an unparsable
// document each refuse without producing a document.
func TestUpsertEntryTOMLRefusals(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("testdata", "entry", "script_edit.input.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertEntryTOML(input, "scripts", "name", "no such", map[string]any{"name": "x"},
		scriptKeyOrder, nil); err == nil || !strings.Contains(err.Error(), `named "no such"`) {
		t.Errorf("unknown entry error = %v, want it named", err)
	}
	if _, err := UpsertEntryTOML([]byte("not = toml ["), "scripts", "name", "", map[string]any{"name": "x"},
		scriptKeyOrder, nil); err == nil || !strings.Contains(err.Error(), "fix it by hand") {
		t.Errorf("unparsable document error = %v, want the hand-fix pointer", err)
	}
}

// TestDeleteEntryTOMLGolden: removal takes the entry's block, its sub-tables,
// and the comment glued to its header — a comment separated by a blank line
// (a section header) stays — and collapses the separator so no double blank
// remains, with every other byte preserved.
func TestDeleteEntryTOMLGolden(t *testing.T) {
	cases := []struct {
		name   string
		family string
		entry  string
	}{
		// A middle entry with a glued comment and a section-header comment
		// above it: the glued one goes, the section header stays.
		{"routine_delete", "routines", "evening"},
		// The last entry of the document: no trailing blank lines left behind.
		{"script_delete", "scripts", "rotate wallpaper"},
		// A feed (#100): the glued comment goes with its entry, the feeds
		// section header stays, and the [tts] table below is untouched.
		{"feed_delete", "knowledge.feeds", "amd"},
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
			got, err := DeleteEntryTOML(input, tc.family, "name", tc.entry)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(golden) {
				t.Errorf("rewrite mismatch\n--- got ---\n%s\n--- want ---\n%s", got, golden)
			}
		})
	}
}

// TestDeleteEntryTOMLRefusals: an unknown entry refuses by name; the sibling
// families are untouched by a delete in one.
func TestDeleteEntryTOMLRefusals(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("testdata", "entry", "routine_delete.input.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DeleteEntryTOML(input, "routines", "name", "no such"); err == nil ||
		!strings.Contains(err.Error(), `named "no such"`) {
		t.Errorf("unknown entry error = %v, want it named", err)
	}
	if _, err := DeleteEntryTOML([]byte("not = toml ["), "routines", "name", "evening"); err == nil ||
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

// TestEntryEditorAddressesByADeclaredKey pins the identity-key generalisation
// (#164): `[[intents.custom]]` has no `name`, its identity is the phrase it
// matches, and the array editor addresses it by whichever key the caller
// names — insert, edit and delete, byte-preserving throughout.
//
// The three goldens are the same three motions the other families' goldens
// pin, and for the same reason: what must survive is everything outside the
// block, comments glued to a header included.
func TestEntryEditorAddressesByADeclaredKey(t *testing.T) {
	order := []string{"match", "run", "say"}
	cases := []struct {
		name   string
		target string
		draft  map[string]any
	}{
		{"intent_insert", "", map[string]any{
			"match": "mute the music", "run": "playerctl pause"}},
		{"intent_edit", "lock the screen", map[string]any{
			"match": "lock the screen", "run": "hyprlock --immediate", "say": "Locking now."}},
		{"intent_delete", "lock the screen", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := readGolden(t, tc.name+".input.toml")
			golden := readGolden(t, tc.name+".golden.toml")
			var got []byte
			var err error
			if tc.draft == nil {
				got, err = DeleteEntryTOML(input, "intents.custom", "match", tc.target)
			} else {
				got, err = UpsertEntryTOML(input, "intents.custom", "match", tc.target,
					tc.draft, order, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(golden) {
				t.Errorf("rewrite mismatch\n--- got ---\n%s\n--- want ---\n%s", got, golden)
			}
		})
	}
}

// TestEntryEditorReadsAndIndexesByTheDeclaredKey: the read and the index verbs
// address by the same key, so the whole-document problems a validator labels
// `intents.custom[1]` can be matched back to the entry that caused them.
func TestEntryEditorReadsAndIndexesByTheDeclaredKey(t *testing.T) {
	input := readGolden(t, "intent_edit.input.toml")
	entry, ok, err := EntryValue(input, "intents.custom", "match", "MUTE the music")
	if err != nil || !ok {
		t.Fatalf("read = %v, %v — array families still match case-insensitively", ok, err)
	}
	if entry["run"] != "playerctl pause" {
		t.Errorf("entry = %v", entry)
	}
	index, ok, err := EntryIndex(input, "intents.custom", "match", "mute the music")
	if err != nil || !ok || index != 1 {
		t.Errorf("index = %d, %v, %v; want 1", index, ok, err)
	}
	names, err := EntryNames(input, "intents.custom", "match")
	if err != nil || len(names) != 2 || names[0] != "lock the screen" {
		t.Errorf("names = %v, %v", names, err)
	}
}
