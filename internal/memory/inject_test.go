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
// Since #104 they also pin the retrieval policy (ADR 0037): all-ambient
// while nothing is pinned and everything fits, pinned-only once a pin
// exists, search-only when an unpinned book outgrows the budget.

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

// pinAll pins every fact in the book, so the budget tests exercise the trim
// path — which since ADR 0037 only the ambient (pinned) set can reach.
func pinAll(t *testing.T, b *Book) {
	t.Helper()
	for _, f := range b.List("") {
		if _, err := b.SetPinned(f.ID, true); err != nil {
			t.Fatal(err)
		}
	}
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
	// searching for facts that are all present. This is the zero-regression
	// case of ADR 0037: nothing pinned, everything fitting, so the block
	// must not mention memory.search at all.
	if strings.Contains(inj.Message, "left out") {
		t.Errorf("untrimmed message discloses a trim:\n%s", inj.Message)
	}
	if strings.Contains(inj.Message, "not shown") || strings.Contains(inj.Message, "memory.search") {
		t.Errorf("all-ambient message points at search:\n%s", inj.Message)
	}
	if inj.Searchable != 0 {
		t.Errorf("Searchable = %d with everything ambient, want 0", inj.Searchable)
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
// everything. Since ADR 0037 the budget governs the ambient set, so the
// fixture pins every fact — an over-budget *unpinned* book takes the
// search-only path instead (its own test below).
func TestInjectTrimsOldestFromInjectionOnly(t *testing.T) {
	// A budget that comfortably holds the preamble and a couple of facts,
	// but nowhere near all six.
	b, _ := injectFixture(t, 6, BookOptions{MaxInjectedTokens: 120})
	pinAll(t, b)
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
	if !strings.Contains(inj.Message, "memory.search") {
		t.Errorf("trim disclosure does not name memory.search:\n%s", inj.Message)
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
	// of the survivors is what kills mutations.) Everything is pinned so the
	// budget bites as a trim — an unpinned over-budget book goes search-only
	// instead. A fully-pinned in-budget block renders identically to the
	// all-ambient one, so full.EstTokens carries over exactly.
	under, _ := injectFixture(t, 4, BookOptions{MaxInjectedTokens: full.EstTokens - 1})
	pinAll(t, under)
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
// pathological pinned fact must still tell the model memory exists —
// silence would read as "nothing is remembered", which is false.
func TestInjectDisclosesEvenWhenNothingFits(t *testing.T) {
	b, _, _ := newTestBook(t, BookOptions{MaxInjectedTokens: MinInjectedTokens})
	b.mustAdd(t, strings.Repeat("very long fact ", 60))
	pinAll(t, b)
	inj := b.Inject()
	if len(inj.Facts) != 0 || inj.Trimmed != 1 {
		t.Fatalf("injection = %+v, want the oversized fact trimmed", inj)
	}
	if !strings.Contains(inj.Message, "1 more remembered fact was left out") {
		t.Errorf("message = %q, want the trim disclosed", inj.Message)
	}
}

// TestInjectPinnedSplitsAmbientFromSearchable is the core of issue #104:
// with any pin, exactly the pinned facts are in the prompt, the unpinned
// rest is disclosed as searchable, and the block tells the model not to
// re-search what it already has.
func TestInjectPinnedSplitsAmbientFromSearchable(t *testing.T) {
	b, clock, _ := newTestBook(t, BookOptions{})
	pinnedFact := b.mustAdd(t, "the staging server is called atlas")
	clock.advance(time.Minute)
	b.mustAdd(t, "the user's editor is neovim")
	clock.advance(time.Minute)
	b.mustAdd(t, "the user's terminal is Ghostty")
	if _, err := b.SetPinned(pinnedFact.ID, true); err != nil {
		t.Fatal(err)
	}

	inj := b.Inject()
	if len(inj.Facts) != 1 || inj.Facts[0].ID != pinnedFact.ID {
		t.Fatalf("ambient facts = %+v, want exactly the pinned one", inj.Facts)
	}
	if inj.Searchable != 2 || inj.Total != 3 || inj.Trimmed != 0 {
		t.Errorf("injection = {searchable %d, total %d, trimmed %d}, want {2, 3, 0}",
			inj.Searchable, inj.Total, inj.Trimmed)
	}
	if !strings.Contains(inj.Message, "atlas") {
		t.Errorf("pinned fact missing from the block:\n%s", inj.Message)
	}
	for _, leaked := range []string{"neovim", "Ghostty"} {
		if strings.Contains(inj.Message, leaked) {
			t.Errorf("unpinned fact %q leaked into the prompt:\n%s", leaked, inj.Message)
		}
	}
	if !strings.Contains(inj.Message, "2 further remembered facts are not shown here by design") ||
		!strings.Contains(inj.Message, "memory.search") {
		t.Errorf("block does not disclose the searchable rest:\n%s", inj.Message)
	}
	if !strings.Contains(inj.Message, "do not search for those") {
		t.Errorf("block does not tell the model its ambient facts need no search:\n%s", inj.Message)
	}
}

// TestInjectUnpinnedOverBudgetIsSearchOnly pins ADR 0037's third case: a
// book that outgrew the budget with nothing pinned injects no facts — the
// old silent tail-drop is replaced by an explicit "N facts, none shown,
// search" block — and the store is untouched.
func TestInjectUnpinnedOverBudgetIsSearchOnly(t *testing.T) {
	b, _ := injectFixture(t, 6, BookOptions{MaxInjectedTokens: 120})
	inj := b.Inject()
	if len(inj.Facts) != 0 || inj.Trimmed != 0 {
		t.Fatalf("injection = {facts %d, trimmed %d}, want none ambient and nothing counted as trimmed",
			len(inj.Facts), inj.Trimmed)
	}
	if inj.Searchable != 6 || inj.Total != 6 {
		t.Errorf("searchable = %d, total = %d, want 6 and 6", inj.Searchable, inj.Total)
	}
	for _, want := range []string{"6 remembered facts", "none are shown", "memory.search"} {
		if !strings.Contains(inj.Message, want) {
			t.Errorf("search-only block missing %q:\n%s", want, inj.Message)
		}
	}
	if strings.Contains(inj.Message, "fact number") {
		t.Errorf("search-only block leaked fact content:\n%s", inj.Message)
	}
	if facts := b.List(""); len(facts) != 6 {
		t.Errorf("store holds %d facts after a search-only injection, want 6", len(facts))
	}
	// The budget must actually be respected by the replacement block too.
	if inj.EstTokens > 120 {
		t.Errorf("search-only block costs %d tokens, exceeding the cap", inj.EstTokens)
	}
}

// TestAmbientWarning pins the Memory-tab warning contract (#104): silent in
// every state the user chose, a sentence naming the fix whenever the budget
// is dropping facts the user did not choose to drop.
func TestAmbientWarning(t *testing.T) {
	t.Run("quiet while everything fits unpinned", func(t *testing.T) {
		b, _ := injectFixture(t, 3, BookOptions{})
		if w := b.AmbientWarning(); w != "" {
			t.Errorf("warning = %q on an in-budget unpinned book", w)
		}
	})
	t.Run("quiet on the designed split", func(t *testing.T) {
		b, _ := injectFixture(t, 3, BookOptions{})
		if _, err := b.SetPinned("m1", true); err != nil {
			t.Fatal(err)
		}
		if w := b.AmbientWarning(); w != "" {
			t.Errorf("warning = %q on a fitting pin/search split — that state is the feature working", w)
		}
	})
	t.Run("quiet on an empty book", func(t *testing.T) {
		b, _, _ := newTestBook(t, BookOptions{})
		if w := b.AmbientWarning(); w != "" {
			t.Errorf("warning = %q on an empty book", w)
		}
	})
	t.Run("pinned set over budget", func(t *testing.T) {
		b, _ := injectFixture(t, 6, BookOptions{MaxInjectedTokens: 120})
		pinAll(t, b)
		w := b.AmbientWarning()
		if !strings.Contains(w, "pinned") || !strings.Contains(w, "memory.max_injected_tokens") ||
			!strings.Contains(w, "unpin") {
			t.Errorf("warning = %q, want it to name the pins, the setting, and the fix", w)
		}
	})
	t.Run("unpinned book over budget", func(t *testing.T) {
		b, _ := injectFixture(t, 6, BookOptions{MaxInjectedTokens: 120})
		w := b.AmbientWarning()
		if !strings.Contains(w, "none are pinned") || !strings.Contains(w, "memory.search") ||
			!strings.Contains(w, "pin the facts") {
			t.Errorf("warning = %q, want it to explain the search-only state and how to pin", w)
		}
	})
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
