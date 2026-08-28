package config

import (
	"strings"
	"testing"
)

// These tests pin the scalar-map editor (issue #164) to the contract the other
// two shapes already meet: golden files prove that everything outside the one
// LINE being written — comments above it, comments beside its neighbours, the
// section's other keys, ordering, formatting — survives byte-for-byte across
// insert, edit and delete.

// TestScalarMapEntryGolden drives the editor over hand-written documents and
// compares byte-for-byte.
func TestScalarMapEntryGolden(t *testing.T) {
	cases := []struct {
		name string
		// target is the entry to replace or delete ("" creates).
		target string
		// entry is the whole draft, `name` carrying the TOML key. Nil deletes.
		entry map[string]any
	}{
		// Creating: the new line joins the table, after its last entry, rather
		// than landing at the end of the file under a second [tts.lexicon]
		// header TOML would refuse to parse.
		{"lexicon_insert", "", map[string]any{"name": "Hyprland", "spoken": "hyper land"}},
		// Editing in place: the line keeps its position AND its inline comment.
		// For a family whose entry is one line, that comment is the only place
		// the entry can be documented at all — see scalarmap_rewrite.go.
		{"lexicon_edit", "Kubernetes", map[string]any{
			"name": "Kubernetes", "spoken": "koober net ees"}},
		// Deleting: the line and the comment glued above it go together, and
		// the neighbours keep their own.
		{"lexicon_delete", "k9s", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := readGolden(t, tc.name+".input.toml")
			golden := readGolden(t, tc.name+".golden.toml")
			var got []byte
			var err error
			if tc.entry == nil {
				got, err = DeleteScalarMapEntryTOML(input, "tts.lexicon", tc.target, nil)
			} else {
				got, err = UpsertScalarMapEntryTOML(input, "tts.lexicon", "spoken",
					tc.target, tc.entry, nil)
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

// TestScalarMapRenameRewritesTheKey: the written form IS the key, so a rename
// replaces the line rather than adding a second one — and the old spelling
// must not survive alongside the new.
func TestScalarMapRenameRewritesTheKey(t *testing.T) {
	input := readGolden(t, "lexicon_edit.input.toml")
	out, err := UpsertScalarMapEntryTOML(input, "tts.lexicon", "spoken", "k9s",
		map[string]any{"name": "k9s-cli", "spoken": "kay nine ess see ell eye"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	names, err := ScalarMapEntryNames(out, "tts.lexicon", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "Kubernetes,k9s-cli" {
		t.Errorf("names after rename = %v, want the old key gone", names)
	}
}

// TestScalarMapAddressingIsExact: the written form is a TOML key, and TOML
// keys are case-sensitive. Matching "GIF" to "gif" would edit an entry the user
// can see is a different one — the mistake a byte-preserving editor exists to
// make impossible.
func TestScalarMapAddressingIsExact(t *testing.T) {
	input := readGolden(t, "lexicon_edit.input.toml")
	if _, err := UpsertScalarMapEntryTOML(input, "tts.lexicon", "spoken", "kubernetes",
		map[string]any{"name": "kubernetes", "spoken": "x"}, nil); err == nil {
		t.Error("a differently-cased key was accepted as the same entry")
	}
	_, ok, err := ScalarMapEntryValue(input, "tts.lexicon", "spoken", "KUBERNETES", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a differently-cased key read back as the same entry")
	}
}

// TestScalarMapCreatesTheTable: a config with no [tts.lexicon] at all gets one,
// rather than a dotted key under [tts] that would read as a different value.
func TestScalarMapCreatesTheTable(t *testing.T) {
	input := []byte("[tts]\nprovider = \"kokoro\"\n")
	out, err := UpsertScalarMapEntryTOML(input, "tts.lexicon", "spoken", "",
		map[string]any{"name": "nginx", "spoken": "engine ex"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "[tts.lexicon]") {
		t.Errorf("no table header in:\n%s", out)
	}
	cfg, err := ParseBytes(out)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TTS.Lexicon["nginx"] != "engine ex" || cfg.TTS.Provider != "kokoro" {
		t.Errorf("parsed back as %v / %q", cfg.TTS.Lexicon, cfg.TTS.Provider)
	}
}

// TestScalarMapQuotesAKeyThatNeedsIt: a written form with a space is a legal
// thing to respell, and the key it becomes has to be quoted or the document
// stops parsing. The read-back guard is what would catch a renderer that
// forgot; this pins that it does not have to.
func TestScalarMapQuotesAKeyThatNeedsIt(t *testing.T) {
	input := []byte("[tts.lexicon]\nnginx = \"engine ex\"\n")
	out, err := UpsertScalarMapEntryTOML(input, "tts.lexicon", "spoken", "",
		map[string]any{"name": "New York", "spoken": "new york"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseBytes(out)
	if err != nil {
		t.Fatalf("%v in:\n%s", err, out)
	}
	if cfg.TTS.Lexicon["New York"] != "new york" {
		t.Errorf("lexicon = %v", cfg.TTS.Lexicon)
	}
	// And it can be found again by the same name it was written under.
	back, err := UpsertScalarMapEntryTOML(out, "tts.lexicon", "spoken", "New York",
		map[string]any{"name": "New York", "spoken": "noo york"}, nil)
	if err != nil {
		t.Fatalf("a quoted key could not be edited: %v", err)
	}
	cfg, _ = ParseBytes(back)
	if cfg.TTS.Lexicon["New York"] != "noo york" {
		t.Errorf("lexicon after edit = %v", cfg.TTS.Lexicon)
	}
}

// TestScalarMapRefusesTheImpossible: an unparsable document, a missing entry,
// and a draft with no written form all fail with nothing written.
func TestScalarMapRefusesTheImpossible(t *testing.T) {
	if _, err := UpsertScalarMapEntryTOML([]byte("not = toml ["), "tts.lexicon", "spoken", "",
		map[string]any{"name": "x", "spoken": "y"}, nil); err == nil {
		t.Error("an unparsable document was edited")
	}
	input := readGolden(t, "lexicon_edit.input.toml")
	if _, err := UpsertScalarMapEntryTOML(input, "tts.lexicon", "spoken", "no such",
		map[string]any{"name": "no such", "spoken": "y"}, nil); err == nil {
		t.Error("an absent entry was edited")
	}
	if _, err := UpsertScalarMapEntryTOML(input, "tts.lexicon", "spoken", "",
		map[string]any{"name": "  ", "spoken": "y"}, nil); err == nil {
		t.Error("an empty written form was accepted")
	}
	if _, err := DeleteScalarMapEntryTOML(input, "tts.lexicon", "no such", nil); err == nil {
		t.Error("an absent entry was deleted")
	}
}

// TestScalarMapIgnoresNonStringNeighbours: only string-valued keys of the table
// are entries. Anything else under the same header belongs to somebody else and
// is left exactly where it is — the guard that lets a scalar-map family share a
// table one day the way [ai] shares one now.
func TestScalarMapIgnoresNonStringNeighbours(t *testing.T) {
	input := []byte("[tts.lexicon]\nnginx = \"engine ex\"\n")
	names, err := ScalarMapEntryNames(input, "tts.lexicon", map[string]bool{"nginx": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("names = %v, want a reserved key excluded", names)
	}
}

// TestLexiconValidationIsFieldKeyed: the two states a hand edit can reach that
// the compiler silently discards are refused at load, worded so the entry
// pipeline can pin each to the input that caused it.
func TestLexiconValidationIsFieldKeyed(t *testing.T) {
	cfg := Default()
	cfg.TTS.Lexicon = map[string]string{"Kubernetes": "  "}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "tts.lexicon.Kubernetes: spoken form is empty") {
		t.Errorf("an empty spoken form was accepted: %v", err)
	}
	cfg.TTS.Lexicon = map[string]string{"": "koo ber net eez"}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "empty written form") {
		t.Errorf("an empty written form was accepted: %v", err)
	}
	cfg.TTS.Lexicon = map[string]string{"Kubernetes": "koo ber net eez"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a good lexicon was refused: %v", err)
	}
}
