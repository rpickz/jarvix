package sentence

import (
	"strings"
	"testing"
)

// This package is the shape half of two features' model contracts, so a change
// here lands on both at once. These tests are the pin that keeps that safe.

// TestOneIsTolerantAboutShapeAndFirmAboutLength. A model that adds a bullet, a
// label, or a second sentence has still answered, and the first sentence is the
// answer — refusal is for the claims, which is where a wrong answer costs
// something. Length is the caller's business, so One never truncates: it hands
// back whatever the first sentence was.
func TestOneIsTolerantAboutShapeAndFirmAboutLength(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"One session is waiting on you.", "One session is waiting on you."},
		{"  One session is waiting on you.  ", "One session is waiting on you."},
		{"- One session is waiting on you.", "One session is waiting on you."},
		{"• One session is waiting on you.", "One session is waiting on you."},
		{"1. One session is waiting on you.", "One session is waiting on you."},
		{"2) One session is waiting on you.", "One session is waiting on you."},
		{"Headline: One session is waiting on you.", "One session is waiting on you."},
		{"One session is waiting on you. And another finished.",
			"One session is waiting on you."},
		{"", ""},
		// A bare leading digit is a COUNT, not an enumerator: trimming it would
		// turn a sentence about three sessions into one about no sessions.
		{"3 sessions are still working.", "3 sessions are still working."},
		// A decimal point and an initial are not sentence ends.
		{"Version 1.2 is still building.", "Version 1.2 is still building."},
		// A colon deep in a real sentence is punctuation, not a label.
		{"Two things are going on here and the shape is this: one is waiting.",
			"Two things are going on here and the shape is this: one is waiting."},
		// No terminator at all is still an answer.
		{"One session is waiting on you", "One session is waiting on you"},
	} {
		if got := One(c.in); got != c.want {
			t.Errorf("One(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestClaimedPositivelyLetsADenialThrough. The guard exists to catch an
// invention, not to forbid the truth: "nothing has finished" is honest, and a
// guard that refused it would leave every caller speaking its plain fallback on
// exactly the occasions the model got it right.
func TestClaimedPositivelyLetsADenialThrough(t *testing.T) {
	for _, denied := range []string{
		"nothing has finished overnight.",
		"none of them finished.",
		"no sessions finished.",
		"not one of them finished.",
		"nothing finished, and nothing is waiting.",
		"never finished.",
	} {
		if ClaimedPositively(denied, "finish") {
			t.Errorf("a denial was read as a claim: %q", denied)
		}
	}
	for _, claimed := range []string{
		"two sessions finished overnight.",
		"the deploy finished.",
	} {
		if !ClaimedPositively(claimed, "finish") {
			t.Errorf("a claim was not caught: %q", claimed)
		}
	}

	// The known boundary, asserted rather than left to be discovered: a
	// denial about ONE thing shields a claim about another when the two are
	// close enough together. It is the cost of a substring rule with a window
	// instead of a parser, it is bounded by negationWindow, and the next test
	// pins that the shielding stops there. Written down here so a reader of a
	// refusal knows what the guard does and does not promise.
	near := "nothing is waiting, but the refactor finished."
	if ClaimedPositively(near, "finish") {
		t.Errorf("the negation window has changed shape; update this note: %q", near)
	}
}

// TestADenialDoesNotCoverTheWholeSentence. The negation window is deliberately
// short: a denial in the first clause must not licence an invention in a clause
// forty characters later.
func TestADenialDoesNotCoverTheWholeSentence(t *testing.T) {
	far := "nothing at all is waiting on you across any of the threads or the reminders, " +
		"and both of the overnight builds finished."
	if !ClaimedPositively(far, "finish") {
		t.Errorf("a distant denial covered a later claim: %q", far)
	}
	if strings.Index(far, "finished")-strings.Index(far, "nothing") <= negationWindow {
		t.Fatal("the fixture is inside the negation window, so it proves nothing")
	}
}

// TestNumbersReadsDigitsAndWords. Only the caller's own number words are
// recognised: a sentence reaching past the table is describing something the
// caller's facts do not have, and its number is therefore not one they can
// vouch for.
func TestNumbersReadsDigitsAndWords(t *testing.T) {
	words := []string{"zero", "one", "two", "three"}
	for _, c := range []struct {
		in   string
		want []int
	}{
		{"three sessions are waiting.", []int{3}},
		{"3 sessions are waiting.", []int{3}},
		{"two waiting and one finished.", []int{2, 1}},
		{"nothing at all.", nil},
		// Not in the caller's table, so it contributes no number — which is
		// what makes an out-of-range count fail the caller's own check rather
		// than pass it silently.
		{"seventeen sessions are waiting.", nil},
		// A word that merely contains a number word is not one.
		{"onerous work is still going.", nil},
	} {
		got := Numbers(c.in, words)
		if len(got) != len(c.want) {
			t.Errorf("Numbers(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("Numbers(%q) = %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
}
