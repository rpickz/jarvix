package desktop

import (
	"context"
	"errors"
	"testing"
)

// Contract tests for FakeNotifier — the double the daemon's notification
// tests rely on for click-through behaviour.

func TestFakeNotifierRecordsAndReturnsScriptedClick(t *testing.T) {
	f := &FakeNotifier{InvokeAction: DefaultActionID}
	n := Notification{Summary: "Jarvix", Body: "answer ready",
		Actions: []Action{{ID: DefaultActionID, Label: "Open"}}}
	invoked, err := f.Send(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	if invoked != DefaultActionID {
		t.Errorf("invoked = %q", invoked)
	}
	sent := f.Sent()
	if len(sent) != 1 || sent[0].Summary != "Jarvix" || sent[0].Body != "answer ready" {
		t.Errorf("sent = %+v", sent)
	}
}

func TestFakeNotifierScriptedFailureStillRecords(t *testing.T) {
	f := &FakeNotifier{Err: errors.New("no daemon")}
	if _, err := f.Send(context.Background(), Notification{Summary: "x"}); !errors.Is(err, f.Err) {
		t.Errorf("err = %v", err)
	}
	// The attempt is recorded even when delivery fails, so tests can assert
	// on the log-only degradation path.
	if len(f.Sent()) != 1 {
		t.Errorf("sent = %+v", f.Sent())
	}
}

func TestFakeNotifierDismissalIsEmptyAction(t *testing.T) {
	f := &FakeNotifier{} // InvokeAction "" simulates dismissal/expiry
	invoked, err := f.Send(context.Background(), Notification{Summary: "x"})
	if err != nil || invoked != "" {
		t.Errorf("invoked = %q, err = %v", invoked, err)
	}
}
