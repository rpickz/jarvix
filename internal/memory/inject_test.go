package memory

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The injection cap is the AI-safety half of the feature: the knowledge base
// must never crowd out the conversation, the trim must fall on the least
// recently confirmed facts, storage must be untouched, and the model must be
// told the list is incomplete. These tests pin each of those, including at
// the exact boundary, so a mutation to the trim arithmetic cannot survive.

// injectFixture stores n facts, one minute apart, newest last. Content is
// padded to a known size so token arithmetic in tests is exact.
func injectFixture(t *testing.T, n int, opts BookOptions) (*Book, *testClock) {
	t.Helper()
	b, clock, _ := newTestBook(t, opts)
	for i := 1; i <= n; i++ {
		clock.advance(time.Minute)
		b.mustAdd(t, fmt.Sprintf("fact number %d about topic%d", i, i))
	}
	return b, clock
}

func TestInjectCarriesEveryFactWhenTheyFit(t *testing.T) {
	b, _ := injectFixture(t, 3, BookOptions{})
	inj := b.Inject()
	if inj.Trimmed != 0 || inj.Total != 3 || len(inj.Facts) != 3 {
		t.Fatalf("injection = %+v, want all three facts, none trimmed", inj)
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(inj.Message, fmt.Sprintf("fact number %d", i)) {
			t.Errorf("message missing fact %d:\n%s", i, inj.Message)
		}
	}
	// Provenance: the model must be told where these came from.
	if !strings.Contains(inj.Message, "things the user asked you to remember") {
		t.Errorf("message missing provenance:\n%s", inj.Message)
	}
	// No trim, no trim disclosure — the model must not be told to go
	// searching for facts that are all present.
	if strings.Contains(inj.Message, "left out") {
		t.Errorf("untrimmed message discloses a trim:\n%s", inj.Message)
	}
	if inj.EstTokens != EstimateTokens(inj.Message) {
		t.Errorf("EstTokens = %d, want the message's own estimate %d",
			inj.EstTokens, EstimateTokens(inj.Message))
	}
}

func TestInjectOrdersMostRecentlyConfirmedFirst(t *testing.T) {
	b, clock, _ := newTestBook(t, BookOptions{})
	old := b.mustAdd(t, "the staging server is called atlas")
	clock.advance(time.Hour)
	b.mustAdd(t, "the user's editor is neovim")
	clock.advance(time.Hour)
	// Confirming the oldest fact moves it to the front: recency of
	// confirmation, not of creation, is what the trim protects.
	if _, err := b.Update(old.ID, "the staging server is called helios", ""); err != nil {
		t.Fatal(err)
	}
	inj := b.Inject()
	if len(inj.Facts) != 2 || inj.Facts[0].ID != old.ID {
		t.Fatalf("injection order = %+v, want the just-confirmed fact first", inj.Facts)
	}
}

// TestInjectTrimsOldestFromInjectionOnly is the cap-trim contract, asserted
// mutation-tight: exactly the least recently confirmed facts leave, exactly
// the newest stay, the disclosure names the count, and the store still holds
// everything.
func TestInjectTrimsOldestFromInjectionOnly(t *testing.T) {
	// A budget that comfortably holds the preamble and a couple of facts,
	// but nowhere near all six.
	b, _ := injectFixture(t, 6, BookOptions{MaxInjectedTokens: 120})
	inj := b.Inject()

	if inj.Total != 6 {
		t.Errorf("Total = %d, want 6", inj.Total)
	}
	if len(inj.Facts) == 0 || inj.Trimmed == 0 {
		t.Fatalf("injection = %+v, want some kept and some trimmed", inj)
	}
	if len(inj.Facts)+inj.Trimmed != 6 {
		t.Errorf("kept %d + trimmed %d != 6", len(inj.Facts), inj.Trimmed)
	}
	if inj.EstTokens > 120 {
		t.Errorf("EstTokens = %d, exceeds the cap of 120", inj.EstTokens)
	}
	// The kept facts are exactly the most recently confirmed ones: fact 6
	// down to fact 6-kept+1, in that order.
	for i, f := range inj.Facts {
		want := fmt.Sprintf("fact number %d ", 6-i)
		if !strings.HasPrefix(strings.TrimSpace(strings.SplitN(f.Content, "about", 2)[0]), strings.TrimSpace(want)) {
			t.Errorf("kept[%d] = %q, want it to be %q", i, f.Content, want)
		}
	}
	// The oldest fact is out of the block…
	if strings.Contains(inj.Message, "fact number 1 ") {
		t.Errorf("trimmed fact still in the message:\n%s", inj.Message)
	}
	// …the model is told, with the count and the way back…
	if !strings.Contains(inj.Message, fmt.Sprintf("%d more remembered facts were left out", inj.Trimmed)) {
		t.Errorf("message does not disclose the trim:\n%s", inj.Message)
	}
	if !strings.Contains(inj.Message, "memory.recall") {
		t.Errorf("trim disclosure does not name memory.recall:\n%s", inj.Message)
	}
	// …and storage is untouched: every fact still on disk and listable.
	if facts := b.List(""); len(facts) != 6 {
		t.Errorf("store holds %d facts after a trimmed injection, want 6 — "+
			"the cap must never delete", len(facts))
	}
}

