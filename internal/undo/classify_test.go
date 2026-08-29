package undo

import (
	"strings"
	"testing"
)

// TestEveryOneWayToolSaysWhy: a warning that does not say what it is warning
// about is a warning people learn to click past, so every irreversible tool
// carries its own reason rather than sharing a generic one.
func TestEveryOneWayToolSaysWhy(t *testing.T) {
	for _, name := range ClassifiedTools() {
		if Classify(name) != NatureIrreversible {
			continue
		}
		reason, ok := oneWayReasons[name]
		if !ok || strings.TrimSpace(reason) == "" {
			t.Errorf("%s is marked one-way but says nothing about why", name)
			continue
		}
		if strings.HasSuffix(reason, ".") {
			t.Errorf("%s's reason %q ends in a full stop; it is a clause, and CardNote adds one",
				name, reason)
		}
	}
}

// TestTheCardSaysItAndTheSpokenPromptSaysTheShortForm pins both wordings and
// the relationship between them: the short form is a prefix of the written
// one, so a user who hears one and then reads the other is not told the same
// thing two ways.
func TestTheCardSaysItAndTheSpokenPromptSaysTheShortForm(t *testing.T) {
	card := CardNote("shell.run")
	spoken := SpokenNote("shell.run")
	if card == "" || spoken == "" {
		t.Fatalf("shell.run says nothing about being one-way (card %q, spoken %q)", card, spoken)
	}
	if !strings.HasPrefix(card, strings.TrimSuffix(spoken, ".")) {
		t.Errorf("the card note %q does not open with the spoken note %q", card, spoken)
	}
	if !strings.Contains(card, "a command that has run has run") {
		t.Errorf("the card note %q drops the reason, which is the half worth reading", card)
	}
	if strings.Contains(spoken, "a command that has run has run") {
		t.Errorf("the spoken note %q carries the reason; the card is where that belongs", spoken)
	}
}

// TestNothingIsClaimedAboutAToolWeHaveNotClassified. Silence is the answer
// for an unknown capability: guessing "reversible" would let a user approve a
// one-way change believing otherwise, which is the exact failure the warning
// exists to prevent.
func TestNothingIsClaimedAboutAToolWeHaveNotClassified(t *testing.T) {
	for _, name := range []string{"mystery.op", "", "   "} {
		if got := Classify(name); got != NatureUnknown {
			t.Errorf("Classify(%q) = %v, want NatureUnknown", name, got)
		}
		if note := CardNote(name); note != "" {
			t.Errorf("CardNote(%q) = %q, want silence", name, note)
		}
		if note := SpokenNote(name); note != "" {
			t.Errorf("SpokenNote(%q) = %q, want silence", name, note)
		}
	}
}

// TestReversibleAndReadOnlyToolsSayNothingOnTheCard: the warning is for
// one-way decisions only. A card that said something about every action would
// be a card nobody reads.
func TestReversibleAndReadOnlyToolsSayNothingOnTheCard(t *testing.T) {
	for _, name := range ClassifiedTools() {
		if Classify(name) == NatureIrreversible {
			continue
		}
		if note := CardNote(name); note != "" {
			t.Errorf("%s is not one-way but the card would say %q", name, note)
		}
	}
}

// TestAnnotateDoesNotSayItTwice. The summary is annotated once and reaches
// several surfaces; a caller that annotates an already-annotated string must
// not double the clause.
func TestAnnotateDoesNotSayItTwice(t *testing.T) {
	once := Annotate("shell.run", "Run `rm -rf ./build`?")
	twice := Annotate("shell.run", once)
	if once != twice {
		t.Errorf("annotating twice changed the sentence:\n once %q\ntwice %q", once, twice)
	}
	if strings.Count(once, "This can't be undone") != 1 {
		t.Errorf("the annotated summary %q does not carry the clause exactly once", once)
	}
	if untouched := Annotate("config.write_entry", "Save it?"); untouched != "Save it?" {
		t.Errorf("a reversible tool's summary was annotated: %q", untouched)
	}
}

// TestOneWayUsesTheSameWordsAsTheCard is the promise that a user is told the
// same thing at approval and at review. Two tables would eventually disagree,
// and the disagreement would be exactly the surprise this feature exists to
// remove.
func TestOneWayUsesTheSameWordsAsTheCard(t *testing.T) {
	for _, name := range ClassifiedTools() {
		if Classify(name) != NatureIrreversible {
			continue
		}
		restore := OneWay(name)
		if restore.Kind != KindNone {
			t.Errorf("OneWay(%q) is kind %q, want %q", name, restore.Kind, KindNone)
		}
		if !strings.Contains(CardNote(name), restore.Because) {
			t.Errorf("the account says %q and the card says %q for %s; they must be the same words",
				restore.Because, CardNote(name), name)
		}
	}
}
