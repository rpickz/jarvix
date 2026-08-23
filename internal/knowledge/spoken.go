package knowledge

import (
	"fmt"
	"time"
)

// This file phrases a value's age the way a person would say it — "four
// minutes ago", "an hour ago" — the sub-day counterpart of the archive
// search's spokenWhen (ADR 0028), which starts at "earlier today" because a
// conversation's age is a calendar question. A feed value's age is not: the
// difference between four minutes and four hours is the whole point of
// disclosing it, so this scale is elapsed time, in words, because the
// sentence is destined for a speech engine.

// SpokenAge phrases how long ago when was, relative to now: "just now",
// "four minutes ago", "an hour ago", "nineteen hours ago", "yesterday",
// "five days ago", "three weeks ago". A healthy feed never gets past the
// hours; the far end exists so a long-failing feed's last value is still
// disclosed honestly.
func SpokenAge(now, when time.Time) string {
	d := now.Sub(when)
	switch {
	case d < time.Minute:
		return "just now"
	case d < 2*time.Minute:
		return "a minute ago"
	case d < time.Hour:
		return spokenCount(int(d/time.Minute)) + " minutes ago"
	case d < 2*time.Hour:
		return "an hour ago"
	case d < 24*time.Hour:
		return spokenCount(int(d/time.Hour)) + " hours ago"
	case d < 48*time.Hour:
		return "yesterday"
	case d < 7*24*time.Hour:
		return spokenCount(int(d/(24*time.Hour))) + " days ago"
	case d < 14*24*time.Hour:
		return "a week ago"
	default:
		return spokenCount(int(d/(7*24*time.Hour))) + " weeks ago"
	}
}

// Number words for the counts SpokenAge speaks. Minutes cap at fifty-nine,
// so ones, teens and tens compose everything needed; anything the tables
// somehow miss falls back to digits, which the speech normaliser reads
// correctly anyway.
var (
	onesWords = [...]string{"zero", "one", "two", "three", "four", "five", "six", "seven",
		"eight", "nine", "ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen",
		"sixteen", "seventeen", "eighteen", "nineteen"}
	tensWords = [...]string{2: "twenty", 3: "thirty", 4: "forty", 5: "fifty"}
)

// spokenCount renders a small count in words: "four", "twenty-five".
func spokenCount(n int) string {
	switch {
	case n >= 0 && n < len(onesWords):
		return onesWords[n]
	case n < 60:
		tens, ones := n/10, n%10
		if ones == 0 {
			return tensWords[tens]
		}
		return tensWords[tens] + "-" + onesWords[ones]
	default:
		return fmt.Sprint(n)
	}
}
