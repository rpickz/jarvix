package intent

import (
	"strings"
	"time"
)

// Time expressions for one-shot reminders (#141, ADR 0046).
//
// ParseWhen is the {when} slot's grammar: pure code over normalized words,
// beside the number table it reuses (numbers.go — one copy of
// words-to-numbers, deliberately). Parsing is syntax only and needs no clock,
// which is what lets the pattern matcher validate a {when} slot the way it
// validates {volume}: an expression the table does not recognise is a miss,
// and the utterance falls through to the model — where the reminder.set tool
// answers natural phrasings the grammar cannot claim.
//
// Resolution is the separate half: Resolve applies the next-occurrence rule
// against a caller-supplied now, so the reminders service — which owns the
// injected clock — decides *when* "three" is, and the confirmation it speaks
// says which reading won ("Reminding you at three this afternoon…"). Keeping
// Resolve pure in now is what makes the whole table testable to the minute
// without a single sleep.
//
// The accepted shapes, exhaustively (everything else is a miss):
//
//	at three            — 12-hour, am/pm unknown: the NEXT 3:00 or 15:00 wins
//	at 15:00            — 24-hour ("15:00" normalizes to "15 00"); exact
//	at three thirty     — minutes as words or a two-digit numeral
//	at nine oh five     — "oh" minutes, 0–9
//	at three pm         — meridiem: am/pm ("a m"/"p m"/"3pm" included),
//	at three oclock       "in the morning/afternoon/evening", "at night";
//	                      "oclock" is consumed but decides nothing
//	at noon / midday / midnight
//	tomorrow at nine    — also "at nine tomorrow"; an ambiguous hour takes
//	                      the daytime reading (nine in the morning)
//	in twenty minutes   — relative: "in N minutes/hours", "in an hour",
//	in an hour and a half "in half an hour", "in two and a half hours",
//	                      "in one hour and five minutes" — and "an" for the
//	                      one, so the parser reads back every sentence
//	                      SpokenDuration can say

// When is one parsed time expression: either a relative delay or a clock
// reading, with the ambiguity of a bare 12-hour hour carried explicitly so
// Resolve can settle it against a clock this package never reads.
type When struct {
	// Relative marks an "in …" delay; Rel is its length.
	Relative bool
	Rel      time.Duration
	// Hour and Minute are the clock reading. With Ambiguous false the hour
	// is 24-hour and exact; with Ambiguous true it is 1–12 and the
	// next-occurrence rule in Resolve decides between morning and afternoon.
	Hour, Minute int
	Ambiguous    bool
	// Tomorrow anchors the clock reading to the next calendar day.
	Tomorrow bool
}

// maxWhenWords bounds how many words a {when} slot may swallow: "in two
// hours and thirty five minutes" — seven words — is the longest expression
// the table admits, with one spare for a trailing "tomorrow".
const maxWhenWords = 8

// ParseWhen reads one time expression. ok is false for anything the table
// above does not spell out — the caller treats that as no match, never a
// guess.
func ParseWhen(raw string) (When, bool) {
	return parseWhenWords(normalize(raw))
}

// parseWhenWords is ParseWhen over already-normalized words — the form the
// pattern matcher holds them in.
func parseWhenWords(words []string) (When, bool) {
	if len(words) < 2 || len(words) > maxWhenWords {
		return When{}, false
	}
	if words[0] == "in" {
		rel, ok := parseRelative(words[1:])
		if !ok {
			return When{}, false
		}
		return When{Relative: true, Rel: rel}, true
	}
	tomorrow := false
	rest := words
	if rest[0] == "tomorrow" {
		tomorrow, rest = true, rest[1:]
	}
	if len(rest) < 2 || rest[0] != "at" {
		return When{}, false
	}
	rest = rest[1:]
	if !tomorrow && len(rest) > 1 && rest[len(rest)-1] == "tomorrow" {
		tomorrow, rest = true, rest[:len(rest)-1]
	}
	w, ok := parseClock(rest)
	if !ok {
		return When{}, false
	}
	w.Tomorrow = tomorrow
	return w, true
}

// meridiem is what a clock suffix decided: nothing, morning, or afternoon.
type meridiem int

const (
	meridiemNone meridiem = iota
	meridiemAM
	meridiemPM
)

