package session

import (
	"strings"
	"sync"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/knowledge"
)

// Engine-side feed cache (ADR 0031): where the feed block lands in the
// message list, that only provider turns consult it, and that the disclosure
// event carries counts, never values.

// fakeFeedInjector is a scripted KnowledgeInjector counting consultations.
type fakeFeedInjector struct {
	mu        sync.Mutex
	injection knowledge.Injection
	calls     int
}

func (f *fakeFeedInjector) Inject() knowledge.Injection {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.injection
}

func (f *fakeFeedInjector) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestFeedValuesReachTheModelBesideTheQuestion(t *testing.T) {
	injector := &fakeFeedInjector{injection: knowledge.Injection{
		Message:   "Live feed values:\n- amd: 187.42 (as of four minutes ago)",
		Feeds:     1,
		EstTokens: 20,
	}}
	opts := Options{SystemPrompt: "You are Jarvix."}
	opts.Knowledge = injector
	h := newHarness(t, opts)

	h.ask(t, "what's the AMD price?")

	if injector.Calls() != 1 {
		t.Fatalf("injector consulted %d times, want once per model turn", injector.Calls())
	}
	msgs := h.provider.LastRequest.Messages
	if len(msgs) != 3 {
		t.Fatalf("messages = %d (%v), want system prompt, feed block, question", len(msgs), roles(msgs))
	}
	// A system message directly before the question: a reading describes
	// "right now", so like the desktop capture it stays adjacent to the
	// moment it belongs to — and is rebuilt fresh each turn, never history.
	if msgs[1].Role != ai.RoleSystem || !strings.Contains(msgs[1].Content, "187.42") {
		t.Errorf("feed message = %+v, want the injected block as system", msgs[1])
	}
	if msgs[2].Role != ai.RoleUser {
		t.Errorf("last message = %+v, want the question", msgs[2])
	}
}

func TestFeedInjectionEventCarriesCountsOnly(t *testing.T) {
	injector := &fakeFeedInjector{injection: knowledge.Injection{
		Message:   "Live feed values:\n- amd: 187.42 (as of just now)",
		Feeds:     1,
		Trimmed:   2,
		EstTokens: 15,
	}}
	opts := Options{}
	opts.Knowledge = injector
	h := newHarness(t, opts)

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("what's the AMD price?"); err != nil {
		t.Fatal(err)
	}
	events := h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	ev, published := events["knowledge.injected"]
	if !published {
		t.Fatal("no knowledge.injected event; the injection must be disclosed")
	}
	if ev.Data["feeds"] != 1 || ev.Data["trimmed"] != 2 {
		t.Errorf("event data = %v, want the counts", ev.Data)
	}
	for k, v := range ev.Data {
		if s, isString := v.(string); isString && strings.Contains(s, "187.42") {
			t.Errorf("event field %q carries a feed value; events fan out to every client", k)
		}
	}
}

func TestNoFeedInjectorMeansNoConsultationAndNoEvent(t *testing.T) {
	h := newHarness(t, Options{})
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("hello"); err != nil {
		t.Fatal(err)
	}
	events := h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if _, published := events["knowledge.injected"]; published {
		t.Error("a knowledge.injected event with no injector configured")
	}
}
