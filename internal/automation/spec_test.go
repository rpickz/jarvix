package automation

import (
	"strings"
	"testing"
	"time"
)

// TestParseSpecAccepted pins every documented form — the syntax is one
// friendly grammar, validated hard, so each accepted shape is a contract.
func TestParseSpecAccepted(t *testing.T) {
	for _, tc := range []struct {
		in   string
		days [7]bool // Sunday first, time.Weekday order
	}{
		{"08:30", [7]bool{true, true, true, true, true, true, true}},
		{"02:00 daily", [7]bool{true, true, true, true, true, true, true}},
		{"08:30 mon-fri", [7]bool{false, true, true, true, true, true, false}},
		{"08:30 weekdays", [7]bool{false, true, true, true, true, true, false}},
		{"10:00 weekends", [7]bool{true, false, false, false, false, false, true}},
		{"22:15 mon,wed,fri", [7]bool{false, true, false, true, false, true, false}},
		{"23:59 sat", [7]bool{false, false, false, false, false, false, true}},
		// A wrapping range walks forward through the week's end.
		{"07:00 fri-mon", [7]bool{true, true, false, false, false, true, true}},
		// Mixed list-and-range, and a single-digit hour.
		{"9:05 mon-wed,sat", [7]bool{false, true, true, true, false, false, true}},
		{"00:00", [7]bool{true, true, true, true, true, true, true}},
	} {
		spec, err := ParseSpec(tc.in)
		if err != nil {
			t.Errorf("ParseSpec(%q) = %v, want accepted", tc.in, err)
			continue
		}
		if spec.days != tc.days {
			t.Errorf("ParseSpec(%q) days = %v, want %v", tc.in, spec.days, tc.days)
		}
		if spec.String() != strings.TrimSpace(tc.in) {
			t.Errorf("ParseSpec(%q).String() = %q", tc.in, spec.String())
		}
		if spec.IsZero() {
			t.Errorf("ParseSpec(%q).IsZero() = true", tc.in)
		}
	}
}

// TestParseSpecRejected: hard validation with a teaching error — every
// refusal names the offending part and carries a worked example, because the
// message is read in config validation output where it must fix itself.
func TestParseSpecRejected(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string // a fragment the error must contain
	}{
		{"", "schedule is empty"},
		{"   ", "schedule is empty"},
		{"8am", "not a 24-hour time"},
		{"24:00", "not a 24-hour time"},
		{"12:60", "not a 24-hour time"},
		{"12:5", "not a 24-hour time"},
		{"1230", "not a 24-hour time"},
		{"-1:30", "not a 24-hour time"},
		{"08:30 mon-fri extra", "too many parts"},
		{"08:30 monday", `"monday" is not a day`},
		{"08:30 mon-", "is not a day"},
		{"08:30 mon;fri", "is not a day"},
		// Five-field cron is exactly the syntax this feature does not take.
		{"* * * * *", "too many parts"},
	} {
		_, err := ParseSpec(tc.in)
		if err == nil {
			t.Errorf("ParseSpec(%q) accepted, want a refusal", tc.in)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ParseSpec(%q) = %q, want it to contain %q", tc.in, err, tc.want)
		}
		if !strings.Contains(err.Error(), `"08:30`) {
			t.Errorf("ParseSpec(%q) = %q, want a worked example in the message", tc.in, err)
		}
	}
}

// mustSpec parses or fails the test.
func mustSpec(t *testing.T, s string) Spec {
	t.Helper()
	spec, err := ParseSpec(s)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// TestSpecNext pins the next-fire arithmetic, boundaries included: same-day
// later time, same-minute (strictly after), day filtering, and the wrap to
// next week.
func TestSpecNext(t *testing.T) {
	// A Thursday. Fixed zone: the arithmetic is wall-clock, not UTC.
	zone := time.FixedZone("test", 3600)
	thu := time.Date(2026, 8, 20, 12, 0, 0, 0, zone)
	for _, tc := range []struct {
		spec string
		from time.Time
		want time.Time
	}{
		// Later today.
		{"22:15", thu, time.Date(2026, 8, 20, 22, 15, 0, 0, zone)},
		// Already past today: tomorrow.
		{"08:30", thu, time.Date(2026, 8, 21, 8, 30, 0, 0, zone)},
		// Exactly now: strictly after, so next occurrence — never a double
		// fire of the same moment.
		{"12:00", thu, time.Date(2026, 8, 21, 12, 0, 0, 0, zone)},
		// Weekdays only, asked on Friday afternoon: Monday.
		{"08:30 mon-fri", time.Date(2026, 8, 21, 9, 0, 0, 0, zone),
			time.Date(2026, 8, 24, 8, 30, 0, 0, zone)},
		// A single day a week away: the full wrap.
		{"12:00 thu", time.Date(2026, 8, 20, 12, 0, 0, 0, zone),
			time.Date(2026, 8, 27, 12, 0, 0, 0, zone)},
		{"02:00 sat,sun", thu, time.Date(2026, 8, 22, 2, 0, 0, 0, zone)},
	} {
		got := mustSpec(t, tc.spec).Next(tc.from)
		if !got.Equal(tc.want) {
			t.Errorf("Next(%q from %v) = %v, want %v", tc.spec, tc.from, got, tc.want)
		}
	}
}

// TestSpecPrev pins the boot-time comparison's other half: the most recent
// occurrence at or before a moment.
func TestSpecPrev(t *testing.T) {
	zone := time.FixedZone("test", 3600)
	thu := time.Date(2026, 8, 20, 12, 0, 0, 0, zone)
	for _, tc := range []struct {
		spec string
		from time.Time
		want time.Time
	}{
		{"08:30", thu, time.Date(2026, 8, 20, 8, 30, 0, 0, zone)},
		{"22:15", thu, time.Date(2026, 8, 19, 22, 15, 0, 0, zone)},
		// Exactly now counts: a firing due this minute was due.
		{"12:00", thu, thu},
		// Weekdays only, asked on Sunday: Friday's firing.
		{"08:30 mon-fri", time.Date(2026, 8, 23, 12, 0, 0, 0, zone),
			time.Date(2026, 8, 21, 8, 30, 0, 0, zone)},
	} {
		got := mustSpec(t, tc.spec).Prev(tc.from)
		if !got.Equal(tc.want) {
			t.Errorf("Prev(%q from %v) = %v, want %v", tc.spec, tc.from, got, tc.want)
		}
	}
}
