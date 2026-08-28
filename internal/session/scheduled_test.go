package session

import (
	"testing"

	"github.com/rpickz/jarvix/internal/tools"
)

// The scheduled-session tests cover the engine half of ADR 0032: a clockfire
// enters through StartScheduledSession and then travels the ordinary session
// path — router, gate, runner, events, history — with exactly two
// differences, each pinned here: it refuses rather than interrupts when a
// session is active, and with announce off it is quiet, meaning the one run
// speaks not a single sentence while everything observable elsewhere
// (events, acknowledgement, archive) stays byte-identical.

// fireScheduled drives one clockfire through the engine the way the daemon
// does: scheduled session, submitted phrase, run to completion.
func fireScheduled(t *testing.T, h *harness, announce bool, phrase string) map[string]Event {
	t.Helper()
	if _, err := h.engine.StartScheduledSession(announce); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit(phrase); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	return seen
}

// TestScheduledFireSpeaksNothingByDefault is the announce-suppression
// criterion, asserted as a count so a mutation that re-enables any speech
// exit fails here: a quiet clockfire synthesizes zero sentences, while the
// run's events and recorded acknowledgement are exactly the spoken run's.
func TestScheduledFireSpeaksNothingByDefault(t *testing.T) {
	runner := &fakeRoutines{summary: "Morning setup: all five apps placed."}
	h := newRoutineHarness(t, runner, nil)

	seen := fireScheduled(t, h, false, "morning setup")

	if n := h.tts.Speaks(); n != 0 {
		t.Fatalf("a quiet clockfire synthesized %d sentences, want zero — a 3am TTS announcement is an anti-feature", n)
	}
	if ran := runner.ran(); len(ran) != 1 || ran[0] != "morning setup" {
		t.Fatalf("runner ran %v", ran)
	}
	ev, ok := seen["intent.executed"]
	if !ok {
		t.Fatal("no intent.executed event; a quiet run must still report")
	}
	if ev.Data["status"] != "ok" || ev.Data["acknowledgement"] != "Morning setup: all five apps placed." {
		t.Errorf("event = %v; quiet suppresses speech, never the record", ev.Data)
	}
	if len(h.provider.Requests) != 0 {
		t.Errorf("a scheduled routine made %d provider calls", len(h.provider.Requests))
	}
}

// TestScheduledFireWithAnnounceSpeaks: announce = true opts the summary back
// into speech — the same sentence the spoken phrase would produce.
func TestScheduledFireWithAnnounceSpeaks(t *testing.T) {
	runner := &fakeRoutines{summary: "Morning setup: all five apps placed."}
	h := newRoutineHarness(t, runner, nil)

	fireScheduled(t, h, true, "morning setup")

	if n := h.tts.Speaks(); n == 0 {
		t.Fatal("announce = true spoke nothing")
	}
	if h.tts.Last().Text != "Morning setup: all five apps placed." {
		t.Errorf("spoken = %q, want the run's summary", h.tts.Last().Text)
	}
}

// TestScheduledSessionRefusesWhileActive: a clockfire never interrupts. A
// spoken activation cancels whatever is in flight — interruption must feel
// instant — but nobody is waiting on a timer, so the timer yields.
func TestScheduledSessionRefusesWhileActive(t *testing.T) {
	runner := &fakeRoutines{summary: "unused"}
	h := newRoutineHarness(t, runner, nil)

	id, err := h.engine.StartSession()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.StartScheduledSession(false); err == nil {
		t.Fatal("a clockfire started over an active session; it must refuse, not interrupt")
	}
	if _, current := h.engine.State(); current != id {
		t.Errorf("active session = %q after the refused clockfire, want %q untouched", current, id)
	}
}

// TestScheduledRunIsAbortedByStop: a schedule-fired run is an ordinary
// session, so the one cancel mechanism reaches it — "stop" aborts it exactly
// like a spoken one, mid-run.
func TestScheduledRunIsAbortedByStop(t *testing.T) {
	runner := &fakeRoutines{block: true, started: make(chan struct{})}
	h := newRoutineHarness(t, runner, nil)

	if _, err := h.engine.StartScheduledSession(false); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("morning setup"); err != nil {
		t.Fatal(err)
	}
	<-runner.started
	if err := h.engine.Cancel(); err != nil {
		t.Fatal(err)
	}
	h.collectUntil(t, "session.cancelled")
	h.waitIdle(t)

	runner.mu.Lock()
	ctx := runner.lastCtx
	runner.mu.Unlock()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("cancelling did not cancel the schedule-fired run's context")
	}
}

// TestScheduledFireNeverSpeaksAConfirmation is belt and braces on the quiet
// contract: the daemon refuses ask-tier clockfires before a session exists,
// but should one ever reach the gate, the question appears on the overlay and
// the decline path runs — silently.
func TestScheduledFireNeverSpeaksAConfirmation(t *testing.T) {
	runner := &fakeRoutines{summary: "never"}
	h := newRoutineHarness(t, runner, &tools.PolicyConfig{
		Tools: map[string]tools.PolicyDecision{tools.RoutineToolName: tools.PolicyAsk},
	})

	if _, err := h.engine.StartScheduledSession(false); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("morning setup"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "tool.confirmation_required")
	if n := h.tts.Speaks(); n != 0 {
		t.Fatalf("a quiet session spoke its confirmation question %d times", n)
	}
	if err := h.engine.Confirm(false); err != nil {
		t.Fatal(err)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if len(runner.ran()) != 0 {
		t.Fatalf("a declined clockfire ran: %v", runner.ran())
	}
	if n := h.tts.Speaks(); n != 0 {
		t.Fatalf("the declined quiet run spoke %d sentences, want silence", n)
	}
}
