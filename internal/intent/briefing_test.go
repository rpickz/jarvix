package intent

import (
	"strings"
	"testing"
)

// The return briefing's phrases (#150, ADR 0050). A fixed request with one
// right outcome belongs to the table (ADR 0017), so these must match locally
// and never reach the model — and, just as importantly, the near-misses must
// fall through, because "what did I miss in the standup" is a question about
// a meeting.

func TestBriefingPhrases(t *testing.T) {
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, utterance := range []string{
		"what did i miss",
		"What did I miss?",
		"what have i missed",
		"what did i miss while i was away",
		"what happened while i was away",
		"what happened while i was out",
		"give me the briefing",
		"give me my briefing",
		"brief me",
		"catch me up",
	} {
		m, ok := r.Match(utterance)
		if !ok {
			t.Errorf("%q did not match", utterance)
			continue
		}
		if m.Name != BriefingIntentName || !m.Briefing {
			t.Errorf("%q → %+v, want the %s intent", utterance, m, BriefingIntentName)
		}
	}
}

// TestBriefingNearMissesReachTheModel. Matching is whole-utterance and
// strict: anything the table does not claim verbatim is a sentence for the
// assistant, which has the briefing tool for exactly these phrasings.
func TestBriefingNearMissesReachTheModel(t *testing.T) {
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, utterance := range []string{
		"what did i miss in the standup",
		"did i miss anything",
		"brief me on the pricing changes",
		"what happened",
		"catch me up on the release",
		"give me the briefing document",
	} {
		if m, ok := r.Match(utterance); ok {
			t.Errorf("%q matched %q; it should fall through to the model", utterance, m.Name)
		}
	}
}

// TestBriefingPhrasesAreOwned: the same closed world as any built-in — a
// custom intent or routine that wants one is refused naming this owner,
// never silently shadowed or shadowing.
func TestBriefingPhrasesAreOwned(t *testing.T) {
	_, err := New(Options{Custom: []Custom{{Match: "what did i miss", Run: "true"}}})
	if err == nil || !strings.Contains(err.Error(), BriefingIntentName) {
		t.Errorf("err = %v, want a collision naming %s", err, BriefingIntentName)
	}
	_, err = New(Options{Routines: []RoutinePhrases{{Name: "r", Phrases: []string{"brief me"}}}})
	if err == nil || !strings.Contains(err.Error(), BriefingIntentName) {
		t.Errorf("err = %v, want a collision naming %s", err, BriefingIntentName)
	}
}

// TestBriefingCarriesNoFreeText. The phrases take no slot: whatever happened
// while the user was away is read from records, never from the sentence, so
// there is nothing for an utterance to smuggle in.
func TestBriefingCarriesNoFreeText(t *testing.T) {
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := r.Match("what did i miss")
	if !ok {
		t.Fatal("the phrase did not match")
	}
	if m.Slot != 0 || m.HasSlot || m.FocusText != "" || len(m.Argv) != 0 || m.Command != "" {
		t.Errorf("the briefing match carries data it should not: %+v", m)
	}
}
