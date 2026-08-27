package intent

import (
	"strings"
	"testing"
)

// The router's half of issue #129: teach carries both slots deterministically,
// ambiguity falls through to the model, the listen and listing phrases match
// their literal shapes, and — the stated out-of-scope — nothing about
// teaching touches the collision guarantees.

func TestVocabTeachPhrasesCarryBothSlots(t *testing.T) {
	r := mustRouter(t)
	cases := map[string]struct{ phrase, meaning string }{
		"when I say quid I mean pounds":              {"quid", "pounds"},
		"When I say quid, I mean pounds.":            {"quid", "pounds"},
		"when i say the telly it means the tv":       {"the telly", "the tv"},
		"when I say bob that means my brother":       {"bob", "my brother"},
		"if I say quid I mean pounds":                {"quid", "pounds"},
		"when I say sprint week I mean release prep": {"sprint week", "release prep"},
	}
	for utterance, want := range cases {
		m, ok := r.Match(utterance)
		if !ok {
			t.Errorf("%q did not match", utterance)
			continue
		}
		if m.Name != VocabTeachIntentName || m.VocabPhrase != want.phrase || m.VocabMeaning != want.meaning {
			t.Errorf("%q → %q/%q (intent %q), want %q/%q",
				utterance, m.VocabPhrase, m.VocabMeaning, m.Name, want.phrase, want.meaning)
		}
	}
}

// TestVocabTeachAmbiguityBelongsToTheModel: a second separator makes the
// boundary undecidable, and the router only claims utterances it is certain
// about — so these fall through to the assistant untouched.
func TestVocabTeachAmbiguityBelongsToTheModel(t *testing.T) {
	r := mustRouter(t)
	for _, utterance := range []string{
		"when I say I mean it I mean I am serious", // "i mean" twice
		"when I say quid",                          // no separator
		"when I say I mean pounds",                 // empty phrase slot
		"when I say quid I mean",                   // empty meaning slot
		"when I say " + strings.Repeat("word ", maxNameWords+1) + "I mean pounds",   // phrase too long
		"when I say quid I mean " + strings.Repeat("word ", maxVocabMeaningWords+1), // meaning too long
	} {
		if m, ok := r.Match(utterance); ok && m.Name == VocabTeachIntentName {
			t.Errorf("%q matched %+v; ambiguity belongs to the model", utterance, m)
		}
	}
}

func TestVocabListenPhrasesCarryThePhrase(t *testing.T) {
	r := mustRouter(t)
	cases := map[string]string{
		"listen for the word quid":          "quid",
		"listen for the phrase sprint week": "sprint week",
		"listen out for the word hyprland":  "hyprland",
	}
	for utterance, want := range cases {
		m, ok := r.Match(utterance)
		if !ok || m.Name != VocabListenIntentName || m.VocabListen != want {
			t.Errorf("%q → %+v, want listen %q", utterance, m, want)
		}
	}
	// Bare "listen for X" is a sentence for the model, not a flag.
	if m, ok := r.Match("listen for a moment"); ok {
		t.Errorf("\"listen for a moment\" matched %+v; only the-word/the-phrase forms flag", m)
	}
}

func TestVocabListingPhrases(t *testing.T) {
	r := mustRouter(t)
	for _, utterance := range []string{
		"what words have I taught you",
		"What words did I teach you?",
		"which words have i taught you",
		"what vocabulary have I taught you",
		"list my taught words",
	} {
		m, ok := r.Match(utterance)
		if !ok || m.Name != VocabListIntentName || !m.VocabList {
			t.Errorf("%q → %+v, want the %s intent", utterance, m, VocabListIntentName)
		}
	}
}

// TestVocabListingPhrasesAreOwned: the listing phrases face the same closed
// world as every built-in — a custom intent or routine that wants one is
// refused naming this owner.
func TestVocabListingPhrasesAreOwned(t *testing.T) {
	_, err := New(Options{Custom: []Custom{{Match: "what words have i taught you", Run: "true"}}})
	if err == nil || !strings.Contains(err.Error(), VocabListIntentName) {
		t.Errorf("err = %v, want a collision naming %s", err, VocabListIntentName)
	}
	_, err = New(Options{Routines: []RoutinePhrases{{Name: "r", Phrases: []string{"list my taught words"}}}})
	if err == nil || !strings.Contains(err.Error(), VocabListIntentName) {
		t.Errorf("err = %v, want a collision naming %s", err, VocabListIntentName)
	}
}

// TestLiteralPhrasesBeatTheTeachSlots: a routine whose trigger happens to
// start with "when i say" wins over the free-text rules — insertion order,
// the capture patterns' guarantee, held for teach too.
func TestLiteralPhrasesBeatTheTeachSlots(t *testing.T) {
	r, err := New(Options{Routines: []RoutinePhrases{{
		Name: "demo", Phrases: []string{"when i say go i mean go"}}}})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := r.Match("when i say go i mean go")
	if !ok || m.Routine != "demo" {
		t.Errorf("literal routine phrase → %+v, want the routine to own it", m)
	}
}

func mustRouter(t *testing.T) *Router {
	t.Helper()
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
