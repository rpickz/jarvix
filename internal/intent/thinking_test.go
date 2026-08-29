package intent

import (
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
)

// The pins are whole utterances and go through the table, so they are tested
// the way every other family is: claimed exactly, and not claimed for anything
// near them.
func TestThinkingPinsAreClaimedWholeAndNothingNear(t *testing.T) {
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for utterance, want := range map[string]ai.Tier{
		"switch to deep":                ai.TierDeep,
		"stay on the deep model":        ai.TierDeep,
		"think hard from now on":        ai.TierDeep,
		"Switch to the balanced model.": ai.TierMedium,
		"back to normal answers":        ai.TierMedium,
		"quick answers from now on":     ai.TierInstant,
		"use the quick model":           ai.TierInstant,
	} {
		m, ok := r.Match(utterance)
		if !ok {
			t.Errorf("%q was not claimed", utterance)
			continue
		}
		if m.Name != ThinkingIntentName || m.Thinking != want {
			t.Errorf("%q → %q/%q, want %q/%q", utterance, m.Name, m.Thinking,
				ThinkingIntentName, want)
		}
	}
	// Near-misses belong to the model, as ADR 0017 requires: the cost of
	// being conservative is one model call, and the cost of being liberal is
	// changing a setting the user did not ask to change.
	for _, utterance := range []string{
		"switch to deep sea diving",
		"should i switch to deep",
		"stay on the deep model please",
		"deep",
	} {
		if m, ok := r.Match(utterance); ok && m.Thinking != ai.TierNone {
			t.Errorf("%q was claimed as a thinking pin", utterance)
		}
	}
}

// An escalation annotates a turn; it never claims it. Both halves matter: the
// tier must be recognised, and the utterance must still reach the model.
func TestTurnTierRecognisesEscalationsAndClaimsNothing(t *testing.T) {
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for utterance, want := range map[string]ai.Tier{
		"think hard about this, what should i do":            ai.TierDeep,
		"Think hard about the rota.":                         ai.TierDeep,
		"take your time and tell me what to cook":            ai.TierDeep,
		"think carefully about whether to sell the car":      ai.TierDeep,
		"give this some thought and get back to me":          ai.TierDeep,
		"quick answer what time is it":                       ai.TierInstant,
		"quickly what is the weather":                        ai.TierInstant,
		"just quickly remind me what the capital of peru is": ai.TierInstant,
	} {
		got, ok := TurnTier(utterance)
		if !ok || got != want {
			t.Errorf("TurnTier(%q) = %q, %v; want %q", utterance, got, ok, want)
		}
		if _, claimed := r.Match(utterance); claimed {
			t.Errorf("%q was claimed by the router; an escalation must fall through to the model", utterance)
		}
	}
	// The longest phrase decides, so the two "think hard" entries are
	// distinguishable rather than one shadowing the other.
	if got, _ := TurnTier("think hard about this thing"); got != ai.TierDeep {
		t.Errorf("longest-match escalation = %q", got)
	}
	for _, utterance := range []string{
		"",
		"quickly",        // the whole utterance is the phrase: no question left
		"take your time", // likewise
		"i had a quick answer for you",
		"what do you think hard about",
		"a quick question about the rota",
	} {
		if got, ok := TurnTier(utterance); ok {
			t.Errorf("TurnTier(%q) = %q, true — nothing here asked for a tier", utterance, got)
		}
	}
}

// The pin phrases are owned, so a user's own intent or routine claiming one is
// a config error naming this owner rather than a coin toss. Two things able to
// move a setting by voice is one too many.
func TestThinkingPhrasesAreOwned(t *testing.T) {
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range ThinkingPhrases() {
		owner, taken := r.Owner(phrase)
		if !taken || owner == "" {
			t.Errorf("%q is not owned by anything", phrase)
		}
	}
	if _, err := New(Options{Custom: []Custom{{Match: "switch to deep", Run: "true"}}}); err == nil {
		t.Error("a custom intent was allowed to claim a thinking pin")
	}
}
