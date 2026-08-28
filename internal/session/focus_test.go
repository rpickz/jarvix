package session

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/rpickz/jarvix/internal/intent"
)

// The engine half of focus threads (#123): a routed focus phrase reaches the
// focus runner through the IntentRunner seam and its answer is spoken as the
// acknowledgement — no provider call, no argv, no shell — and a daemon with
// no focus-capable runner refuses in words rather than shrugging.

// fakeFocusRunner is an intent.Runner that also answers focus actions,
// standing in for internal/focus.IntentRunner. Safe for concurrent use like
// the FakeRunner it embeds.
type fakeFocusRunner struct {
	intent.FakeRunner
	mu      sync.Mutex
	matches []intent.Match
	spoken  string
	err     error
}

func (f *fakeFocusRunner) RunFocus(_ context.Context, m intent.Match) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.matches = append(f.matches, m)
	return f.spoken, f.err
}

func (f *fakeFocusRunner) seen() []intent.Match {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]intent.Match(nil), f.matches...)
}

func newFocusHarness(t *testing.T, runner intent.Runner) *harness {
	t.Helper()
	router, err := intent.New(intent.Options{})
	if err != nil {
		t.Fatalf("intent.New: %v", err)
	}
	return newHarness(t, Options{
		SpeakResponses: true, HistoryTurns: 8,
		Intents: router, IntentRunner: runner,
	})
}

func sayFocus(t *testing.T, h *harness, text string) map[string]Event {
	t.Helper()
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit(text); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	return seen
}

func TestFocusPhraseReachesTheRunnerAndSpeaksItsRecap(t *testing.T) {
	runner := &fakeFocusRunner{spoken: "Back on deploy — last here ten minutes ago."}
	h := newFocusHarness(t, runner)

	seen := sayFocus(t, h, "switch to the deploy thread")

	if len(h.provider.Requests) != 0 {
		t.Fatalf("the provider was called %d times for a focus intent", len(h.provider.Requests))
	}
	matches := runner.seen()
	if len(matches) != 1 || matches[0].Focus != intent.FocusSwitch || matches[0].FocusText != "deploy" {
		t.Fatalf("runner saw %+v", matches)
	}
	// Nothing spoken to the focus family may reach a command line.
	if runner.Argv() != nil || runner.Shell() != nil {
		t.Errorf("a focus phrase reached argv=%v shell=%v", runner.Argv(), runner.Shell())
	}
	ev, ok := seen["intent.executed"]
	if !ok {
		t.Fatal("no intent.executed event")
	}
	if ev.Data["intent"] != "focus.switch" || ev.Data["source"] != "focus" || ev.Data["status"] != "ok" {
		t.Errorf("event data = %v", ev.Data)
	}
	// The recap is the acknowledgement, spoken verbatim.
	if h.tts.Last().Text != "Back on deploy — last here ten minutes ago." {
		t.Errorf("spoken recap = %q", h.tts.Last().Text)
	}
}

func TestFocusRefusalIsOneSpokenSentence(t *testing.T) {
	runner := &fakeFocusRunner{err: errors.New("no thread is called \"deploy\"")}
	h := newFocusHarness(t, runner)

	seen := sayFocus(t, h, "switch to the deploy thread")

	ev := seen["intent.executed"]
	if ev.Data["status"] != "failed" {
		t.Errorf("event data = %v", ev.Data)
	}
	if h.tts.Last().Text != "Sorry, no thread is called \"deploy\"." {
		t.Errorf("spoken refusal = %q", h.tts.Last().Text)
	}
}

func TestFocusWithoutARunnerRefusesInWords(t *testing.T) {
	// A bare FakeRunner has no RunFocus: the honest refusal, never a silent
	// success or a stuck session.
	h := newFocusHarness(t, &intent.FakeRunner{})

	seen := sayFocus(t, h, "what did i park")

	ev := seen["intent.executed"]
	if ev.Data["status"] != "failed" {
		t.Errorf("event data = %v", ev.Data)
	}
	if h.tts.Last().Text != "Sorry, focus threads are not available on this daemon." {
		t.Errorf("spoken refusal = %q", h.tts.Last().Text)
	}
}
