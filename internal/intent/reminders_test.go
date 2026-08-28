package intent

import "testing"

// The reminder grammar (#141, ADR 0046): which utterances the table claims,
// how the time words and the reminder's words split, and — just as load-
// bearing — what falls through to the model, where the reminder.set tool
// lives.

func reminderRouter(t *testing.T) *Router {
	t.Helper()
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReminderSetMatches(t *testing.T) {
	r := reminderRouter(t)
	cases := []struct {
		in       string
		wantWhen string
		wantText string
	}{
		// The acceptance criteria's own phrasings.
		{"remind me at three to call the pharmacy", "at three", "call the pharmacy"},
		{"remind me at 15:00 to call the pharmacy", "at 15 00", "call the pharmacy"},
		{"remind me in twenty minutes to stretch", "in twenty minutes", "stretch"},
		// Text first, time trailing.
		{"remind me to call the pharmacy at three", "at three", "call the pharmacy"},
		{"remind me to stretch in an hour and a half", "in an hour and a half", "stretch"},
		{"remind me to check the oven tomorrow at nine", "tomorrow at nine", "check the oven"},
		// "that" for statements rather than errands.
		{"remind me at six that the oven is on", "at six", "the oven is on"},
		// The split is anchored by what parses: "at school" is the errand's,
		// "at three" is the clock's.
		{"remind me to pick up the kids at school at three", "at three", "pick up the kids at school"},
		// A meridiem rides the when slot.
		{"remind me at three pm to call the pharmacy", "at three pm", "call the pharmacy"},
		// Punctuation and case are STT noise, not meaning.
		{"Remind me at three to call the pharmacy.", "at three", "call the pharmacy"},
	}
	for _, tc := range cases {
		m, ok := r.Match(tc.in)
		if !ok {
			t.Errorf("Match(%q) missed", tc.in)
			continue
		}
		if m.Name != "reminder.set" || m.Reminder != ReminderSet {
			t.Errorf("Match(%q) = %q/%q", tc.in, m.Name, m.Reminder)
			continue
		}
		if m.ReminderWhen != tc.wantWhen || m.ReminderText != tc.wantText {
			t.Errorf("Match(%q): when %q text %q, want %q / %q",
				tc.in, m.ReminderWhen, m.ReminderText, tc.wantWhen, tc.wantText)
		}
		if m.FocusText != "" {
			t.Errorf("Match(%q) leaked text into FocusText %q", tc.in, m.FocusText)
		}
	}
}

// TestReminderMissesBelongToTheModel: a phrasing the table cannot claim with
// certainty is a miss, and the model's reminder.set tool answers it.
func TestReminderMissesBelongToTheModel(t *testing.T) {
	r := reminderRouter(t)
	for _, in := range []string{
		"remind me to stretch",                          // no time at all
		"remind me at half past three to stretch",       // half-past is not in the table
		"remind me when the kettle boils to get it",     // not a clock
		"remind me at some point to call dan",           //
		"remind me tomorrow to call dan",                // a day is not a moment
		"remind me to call the pharmacy about the meds", // still no time
		"can you remind me at three to call",            // leading words break the literal lead
	} {
		if m, ok := r.Match(in); ok {
			t.Errorf("Match(%q) = %+v, want a miss for the model", in, m)
		}
	}
}

func TestReminderFixedPhrases(t *testing.T) {
	r := reminderRouter(t)
	cases := []struct {
		in   string
		want ReminderAction
	}{
		{"what reminders do i have", ReminderList},
		{"what are my reminders", ReminderList},
		{"list my reminders", ReminderList},
		{"do i have any reminders", ReminderList},
		{"what reminders fired today", ReminderHistory},
		{"what fired today", ReminderHistory},
		{ReminderCheckPhrase, ReminderDue},
	}
	for _, tc := range cases {
		m, ok := r.Match(tc.in)
		if !ok || m.Reminder != tc.want {
			t.Errorf("Match(%q) = %+v ok=%v, want %q", tc.in, m, ok, tc.want)
		}
	}
}

func TestReminderCancelMatches(t *testing.T) {
	r := reminderRouter(t)
	cases := []struct {
		in       string
		wantText string
	}{
		{"cancel the pharmacy reminder", "pharmacy"},
		{"cancel my stretch reminder", "stretch"},
		{"cancel the reminder to call the pharmacy", "call the pharmacy"},
		{"cancel the reminder about the oven", "the oven"},
	}
	for _, tc := range cases {
		m, ok := r.Match(tc.in)
		if !ok || m.Reminder != ReminderCancel || m.ReminderText != tc.wantText {
			t.Errorf("Match(%q) = %+v ok=%v, want cancel %q", tc.in, m, ok, tc.wantText)
		}
	}
}

// TestFocusCheckInStaysAFocusPhrase pins the neighbour: the focus grammar's
// own "remind me …" phrase compiled with the built-ins and wins its exact
// words — the reminder slots never eat a sibling's sentence.
func TestFocusCheckInStaysAFocusPhrase(t *testing.T) {
	r := reminderRouter(t)
	m, ok := r.Match("remind me where i am every 45 minutes")
	if !ok || m.Focus != FocusRemind || m.Slot != 45 {
		t.Fatalf("Match = %+v ok=%v, want focus.remind slot 45", m, ok)
	}
	if m.Reminder != ReminderNone {
		t.Errorf("the focus phrase carried a reminder action: %+v", m)
	}
}

// TestReminderPhrasesAreOwnedLiterals: the fixed phrases sit in the collision
// set, so a routine claiming one is a config error naming both owners.
func TestReminderPhrasesAreOwnedLiterals(t *testing.T) {
	_, err := New(Options{Routines: []RoutinePhrases{{
		Name: "checker", Phrases: []string{"reminder check"},
	}}})
	if err == nil {
		t.Fatal("a routine claimed the reminder.due phrase without an error")
	}
}

// TestReminderSetIsBoundedFuzz: no matched set utterance may put words
// anywhere but the two reminder slots, whatever the split.
func TestReminderSetKeepsWordsOutOfCommands(t *testing.T) {
	r := reminderRouter(t)
	m, ok := r.Match("remind me at nine oh five to run the backup script now please ok")
	if !ok {
		t.Fatal("missed")
	}
	if len(m.Argv) != 0 || m.Command != "" || m.Program != "" {
		t.Fatalf("a reminder match carried something executable: %+v", m)
	}
}
