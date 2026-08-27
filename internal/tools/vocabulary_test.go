package tools

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/vocabulary"
)

// The model's hands on the taught vocabulary (issue #129): teach stores with
// source and supersedes on a repeated phrase, a refused hard-to-hear flag is
// reported honestly beside the successful teach, forget confirms the exact
// entry — and the gate tiers pin the reversibility split: teach is built-in
// allow, forget takes the policy default.

func testVocabulary(t *testing.T) (*Vocabulary, *vocabulary.Store) {
	t.Helper()
	now := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	store := vocabulary.NewStore(filepath.Join(t.TempDir(), "vocabulary.toml"),
		vocabulary.StoreOptions{Now: func() time.Time { return now }}, nil)
	return NewVocabulary(VocabularyOptions{Store: store, Source: func() string { return "s7" }}), store
}

func vocabularyTool(t *testing.T, v *Vocabulary, name string) Tool {
	t.Helper()
	for _, tool := range v.Tools() {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("no tool named %s", name)
	return nil
}

func TestTeachStoresWithSourceAndConfirms(t *testing.T) {
	v, store := testVocabulary(t)
	teach := vocabularyTool(t, v, VocabularyTeachToolName)

	result := execute(t, teach, `{"phrase":"quid","meaning":"pounds","note":"UK money slang"}`)
	if !strings.Contains(result, "Taught") || !strings.Contains(result, "one short sentence") {
		t.Errorf("result = %q, want the taught confirmation with the one-sentence instruction", result)
	}
	entries := store.List("")
	if len(entries) != 1 || entries[0].Phrase != "quid" || entries[0].Meaning != "pounds" ||
		entries[0].Note != "UK money slang" || entries[0].Source != "s7" {
		t.Fatalf("entries = %+v", entries)
	}
}

// TestTeachSupersedesOnRepeatedPhrase: the tool needs no update_id — the
// phrase IS the identity, so a re-teach lands as a supersede with the trail
// in the result for the model to speak from.
func TestTeachSupersedesOnRepeatedPhrase(t *testing.T) {
	v, store := testVocabulary(t)
	teach := vocabularyTool(t, v, VocabularyTeachToolName)
	execute(t, teach, `{"phrase":"quid","meaning":"pounds"}`)

	result := execute(t, teach, `{"phrase":"quid","meaning":"euros"}`)
	if !strings.Contains(result, "previously meant \"pounds\"") {
		t.Errorf("result = %q, want the supersede trail visible", result)
	}
	entries := store.List("")
	if len(entries) != 1 || entries[0].Meaning != "euros" || len(entries[0].Previous) != 1 {
		t.Fatalf("entries after re-teach = %+v, want one superseded entry", entries)
	}
}

func TestTeachSetsTheHardToHearFlag(t *testing.T) {
	v, store := testVocabulary(t)
	teach := vocabularyTool(t, v, VocabularyTeachToolName)
	result := execute(t, teach, `{"phrase":"hyprland","meaning":"the window manager","hard_to_hear":true}`)
	if !strings.Contains(result, "[listened for]") {
		t.Errorf("result = %q, want the flag visible", result)
	}
	if got := store.HardToHear(); len(got) != 1 || got[0] != "hyprland" {
		t.Errorf("HardToHear = %v", got)
	}
}

// TestTeachReportsARefusedFlagHonestly: at the bias cap the entry is still
// taught, and the result says the flag did NOT land — neither half silent.
func TestTeachReportsARefusedFlagHonestly(t *testing.T) {
	v, store := testVocabulary(t)
	teach := vocabularyTool(t, v, VocabularyTeachToolName)
	for i := 0; i < vocabulary.MaxHardToHear; i++ {
		execute(t, teach, fmt.Sprintf(`{"phrase":"word%d","meaning":"m","hard_to_hear":true}`, i))
	}

	result := execute(t, teach, `{"phrase":"quid","meaning":"pounds","hard_to_hear":true}`)
	if !strings.Contains(result, "Taught") || !strings.Contains(result, "NOT be listened for") {
		t.Errorf("result = %q, want the teach AND the flag refusal", result)
	}
	if _, found := store.ByPhrase("quid"); !found {
		t.Error("the entry was not taught despite the refused flag")
	}
	if n, _ := store.BiasCount(); n != vocabulary.MaxHardToHear {
		t.Errorf("BiasCount = %d, want the cap held", n)
	}
}

func TestForgetResolvesByPhraseAndConfirmsTheExactEntry(t *testing.T) {
	v, store := testVocabulary(t)
	teach := vocabularyTool(t, v, VocabularyTeachToolName)
	execute(t, teach, `{"phrase":"quid","meaning":"pounds"}`)
	forget := vocabularyTool(t, v, VocabularyForgetToolName)

	confirmable, ok := forget.(Confirmable)
	if !ok {
		t.Fatal("vocabulary.forget does not implement Confirmable; the gate could not name the entry")
	}
	command, summary, ok := confirmable.Confirmation([]byte(`{"phrase":"Quid"}`))
	if !ok {
		t.Fatal("Confirmation did not resolve the taught phrase")
	}
	if !strings.Contains(command, "w1") || !strings.Contains(summary, "quid means pounds") {
		t.Errorf("confirmation = %q / %q, want the exact entry named", command, summary)
	}

	result := execute(t, forget, `{"phrase":"quid"}`)
	if !strings.Contains(result, "Forgotten and deleted") {
		t.Errorf("result = %q", result)
	}
	if entries := store.List(""); len(entries) != 0 {
		t.Fatalf("entries after forget = %+v", entries)
	}

	// An unknown phrase deletes nothing and says so.
	result = execute(t, forget, `{"phrase":"quid"}`)
	if !strings.Contains(result, "nothing was forgotten") {
		t.Errorf("result = %q", result)
	}
}

// TestVocabularyGateTiers pins the reversibility split at the policy: teach
// runs silently under ask-by-default (the user's explicit word), forget does
// not (deletion destroys the taught history).
func TestVocabularyGateTiers(t *testing.T) {
	policy, err := NewPolicy(PolicyConfig{Default: PolicyAsk})
	if err != nil {
		t.Fatal(err)
	}
	if d := policy.ToolDecision(VocabularyTeachToolName); d != PolicyAllow {
		t.Errorf("vocabulary.teach tier = %v, want allow", d)
	}
	if d := policy.ToolDecision(VocabularyForgetToolName); d != PolicyAsk {
		t.Errorf("vocabulary.forget tier = %v, want the policy default (ask)", d)
	}
}
