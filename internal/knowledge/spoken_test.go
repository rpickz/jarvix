package knowledge

import (
	"testing"
	"time"
)

// The spoken ages are what the user actually hears about freshness, so the
// boundaries are pinned exactly: a mutated threshold moves a sentence.
func TestSpokenAge(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		ago  time.Duration
		want string
	}{
		{0, "just now"},
		{59 * time.Second, "just now"},
		{time.Minute, "a minute ago"},
		{119 * time.Second, "a minute ago"},
		{2 * time.Minute, "two minutes ago"},
		{4 * time.Minute, "four minutes ago"},
		{25 * time.Minute, "twenty-five minutes ago"},
		{59 * time.Minute, "fifty-nine minutes ago"},
		{time.Hour, "an hour ago"},
		{119 * time.Minute, "an hour ago"},
		{2 * time.Hour, "two hours ago"},
		{23 * time.Hour, "twenty-three hours ago"},
		{24 * time.Hour, "yesterday"},
		{47 * time.Hour, "yesterday"},
		{3 * 24 * time.Hour, "three days ago"},
		{8 * 24 * time.Hour, "a week ago"},
		{21 * 24 * time.Hour, "three weeks ago"},
	} {
		if got := SpokenAge(now, now.Add(-tc.ago)); got != tc.want {
			t.Errorf("SpokenAge(%v ago) = %q, want %q", tc.ago, got, tc.want)
		}
	}
}
