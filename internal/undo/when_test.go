package undo

import (
	"testing"
	"time"
)

// How the account says when something happened (#210).
//
// The scale itself is a judgement call and is not what these pin. What they
// pin is the pair of edges that are not: a row whose time did not survive a
// hand-edit, and a row whose time is in the future, both of which have to say
// what was actually found rather than run arithmetic on a value that means
// something else. "56 years ago" for a missing timestamp and "just now" for a
// clock that has been put back are both confident wrongness of exactly the
// kind this package exists to remove.

func TestAgoSpeaksTheScaleAPersonScansBy(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		at   time.Time
		want string
	}{
		{"seconds", now.Add(-30 * time.Second), "just now"},
		{"one minute", now.Add(-90 * time.Second), "a minute ago"},
		{"minutes", now.Add(-4 * time.Minute), "4 minutes ago"},
		{"one hour", now.Add(-90 * time.Minute), "an hour ago"},
		{"hours", now.Add(-5 * time.Hour), "5 hours ago"},
		{"yesterday", now.Add(-30 * time.Hour), "yesterday"},
		{"days", now.Add(-3 * 24 * time.Hour), "3 days ago"},
		{"one week", now.Add(-9 * 24 * time.Hour), "last week"},
		{"weeks", now.Add(-30 * 24 * time.Hour), "4 weeks ago"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Ago(now, tc.at); got != tc.want {
				t.Errorf("Ago = %q, want %q", got, tc.want)
			}
		})
	}
}

// A record whose time is missing says so. The account is a hand-editable file
// and a stanza can lose a field; dating it from the zero time would put "56
// years ago" on a row that happened this morning.
func TestAgoRefusesToDateARecordThatHasNoTime(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	got := Ago(now, time.Time{})
	if got != "at a time I no longer have" {
		t.Errorf("Ago on a zero time = %q, want the honest absence", got)
	}
}

// A record dated in the future says so too. It happens — a restored backup, a
// hand-edit, a clock put back — and "just now" would be a reassurance nobody
// checked.
func TestAgoSaysSoWhenTheClockHasMoved(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	got := Ago(now, now.Add(time.Hour))
	if got == "just now" {
		t.Fatal("an action dated in the future is reported as having just happened")
	}
	if got != "later than now, by this machine's clock" {
		t.Errorf("Ago on a future time = %q, want the sentence that says what was found", got)
	}
}

// The clock the phrasing is measured against travels with the rows, so a
// surface reading the account over a socket never subtracts its own machine's
// idea of the time from another's.
func TestTheListingCarriesTheStoresOwnClock(t *testing.T) {
	at := time.Date(2026, 8, 29, 11, 56, 0, 0, time.UTC)
	s := NewStore(t.TempDir()+"/undo.toml", StoreOptions{
		Now: func() time.Time { return at },
	}, nil)
	if _, err := s.Append(Action{Tool: "shell.run", Summary: "ran a command",
		Restore: OneWay("shell.run")}); err != nil {
		t.Fatal(err)
	}
	view := s.List()
	if !view.Now.Equal(at) {
		t.Fatalf("View.Now = %v, want the store's injected clock %v", view.Now, at)
	}
	if got := Ago(view.Now, view.Records[0].At); got != "just now" {
		t.Errorf("an action written at the view's own instant reads as %q", got)
	}
}
