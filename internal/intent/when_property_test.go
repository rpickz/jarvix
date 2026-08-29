package intent

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Properties of the spoken-time parser (issue #172).
//
// when_test.go is a table: it proves the cases somebody thought of. This file
// states the same component's contract as LAWS — sentences that must hold for
// every input, not for a list of them — and attacks each law with the whole
// domain the grammar admits, plus (in FuzzParseWhen) with arbitrary bytes.
//
// The distinction earns its keep immediately: the round-trip law below found a
// real defect on its first run. SpokenDuration says "an hour and five minutes"
// when it confirms a reminder, and parseRelative could not read that sentence
// back — only the synonymous "one hour and five minutes" — so a user repeating
// the assistant's own words missed the grammar for the whole
// hour-and-something family and fell through to the model. See parseCount in
// when.go for the fix and why it went on the parsing side.
//
// Everything here is deterministic. Time zones are pinned to UTC on purpose:
// a DST boundary makes time.Date normalise a nonexistent local hour, and a
// test that occasionally disagrees with itself about what "half past two"
// means is a test that gets deleted rather than read. The next-occurrence rule
// is what these laws are about, and it is the same rule in every zone.

// probeNows are the clock readings each law is checked against: the two edges
// of a day, an ordinary afternoon, and a year boundary, because resolveClock
// does calendar arithmetic and a month or year rollover is exactly the sort of
// thing an off-by-one hides in.
var probeNows = []time.Time{
	time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	time.Date(2026, 3, 1, 0, 0, 1, 0, time.UTC),
	time.Date(2026, 3, 1, 11, 59, 0, 0, time.UTC),
	time.Date(2026, 3, 1, 13, 45, 30, 0, time.UTC),
	time.Date(2026, 3, 1, 23, 59, 59, 0, time.UTC),
	time.Date(2026, 12, 31, 18, 30, 0, 0, time.UTC),
	time.Date(2028, 2, 28, 23, 30, 0, 0, time.UTC), // into a leap day
}

// everyClockReading enumerates every When the clock half of the grammar can
// produce: every hour and minute, both anchors, both readings. Generating the
// domain rather than sampling it is what makes these statements laws — there
// is no "and the cases we didn't list".
func everyClockReading(yield func(When)) {
	for h := 0; h < 24; h++ {
		for m := 0; m < 60; m++ {
			for _, tomorrow := range []bool{false, true} {
				yield(When{Hour: h, Minute: m, Tomorrow: tomorrow})
				if h >= 1 && h <= 12 {
					// Ambiguous readings are 1–12 by construction
					// (resolveMeridiem never builds any other).
					yield(When{Hour: h, Minute: m, Ambiguous: true, Tomorrow: tomorrow})
				}
			}
		}
	}
}

// TestAParsedTimeAlwaysResolvesToTheFuture is the law that matters most to a
// reminder: a moment in the past is one that never fires, and it would fail
// silently — the confirmation would still sound right.
func TestAParsedTimeAlwaysResolvesToTheFuture(t *testing.T) {
	for _, now := range probeNows {
		everyClockReading(func(w When) {
			due, phrase := w.Resolve(now)
			if !due.After(now) {
				t.Errorf("now %v: %+v resolved to %v (%q), which is not ahead of now",
					now, w, due, phrase)
			}
		})
		for mins := 1; mins <= 999*60+59; mins += 7 {
			w := When{Relative: true, Rel: time.Duration(mins) * time.Minute}
			if due, _ := w.Resolve(now); !due.After(now) {
				t.Errorf("now %v: %v ahead resolved to %v", now, w.Rel, due)
			}
		}
	}
}

// TestATwelveHourReadingPicksTheNextOccurrence states the rule ADR 0046
// promises as a law over the whole domain: the resolved moment is the EARLIEST
// moment after now that reads as the spoken hour on a twelve-hour clock. A
// weaker phrasing — "it is in the future and the hour matches" — would pass on
// an implementation that skipped a day, which is the mistake worth catching.
func TestATwelveHourReadingPicksTheNextOccurrence(t *testing.T) {
	for _, now := range probeNows {
		for h := 1; h <= 12; h++ {
			for _, m := range []int{0, 1, 30, 59} {
				w := When{Hour: h, Minute: m, Ambiguous: true}
				due, phrase := w.Resolve(now)
				if got := due.Hour() % 12; got != h%12 || due.Minute() != m {
					t.Errorf("now %v: %+v resolved to %v (%q), which does not read as %d:%02d",
						now, w, due, phrase, h, m)
					continue
				}
				// A twelve-hour reading recurs every twelve hours, so the next
				// one is never further off than that. Asserted before the walk
				// below rather than after it, because it is both a law in its
				// own right and the walk's bound: without it a mutant that
				// resolved to next week would be measured one minute at a time.
				if gap := due.Sub(now); gap > 12*time.Hour {
					t.Errorf("now %v: %+v resolved to %v, %v away — a twelve-hour reading comes round twice a day",
						now, w, due, gap)
					continue
				}
				// Nothing earlier may read as that hour. The occurrences are
				// exactly twelve hours apart, so the one before this is
				// due-12h, and it must already have gone by — an implementation
				// that skipped a reading fails here without a scan.
				if prev := due.Add(-12 * time.Hour); prev.After(now) {
					t.Errorf("now %v: %+v resolved to %v, but %v reads the same and came first",
						now, w, due, prev)
				}
			}
		}
	}
}

// TestAResolvedMomentRoundTripsThroughItsSpokenForm: whatever the grammar
// resolved, saying it back in words and parsing that must land on the same
// moment. This is the law that catches a renderer and a parser drifting apart
// — the failure where Jarvix says "at nine oh five tomorrow", the user repeats
// it, and the second reminder lands somewhere else.
//
// The spoken form used here is the unambiguous one (an explicit am/pm, and the
// "tomorrow" anchor when the original carried it), because that is what a
// person reading a resolved moment back would say. The ambiguity of a bare
// "at nine" is the previous law's subject, not this one's.
func TestAResolvedMomentRoundTripsThroughItsSpokenForm(t *testing.T) {
	for _, now := range probeNows {
		everyClockReading(func(w When) {
			due, _ := w.Resolve(now)
			phrase := unambiguousSpoken(due, w.Tomorrow)
			w2, ok := ParseWhen(phrase)
			if !ok {
				t.Errorf("now %v: %+v resolved to %v, whose spoken form %q does not parse",
					now, w, due, phrase)
				return
			}
			due2, _ := w2.Resolve(now)
			if !due2.Equal(due) {
				t.Errorf("now %v: %+v resolved to %v; %q re-resolved to %v",
					now, w, due, phrase, due2)
			}
		})
	}
}

// unambiguousSpoken renders a moment the way a person reads a clock back:
// twelve-hour words plus the meridiem, with the day named when it is not
// today's. SpokenClock is the production renderer; only the suffix is the
// test's, because Resolve's own phrase ("at three this afternoon") is prose
// for a confirmation rather than an expression the grammar claims.
func unambiguousSpoken(t time.Time, tomorrow bool) string {
	suffix := " am"
	if t.Hour() >= 12 {
		suffix = " pm"
	}
	phrase := "at " + SpokenClock(t.Hour(), t.Minute()) + suffix
	if tomorrow {
		phrase = "tomorrow " + phrase
	}
	return phrase
}

// TestEverySpokenDurationReadsBackAsItself is the relative half of the
// round-trip law, and the one that found the defect. It runs over the
// durations the grammar can actually PRODUCE — enumerated from the number
// table and the unit words, never from SpokenDuration, so the two sides of the
// round trip have independent origins.
//
// The domain is bounded by maxWhenWords rather than by the number table: "in
// one hundred and one hours and one minute" is nine words and the slot admits
// eight, so it is not an expression this grammar has, and its absence is a
// deliberate cap rather than a gap.
func TestEverySpokenDurationReadsBackAsItself(t *testing.T) {
	reachable := 0
	for _, phrase := range everyRelativePhrase() {
		w, ok := ParseWhen(phrase)
		if !ok {
			continue
		}
		if !w.Relative {
			t.Errorf("%q parsed as a clock reading: %+v", phrase, w)
			continue
		}
		reachable++
		if why, ok := durationReadsBack(w.Rel); !ok {
			t.Errorf("%q means %v: %s", phrase, w.Rel, why)
		}
	}
	// A domain that quietly emptied would make every law above vacuous.
	if reachable < 5000 {
		t.Errorf("only %d relative expressions parsed; the generator has stopped generating", reachable)
	}
}

// everyRelativePhrase enumerates the relative shapes the grammar documents,
// spelled with the number table's own words. Some of these deliberately do not
// parse ("in one hundred hours and thirty five minutes" overruns the slot);
// the caller skips those, which is how the reachable set is discovered rather
// than assumed.
func everyRelativePhrase() []string {
	var out []string
	minutes := []int{1, 2, 5, 9, 10, 15, 20, 29, 30, 31, 45, 59}
	for n := 0; n <= 999; n++ {
		w := spokenAsWords(n)
		out = append(out,
			"in "+w+" minute", "in "+w+" minutes",
			"in "+w+" hour", "in "+w+" hours",
			"in "+w+" and a half hours",
			"in "+w+" hours and a half")
		for _, m := range minutes {
			mw := spokenAsWords(m)
			out = append(out,
				"in "+w+" hours and "+mw+" minutes",
				"in "+w+" hour and "+mw+" minute")
		}
	}
	for _, m := range minutes {
		mw := spokenAsWords(m)
		out = append(out,
			"in an hour and "+mw+" minutes",
			"in a hour and "+mw+" minutes",
			"in one hour and "+mw+" minutes")
	}
	return append(out, "in a minute", "in an hour", "in half an hour",
		"in an hour and a half", "in 1 minute", "in 90 minutes", "in 24 hours")
}

// spokenAsWords is SpokenNumber split the way normalize splits it — the hyphen
// in "twenty-five" is a word boundary to the parser.
func spokenAsWords(n int) string {
	return strings.ReplaceAll(SpokenNumber(n), "-", " ")
}

// durationReadsBack is the relative round-trip law with its one exemption
// stated precisely rather than waved at.
//
// Every duration the grammar accepts must come back through SpokenDuration
// unchanged — that is the law, and it is what caught "an hour and five
// minutes". The exemption is the {when} slot's word budget: the digit
// shorthand lets a few words carry a large number ("in 101 hour and five
// minute" is seven words), while the same delay in English is eleven, and
// maxWhenWords admits eight. FuzzParseWhen found exactly that, and it is a
// documented BOUND rather than a defect — widening the slot to twelve words so
// a four-day reminder can be repeated aloud would let the deterministic
// grammar claim far longer utterances from the model for no benefit anybody
// asked for.
//
// So the exemption is checked, not assumed: the spoken form must be over the
// budget, and if it is not, the round trip failed for a real reason and this
// says so. The failing input is committed under
// testdata/fuzz/FuzzParseWhen/7c19a5a189930f65.
func durationReadsBack(d time.Duration) (string, bool) {
	spoken := "in " + SpokenDuration(d)
	w, ok := ParseWhen(spoken)
	switch {
	case ok && w.Relative && w.Rel == d:
		return "", true
	case !ok && len(normalize(spoken)) > maxWhenWords:
		return "", true // over the slot's word budget; see above
	case !ok:
		return fmt.Sprintf("Jarvix says it as %q, which is %d words and cannot be read back",
			spoken, len(normalize(spoken))), false
	default:
		return fmt.Sprintf("Jarvix says it as %q, which reads back as %v", spoken, w.Rel), false
	}
}

// TestUnparseableInputNeverYieldsATime: a miss must be a miss all the way
// down. A caller that ignored ok and used the zero When would schedule
// midnight, so the refusal has to be carried in the value as well as the flag.
func TestUnparseableInputNeverYieldsATime(t *testing.T) {
	misses := []string{
		"", "   ", "at", "in", "tomorrow", "at tomorrow", "in tomorrow",
		"at twenty five", "at 25", "at 24", "at 23 am", "at 13 pm", "at 0 pm",
		"at three five", "at three sixty", "at 3 61", "at nine oh ten",
		"in zero minutes", "in minus five minutes", "in some minutes",
		"in a fortnight", "at half past three", "next tuesday at nine",
		"at three tomorrow evening at four", "at ٣",
		"in one hundred and one hours and one minute",
		strings.Repeat("at three ", 40),
	}
	for _, in := range misses {
		if w, ok := ParseWhen(in); ok {
			t.Errorf("%q parsed as %+v; the table does not claim it", in, w)
		} else if w != (When{}) {
			t.Errorf("%q was refused but still carried %+v", in, w)
		}
	}
}

// FuzzParseWhen throws arbitrary bytes at the grammar — which is what a
// speech-to-text engine does — and re-states the laws above over whatever the
// fuzzer finds that parses. Seeds cover each documented shape, the refusals,
// and the round-trip defect this target's law caught; the minimised cases live
// in testdata/fuzz/FuzzParseWhen so they can never come back.
func FuzzParseWhen(f *testing.F) {
	seeds := []string{
		"at three", "at 15:00", "at three thirty", "at nine oh five",
		"at three pm", "at three oclock", "at noon", "at midnight", "at midday",
		"tomorrow at nine", "at nine tomorrow", "at three in the afternoon",
		"at eleven at night", "at 3pm", "at 11am", "at twelve am", "at twelve pm",
		"in twenty minutes", "in an hour", "in half an hour",
		"in an hour and a half", "in two and a half hours",
		"in one hour and five minutes", "in an hour and five minutes",
		"", "   ", "at", "in", "tomorrow", "at 25", "at three five",
		"at\x00three", "in ٣٠ minutes", "AT THREE PM", "at   three   thirty  ",
		"in 999 hours", "at 00:00", "at 23:59",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	// A fixed clock: the laws are about the next-occurrence rule, and a clock
	// that moved would make a failure unreproducible from the corpus entry
	// alone — which is the one thing a committed corpus is for.
	now := time.Date(2026, 3, 1, 13, 45, 30, 0, time.UTC)

	f.Fuzz(func(t *testing.T, text string) {
		w, ok := ParseWhen(text)
		if !ok {
			// Law: a refusal carries nothing. A caller that forgot to check ok
			// must not be handed a usable-looking midnight.
			if w != (When{}) {
				t.Fatalf("%q was refused but carried %+v", text, w)
			}
			return
		}
		// Law: parsing is a pure function of the words.
		if w2, ok2 := ParseWhen(text); !ok2 || w2 != w {
			t.Fatalf("%q parsed as %+v then %+v (ok %v)", text, w, w2, ok2)
		}
		// Law: the reading is inside the grammar's own bounds. An hour of 24 or
		// an ambiguous 0 would resolve to something, silently and wrongly.
		switch {
		case w.Relative:
			if w.Rel < time.Minute {
				t.Fatalf("%q parsed as a delay of %v", text, w.Rel)
			}
		default:
			if w.Hour < 0 || w.Hour > 23 || w.Minute < 0 || w.Minute > 59 {
				t.Fatalf("%q parsed as %+v, which is not a clock reading", text, w)
			}
			if w.Ambiguous && (w.Hour < 1 || w.Hour > 12) {
				t.Fatalf("%q parsed as an ambiguous %+v; ambiguity is a 1–12 reading", text, w)
			}
		}
		// Law: it resolves to a moment in the future, from any clock.
		for _, from := range append([]time.Time{now}, probeNows...) {
			due, phrase := w.Resolve(from)
			if !due.After(from) {
				t.Fatalf("%q (%+v) resolved to %v from %v — not ahead of now (%q)",
					text, w, due, from, phrase)
			}
			if strings.TrimSpace(phrase) == "" {
				t.Fatalf("%q (%+v) resolved with nothing to say", text, w)
			}
		}
		// Law: the resolved moment survives being said and read back.
		due, _ := w.Resolve(now)
		var spoken string
		if w.Relative {
			spoken = "in " + SpokenDuration(w.Rel)
		} else {
			spoken = unambiguousSpoken(due, w.Tomorrow)
		}
		if w.Relative {
			if why, ok := durationReadsBack(w.Rel); !ok {
				t.Fatalf("%q means %v: %s", text, w.Rel, why)
			}
			return
		}
		w2, ok2 := ParseWhen(spoken)
		if !ok2 {
			t.Fatalf("%q means %v, which Jarvix says as %q — and cannot read back",
				text, due, spoken)
		}
		if due2, _ := w2.Resolve(now); !due2.Equal(due) {
			t.Fatalf("%q means %v, said as %q, read back as %v", text, due, spoken, due2)
		}
	})
}