// clockSuffixes are the trailing words that pin (or merely decorate) a clock
// reading, longest first so "in the morning" is never read as minutes.
var clockSuffixes = []struct {
	words []string
	m     meridiem
}{
	{[]string{"in", "the", "morning"}, meridiemAM},
	{[]string{"in", "the", "afternoon"}, meridiemPM},
	{[]string{"in", "the", "evening"}, meridiemPM},
	{[]string{"at", "night"}, meridiemPM},
	{[]string{"a", "m"}, meridiemAM},
	{[]string{"p", "m"}, meridiemPM},
	{[]string{"am"}, meridiemAM},
	{[]string{"pm"}, meridiemPM},
	// "oclock" ("o'clock" once normalize drops the apostrophe) is consumed
	// but decides nothing: "at three oclock" is as ambiguous as "at three".
	{[]string{"oclock"}, meridiemNone},
}

// parseClock reads the words after "at": an hour, optional minutes, and an
// optional meridiem suffix.
func parseClock(words []string) (When, bool) {
	if len(words) == 0 {
		return When{}, false
	}
	switch strings.Join(words, " ") {
	case "noon", "midday":
		return When{Hour: 12}, true
	case "midnight":
		return When{Hour: 0}, true
	}
	// "3pm" arrives as one token; split it so the general path reads it.
	if h, m, ok := splitDigitMeridiem(words[len(words)-1]); ok {
		words = append(append([]string(nil), words[:len(words)-1]...), h, m)
	}
	mer := meridiemNone
	for _, s := range clockSuffixes {
		n := len(s.words)
		if len(words) > n && equalWords(words[len(words)-n:], s.words) {
			mer = s.m
			words = words[:len(words)-n]
			break
		}
	}
	// The hour is one or two words ("three", "23", "twenty three"), longest
	// reading first so "at twenty three" is 23:00, never 20:03.
	for hn := 2; hn >= 1; hn-- {
		if hn > len(words) {
			continue
		}
		h, ok := parseNumber(words[:hn])
		if !ok || h < 0 || h > 23 {
			continue
		}
		m, ok := parseClockMinutes(words[hn:])
		if !ok {
			continue
		}
		return resolveMeridiem(h, m, mer)
	}
	return When{}, false
}

// resolveMeridiem folds a spoken meridiem into the hour, and decides whether
// the reading is exact or owed to the next-occurrence rule.
func resolveMeridiem(h, m int, mer meridiem) (When, bool) {
	switch mer {
	case meridiemAM:
		if h < 1 || h > 12 {
			return When{}, false // "23 am" is nobody's clock
		}
		return When{Hour: h % 12, Minute: m}, true
	case meridiemPM:
		if h < 1 || h > 12 {
			return When{}, false
		}
		return When{Hour: h%12 + 12, Minute: m}, true
	default:
		if h == 0 || h > 12 {
			return When{Hour: h, Minute: m}, true // 24-hour, exact
		}
		return When{Hour: h, Minute: m, Ambiguous: true}, true
	}
}

// parseClockMinutes reads the minute words of a clock expression. Absent
// minutes are 0. A single-digit numeral or number word alone is refused —
// "at three five" is far more likely a mistranscription than 3:05; minutes
// under ten are spoken "oh five", and the table holds people to that.
func parseClockMinutes(words []string) (int, bool) {
	switch len(words) {
	case 0:
		return 0, true
	case 1:
		w := words[0]
		if len(w) == 2 && isDigits(w) {
			// "00"–"59" as one token — "15:00" normalized to ["15", "00"].
			n, _ := parseNumber(words)
			if n <= 59 {
				return n, true
			}
			return 0, false
		}
		if n, ok := smallWords[w]; ok && n >= 10 {
			return n, true // "fifteen" through "nineteen"
		}
		if n, ok := tensWords[w]; ok && n <= 59 {
			return n, true // "thirty"
		}
		// The bound is not decoration. tensWords runs to "ninety", so without
		// it "at three sixty" read as 3:60 — and time.Date normalises a minute
		// of 60 into the next hour rather than rejecting it, so the reminder
		// was accepted, confirmed as "at four this afternoon", and fired an
		// hour from where the words pointed. A minute that does not exist has
		// to be a miss here, where the caller can fall through to the model,
		// not a silent carry in the calendar (issue #172).
		return 0, false
	case 2:
		if words[0] == "oh" {
			n, ok := parseNumber(words[1:])
			if ok && n >= 0 && n <= 9 {
				return n, true // "oh five"
			}
			return 0, false
		}
		n, ok := parseNumber(words)
		if ok && n >= 21 && n <= 59 {
			return n, true // "thirty five"
		}
		return 0, false
	}
	return 0, false
}

