package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/intent"
)

// The capture tests cover the engine half of #62: "save this as <name>" is
// claimed by the router (no provider call), planned through the injected
// capture service — never the real one; no test here reads a desktop or
// writes a config file — asked about when it would replace an existing
// routine, and acknowledged with the service's one spoken confirmation.

// fakeCapturer is a scripted RoutineCapturer.
type fakeCapturer struct {
	mu           sync.Mutex
	question     string
	replaces     bool
	planErr      error
	commitSpoken string
	commitErr    error

	planned []string
	commits int
}

func (f *fakeCapturer) Plan(_ context.Context, name string) (CapturePlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.planned = append(f.planned, name)
	if f.planErr != nil {
		return nil, f.planErr
	}
	return &fakeCapturePlan{f: f}, nil
}

func (f *fakeCapturer) plans() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.planned...)
}

func (f *fakeCapturer) committed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commits
}

type fakeCapturePlan struct{ f *fakeCapturer }

func (p *fakeCapturePlan) ReplaceQuestion() (string, bool) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	return p.f.question, p.f.replaces
}

func (p *fakeCapturePlan) Commit(context.Context) (string, error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.commits++
	return p.f.commitSpoken, p.f.commitErr
}

// newCaptureHarness wires an engine whose router carries the built-in capture
// patterns (they always compile) and whose capture service is the fake. A nil
// capturer exercises the "daemon without the service" refusal.
func newCaptureHarness(t *testing.T, capturer RoutineCapturer) *harness {
	t.Helper()
	router, err := intent.New(intent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Options{})
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	t.Cleanup(h.cancel)
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, nil, nil, bus, nil, Options{
		Model: "m", SpeakResponses: true, HistoryTurns: 8,
		ConfirmTimeout: 5 * time.Second,
		Intents:        router, IntentRunner: &intent.FakeRunner{},
		Capture: capturer,
	})
	return h
}

// TestCapturePhraseSavesWithoutAProviderCall is the trigger acceptance
// criterion end to end: the phrase reaches the capture service with the
// spoken name, no model is consulted, and the service's confirmation is the
// one thing spoken.
func TestCapturePhraseSavesWithoutAProviderCall(t *testing.T) {
	capturer := &fakeCapturer{commitSpoken: "Seven windows across three workspaces, saved as morning setup."}
	h := newCaptureHarness(t, capturer)

	seen := sayRoutine(t, h, "save this as morning setup")

	if len(h.provider.Requests) != 0 {
		t.Fatalf("the provider was called %d times for a capture phrase", len(h.provider.Requests))
	}
	if plans := capturer.plans(); len(plans) != 1 || plans[0] != "morning setup" {
		t.Fatalf("planned %v, want the spoken name", plans)
	}
	if capturer.committed() != 1 {
		t.Fatalf("committed %d times", capturer.committed())
	}
	ev, ok := seen["intent.executed"]
	if !ok {
		t.Fatal("no intent.executed event")
	}
	if ev.Data["intent"] != intent.CaptureIntentName || ev.Data["routine"] != "morning setup" {
		t.Errorf("event = %v", ev.Data)
	}
	if ev.Data["acknowledgement"] != capturer.commitSpoken {
		t.Errorf("acknowledgement = %v", ev.Data["acknowledgement"])
	}
	if h.tts.Last().Text != capturer.commitSpoken {
		t.Errorf("spoken confirmation = %q", h.tts.Last().Text)
	}
}

// TestCaptureReplaceAsksFirst: an existing name goes through the one shared
// confirmation exchange (ADR 0014), nothing commits until the answer is yes,
// and the question names the routine.
func TestCaptureReplaceAsksFirst(t *testing.T) {
	capturer := &fakeCapturer{
		replaces:     true,
		question:     "A routine called morning setup already exists. Should I replace it with this layout?",
		commitSpoken: "Two windows on one workspace, saved as morning setup.",
	}
	h := newCaptureHarness(t, capturer)

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("save this as morning setup"); err != nil {
		t.Fatal(err)
	}
	ev := h.waitFor(t, "tool.confirmation_required")
	if ev.Data["tool"] != captureToolName || ev.Data["command"] != `replace routine "morning setup"` {
		t.Errorf("confirmation = %v", ev.Data)
	}
	if ev.Data["summary"] != capturer.question {
		t.Errorf("summary = %v", ev.Data["summary"])
	}
	if capturer.committed() != 0 {
		t.Fatal("the capture committed before the replace was confirmed")
	}
	if err := h.engine.Confirm(true); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if capturer.committed() != 1 {
		t.Fatalf("committed %d times after approval", capturer.committed())
	}
	if seen["intent.executed"].Data["status"] != "ok" {
		t.Errorf("status = %v", seen["intent.executed"].Data["status"])
	}
}

// TestCaptureReplaceDeclinedWritesNothing: declining leaves the curated
// routine untouched — the clobber-on-misheard-phrase failure this exchange
// exists to prevent — and the user hears a plain cancellation.
func TestCaptureReplaceDeclinedWritesNothing(t *testing.T) {
	capturer := &fakeCapturer{replaces: true, question: "Replace it?", commitSpoken: "never"}
	h := newCaptureHarness(t, capturer)

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("save this as morning setup"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "tool.confirmation_required")
	if err := h.engine.Confirm(false); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	if capturer.committed() != 0 {
		t.Fatalf("a declined replace committed %d times", capturer.committed())
	}
	if seen["intent.executed"].Data["acknowledgement"] != "Cancelled." {
		t.Errorf("acknowledgement = %v", seen["intent.executed"].Data["acknowledgement"])
	}
	if _, declined := seen["tool.declined"]; !declined {
		t.Error("a declined replace must reach the audit trail")
	}
}

// TestCaptureWithoutServiceIsAnHonestRefusal: a daemon built without the
// capture service speaks a sentence instead of silently dropping the phrase.
func TestCaptureWithoutServiceIsAnHonestRefusal(t *testing.T) {
	h := newCaptureHarness(t, nil)

	seen := sayRoutine(t, h, "save this as morning setup")

	ev := seen["intent.executed"]
	if ev.Data["status"] != "failed" {
		t.Errorf("status = %v", ev.Data["status"])
	}
	if ev.Data["acknowledgement"] != "Sorry, saving layouts is not available on this daemon." {
		t.Errorf("acknowledgement = %v", ev.Data["acknowledgement"])
	}
}

// TestCapturePlanFailureIsSpoken: a plan that cannot happen — no compositor,
// nothing on screen — is one spoken sentence, and nothing commits.
func TestCapturePlanFailureIsSpoken(t *testing.T) {
	capturer := &fakeCapturer{planErr: errors.New("I cannot reach the window manager")}
	h := newCaptureHarness(t, capturer)

	seen := sayRoutine(t, h, "save this as morning setup")

	if capturer.committed() != 0 {
		t.Fatalf("a failed plan committed %d times", capturer.committed())
	}
	if seen["intent.executed"].Data["acknowledgement"] != "Sorry, I cannot reach the window manager." {
		t.Errorf("acknowledgement = %v", seen["intent.executed"].Data["acknowledgement"])
	}
}
