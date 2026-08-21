package session

import (
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/tools"
)

// The conversation window's confirmation card (issue #76) derives its
// countdown from the daemon's actual deadline, never from a hardcoded 30.
// These tests pin the two seams that makes possible: the
// tool.confirmation_deadline event published when the clock starts, and the
// PendingConfirmation snapshot a window opened mid-wait renders from.

// deadlineMillis reads the deadline_ms an event or snapshot carried. The bus
// is in-process, so the value arrives as the int64 it was published as.
func deadlineMillis(t *testing.T, data map[string]any, key string) int64 {
	t.Helper()
	ms, ok := data[key].(int64)
	if !ok {
		t.Fatalf("%s = %v (%T), want int64 milliseconds", key, data[key], data[key])
	}
	return ms
}

// TestConfirmationDeadlineFollowsTheConfiguredTimeout: the deadline event is
// published once the question has been asked, and both its timeout and its
// deadline come from Options.ConfirmTimeout — a window configured for 45
// seconds must never be told 30.
func TestConfirmationDeadlineFollowsTheConfiguredTimeout(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{ConfirmTimeout: 45 * time.Second}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "Understood, nothing was deleted.")

	before := time.Now()
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")

	// Ordering is part of the contract: the question (with the verbatim
	// command) goes out first, the deadline follows when the clock starts.
	// waitFor discards everything before its match, so a deadline event that
	// jumped the queue would leave the second wait hanging and fail the test.
	required := h.waitFor(t, "tool.confirmation_required")
	if required.Data["timeout_sec"] != 45 {
		t.Errorf("confirmation_required timeout_sec = %v, want the configured 45",
			required.Data["timeout_sec"])
	}
	started := h.waitFor(t, "tool.confirmation_deadline")
	if started.Data["timeout_sec"] != 45 {
		t.Errorf("confirmation_deadline timeout_sec = %v, want the configured 45",
			started.Data["timeout_sec"])
	}
	if started.Data["command"] != "rm -rf ./build" {
		t.Errorf("confirmation_deadline command = %v, want the verbatim command",
			started.Data["command"])
	}
	deadline := deadlineMillis(t, started.Data, "deadline_ms")
	low := before.Add(45 * time.Second).UnixMilli()
	high := time.Now().Add(45 * time.Second).UnixMilli()
	if deadline < low || deadline > high {
		t.Errorf("deadline_ms = %d, want between %d and %d (now + configured timeout)",
			deadline, low, high)
	}

	if err := h.engine.Confirm(false); err != nil {
		t.Fatal(err)
	}
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
}

// TestPendingConfirmationIsExposedUntilResolved: the engine answers for the
// wait it is in — question, verbatim command, and deadline — and stops
// answering the moment the confirmation resolves. This is what
// conversation.get serves to a window opened during the wait.
func TestPendingConfirmationIsExposedUntilResolved(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "Build directory removed.")

	if _, ok := h.engine.PendingConfirmation(); ok {
		t.Fatal("nothing should be pending before the gate asks")
	}

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	h.waitFor(t, "tool.confirmation_required")
	// Wait for the clock to start, so the snapshot's deadline is a fact
	// rather than a race with the speaking of the question.
	started := h.waitFor(t, "tool.confirmation_deadline")

	pending, ok := h.engine.PendingConfirmation()
	if !ok {
		t.Fatal("a confirmation is pending; the snapshot must say so")
	}
	if pending.Command != "rm -rf ./build" || pending.Tool != "shell.run" {
		t.Errorf("pending = %+v, want the verbatim command and tool", pending)
	}
	if pending.Summary == "" {
		t.Error("pending confirmation carries no question to restate")
	}
	if pending.Timeout != DefaultConfirmTimeout {
		t.Errorf("pending timeout = %v, want the default %v", pending.Timeout, DefaultConfirmTimeout)
	}
	if got := pending.Deadline.UnixMilli(); got != deadlineMillis(t, started.Data, "deadline_ms") {
		t.Errorf("snapshot deadline %d disagrees with the published deadline event", got)
	}

	if err := h.engine.Confirm(true); err != nil {
		t.Fatal(err)
	}
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
	if _, ok := h.engine.PendingConfirmation(); ok {
		t.Error("resolved confirmations must leave the snapshot; a window would render a stale card")
	}
}
