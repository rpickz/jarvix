package session

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/rpickz/jarvix/internal/intent"
)

// The engine half of one-shot reminders (#141): a routed reminder phrase
// reaches the reminder runner through the IntentRunner seam and its answer is
// spoken as the acknowledgement — no provider call, no argv, no shell, and
// above all NO confirmation card (pinned below) — and a daemon with no
// reminder-capable runner refuses in words rather than shrugging.

// fakeReminderRunner is an intent.Runner that also answers reminder actions,
// standing in for internal/reminders.IntentRunner.
type fakeReminderRunner struct {
	intent.FakeRunner
	mu      sync.Mutex
	matches []intent.Match
	spoken  string
	err     error
}

func (f *fakeReminderRunner) RunReminder(_ context.Context, m intent.Match) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.matches = append(f.matches, m)
	return f.spoken, f.err
}

func (f *fakeReminderRunner) seen() []intent.Match {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]intent.Match(nil), f.matches...)
}

// TestReminderPhraseSpeaksWithNoConfirmationCard is the pinned no-ceremony
// test: creating a reminder by voice routes deterministically, never reaches
// the model or the tool gate, and raises no confirmation of any kind — the
// spoken sentence IS the authorisation (ADR 0046). If this test starts
// seeing tool.confirmation_required, the feature has regrown the config-
// write card it exists to remove.
func TestReminderPhraseSpeaksWithNoConfirmationCard(t *testing.T) {
	runner := &fakeReminderRunner{spoken: "Reminding you at three this afternoon: call the pharmacy."}
	h := newFocusHarness(t, runner)

	seen := sayFocus(t, h, "remind me at three to call the pharmacy")

	if len(h.provider.Requests) != 0 {
		t.Fatalf("the provider was called %d times for a reminder intent", len(h.provider.Requests))
	}
	for _, forbidden := range []string{
		"tool.confirmation_required", "tool.confirmation_deadline",
		"tool.started", "tool.denied",
	} {
		if _, ok := seen[forbidden]; ok {
			t.Errorf("creating a reminder raised %s — the ceremony this feature removes", forbidden)
		}
	}
	matches := runner.seen()
	if len(matches) != 1 || matches[0].Reminder != intent.ReminderSet {
		t.Fatalf("runner saw %+v", matches)
	}
	if matches[0].ReminderWhen != "at three" || matches[0].ReminderText != "call the pharmacy" {
		t.Errorf("match carried when=%q text=%q", matches[0].ReminderWhen, matches[0].ReminderText)
	}
	if runner.Argv() != nil || runner.Shell() != nil {
		t.Errorf("a reminder phrase reached argv=%v shell=%v", runner.Argv(), runner.Shell())
	}
	ev, ok := seen["intent.executed"]
	if !ok {
		t.Fatal("no intent.executed event")
	}
	if ev.Data["intent"] != "reminder.set" || ev.Data["source"] != "reminder" || ev.Data["status"] != "ok" {
		t.Errorf("event data = %v", ev.Data)
	}
	// The confirmation — which reading of "three" won — is the spoken ack.
	if h.tts.Last().Text != "Reminding you at three this afternoon: call the pharmacy." {
		t.Errorf("spoken confirmation = %q", h.tts.Last().Text)
	}
}

func TestReminderRefusalIsOneSpokenSentence(t *testing.T) {
	runner := &fakeReminderRunner{err: errors.New("more than one reminder matches \"call\"")}
	h := newFocusHarness(t, runner)

	seen := sayFocus(t, h, "cancel the call reminder")

	ev := seen["intent.executed"]
	if ev.Data["status"] != "failed" {
		t.Errorf("event data = %v", ev.Data)
	}
	if h.tts.Last().Text != "Sorry, more than one reminder matches \"call\"." {
		t.Errorf("spoken refusal = %q", h.tts.Last().Text)
	}
}

func TestReminderWithoutARunnerRefusesInWords(t *testing.T) {
	// A bare FakeRunner has no RunReminder: the honest refusal, never a
	// silent success or a stuck session.
	h := newFocusHarness(t, &intent.FakeRunner{})

	seen := sayFocus(t, h, "what reminders do i have")

	ev := seen["intent.executed"]
	if ev.Data["status"] != "failed" {
		t.Errorf("event data = %v", ev.Data)
	}
	if h.tts.Last().Text != "Sorry, reminders are not available on this daemon." {
		t.Errorf("spoken refusal = %q", h.tts.Last().Text)
	}
}
