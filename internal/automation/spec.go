// Package automation runs routines and scripts on a clock (ADR 0032): each
// [[routines]] or [[scripts]] table may carry a `schedule`, and the daemon
// fires it through the exact same gated session path a spoken phrase takes.
//
// The scheduler is a sibling of the knowledge feed scheduler (ADR 0031), not
// an extraction of it: feeds run on intervals fused to fetch state (backoff,
// ttl, boot-warm remainders) while automations run at wall-clock moments with
// their own overlap and missed-while-down policies. What the two share is the
// discipline, restated here in full — every goroutine tracked in one
// quiesce.Group from the moment it exists, an injected clock and timer so no
// test ever sleeps, a generation swap on reload that can never orphan a loop,
// and a Drain shutdown treats as one more bounded stage.
package automation

import (
	"fmt"
	"strings"
	"time"
)

// Spec is one parsed schedule: a time of day and the weekdays it fires on.
// The syntax is the friendly form and only the friendly form — `"08:30"`,
// `"02:00 daily"`, `"08:30 mon-fri"`, `"22:15 mon,wed,fri"` — chosen over
// five-field cron deliberately (ADR 0032): one field that reads aloud, with
// nothing position-dependent to transpose at two in the morning.
type Spec struct {
	hour, minute int
	// days is indexed by time.Weekday; at least one is always set.
	days [7]bool
	raw  string
}

// dayNames maps the three-letter tokens the parser accepts, in time.Weekday
// order (Sunday first, as the standard library counts).
var dayNames = [7]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// specExamples is the worked-example clause every parse error ends with, so a
// bad schedule teaches the syntax instead of merely rejecting it.
const specExamples = `examples: "08:30", "02:00 daily", "08:30 mon-fri", "22:15 mon,wed,fri"`

// ParseSpec validates and parses one schedule string. Errors are complete
// sentences naming what is wrong and showing the accepted forms — they are
// destined for config validation output, where an actionable message is the
// whole point of validating hard.
func ParseSpec(s string) (Spec, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Spec{}, fmt.Errorf(`schedule is empty; write a time like "08:30" (24-hour), optionally with days — %s`, specExamples)
	}
	fields := strings.Fields(raw)
	if len(fields) > 2 {
		return Spec{}, fmt.Errorf(`schedule %q has too many parts; write "HH:MM" or "HH:MM days" — %s`, raw, specExamples)
	}
	spec := Spec{raw: raw}
	var err error
	if spec.hour, spec.minute, err = parseClock(fields[0]); err != nil {
		return Spec{}, fmt.Errorf("schedule %q: %w", raw, err)
	}
	if len(fields) == 1 {
		for d := range spec.days {
			spec.days[d] = true
		}
		return spec, nil
	}
	if spec.days, err = parseDays(fields[1]); err != nil {
		return Spec{}, fmt.Errorf("schedule %q: %w", raw, err)
	}
	return spec, nil
}

// parseClock reads the "HH:MM" half: a 24-hour time, minutes always two
// digits.
func parseClock(tok string) (hour, minute int, err error) {
	fail := func() (int, int, error) {
		return 0, 0, fmt.Errorf(`%q is not a 24-hour time; write it as "HH:MM", like "08:30" or "22:05"`, tok)
	}
	h, m, ok := strings.Cut(tok, ":")
	if !ok || len(m) != 2 || len(h) == 0 || len(h) > 2 {
		return fail()
	}
	if hour, err = atoiDigits(h); err != nil {
		return fail()
	}
	if minute, err = atoiDigits(m); err != nil {
		return fail()
	}
	if hour > 23 || minute > 59 {
		return fail()
	}
	return hour, minute, nil
}

// atoiDigits parses a bare run of ASCII digits — strconv.Atoi would also take
// signs and underscores, which are not a time of day.
func atoiDigits(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not digits")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// parseDays reads the day half: comma-separated day names and ranges, or one
// of the three whole-word forms.
func parseDays(tok string) ([7]bool, error) {
	var days [7]bool
	switch strings.ToLower(tok) {
	case "daily":
		for d := range days {
			days[d] = true
		}
		return days, nil
	case "weekdays":
		for d := time.Monday; d <= time.Friday; d++ {
			days[d] = true
		}
		return days, nil
	case "weekends":
		days[time.Saturday], days[time.Sunday] = true, true
		return days, nil
	}
	for _, part := range strings.Split(tok, ",") {
		from, to, isRange := strings.Cut(part, "-")
		a, err := parseDay(from)
		if err != nil {
			return days, err
		}
		b := a
		if isRange {
			if b, err = parseDay(to); err != nil {
				return days, err
			}
		}
		// Ranges walk forward and may wrap: "fri-mon" is fri, sat, sun, mon.
		for d := a; ; d = (d + 1) % 7 {
			days[d] = true
			if d == b {
				break
			}
		}
	}
	return days, nil
}

// parseDay resolves one three-letter day token.
func parseDay(tok string) (time.Weekday, error) {
	want := strings.ToLower(strings.TrimSpace(tok))
	for d, name := range dayNames {
		if want == name {
			return time.Weekday(d), nil
		}
	}
	return 0, fmt.Errorf(`%q is not a day; use mon tue wed thu fri sat sun, a range like "mon-fri", a list like "mon,wed,fri", or daily / weekdays / weekends`, strings.TrimSpace(tok))
}

// String returns the schedule as configured.
func (s Spec) String() string { return s.raw }

// IsZero reports an unparsed (absent) spec.
func (s Spec) IsZero() bool { return s.raw == "" }

// Next returns the first scheduled moment strictly after t, in t's location.
// Building candidates with time.Date keeps the arithmetic honest across DST:
// a nominal time that does not exist on a transition day is normalised
// forward by the library, and a repeated one fires once, because the search
// after a firing is strictly-after it.
func (s Spec) Next(t time.Time) time.Time {
	for i := 0; i <= 7; i++ {
		c := time.Date(t.Year(), t.Month(), t.Day()+i, s.hour, s.minute, 0, 0, t.Location())
		if s.days[c.Weekday()] && c.After(t) {
			return c
		}
	}
	// Unreachable: at least one weekday is set, so a candidate exists within
	// seven days — but a zero time must never become "fire immediately".
	return time.Date(t.Year(), t.Month(), t.Day()+7, s.hour, s.minute, 0, 0, t.Location())
}

// Prev returns the most recent scheduled moment at or before t — what the
// boot-time missed-while-down check compares the persisted trail against.
func (s Spec) Prev(t time.Time) time.Time {
	for i := 0; i <= 7; i++ {
		c := time.Date(t.Year(), t.Month(), t.Day()-i, s.hour, s.minute, 0, 0, t.Location())
		if s.days[c.Weekday()] && !c.After(t) {
			return c
		}
	}
	return time.Date(t.Year(), t.Month(), t.Day()-7, s.hour, s.minute, 0, 0, t.Location())
}
