package intent

import (
	"strings"
	"testing"
)

// The window-nickname trigger tests (#126) pin the router's half of the
// feature: which phrases claim an utterance, that only the raw name travels,
// and that the collision map the config checks were judged against is the
// same one Owner answers nickname assignments from.

func TestWindowNamePhrasesCarryTheSpokenName(t *testing.T) {
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for utterance, want := range map[string]string{
		"call this window builds":      "builds",
		"Call this window Builds.":     "builds",
		"call that window mail":        "mail",
		"name this window tests":       "tests",
		"nickname this window scratch": "scratch",
		// Multi-word names still travel raw: the assignment seam owns the
		// single-word refusal, and its guidance needs the words as spoken.
		"call this window the build terminal": "the build terminal",
	} {
		m, ok := r.Match(utterance)
		if !ok {
			t.Errorf("%q did not match", utterance)
			continue
		}
		if m.Name != WindowNameIntentName || m.WindowName != want {
			t.Errorf("%q → %q (intent %q), want name %q", utterance, m.WindowName, m.Name, want)
		}
		if m.Ack != "" {
			t.Errorf("%q carries ack %q; the window-name seam owns what is spoken", utterance, m.Ack)
		}
	}
}

// TestWindowNameSlotIsBounded: no name, or a name past maxNameWords, is not
// an assignment — and "call this X" without the word "window" is a sentence
// for the model, never a claim.
func TestWindowNameSlotIsBounded(t *testing.T) {
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, utterance := range []string{
		"call this window",
		"call this window " + strings.Repeat("word ", maxNameWords+1),
		"call this a great success",
		"name this builds",
	} {
		if m, ok := r.Match(utterance); ok {
			t.Errorf("%q matched %q; it should fall through to the model", utterance, m.Name)
		}
	}
}

func TestWindowNamesListingPhrases(t *testing.T) {
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, utterance := range []string{
		"what are my windows called",
		"What are my windows called?",
		"what are the windows called",
		"what are my windows named",
		"what did I call my windows",
		"list my window names",
	} {
		m, ok := r.Match(utterance)
		if !ok {
			t.Errorf("%q did not match", utterance)
			continue
		}
		if m.Name != WindowNamesIntentName || !m.WindowNames {
			t.Errorf("%q → %+v, want the %s intent", utterance, m, WindowNamesIntentName)
		}
	}
}

// TestWindowNamesPhrasesAreOwned: the listing phrases face the same closed
// world as any built-in — a custom intent or routine that wants one is
// refused naming this owner, never silently shadowed or shadowing.
func TestWindowNamesPhrasesAreOwned(t *testing.T) {
	_, err := New(Options{Custom: []Custom{{Match: "what are my windows called", Run: "true"}}})
	if err == nil || !strings.Contains(err.Error(), WindowNamesIntentName) {
		t.Errorf("err = %v, want a collision naming %s", err, WindowNamesIntentName)
	}
	_, err = New(Options{Routines: []RoutinePhrases{{Name: "r", Phrases: []string{"list my window names"}}}})
	if err == nil || !strings.Contains(err.Error(), WindowNamesIntentName) {
		t.Errorf("err = %v, want a collision naming %s", err, WindowNamesIntentName)
	}
}

// TestOwnerAnswersFromTheCollisionMap: Owner is the nickname assignment's
// collision check, and it must name owners with the exact wording a config
// collision error uses, across all the phrase families.
func TestOwnerAnswersFromTheCollisionMap(t *testing.T) {
	r, err := New(Options{
		Custom:   []Custom{{Match: "lock it down", Run: "loginctl lock-session"}},
		Routines: []RoutinePhrases{{Name: "standup", Phrases: []string{"standup time"}}},
		Scripts:  []ScriptPhrases{{Name: "backup", Phrases: []string{"run my backup"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for utterance, wantOwner := range map[string]string{
		"mute":          `the built-in intent "volume.mute"`,
		"Mute!":         `the built-in intent "volume.mute"`,
		"stop":          `the built-in intent "speech.stop"`,
		"lock it down":  `intents.custom[0] ("lock it down")`,
		"standup time":  `the trigger for routine "standup"`,
		"run my backup": `the trigger for script "backup"`,
	} {
		owner, taken := r.Owner(utterance)
		if !taken || owner != wantOwner {
			t.Errorf("Owner(%q) = %q, %v; want %q", utterance, owner, taken, wantOwner)
		}
	}
	for _, utterance := range []string{"builds", "volume", ""} {
		if owner, taken := r.Owner(utterance); taken {
			t.Errorf("Owner(%q) = %q; want unowned", utterance, owner)
		}
	}
	// Nil-safe, like Match: no router means nothing is spoken for.
	var nilRouter *Router
	if _, taken := nilRouter.Owner("mute"); taken {
		t.Error("a nil router claimed a phrase")
	}
}

// TestDisabledEntriesDoNotOwnPhrases: a parked routine's phrase (#93) is out
// of the grammar AND out of the collision map — a nickname may take it, the
// same way another entry may.
func TestDisabledEntriesDoNotOwnPhrases(t *testing.T) {
	r, err := New(Options{
		Routines: []RoutinePhrases{{Name: "standup", Phrases: []string{"standup"}, Disabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if owner, taken := r.Owner("standup"); taken {
		t.Errorf("Owner(%q) = %q; a disabled routine's phrase must be free", "standup", owner)
	}
}
