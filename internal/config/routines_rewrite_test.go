package config

import (
	"strings"
	"testing"
)

// These tests pin the capture writer's half of ADR 0015: one [[routines]]
// entry lands or is replaced, and every other byte of the user's hand-written
// file — comments included — survives, both this write and the ordinary
// settings rewrites that come after it.

const capturedDoc = `# my config, hand-written
[audio]
max_recording_sec = 25 # tuned by ear

# the routine I curated myself
[[routines]]
name = "evening"
phrases = ["evening mode"]

  [[routines.steps]]
  app = "mpv"
  workspace = 5

[tts]
provider = "piper"
`

func captureEntry() Routine {
	return Routine{
		Name:    "morning setup",
		Phrases: []string{"morning setup"},
		Steps: []RoutineStep{
			{App: "alacritty", Workspace: 1, Tile: "split"},
			{App: "signal-desktop", Match: "Signal", Workspace: 9, Float: true,
				Size: []int{1200, 800}, Position: []int{100, 120}},
		},
	}
}

// TestUpsertAppendsPreservingEveryOtherByte: a new name lands at the end of
// the document with its provenance comment, and the rest of the file — the
// hand comments especially — is byte-identical.
func TestUpsertAppendsPreservingEveryOtherByte(t *testing.T) {
	out, err := UpsertRoutineTOML([]byte(capturedDoc), captureEntry(), "captured 2026-08-21",
		[]string{"", ""})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.HasPrefix(got, capturedDoc) {
		t.Fatalf("the existing document changed:\n%s", got)
	}
	added := strings.TrimPrefix(got, capturedDoc)
	if !strings.Contains(added, "# captured 2026-08-21\n[[routines]]") {
		t.Errorf("provenance comment missing above the entry:\n%s", added)
	}
	cfg, err := ParseBytes(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routines) != 2 || cfg.Routines[1].Name != "morning setup" {
		t.Fatalf("routines = %+v", cfg.Routines)
	}
	if len(cfg.Routines[1].Steps) != 2 || cfg.Routines[1].Steps[1].Size[0] != 1200 {
		t.Fatalf("steps did not round-trip: %+v", cfg.Routines[1].Steps)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a captured document must validate: %v", err)
	}
}

// TestUpsertReplacesOnlyTheNamedBlock: replacing "evening" rewrites that
// block — refreshing its provenance, not stacking a second one — and leaves
// the neighbouring tables and the hand comment above the block alone.
func TestUpsertReplacesOnlyTheNamedBlock(t *testing.T) {
	// First capture it, so the block carries an old provenance comment.
	first, err := UpsertRoutineTOML([]byte(capturedDoc), Routine{
		Name: "evening", Phrases: []string{"evening mode"},
		Steps: []RoutineStep{{App: "mpv", Workspace: 5, Tile: "split"}},
	}, "captured 2026-08-20", nil)
	if err != nil {
		t.Fatal(err)
	}
	replacement := Routine{
		Name: "evening", Phrases: []string{"evening mode"},
		Steps: []RoutineStep{
			{App: "mpv", Workspace: 5, Tile: "split"},
			{App: "spotify", Workspace: 6, Tile: "split"},
		},
	}
	out, err := UpsertRoutineTOML(first, replacement, "captured 2026-08-21", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, kept := range []string{
		"# my config, hand-written",
		"max_recording_sec = 25 # tuned by ear",
		"# the routine I curated myself",
		"[tts]\nprovider = \"piper\"",
	} {
		if !strings.Contains(got, kept) {
			t.Errorf("hand-written content lost: %q\n%s", kept, got)
		}
	}
	if strings.Contains(got, "captured 2026-08-20") {
		t.Errorf("stale provenance survived the replace:\n%s", got)
	}
	if strings.Count(got, "# captured 2026-08-21") != 1 {
		t.Errorf("provenance count wrong:\n%s", got)
	}
	cfg, err := ParseBytes(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routines) != 1 || len(cfg.Routines[0].Steps) != 2 {
		t.Fatalf("routines = %+v, want the one replaced entry with two steps", cfg.Routines)
	}
}

// TestUpsertNotesMarkPlaceholderSteps: the TODO a placeholder step carries is
// written directly above its table, so the file explains itself.
func TestUpsertNotesMarkPlaceholderSteps(t *testing.T) {
	entry := Routine{
		Name: "chat", Phrases: []string{"chat"},
		Steps: []RoutineStep{{App: "CHANGE-ME", Match: "chrome-web.whatsapp.com__-Default",
			Workspace: 3, Tile: "split"}},
	}
	out, err := UpsertRoutineTOML(nil, entry, "captured 2026-08-21",
		[]string{`TODO: set app to the program that launches it`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "# TODO: set app to the program that launches it\n  [[routines.steps]]") {
		t.Errorf("note not written above the step:\n%s", out)
	}
	cfg, err := ParseBytes(out)
	if err != nil || len(cfg.Routines) != 1 {
		t.Fatalf("cfg = %+v, %v", cfg.Routines, err)
	}
	if !cfg.Routines[0].Incomplete() {
		t.Error("a placeholder entry must list as incomplete")
	}
}

// TestCapturedEntrySurvivesTheSettingsRewrite is the requirement the issue
// singles out: after a capture, the ordinary config.set editor must carry
// the [[routines]] block — provenance comment, notes, and all — through
// byte-for-byte.
func TestCapturedEntrySurvivesTheSettingsRewrite(t *testing.T) {
	captured, err := UpsertRoutineTOML([]byte(capturedDoc), captureEntry(), "captured 2026-08-21",
		[]string{"", "TODO: check the size"})
	if err != nil {
		t.Fatal(err)
	}
	rewritten, err := RewriteTOML(captured, map[string]any{"audio.max_recording_sec": 30})
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(captured), "# captured 2026-08-21")
	if start < 0 {
		t.Fatal("no captured block to compare")
	}
	block := string(captured)[start:]
	if !strings.Contains(string(rewritten), block) {
		t.Fatalf("the settings rewrite disturbed the captured block:\n%s", rewritten)
	}
	if !strings.Contains(string(rewritten), "max_recording_sec = 30") {
		t.Fatalf("the setting itself did not land:\n%s", rewritten)
	}
}

// TestUpsertRefusesAnUnparsableDocument: a broken file is fixed by hand, not
// appended to blind.
func TestUpsertRefusesAnUnparsableDocument(t *testing.T) {
	if _, err := UpsertRoutineTOML([]byte("this is not toml = = ="), captureEntry(), "captured", nil); err == nil {
		t.Fatal("an unparsable document must refuse the write")
	}
}

// TestUpsertIntoEmptyDocument: a machine with no config.toml yet gets one
// holding exactly the entry.
func TestUpsertIntoEmptyDocument(t *testing.T) {
	out, err := UpsertRoutineTOML(nil, captureEntry(), "captured 2026-08-21", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseBytes(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routines) != 1 || cfg.Routines[0].Name != "morning setup" {
		t.Fatalf("routines = %+v", cfg.Routines)
	}
	if !strings.HasPrefix(string(out), "# captured 2026-08-21\n[[routines]]\n") {
		t.Errorf("document = %s", out)
	}
}
