package focus

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/intent"
)

// The runner: every routed focus action lands on the service, and the argv /
// shell halves pass through to the fallback untouched — dressing the service
// as the intent runner must never change what ordinary intents do.

type recordingRunner struct {
	argv  [][]string
	shell []string
}

func (r *recordingRunner) Run(_ context.Context, argv []string) error {
	r.argv = append(r.argv, argv)
	return nil
}

func (r *recordingRunner) RunShell(_ context.Context, command string) error {
	r.shell = append(r.shell, command)
	return nil
}

func TestRunFocusDrivesTheServiceEndToEnd(t *testing.T) {
	clock := newTestClock()
	s := newStoreService(t, clock)
	r := &IntentRunner{Service: s, Log: testLogger(t)}
	ctx := context.Background()

	say := func(utterance string) (string, error) {
		t.Helper()
		router, err := intent.New(intent.Options{})
		if err != nil {
			t.Fatal(err)
		}
		m, ok := router.Match(utterance)
		if !ok {
			t.Fatalf("%q did not route", utterance)
		}
		return r.RunFocus(ctx, m)
	}

	// The voice round trip: create, park, list, timebox, status, end —
	// each utterance through the real router, each ack from the record.
	if ack, err := say("new thread called the ci refactor"); err != nil ||
		ack != "New thread: the ci refactor." {
		t.Fatalf("create = %q, %v", ack, err)
	}
	if ack, err := say("later reply to dan"); err != nil || ack != "Parked." {
		t.Fatalf("park = %q, %v", ack, err)
	}
	if ack, err := say("what did i park"); err != nil ||
		ack != "One parked on the ci refactor: reply to dan." {
		t.Fatalf("parked = %q, %v", ack, err)
	}
	if ack, err := say("focus on the ci refactor for 25 minutes"); err != nil ||
		ack != "Focusing on the ci refactor for twenty-five minutes." {
		t.Fatalf("timebox = %q, %v", ack, err)
	}
	if ack, err := say("focus session update"); err != nil ||
		!strings.Contains(ack, "left on the ci refactor") {
		t.Fatalf("tick = %q, %v", ack, err)
	}
	if ack, err := say("end the focus session"); err != nil ||
		!strings.HasPrefix(ack, "Ended the focus session") {
		t.Fatalf("end session = %q, %v", ack, err)
	}
	if ack, err := say("where am i on everything"); err != nil ||
		!strings.HasPrefix(ack, "You're on the ci refactor") {
		t.Fatalf("status = %q, %v", ack, err)
	}
	if ack, err := say("end this thread"); err != nil ||
		!strings.HasPrefix(ack, "Ended the ci refactor.") {
		t.Fatalf("end = %q, %v", ack, err)
	}

	// A refusal is a sentence for the ear, not a stuck session: the timebox
	// deliberately does not create threads (no automatic creation, per the
	// ticket's out-of-scope).
	if _, err := say("focus on the launch for 25 minutes"); !errors.Is(err, ErrUnknownThread) {
		t.Errorf("timebox on an unknown thread err = %v", err)
	}
}

func TestRunnerPassesOrdinaryIntentsThrough(t *testing.T) {
	rec := &recordingRunner{}
	r := &IntentRunner{Fallback: rec}
	if err := r.Run(context.Background(), []string{"wpctl", "set-mute"}); err != nil {
		t.Fatal(err)
	}
	if err := r.RunShell(context.Background(), "notify-send hi"); err != nil {
		t.Fatal(err)
	}
	if len(rec.argv) != 1 || len(rec.shell) != 1 {
		t.Errorf("fallback saw argv=%v shell=%v", rec.argv, rec.shell)
	}
}
