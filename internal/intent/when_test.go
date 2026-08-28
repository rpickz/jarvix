package intent

import (
	"testing"
	"time"
)

// The time-expression table (#141, ADR 0046): what parses, what it means,
// and — separately — what the next-occurrence rule resolves it to and how
// the confirmation says which reading won. Everything is pure functions of
// words and a supplied now; nothing here can sleep or drift.

func TestParseWhenTable(t *testing.T) {
	cases := []struct {
		in   string
		want When
	}{
		// 24-hour, exact. STT punctuation ("15:00") normalizes to "15 00".
		{"at 15:00", When{Hour: 15}},
		{"at 15 00", When{Hour: 15}},
		{"at 0 30", When{Minute: 30}},
		{"at 23 45", When{Hour: 23, Minute: 45}},
		{"at thirteen", When{Hour: 13}},
		{"at twenty three", When{Hour: 23}},
		{"at twenty three thirty", When{Hour: 23, Minute: 30}},

		// 12-hour without a meridiem: ambiguous, resolved by next occurrence.
		{"at three", When{Hour: 3, Ambiguous: true}},
		{"at 3", When{Hour: 3, Ambiguous: true}},
		{"at three thirty", When{Hour: 3, Minute: 30, Ambiguous: true}},
		{"at three 30", When{Hour: 3, Minute: 30, Ambiguous: true}},
		{"at nine oh five", When{Hour: 9, Minute: 5, Ambiguous: true}},
		{"at nine forty five", When{Hour: 9, Minute: 45, Ambiguous: true}},
		{"at twelve", When{Hour: 12, Ambiguous: true}},
		{"at three oclock", When{Hour: 3, Ambiguous: true}},

		// A meridiem pins the reading.
		{"at three pm", When{Hour: 15}},
		{"at three p m", When{Hour: 15}},
		{"at 3pm", When{Hour: 15}},
		{"at 3 p.m.", When{Hour: 15}},
		{"at three am", When{Hour: 3}},
		{"at 7am", When{Hour: 7}},
		{"at twelve pm", When{Hour: 12}},
		{"at twelve am", When{Hour: 0}},
		{"at three in the afternoon", When{Hour: 15}},
		{"at nine in the morning", When{Hour: 9}},
		{"at eight in the evening", When{Hour: 20}},
		{"at nine at night", When{Hour: 21}},
		{"at nine thirty in the morning", When{Hour: 9, Minute: 30}},

		// The named hours.
		{"at noon", When{Hour: 12}},
		{"at midday", When{Hour: 12}},
		{"at midnight", When{Hour: 0}},

		// Tomorrow, leading or trailing.
		{"tomorrow at nine", When{Hour: 9, Ambiguous: true, Tomorrow: true}},
		{"at nine tomorrow", When{Hour: 9, Ambiguous: true, Tomorrow: true}},
		{"tomorrow at 15:00", When{Hour: 15, Tomorrow: true}},
		{"at half past", When{}}, // refused below; placeholder overwritten
	}
	// The last row is a refusal; trim it from the accept table.
	cases = cases[:len(cases)-1]
	for _, tc := range cases {
		got, ok := ParseWhen(tc.in)
		if !ok {
			t.Errorf("ParseWhen(%q) refused; want %+v", tc.in, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseWhen(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestParseWhenRelativeTable(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"in twenty minutes", 20 * time.Minute},
		{"in 20 minutes", 20 * time.Minute},
		{"in a minute", time.Minute},
		{"in one minute", time.Minute},
		{"in forty five minutes", 45 * time.Minute},
		{"in an hour", time.Hour},
		{"in one hour", time.Hour},
		{"in two hours", 2 * time.Hour},
		{"in half an hour", 30 * time.Minute},
		{"in an hour and a half", 90 * time.Minute},
		{"in two and a half hours", 150 * time.Minute},
		{"in two hours and a half", 150 * time.Minute},
		{"in one hour and five minutes", 65 * time.Minute},
		{"in two hours and thirty five minutes", 155 * time.Minute},
	}
	for _, tc := range cases {
		got, ok := ParseWhen(tc.in)
		if !ok || !got.Relative || got.Rel != tc.want {
			t.Errorf("ParseWhen(%q) = %+v ok=%v, want relative %s", tc.in, got, ok, tc.want)
		}
	}
}

// TestParseWhenRefusals: everything the table does not spell out is a miss —
// the utterance stays the model's, never a guess.
func TestParseWhenRefusals(t *testing.T) {
	for _, in := range []string{
		"",
		"at",
		"in",
		"whenever",
		"at half past three",     // half-past phrasing is not in the table
		"at ten to three",        // nor to-the-hour phrasing
		"at twenty five",         // 25 is no hour, "five" no spoken minute
		"at three five",          // minutes under ten are "oh five"
		"at 99",                  // no such hour
		"at 12 75",               // no such minute
		"at 23 pm",               // a 24-hour reading takes no meridiem
		"at some point",          //
		"in a bit",               //
		"in zero minutes",        // a delay is at least a minute
		"in five",                // a bare number is not a delay
		"at school at three",     // the errand's words are not a time
		"tomorrow",               // tomorrow alone names a day, not a moment
		"in an hour and a house", //
		"at three yesterday",     // the past is not schedulable
	} {
		if got, ok := ParseWhen(in); ok {
			t.Errorf("ParseWhen(%q) = %+v, want a refusal", in, got)
		}
	}
}

// TestResolveNextOccurrence pins the ambiguity policy: an ambiguous hour
// resolves to the NEXT of its two readings, and the confirmation phrase says
// which won.
func TestResolveNextOccurrence(t *testing.T) {
	// A fixed Wednesday, 13:00 local.
	now := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)
	cases := []struct {
		in      string
		want    time.Time
		wantSay string
	}{
		// "at three" at 13:00 → 15:00 today, and the phrase says so — the
		// acceptance criterion's own example.
		{"at three", time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC), "at three this afternoon"},
		// The same words at 16:00 would roll to 3:00 tomorrow — see below.
		{"at 15:00", time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC), "at three this afternoon"},
		{"at nine", time.Date(2026, 8, 26, 21, 0, 0, 0, time.UTC), "at nine tonight"},
		{"at eleven", time.Date(2026, 8, 26, 23, 0, 0, 0, time.UTC), "at eleven tonight"},
		// "at twelve" at 13:00: noon is past, so the next reading is the
		// coming midnight — spoken as tonight's, never "tomorrow morning".
		{"at twelve", time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC), "at twelve tonight"},
		{"at one", time.Date(2026, 8, 26, 13, 30, 0, 0, time.UTC), ""}, // replaced below
		{"tomorrow at nine", time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC), "at nine tomorrow morning"},
		{"tomorrow at twelve", time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC), "at twelve tomorrow afternoon"},
		{"at nine tomorrow", time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC), "at nine tomorrow morning"},
		{"tomorrow at 15:00", time.Date(2026, 8, 27, 15, 0, 0, 0, time.UTC), "at three tomorrow afternoon"},
		{"at nine thirty in the morning", time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC), "at nine thirty tomorrow morning"},
		{"at nine oh five at night", time.Date(2026, 8, 26, 21, 5, 0, 0, time.UTC), "at nine oh five tonight"},
		{"in twenty minutes", time.Date(2026, 8, 26, 13, 20, 0, 0, time.UTC), "in twenty minutes"},
		{"in an hour and a half", time.Date(2026, 8, 26, 14, 30, 0, 0, time.UTC), "in an hour and a half"},
	}
	// "at one" at 13:00: 1:00 pm is past (it IS 13:00 — not after), so the
	// next reading is the coming 1 a.m. — the night that follows today.
	cases[5].want = time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	cases[5].wantSay = "at one tonight"
	for _, tc := range cases {
		w, ok := ParseWhen(tc.in)
		if !ok {
			t.Errorf("ParseWhen(%q) refused", tc.in)
			continue
		}
		due, say := w.Resolve(now)
		if !due.Equal(tc.want) || say != tc.wantSay {
			t.Errorf("Resolve(%q) = %s, %q; want %s, %q", tc.in, due, say, tc.want, tc.wantSay)
		}
	}
}