// parseRelative reads the words after "in": a delay in minutes and hours.
func parseRelative(words []string) (time.Duration, bool) {
	switch strings.Join(words, " ") {
	case "a minute":
		return time.Minute, true
	case "an hour":
		return time.Hour, true
	case "half an hour":
		return 30 * time.Minute, true
	case "an hour and a half":
		return 90 * time.Minute, true
	}
	// "N and a half hours" — "two and a half hours".
	if len(words) >= 4 && words[len(words)-1] == "hours" &&
		equalWords(words[len(words)-4:len(words)-1], []string{"and", "a", "half"}) {
		n, ok := parseNumber(words[:len(words)-4])
		if !ok || n < 1 {
			return 0, false
		}
		return time.Duration(n)*time.Hour + 30*time.Minute, true
	}
	// The general reading: N minutes | N hours [and M minutes].
	for split := 1; split < len(words); split++ {
		n, ok := parseCount(words[:split])
		if !ok || n < 1 {
			continue
		}
		unit := words[split]
		rest := words[split+1:]
		switch unit {
		case "minute", "minutes":
			if len(rest) == 0 {
				return time.Duration(n) * time.Minute, true
			}
		case "hour", "hours":
			if len(rest) == 0 {
				return time.Duration(n) * time.Hour, true
			}
			if equalWords(rest, []string{"and", "a", "half"}) {
				return time.Duration(n)*time.Hour + 30*time.Minute, true
			}
			if len(rest) >= 3 && rest[0] == "and" &&
				(rest[len(rest)-1] == "minute" || rest[len(rest)-1] == "minutes") {
				m, ok := parseNumber(rest[1 : len(rest)-1])
				if ok && m >= 1 && m <= 59 {
					return time.Duration(n)*time.Hour + time.Duration(m)*time.Minute, true
				}
			}
		}
	}
	return 0, false
}

// parseCount reads the quantity in a relative expression: a number, or the
// indefinite article standing in for one.
//
// The article is here because of a round-trip defect the property test in
// when_property_test.go found (issue #172). SpokenDuration renders 65 minutes
// as "an hour and five minutes" — that is the sentence Jarvix speaks when it
// confirms a reminder — and the grammar could not read its own words back:
// parseNumber has no entry for "an", so "in an hour and five minutes" missed
// the table and fell through to the model, while the synonymous "in one hour
// and five minutes" matched. A user repeating what the assistant just said
// took the slower, less certain path for the whole hour-and-something family.
//
// It is fixed on the parsing side rather than by making SpokenDuration say
// "one hour": the parser faces speech, and a person WILL say "an hour and ten
// minutes". Widening what is understood cannot make any previously-matching
// utterance match differently — the loop that calls this already tries every
// split and keeps looking when one fails — so this only ever turns a miss into
// a hit.
func parseCount(words []string) (int, bool) {
	if len(words) == 1 && (words[0] == "a" || words[0] == "an") {
		return 1, true
	}
	return parseNumber(words)
}

// Resolve applies the next-occurrence rule: the moment this expression means,
// measured from now, and the exact phrase the confirmation speaks for it —
// "at three this afternoon", "in twenty minutes" — so an ambiguous "three"
// is always spoken back with which reading won (ADR 0046).
func (w When) Resolve(now time.Time) (time.Time, string) {
	if w.Relative {
		return now.Add(w.Rel), "in " + SpokenDuration(w.Rel)
	}
	due := w.resolveClock(now)
	return due, "at " + SpokenClock(due.Hour(), due.Minute()) + " " + spokenDayPeriod(due, now)
}

// resolveClock picks the concrete day and hour for a clock reading.
func (w When) resolveClock(now time.Time) time.Time {
	day := now
	if w.Tomorrow {
		day = day.AddDate(0, 0, 1)
	}
	at := func(d time.Time, h int) time.Time {
		return time.Date(d.Year(), d.Month(), d.Day(), h, w.Minute, 0, 0, now.Location())
	}
	if !w.Ambiguous {
		due := at(day, w.Hour)
		if !w.Tomorrow && !due.After(now) {
			due = due.AddDate(0, 0, 1) // the next 15:00, not this morning's
		}
		return due
	}
	if w.Tomorrow {
		// "tomorrow at nine": the daytime reading — nine in the morning,
		// noon for twelve — because "tomorrow at nine" said today almost
		// never means tomorrow evening, and the confirmation says which.
		h := w.Hour % 12
		if w.Hour == 12 {
			h = 12
		}
		return at(day, h)
	}
	// The NEXT 3:00 or 15:00, whichever comes first from now. Twelve reads
	// as noon or midnight on the same rule.
	first, second := w.Hour%12, w.Hour%12+12
	candidates := []time.Time{at(day, first), at(day, second),
		at(day.AddDate(0, 0, 1), first), at(day.AddDate(0, 0, 1), second)}
	for _, c := range candidates {
		if c.After(now) {
			return c
		}
	}
	return candidates[len(candidates)-1] // unreachable: tomorrow is always ahead
}

