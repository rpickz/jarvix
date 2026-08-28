package reminders

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The verbs and their sentences: creation always says which reading of an
// ambiguous hour won, listings read soonest first, cancellation is fuzzy but
// never guesses a tie, and refusals are one honest spoken line.

func TestCreateSpeaksWhichReadingWon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	s, _ := newTestService(t, path) // 13:00
	cases := []struct {
		when, text string
		want       string
	}{
		// The acceptance criterion's own sentence: ambiguous "three" at
		// 13:00 resolves to 15:00 and the confirmation says which.
		{"at three", "call the pharmacy", "Reminding you at three this afternoon: call the pharmacy."},
		{"at 15 00", "call the pharmacy", "Reminding you at three this afternoon: call the pharmacy."},
		{"in twenty minutes", "stretch", "Reminding you in twenty minutes: stretch."},
		{"tomorrow at nine", "check the oven", "Reminding you at nine tomorrow morning: check the oven."},
		{"at nine", "wind down", "Reminding you at nine tonight: wind down."},
	}
	for _, tc := range cases {
		got, err := s.Create(tc.when, tc.text)
		if err != nil {
			t.Errorf("Create(%q) error: %v", tc.when, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Create(%q) = %q\nwant                 %q", tc.when, got, tc.want)
		}
	}
}

func TestCreateRefusals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	s, _ := newTestService(t, path)
	if _, err := s.Create("at three", "   "); !errors.Is(err, ErrNoText) {
		t.Errorf("empty text err = %v", err)
	}
	// The unparseable refusal carries the spoken hint — the sentence the
	// model relays when the tool path meets a time the table cannot read.
	_, err := s.Create("at half past whenever", "stretch")
	if !errors.Is(err, ErrBadTime) {
		t.Fatalf("bad time err = %v", err)
	}
	if !strings.Contains(err.Error(), "in twenty minutes") || !strings.Contains(err.Error(), "at 15:00") {
		t.Errorf("the refusal lost its hint: %v", err)
	}
}

func TestCreateRefusesAFullStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	s, _ := newTestService(t, path)
	for range maxPending {
		if _, err := s.Create("at three", "one more"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Create("at three", "past the cap"); !errors.Is(err, ErrStoreFull) {
		t.Errorf("full store err = %v", err)
	}
}

func TestListSpokenSoonestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	s, _ := newTestService(t, path)
	if got := s.ListSpoken(); got != "No reminders set." {
		t.Fatalf("empty listing = %q", got)
	}
	if _, err := s.Create("at three", "call the pharmacy"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("in twenty minutes", "stretch"); err != nil {
		t.Fatal(err)
	}
	got := s.ListSpoken()
	want := "Two reminders: stretch, at one twenty this afternoon; then call the pharmacy, at three this afternoon."
	if got != want {
		t.Errorf("listing = %q\nwant      %q", got, want)
	}
}

func TestListSpokenCapsTheReading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	s, _ := newTestService(t, path)
	for range maxSpokenList + 2 {
		if _, err := s.Create("at three", "one of many"); err != nil {
			t.Fatal(err)
		}
	}
	got := s.ListSpoken()
	if !strings.HasPrefix(got, "Seven reminders: ") || !strings.HasSuffix(got, "; and two more.") {
		t.Errorf("capped listing = %q", got)
	}
}

func TestCancelByWordsAndTheAmbiguityRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	s, _ := newTestService(t, path)
	if _, err := s.Create("at three", "call the pharmacy"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("at four", "call the dentist"); err != nil {
		t.Fatal(err)
	}
	// "call" matches both: a tie is asked about, naming the candidates —
	// never broken by guessing.
	_, err := s.Cancel("call")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ambiguous cancel err = %v", err)
	}
	if !strings.Contains(err.Error(), "call the pharmacy") || !strings.Contains(err.Error(), "call the dentist") {
		t.Errorf("the ambiguity refusal does not name the candidates: %v", err)
	}
	// "pharmacy" pins it; the confirmation names what went.
	got, err := s.Cancel("pharmacy")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Cancelled the reminder: call the pharmacy." {
		t.Errorf("cancel ack = %q", got)
	}
	// Gone from listings, retained in history as cancelled — and "what
	// fired today" does not read a cancellation as a firing.
	if listing := s.ListSpoken(); strings.Contains(listing, "pharmacy") {
		t.Errorf("a cancelled reminder still listed: %q", listing)
	}
	v := s.Snapshot()
	if len(v.History) != 1 || v.History[0].Outcome != OutcomeCancelled {
		t.Fatalf("history = %+v", v.History)
	}
	if got := s.HistorySpoken(); got != "No reminders fired today." {
		t.Errorf("a cancellation counted as a firing: %q", got)
	}
	if _, err := s.Cancel("nothing like this"); !errors.Is(err, ErrUnknownReminder) {
		t.Errorf("unknown cancel err = %v", err)
	}
}

func TestClaimDueMovesFiredIntoHistoryAndListings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	s, clock := newTestService(t, path)
	if _, err := s.Create("in ten minutes", "stretch"); err != nil {
		t.Fatal(err)
	}
	// Not due yet: an early claim takes nothing.
	if spoken, n := s.ClaimDue(); n != 0 || spoken != "No reminders are due." {
		t.Fatalf("early claim = %q, %d", spoken, n)
	}
	clock.advance(11 * time.Minute)
	spoken, n := s.ClaimDue()
	if n != 1 || spoken != "Reminder: stretch." {
		t.Fatalf("claim = %q, %d", spoken, n)
	}
	// Gone from listings; answerable for "what fired today".
	if got := s.ListSpoken(); got != "No reminders set." {
		t.Errorf("a fired reminder still listed: %q", got)
	}
	if got := s.HistorySpoken(); got != "One reminder fired today: stretch, at one ten." {
		t.Errorf("history = %q", got)
	}
	// A second claim finds nothing: fired means fired.
	if _, n := s.ClaimDue(); n != 0 {
		t.Error("a claimed reminder was claimed again")
	}
}

func TestClaimSaysLatePastTheGrace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	s, clock := newTestService(t, path)
	if _, err := s.Create("in ten minutes", "stretch"); err != nil {
		t.Fatal(err)
	}
	clock.advance(30 * time.Minute) // twenty minutes past the moment
	spoken, n := s.ClaimDue()
	if n != 1 || spoken != "Reminder, twenty minutes late: stretch." {
		t.Fatalf("late claim = %q, %d", spoken, n)
	}
	v := s.Snapshot()
	if len(v.History) != 1 || !v.History[0].Late {
		t.Errorf("history did not mark the delay: %+v", v.History)
	}
}

func TestClaimInsideTheGraceSpeaksPlainly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	s, clock := newTestService(t, path)
	if _, err := s.Create("in ten minutes", "stretch"); err != nil {
		t.Fatal(err)
	}
	clock.advance(11 * time.Minute) // one minute past: inside the grace
	if spoken, _ := s.ClaimDue(); spoken != "Reminder: stretch." {
		t.Errorf("in-grace claim = %q", spoken)
	}
}

func TestClaimSpeaksSeveralAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	s, clock := newTestService(t, path)
	if _, err := s.Create("in five minutes", "stretch"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("in six minutes", "hydrate"); err != nil {
		t.Fatal(err)
	}
	clock.advance(7 * time.Minute)
	spoken, n := s.ClaimDue()
	if n != 2 || spoken != "Reminder: stretch. Also: hydrate." {
		t.Errorf("claim = %q, %d", spoken, n)
	}
}
