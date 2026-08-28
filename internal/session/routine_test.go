package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/tools"
)

// The routine tests cover the engine half of ADR 0026: a routine phrase is
// claimed by the router, gated under its own identity, executed through the
// injected runner (never the real one — no test here places a window), and
// acknowledged with the run's single summary. The provider is a fake so the
// headline assertion — zero model calls on a hit — is a count, not a hope.

// fakeRoutines is a scripted RoutineRunner.
type fakeRoutines struct {
	mu      sync.Mutex
	summary string
	err     error
	// block, when set, parks Run until the context is cancelled — the
	// deterministic stand-in for a routine mid-placement. started is closed
	// when Run has the context, so a test can cancel *during* the run without
	// polling.
	block   bool
	started chan struct{}

	runs    []string
	lastCtx context.Context
}

func (f *fakeRoutines) Run(ctx context.Context, name string) (string, error) {
	f.mu.Lock()
	f.runs = append(f.runs, name)
	f.lastCtx = ctx
	block, summary, err := f.block, f.summary, f.err
	started := f.started
	f.mu.Unlock()
	if started != nil {
		close(started)
	}
	if block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return summary, err
}

func (f *fakeRoutines) ran() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.runs...)
}

// newRoutineHarness wires an engine whose router knows one routine and whose
// runner is the fake. A nil policyCfg installs no registry, which for
// routine.run means the shipped default: allow.
func newRoutineHarness(t *testing.T, runner *fakeRoutines, policyCfg *tools.PolicyConfig) *harness {
	t.Helper()
	router, err := intent.New(intent.Options{Routines: []intent.RoutinePhrases{
		{Name: "morning setup", Phrases: []string{"morning setup", "start my usual apps"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Options{})
	var registry *tools.Registry
	if policyCfg != nil {
		policy, err := tools.NewPolicy(*policyCfg)
		if err != nil {
			t.Fatal(err)
		}
		registry = tools.NewRegistry(nil)
		registry.SetPolicy(policy)
	}
	h.tools = registry
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	t.Cleanup(h.cancel)
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, registry, nil, bus, nil, Options{
		Model: "m", SpeakResponses: true, HistoryTurns: 8,
		ConfirmTimeout: 5 * time.Second,
		Intents:        router, IntentRunner: &intent.FakeRunner{},
		Routines: runner,
	})
	return h
}

func sayRoutine(t *testing.T, h *harness, text string) map[string]Event {
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

// TestRoutinePhraseRunsWithoutAProviderCall is the routing acceptance
// criterion end to end: the phrase reaches the runner, no model is consulted,
// and the run's summary is the one thing spoken.
func TestRoutinePhraseRunsWithoutAProviderCall(t *testing.T) {
	runner := &fakeRoutines{summary: "Morning setup: all five apps placed."}
	h := newRoutineHarness(t, runner, nil)

	seen := sayRoutine(t, h, "start my usual apps")

	if len(h.provider.Requests) != 0 {
		t.Fatalf("the provider was called %d times for a routine phrase", len(h.provider.Requests))
	}
	if ran := runner.ran(); len(ran) != 1 || ran[0] != "morning setup" {
		t.Fatalf("runner ran %v", ran)
	}
	ev, ok := seen["intent.executed"]
	if !ok {
		t.Fatal("no intent.executed event")
	}
	if ev.Data["intent"] != "routine.run" || ev.Data["routine"] != "morning setup" {
		t.Errorf("event = %v", ev.Data)
	}
	if ev.Data["source"] != "routine" || ev.Data["status"] != "ok" {
		t.Errorf("event = %v", ev.Data)
	}
	if ev.Data["acknowledgement"] != "Morning setup: all five apps placed." {
		t.Errorf("acknowledgement = %v", ev.Data["acknowledgement"])
	}
	if h.tts.Last().Text != "Morning setup: all five apps placed." {
		t.Errorf("spoken summary = %q", h.tts.Last().Text)
	}
}

// TestRoutineRefusalWhileRunningIsSpoken: the runner's already-running
// refusal comes back as one spoken line, not an interleaved second run.
func TestRoutineRefusalWhileRunningIsSpoken(t *testing.T) {
	runner := &fakeRoutines{err: errors.New("morning setup is already running")}
	h := newRoutineHarness(t, runner, nil)

	seen := sayRoutine(t, h, "morning setup")

	ev := seen["intent.executed"]
	if ev.Data["status"] != "failed" {
		t.Errorf("status = %v", ev.Data["status"])
	}
	if ev.Data["acknowledgement"] != "Sorry, morning setup is already running." {
		t.Errorf("acknowledgement = %v", ev.Data["acknowledgement"])
	}
	if len(h.provider.Requests) != 0 {
		t.Errorf("a refused routine still made %d provider calls", len(h.provider.Requests))
	}
}

// TestRoutineDeniedByPolicyNeverRuns: the routine.run identity is
// override-able, and deny means the runner is never consulted.
func TestRoutineDeniedByPolicyNeverRuns(t *testing.T) {
	runner := &fakeRoutines{summary: "should never be heard"}
	h := newRoutineHarness(t, runner, &tools.PolicyConfig{
		Tools: map[string]tools.PolicyDecision{tools.RoutineToolName: tools.PolicyDeny},
	})

	seen := sayRoutine(t, h, "morning setup")

	if len(runner.ran()) != 0 {
		t.Fatalf("a denied routine ran: %v", runner.ran())
	}
	if _, denied := seen["tool.denied"]; !denied {
		t.Error("a denied routine must reach the audit trail")
	}
	if ev := seen["intent.executed"]; ev.Data["status"] != "failed" {
		t.Errorf("status = %v", ev.Data["status"])
	}
}

// TestRoutineAskPolicyConfirmsFirst: `[tools.policy.tool]."routine.run" =
// "ask"` puts the routine behind the one shared confirmation mechanism, and
// the question names the routine.
func TestRoutineAskPolicyConfirmsFirst(t *testing.T) {
	runner := &fakeRoutines{summary: "Morning setup: all five apps placed."}
	h := newRoutineHarness(t, runner, &tools.PolicyConfig{
		Tools: map[string]tools.PolicyDecision{tools.RoutineToolName: tools.PolicyAsk},
	})

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("morning setup"); err != nil {
		t.Fatal(err)
	}
	ev := h.waitFor(t, "tool.confirmation_required")
	if ev.Data["tool"] != tools.RoutineToolName || ev.Data["command"] != "morning setup" {
		t.Errorf("confirmation = %v", ev.Data)
	}
	if len(runner.ran()) != 0 {
		t.Fatal("the routine ran before it was confirmed")
	}
	if err := h.engine.Confirm(true); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if ran := runner.ran(); len(ran) != 1 {
		t.Fatalf("runner ran %v", ran)
	}
	if seen["intent.executed"].Data["status"] != "ok" {
		t.Errorf("status = %v", seen["intent.executed"].Data["status"])
	}

	// And declining runs nothing.
	runner2 := &fakeRoutines{summary: "never"}
	h2 := newRoutineHarness(t, runner2, &tools.PolicyConfig{
		Tools: map[string]tools.PolicyDecision{tools.RoutineToolName: tools.PolicyAsk},
	})
	_, _ = h2.engine.StartSession()
	_ = h2.engine.Submit("morning setup")
	h2.waitFor(t, "tool.confirmation_required")
	if err := h2.engine.Confirm(false); err != nil {
		t.Fatal(err)
	}
	seen2 := h2.collectUntil(t, "session.finished")
	h2.waitIdle(t)
	if len(runner2.ran()) != 0 {
		t.Fatalf("a declined routine ran: %v", runner2.ran())
	}
	if seen2["intent.executed"].Data["acknowledgement"] != "Cancelled." {
		t.Errorf("acknowledgement = %v", seen2["intent.executed"].Data["acknowledgement"])
	}
}

// TestRoutineHonoursSessionCancellation: the runner receives the session's
// context, so "stop" — or any interruption — aborts a routine mid-placement
// (composing with #54's cancel path), and the cancelled run says nothing.
func TestRoutineHonoursSessionCancellation(t *testing.T) {
	runner := &fakeRoutines{block: true, started: make(chan struct{})}
	h := newRoutineHarness(t, runner, nil)

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("morning setup"); err != nil {
		t.Fatal(err)
	}
	// The runner closes started once it is parked on the session context.
	<-runner.started
	if err := h.engine.Cancel(); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.cancelled")
	h.waitIdle(t)

	runner.mu.Lock()
	ctx := runner.lastCtx
	runner.mu.Unlock()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("cancelling the session did not cancel the routine's context")
	}
	if _, executed := seen["intent.executed"]; executed {
		t.Error("a cancelled routine still reported an outcome; the cancel path owns the events")
	}
}