// SpokenClock renders a clock time the way the confirmation says it: twelve-
// hour words, "oh" for minutes under ten — "three", "three thirty", "nine oh
// five".
func SpokenClock(hour, minute int) string {
	h := hour % 12
	if h == 0 {
		h = 12
	}
	out := SpokenNumber(h)
	switch {
	case minute == 0:
	case minute < 10:
		out += " oh " + SpokenNumber(minute)
	default:
		out += " " + SpokenNumber(minute)
	}
	return out
}

// spokenDayPeriod names which reading of the day a resolved moment landed in
// — the half of the confirmation that answers "which three?". Same calendar
// day as now: "this morning" / "this afternoon" / "this evening" / "tonight";
// the next: the "tomorrow" forms. The night runs 21:00 through 04:59 and is
// anchored to the evening it follows, so midnight and the small hours are
// "tonight" — "at three tonight" is the coming 3 a.m., which is exactly how
// it is said.
func spokenDayPeriod(due, now time.Time) string {
	var period string
	anchor := due
	switch h := due.Hour(); {
	case h < 5:
		period = "night"
		anchor = due.AddDate(0, 0, -1)
	case h < 12:
		period = "morning"
	case h < 17:
		period = "afternoon"
	case h < 21:
		period = "evening"
	default:
		period = "night"
	}
	switch d := calendarDays(now, anchor); {
	case period == "night" && d < 0:
		// Both now and the due moment sit in the same small hours: the night
		// is already this morning's.
		return "this morning"
	case period == "night" && d == 0:
		return "tonight"
	case period == "night":
		return "tomorrow night"
	case d == 0:
		return "this " + period
	default:
		return "tomorrow " + period
	}
}

// calendarDays counts whole calendar-day steps from a's date to b's.
func calendarDays(a, b time.Time) int {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	start := time.Date(ay, am, ad, 0, 0, 0, 0, time.UTC)
	end := time.Date(by, bm, bd, 0, 0, 0, 0, time.UTC)
	return int(end.Sub(start) / (24 * time.Hour))
}

// SpokenDuration renders a delay the way the confirmation says it: "twenty
// minutes", "an hour", "an hour and a half", "two and a half hours", "an hour
// and five minutes".
//
// Every string it can produce parses back through ParseWhen to the same
// duration, and when_property_test.go pins that for the whole expressible
// range rather than for a handful of examples. The comment used to say "one
// hour and five minutes" while the code said "an hour and five minutes", and
// the parser agreed with the comment — see parseCount.
func SpokenDuration(d time.Duration) string {
	if d < time.Minute {
		d = time.Minute
	}
	h := int(d / time.Hour)
	m := int(d/time.Minute) % 60
	switch {
	case h == 0:
		if m == 1 {
			return "a minute"
		}
		return SpokenNumber(m) + " " + pluralWord(m, "minute", "minutes")
	case h == 1 && m == 0:
		return "an hour"
	case h == 1 && m == 30:
		return "an hour and a half"
	case m == 30:
		return SpokenNumber(h) + " and a half hours"
	case m == 0:
		return SpokenNumber(h) + " " + pluralWord(h, "hour", "hours")
	default:
		base := "an hour"
		if h > 1 {
			base = SpokenNumber(h) + " hours"
		}
		return base + " and " + SpokenNumber(m) + " " + pluralWord(m, "minute", "minutes")
	}
}

// splitDigitMeridiem splits a fused "3pm" token into its digits and suffix.
func splitDigitMeridiem(w string) (digits, suffix string, ok bool) {
	for _, s := range []string{"am", "pm"} {
		if body, found := strings.CutSuffix(w, s); found && body != "" && isDigits(body) {
			return body, s, true
		}
	}
	return "", "", false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func equalWords(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
