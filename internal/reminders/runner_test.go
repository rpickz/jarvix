package reminders

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/intent"
)

// The runner: one dispatch per action, refusals as one honest sentence, and
// transparency for the focus runner it wraps.

func TestRunReminderDispatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	svc, clock := newTestService(t, path)
	r := &IntentRunner{Service: svc, Log: testLogger(t)}
	ctx := context.Background()

	spoken, err := r.RunReminder(ctx, intent.Match{
		Reminder: intent.ReminderSet, ReminderWhen: "at three", ReminderText: "call the pharmacy",
	})
	if err != nil || spoken != "Reminding you at three this afternoon: call the pharmacy." {
		t.Fatalf("set = %q, %v", spoken, err)
	}

	spoken, err = r.RunReminder(ctx, intent.Match{Reminder: intent.ReminderList})
	if err != nil || !strings.Contains(spoken, "call the pharmacy") {
		t.Fatalf("list = %q, %v", spoken, err)
	}

	// Nothing due yet; the check phrase answers honestly.
	spoken, err = r.RunReminder(ctx, intent.Match{Reminder: intent.ReminderDue})
	if err != nil || spoken != "No reminders are due." {
		t.Fatalf("due = %q, %v", spoken, err)
	}
	clock.advance(3 * time.Hour)
	spoken, err = r.RunReminder(ctx, intent.Match{Reminder: intent.ReminderDue})
	if err != nil || !strings.HasPrefix(spoken, "Reminder") {
		t.Fatalf("due after the moment = %q, %v", spoken, err)
	}

	spoken, err = r.RunReminder(ctx, intent.Match{Reminder: intent.ReminderHistory})
	if err != nil || !strings.Contains(spoken, "fired today") {
		t.Fatalf("history = %q, %v", spoken, err)
	}

	if _, err := r.RunReminder(ctx, intent.Match{
		Reminder: intent.ReminderCancel, ReminderText: "no such thing",
	}); err == nil {
		t.Fatal("an unknown cancel did not refuse")
	}
}

// TestRunnerForwardsTheChain: the wrapper is transparent for everything that
// is not a reminder — including the focus dispatch it sits in front of.
func TestRunnerForwardsTheChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	svc, _ := newTestService(t, path)
	fb := &fakeChain{}
	r := &IntentRunner{Service: svc, Fallback: fb, Log: testLogger(t)}
	ctx := context.Background()

	if err := r.Run(ctx, []string{"wpctl", "set-mute"}); err != nil || !fb.ran {
		t.Errorf("Run not forwarded: %v", err)
	}
	if err := r.RunShell(ctx, "echo hi"); err != nil || !fb.shelled {
		t.Errorf("RunShell not forwarded: %v", err)
	}
	spoken, err := r.RunFocus(ctx, intent.Match{Focus: intent.FocusStatus})
	if err != nil || spoken != "focus answered" {
		t.Errorf("RunFocus not forwarded: %q, %v", spoken, err)
	}

	// A chain without a focus runner refuses honestly, never silently.
	bare := &IntentRunner{Service: svc, Fallback: &bareChain{}, Log: testLogger(t)}
	if _, err := bare.RunFocus(ctx, intent.Match{Focus: intent.FocusStatus}); err == nil {
		t.Error("a focusless chain did not refuse")
	}
}

type fakeChain struct {
	ran, shelled bool
}

func (f *fakeChain) Run(context.Context, []string) error    { f.ran = true; return nil }
func (f *fakeChain) RunShell(context.Context, string) error { f.shelled = true; return nil }
func (f *fakeChain) RunFocus(context.Context, intent.Match) (string, error) {
	return "focus answered", nil
}

type bareChain struct{}

func (b *bareChain) Run(context.Context, []string) error    { return nil }
func (b *bareChain) RunShell(context.Context, string) error { return nil }