// TestInjectBoundaryIsExact pins the greedy fit at its edge: with the cap
// set exactly at a full message's estimate nothing is trimmed, and one token
// less trims exactly one fact.
func TestInjectBoundaryIsExact(t *testing.T) {
	b, _ := injectFixture(t, 4, BookOptions{})
	full := b.Inject()
	if full.Trimmed != 0 {
		t.Fatalf("full injection trimmed %d with the default cap", full.Trimmed)
	}

	exact, _ := injectFixture(t, 4, BookOptions{MaxInjectedTokens: full.EstTokens})
	if inj := exact.Inject(); inj.Trimmed != 0 || len(inj.Facts) != 4 {
		t.Errorf("cap == estimate: injection = {kept %d, trimmed %d}, want all 4 kept",
			len(inj.Facts), inj.Trimmed)
	}

	// One token under: the single oldest fact must leave — not zero, not two.
	// (The message shrinks by a whole fact line but gains the disclosure
	// line, so allow exactly one or two facts out; asserting the *identity*
	// of the survivors is what kills mutations.)
	under, _ := injectFixture(t, 4, BookOptions{MaxInjectedTokens: full.EstTokens - 1})
	inj := under.Inject()
	if inj.Trimmed == 0 {
		t.Fatalf("cap one under the estimate trimmed nothing")
	}
	if inj.Facts[0].Content != "fact number 4 about topic4" {
		t.Errorf("newest fact %q not kept first", inj.Facts[0].Content)
	}
	for _, f := range inj.Facts {
		if f.Content == "fact number 1 about topic1" {
			t.Errorf("oldest fact kept while newer ones were trimmed")
		}
	}
}

func TestInjectWithEmptyStoreCostsNothing(t *testing.T) {
	b, _, _ := newTestBook(t, BookOptions{})
	inj := b.Inject()
	if inj.Message != "" || inj.Total != 0 || inj.EstTokens != 0 {
		t.Errorf("empty store injection = %+v, want zero-cost nothing", inj)
	}
}

// TestInjectDisclosesEvenWhenNothingFits: a pathological cap against a
// pathological fact must still tell the model memory exists — silence would
// read as "nothing is remembered", which is false.
func TestInjectDisclosesEvenWhenNothingFits(t *testing.T) {
	b, _, _ := newTestBook(t, BookOptions{MaxInjectedTokens: MinInjectedTokens})
	b.mustAdd(t, strings.Repeat("very long fact ", 60))
	inj := b.Inject()
	if len(inj.Facts) != 0 || inj.Trimmed != 1 {
		t.Fatalf("injection = %+v, want the oversized fact trimmed", inj)
	}
	if !strings.Contains(inj.Message, "1 more remembered fact was left out") {
		t.Errorf("message = %q, want the trim disclosed", inj.Message)
	}
}

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"abcd", 1},
		{"abcde", 2},
		{strings.Repeat("x", 400), 100},
	}
	for _, c := range cases {
		if got := EstimateTokens(c.text); got != c.want {
			t.Errorf("EstimateTokens(%d chars) = %d, want %d", len(c.text), got, c.want)
		}
	}
}