// TestResolveRollsPastMidnight: "at three" spoken after 15:00 rolls to the
// small hours — the next occurrence, spoken as the coming night's.
func TestResolveRollsPastMidnight(t *testing.T) {
	now := time.Date(2026, 8, 26, 16, 0, 0, 0, time.UTC)
	w, ok := ParseWhen("at three")
	if !ok {
		t.Fatal("refused")
	}
	due, say := w.Resolve(now)
	want := time.Date(2026, 8, 27, 3, 0, 0, 0, time.UTC)
	if !due.Equal(want) || say != "at three tonight" {
		t.Errorf("Resolve = %s, %q; want %s, %q", due, say, want, "at three tonight")
	}
}

// TestResolveInTheSmallHours: said at 02:00, "at three" is an hour away and
// the night is already this morning's.
func TestResolveInTheSmallHours(t *testing.T) {
	now := time.Date(2026, 8, 27, 2, 0, 0, 0, time.UTC)
	w, _ := ParseWhen("at three")
	due, say := w.Resolve(now)
	want := time.Date(2026, 8, 27, 3, 0, 0, 0, time.UTC)
	if !due.Equal(want) || say != "at three this morning" {
		t.Errorf("Resolve = %s, %q; want %s, %q", due, say, want, "at three this morning")
	}
}

// TestResolveExactRollsForward: an exact 24-hour time already past today is
// tomorrow's, never this morning's.
func TestResolveExactRollsForward(t *testing.T) {
	now := time.Date(2026, 8, 26, 16, 0, 0, 0, time.UTC)
	w, _ := ParseWhen("at 15:00")
	due, say := w.Resolve(now)
	want := time.Date(2026, 8, 27, 15, 0, 0, 0, time.UTC)
	if !due.Equal(want) || say != "at three tomorrow afternoon" {
		t.Errorf("Resolve = %s, %q", due, say)
	}
}

func TestSpokenClock(t *testing.T) {
	cases := []struct {
		h, m int
		want string
	}{
		{15, 0, "three"},
		{3, 30, "three thirty"},
		{9, 5, "nine oh five"},
		{0, 0, "twelve"},
		{12, 0, "twelve"},
		{23, 45, "eleven forty-five"},
	}
	for _, tc := range cases {
		if got := SpokenClock(tc.h, tc.m); got != tc.want {
			t.Errorf("SpokenClock(%d, %d) = %q, want %q", tc.h, tc.m, got, tc.want)
		}
	}
}

func TestSpokenDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{time.Minute, "a minute"},
		{20 * time.Minute, "twenty minutes"},
		{time.Hour, "an hour"},
		{90 * time.Minute, "an hour and a half"},
		{150 * time.Minute, "two and a half hours"},
		{2 * time.Hour, "two hours"},
		{65 * time.Minute, "an hour and five minutes"},
		{155 * time.Minute, "two hours and thirty-five minutes"},
		{30 * time.Second, "a minute"}, // the floor: nothing shorter is spoken
	}
	for _, tc := range cases {
		if got := SpokenDuration(tc.d); got != tc.want {
			t.Errorf("SpokenDuration(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
