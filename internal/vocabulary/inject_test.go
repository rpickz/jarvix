package vocabulary

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The injection half of issue #129: the budget's edge cases (empty, at cap,
// over cap), the pinned preamble wording, and the speak_back stance.

// TestZeroEntriesInjectNothing is the byte-identity criterion at the store
// level: an empty vocabulary produces the zero Injection — no message, no
// counts — so the engine appends no system message and the prompt is
// byte-identical to one before the feature existed.
func TestZeroEntriesInjectNothing(t *testing.T) {
	s, _ := newTestStore(t, StoreOptions{})
	for _, speakBack := range []bool{false, true} {
		inj := s.Inject(speakBack)
		if inj.Message != "" || inj.Total != 0 || inj.Trimmed != 0 ||
			len(inj.Entries) != 0 || inj.EstTokens != 0 {
			t.Errorf("Inject(speakBack=%v) over an empty store = %+v, want the zero value",
				speakBack, inj)
		}
	}
}

// TestInjectionWordingIsPinned pins the block's instruction sentences: use
// without echo-parroting, never recite, and the default do-not-speak-back
// stance. This is the acceptance criterion's wording, not prose to drift.
func TestInjectionWordingIsPinned(t *testing.T) {
	s, _ := newTestStore(t, StoreOptions{})
	mustTeach(t, s, "quid", "pounds", "UK money slang")

	inj := s.Inject(false)
	for _, want := range []string{
		"Taught vocabulary: words and phrases the user explicitly taught you",
		"understand it as its meaning",
		"never echo the phrase back",
		"Never recite this list unprompted.",
		"Do not use these words yourself",
		`- [w1, taught 2026-08-20] "quid" means: pounds (UK money slang)`,
	} {
		if !strings.Contains(inj.Message, want) {
			t.Errorf("block is missing %q:\n%s", want, inj.Message)
		}
	}
	if strings.Contains(inj.Message, "You may use these words") {
		t.Errorf("speak_back=false block carries the opt-in stance:\n%s", inj.Message)
	}

	on := s.Inject(true)
	if !strings.Contains(on.Message, "You may use these words in your own replies") {
		t.Errorf("speak_back=true block is missing the opt-in stance:\n%s", on.Message)
	}
	if strings.Contains(on.Message, "Do not use these words yourself") {
		t.Errorf("speak_back=true block still forbids speaking back:\n%s", on.Message)
	}
}

// TestInjectionKeepsEverythingWithinBudget: the at-cap edge — a vocabulary
// that fits exactly is carried whole, with no trim and no disclosure line.
func TestInjectionKeepsEverythingWithinBudget(t *testing.T) {
	s, _ := newTestStore(t, StoreOptions{})
	mustTeach(t, s, "quid", "pounds", "")
	mustTeach(t, s, "telly", "television", "")

	inj := s.Inject(false)
	if inj.Trimmed != 0 || inj.Total != 2 || len(inj.Entries) != 2 {
		t.Fatalf("injection = %+v, want both entries, nothing trimmed", inj)
	}
	if strings.Contains(inj.Message, "left out to save space") {
		t.Errorf("an untrimmed block carries a trim disclosure:\n%s", inj.Message)
	}
	if inj.EstTokens != estimateTokens(inj.Message) {
		t.Errorf("EstTokens = %d, want the message's own estimate %d",
			inj.EstTokens, estimateTokens(inj.Message))
	}
}

// TestInjectionTrimsLeastRecentlyTaughtAndDiscloses: the over-cap edge —
// the block keeps the most recently taught entries, drops the stale tail
// from the block only (never the store), and says so inside the message.
func TestInjectionTrimsLeastRecentlyTaughtAndDiscloses(t *testing.T) {
	s, clock := newTestStore(t, StoreOptions{MaxInjectedTokens: MinInjectedTokens})
	for i := 0; i < 12; i++ {
		mustTeach(t, s, fmt.Sprintf("word%d", i),
			"a deliberately wordy meaning that costs real budget", "")
		clock.Advance(time.Minute)
	}

	inj := s.Inject(false)
	if inj.Trimmed == 0 || inj.Total != 12 {
		t.Fatalf("a tiny budget trimmed nothing: %+v", inj)
	}
	if len(inj.Entries)+inj.Trimmed != 12 {
		t.Errorf("entries %d + trimmed %d != total 12", len(inj.Entries), inj.Trimmed)
	}
	// Most recently taught survive; the oldest go first.
	if len(inj.Entries) > 0 && inj.Entries[0].Phrase != "word11" {
		t.Errorf("first kept entry = %q, want the most recently taught", inj.Entries[0].Phrase)
	}
	want := fmt.Sprintf("(%d more taught phrases were left out to save space", inj.Trimmed)
	if inj.Trimmed == 1 {
		want = "(1 more taught phrase was left out to save space"
	}
	if !strings.Contains(inj.Message, want) {
		t.Errorf("block does not disclose the trim %q:\n%s", want, inj.Message)
	}
	if estimateTokens(inj.Message) > MinInjectedTokens {
		t.Errorf("message costs %d tokens, over the %d budget",
			estimateTokens(inj.Message), MinInjectedTokens)
	}
	// The trim came out of the block, never the store.
	if n, _ := s.Count(); n != 12 {
		t.Errorf("store count after injection = %d, want 12", n)
	}

	// And the window's warning names the same trim — never silent.
	warning := s.InjectionWarning(false)
	if !strings.Contains(warning, "vocabulary.max_injected_tokens") {
		t.Errorf("InjectionWarning = %q, want the over-budget sentence", warning)
	}
}

// TestInjectionWarningIsEmptyWhenEverythingFits: the warning exists exactly
// when entries are being left out.
func TestInjectionWarningIsEmptyWhenEverythingFits(t *testing.T) {
	s, _ := newTestStore(t, StoreOptions{})
	mustTeach(t, s, "quid", "pounds", "")
	if w := s.InjectionWarning(false); w != "" {
		t.Errorf("InjectionWarning = %q, want none when nothing is trimmed", w)
	}
}

// TestReTaughtEntryLineSaysSo: the line's verb follows the trail, so the
// model can answer "when did that change".
func TestReTaughtEntryLineSaysSo(t *testing.T) {
	s, clock := newTestStore(t, StoreOptions{})
	mustTeach(t, s, "quid", "pounds", "")
	clock.Advance(24 * time.Hour)
	if _, _, err := s.Teach("quid", "euros", "", ""); err != nil {
		t.Fatal(err)
	}
	inj := s.Inject(false)
	if !strings.Contains(inj.Message, `[w1, re-taught 2026-08-21] "quid" means: euros`) {
		t.Errorf("block line does not carry the re-taught verb:\n%s", inj.Message)
	}
}
