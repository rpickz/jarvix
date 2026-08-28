package tools

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/reminders"
)

// The model's reminder verbs (#141): set confirms exactly when the reminder
// will fire, an unreadable time comes back as a relayable hint — never an
// infrastructure error, never a guessed time — and cancel hands ambiguity
// back as candidates.

func newReminderTools(t *testing.T) *Reminders {
	t.Helper()
	svc := reminders.NewService(filepath.Join(t.TempDir(), "reminders.toml"),
		reminders.Options{Now: func() time.Time {
			return time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)
		}}, slog.New(slog.DiscardHandler))
	return NewReminders(RemindersOptions{Service: svc, Log: slog.New(slog.DiscardHandler)})
}

func executeReminder(t *testing.T, fam *Reminders, name string, args string) string {
	t.Helper()
	for _, tool := range fam.Tools() {
		if tool.Name() != name {
			continue
		}
		out, err := tool.Execute(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out
	}
	t.Fatalf("no tool named %s", name)
	return ""
}

func TestReminderSetToolConfirmsWhichReadingWon(t *testing.T) {
	fam := newReminderTools(t)
	out := executeReminder(t, fam, ReminderSetToolName,
		`{"when": "at three", "text": "call the pharmacy"}`)
	if !strings.Contains(out, "Reminding you at three this afternoon: call the pharmacy.") {
		t.Errorf("set result = %q", out)
	}
	if !strings.Contains(out, "Confirm to the user") {
		t.Errorf("set result does not steer the confirmation: %q", out)
	}
}

func TestReminderSetToolRelaysTheUnparseableHint(t *testing.T) {
	fam := newReminderTools(t)
	out := executeReminder(t, fam, ReminderSetToolName,
		`{"when": "when the kettle boils", "text": "get it"}`)
	if !strings.HasPrefix(out, "error: ") || !strings.Contains(out, "couldn't make out the time") {
		t.Errorf("hint result = %q", out)
	}
	if !strings.Contains(out, "in twenty minutes") {
		t.Errorf("the hint lost its examples: %q", out)
	}
	// Nothing was stored on the way to the refusal.
	if out := executeReminder(t, fam, ReminderListToolName, `{}`); out != "No reminders are set." {
		t.Errorf("list after refusal = %q", out)
	}
}

func TestReminderListAndCancelRoundTrip(t *testing.T) {
	fam := newReminderTools(t)
	executeReminder(t, fam, ReminderSetToolName, `{"when": "at three", "text": "call the pharmacy"}`)
	executeReminder(t, fam, ReminderSetToolName, `{"when": "at four", "text": "call the dentist"}`)

	listing := executeReminder(t, fam, ReminderListToolName, `{}`)
	if !strings.Contains(listing, "[r1] call the pharmacy — at three this afternoon") {
		t.Errorf("listing = %q", listing)
	}

	// Ambiguity comes back as candidates for the model to ask about.
	out := executeReminder(t, fam, ReminderCancelToolName, `{"reminder": "call"}`)
	if !strings.HasPrefix(out, "error: ") || !strings.Contains(out, "which one") {
		t.Errorf("ambiguous cancel = %q", out)
	}
	// An id pins it.
	out = executeReminder(t, fam, ReminderCancelToolName, `{"reminder": "r1"}`)
	if !strings.Contains(out, "Cancelled the reminder: call the pharmacy.") {
		t.Errorf("cancel = %q", out)
	}
}
