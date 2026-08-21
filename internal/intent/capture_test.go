package intent

import (
	"strings"
	"testing"
)

// The capture-trigger tests pin the router's one free-text slot (#62): the
// phrases that claim an utterance, the name that travels, and — the part
// grammar collisions hinge on — that every literal phrase always beats the
// slot, so a capture pattern can never shadow something the user configured.

func TestCapturePhrasesCarryTheSpokenName(t *testing.T) {
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for utterance, want := range map[string]string{
		"save this as morning setup":               "morning setup",
		"Save this as Morning Setup.":              "morning setup",
		"save this layout as deep work":            "deep work",
		"save this setup as focus":                 "focus",
		"save this desktop as monday":              "monday",
		"remember this layout as writing":          "writing",
		"save this as my morning setup":            "my morning setup",
		"save this as one two three four five six": "one two three four five six",
	} {
		m, ok := r.Match(utterance)
		if !ok {
			t.Errorf("%q did not match", utterance)
			continue
		}
		if m.Name != CaptureIntentName || m.CaptureName != want {
			t.Errorf("%q → %q (intent %q), want name %q", utterance, m.CaptureName, m.Name, want)
		}
		if m.Ack != "" {
			t.Errorf("%q carries ack %q; the capture service owns what is spoken", utterance, m.Ack)
		}
	}
}

// TestCaptureSlotIsBounded: no name at all, or a name past maxNameWords, is
// not a capture — the utterance belongs to the model like any long sentence.
func TestCaptureSlotIsBounded(t *testing.T) {
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, utterance := range []string{
		"save this as",
		"save this as " + strings.Repeat("word ", maxNameWords+1),
		"save that as morning setup",
	} {
		if m, ok := r.Match(utterance); ok {
			t.Errorf("%q matched %q; it should fall through to the model", utterance, m.Name)
		}
	}
}

// TestLiteralPhrasesBeatTheCaptureSlot: a custom intent or a routine whose
// literal phrase begins with the capture words keeps its phrase — the slot
// only claims utterances no configured phrase owns.
func TestLiteralPhrasesBeatTheCaptureSlot(t *testing.T) {
	r, err := New(Options{
		Custom: []Custom{{Match: "save this as backup", Run: "true", Say: "Backed up."}},
		Routines: []RoutinePhrases{
			{Name: "workday", Phrases: []string{"save this layout as work"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := r.Match("save this as backup"); !ok || !m.UserDefined {
		t.Errorf("custom phrase → %+v, want the user-defined intent", m)
	}
	if m, ok := r.Match("save this layout as work"); !ok || m.Routine != "workday" {
		t.Errorf("routine phrase → %+v, want routine %q", m, "workday")
	}
	// The words around the literals still capture.
	if m, ok := r.Match("save this as backups"); !ok || m.CaptureName != "backups" {
		t.Errorf("unclaimed phrase → %+v, want a capture of %q", m, "backups")
	}
}

// TestCaptureIntentIsListed: the capability shows up in diagnostics like any
// compiled intent.
func TestCaptureIntentIsListed(t *testing.T) {
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range r.Names() {
		if name == CaptureIntentName {
			return
		}
	}
	t.Errorf("Names() = %v, missing %q", r.Names(), CaptureIntentName)
}
