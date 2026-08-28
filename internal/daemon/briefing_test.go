package daemon

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ipc"
)

// The daemon half of the return briefing (#150, ADR 0050): briefing.get over
// the real socket, the source adapters reading only what Jarvix already has,
// and the activity row that records a briefing was given without recording a
// word of it.

// briefingView is the wire shape briefing.get serves.
type briefingView struct {
	Disabled   bool   `json:"disabled"`
	NoAbsence  bool   `json:"no_absence"`
	Empty      bool   `json:"empty"`
	Truncated  bool   `json:"truncated"`
	Headline   string `json:"headline"`
	Spoken     string `json:"spoken"`
	Since      string `json:"since"`
	AwaySpoken string `json:"away_spoken"`
	Sections   []struct {
		Title string   `json:"title"`
		Lines []string `json:"lines"`
	} `json:"sections"`
}

// wasAway drives the service's own seam to the state a night produces: one
// sighting long ago, one sighting now. It is exactly what the engine does at
// the top of a user-started exchange, so nothing here fakes a state the
// daemon could not reach.
func wasAway(h *focusHarness, hours int) {
	h.d.briefing.Arrive(time.Now().Add(-time.Duration(hours) * time.Hour))
	h.d.briefing.Arrive(time.Now())
}

// plantOvernightReminders writes the reminder store by hand into the state a
// night leaves behind: one that fired at three in the morning, one that came
// due while nobody was there. Hand-editing the file is the store's own
// documented path (it re-reads on mtime change), which is what lets a test
// reach a past-dated state the "remind me in a minute" parser cannot make.
func plantOvernightReminders(t *testing.T, h *focusHarness, firedText, dueText string) {
	t.Helper()
	fired := time.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339)
	due := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	content := "version = 1\nnext_id = 3\n\n" +
		"[[reminder]]\nid = \"r2\"\ntext = \"" + dueText + "\"\ndue = " + due +
		"\ncreated = " + fired + "\n\n" +
		"[[fired]]\nid = \"r1\"\ntext = \"" + firedText + "\"\ndue = " + fired +
		"\nat = " + fired + "\noutcome = \"fired\"\n"
	if err := os.WriteFile(h.d.reminders.Path(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func getBriefing(t *testing.T, client *ipc.Client) briefingView {
	t.Helper()
	var view briefingView
	if err := client.Call("briefing.get", nil, &view); err != nil {
		t.Fatal(err)
	}
	return view
}

// TestBriefingGetSaysThereIsNoAbsenceYet. The window's button must have an
// honest answer before anyone has been anywhere.
func TestBriefingGetSaysThereIsNoAbsenceYet(t *testing.T) {
	h := startFocusDaemon(t)
	client := dialDaemon(t, h.socket)

	view := getBriefing(t, client)
	if !view.NoAbsence {
		t.Errorf("briefing.get on a fresh daemon = %+v, want no_absence", view)
	}
	if view.Spoken != "" || len(view.Sections) != 0 {
		t.Errorf("a non-briefing carried content: %+v", view)
	}
}

// TestBriefingGetSaysNothingHappened is the honesty criterion at the wire:
// an absence with nothing in it is reported as nothing, never manufactured
// into a report.
func TestBriefingGetSaysNothingHappened(t *testing.T) {
	h := startFocusDaemon(t)
	client := dialDaemon(t, h.socket)
	wasAway(h, 12)

	view := getBriefing(t, client)
	if !view.Empty {
		t.Errorf("briefing.get = %+v, want empty", view)
	}
	if !strings.HasPrefix(view.Spoken, "Nothing while you were away") {
		t.Errorf("spoken = %q", view.Spoken)
	}
	if view.AwaySpoken == "" || view.Since == "" {
		t.Errorf("the absence is not disclosed: %+v", view)
	}
	if len(view.Sections) != 0 {
		t.Errorf("an empty briefing grew sections: %+v", view.Sections)
	}
}

// TestBriefingReadsTheSourcesJarvixAlreadyHas drives the real adapters: a
// reminder owed now, a reminder that fired while away, and a focus thread —
// each from the store the feature already owns, each landing in the right
// section, in the ticket's order.
func TestBriefingReadsTheSourcesJarvixAlreadyHas(t *testing.T) {
	h := startFocusDaemon(t)
	h.provider.Response = "One thing wants you and one finished."
	client := dialDaemon(t, h.socket)

	if _, _, err := h.d.focus.Create(t.Context(), "the ci refactor", 0); err != nil {
		t.Fatal(err)
	}
	plantOvernightReminders(t, h, "call the pharmacy", "file the expenses")
	wasAway(h, 12)

	view := getBriefing(t, client)
	if view.Empty || view.NoAbsence {
		t.Fatalf("briefing.get = %+v", view)
	}
	titles := make([]string, len(view.Sections))
	for i, s := range view.Sections {
		titles[i] = s.Title
	}
	if strings.Join(titles, "|") != "Waiting for you|Finished|Housekeeping" {
		t.Errorf("sections = %v, want awaiting then completed then housekeeping", titles)
	}
	if !strings.Contains(view.Sections[0].Lines[0], "file the expenses") {
		t.Errorf("the owed reminder is not in the waiting section: %v", view.Sections[0].Lines)
	}
	if !strings.Contains(view.Sections[1].Lines[0], "call the pharmacy") {
		t.Errorf("the fired reminder is not in the finished section: %v", view.Sections[1].Lines)
	}
	if !strings.Contains(view.Sections[2].Lines[0], "the ci refactor") {
		t.Errorf("the focus thread is not in the housekeeping section: %v", view.Sections[2].Lines)
	}
	if !strings.HasPrefix(view.Spoken, "One thing wants you and one finished.") {
		t.Errorf("the headline the provider worded was not used: %q", view.Spoken)
	}
}

// TestBriefingActivityRowNeverCarriesItsContent is the leak-salted criterion
// end to end (#147's shape): a briefing composed from a salted record leaves
// a row saying one was given, and the salt reaches neither the row nor any
// other field the feed serves.
func TestBriefingActivityRowNeverCarriesItsContent(t *testing.T) {
	const salt = "SECRET-PHARMACY-CODE-4312"
	h := startFocusDaemon(t)
	h.provider.Response = "One thing wants you."
	client := dialDaemon(t, h.socket)

	plantOvernightReminders(t, h, "something dull", salt)
	wasAway(h, 12)

	view := getBriefing(t, client)
	if !strings.Contains(view.Spoken, salt) {
		t.Fatalf("the briefing did not carry the reminder at all: %q", view.Spoken)
	}
	row := waitActivityRow(t, client, "Return briefing given")
	for key, value := range row {
		if s, ok := value.(string); ok && strings.Contains(s, salt) {
			t.Errorf("the activity row field %s leaked the briefing: %q", key, s)
		}
	}
	// And the row still says something: a record that reports nothing is a
	// hole, not privacy.
	if detail, _ := row["detail"].(string); !strings.Contains(detail, "opened in the window") ||
		!strings.Contains(detail, "line") {
		t.Errorf("row detail = %q, want who asked and how much was said", detail)
	}

	rows := activityRowsOf(t, client)
	for i, r := range rows {
		for key, value := range r {
			if s, ok := value.(string); ok && strings.Contains(s, salt) {
				t.Errorf("row %d field %s leaked the briefing: %q", i, key, s)
			}
		}
	}
}

// TestBriefingRespectsTheOffSwitch: briefing.enabled = false means nothing is
// prepared, offered, or scheduled — including through the window's own verb.
func TestBriefingRespectsTheOffSwitch(t *testing.T) {
	cfg := testConfig()
	cfg.Briefing.Enabled = false
	h := startFocusDaemonWith(t, cfg)
	client := dialDaemon(t, h.socket)

	plantOvernightReminders(t, h, "call the pharmacy", "file the expenses")
	wasAway(h, 12)

	view := getBriefing(t, client)
	if !view.Disabled {
		t.Errorf("briefing.get with the feature off = %+v, want disabled", view)
	}
	if view.Spoken != "" || len(view.Sections) != 0 {
		t.Errorf("a disabled briefing composed content anyway: %+v", view)
	}
}
