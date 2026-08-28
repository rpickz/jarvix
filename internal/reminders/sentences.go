package reminders

import (
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/intent"
)

// Every sentence the reminder feature speaks is composed here, daemon-side,
// from the store's own record (ADR 0013): a reminder can be wrong only if
// the record is, never because something was invented. Clock words and
// durations come from the intent package's spoken tables, so reminders count
// and date things on exactly the scale every other Jarvix surface uses.

// dueSpoken words a pending reminder's moment for listings and the tab:
// "at three this afternoon", "at nine tomorrow morning", or "due now" for a
// moment already owed.
func dueSpoken(due, now time.Time) string {
	if !due.After(now) {
		return "due now"
	}
	// Reuse the resolver's own phrasing by treating the stored moment as an
	// exact clock reading — the same words the creation confirmation spoke,
	// recomputed against now so "this afternoon" stays true as days pass.
	w := intent.When{Hour: due.Hour(), Minute: due.Minute()}
	if _, say := w.Resolve(now); due.Sub(now) < 24*time.Hour {
		return say
	}
	return "at " + intent.SpokenClock(due.Hour(), due.Minute()) + " on " + due.Weekday().String()
}

// dueClause words a fired reminder's original moment for the late and
// boot-catch-up sentences: "at three", "yesterday at three", "on Monday at
// three".
func dueClause(due, now time.Time) string {
	clock := "at " + intent.SpokenClock(due.Hour(), due.Minute())
	ny, nm, nd := now.Date()
	dy, dm, dd := due.Date()
	if ny == dy && nm == dm && nd == dd {
		return clock
	}
	if yy, ym, yd := now.AddDate(0, 0, -1).Date(); yy == dy && ym == dm && yd == dd {
		return "yesterday " + clock
	}
	return "on " + due.Weekday().String() + " " + clock
}

// listSpoken is "what reminders do I have": soonest first, capped, counted.
func listSpoken(pending []Reminder, now time.Time) string {
	if len(pending) == 0 {
		return "No reminders set."
	}
	items := make([]string, 0, len(pending))
	for _, r := range pending {
		if len(items) == maxSpokenList {
			break
		}
		items = append(items, r.Text+", "+dueSpoken(r.Due, now))
	}
	var line string
	if len(pending) == 1 {
		line = "One reminder: " + items[0]
	} else {
		line = capitalise(intent.SpokenNumber(len(pending))) + " reminders: " +
			strings.Join(items, "; then ")
	}
	if extra := len(pending) - len(items); extra > 0 {
		line += "; and " + intent.SpokenNumber(extra) + " more"
	}
	return line + "."
}

// historySpoken is "what fired today": delivered reminders whose moment of
// delivery falls on today's calendar day. Cancelled entries stay in the file
// for the tab, but a cancellation is not a firing and is not read here.
func historySpoken(fired []Fired, now time.Time) string {
	ny, nm, nd := now.Date()
	var today []Fired
	for _, f := range fired {
		if f.Outcome != OutcomeFired {
			continue
		}
		if fy, fm, fd := f.At.Date(); fy == ny && fm == nm && fd == nd {
			today = append(today, f)
		}
	}
	if len(today) == 0 {
		return "No reminders fired today."
	}
	items := make([]string, 0, len(today))
	for _, f := range today {
		items = append(items, f.Text+", "+dueClause(f.Due, now))
	}
	if len(today) == 1 {
		return "One reminder fired today: " + items[0] + "."
	}
	return capitalise(intent.SpokenNumber(len(today))) + " reminders fired today: " +
		strings.Join(items, "; ") + "."
}

// claimSpoken composes one delivery announcement: the boot catch-up sentence
// for reminders missed while no daemon ran, then the ordinary lines — plain
// when on time, honest about the delay when a live session held them past
// the grace period ("marked deferred in the spoken line", the acceptance
// criterion).
func claimSpoken(boot, claimed []Fired, now time.Time) string {
	var parts []string
	if len(boot) > 0 {
		asks := make([]string, 0, len(boot))
		for _, f := range boot {
			asks = append(asks, "to "+f.Text+" "+dueClause(f.Due, now))
		}
		parts = append(parts, "While I was off: you asked me to remind you "+
			strings.Join(asks, ", and ")+".")
	}
	for i, f := range claimed {
		lead := "Reminder"
		if i > 0 || len(boot) > 0 {
			lead = "Also"
		}
		if f.Late {
			lead += ", " + intent.SpokenDuration(now.Sub(f.Due)) + " late"
		}
		parts = append(parts, lead+": "+f.Text+".")
	}
	return strings.Join(parts, " ")
}

// capitalise upper-cases the first letter for sentence starts (ASCII is
// enough — everything it receives starts with a template word or a number
// word).
func capitalise(s string) string {
	if s == "" {
		return s
	}
	if c := s[0]; c >= 'a' && c <= 'z' {
		return string(c-32) + s[1:]
	}
	return s
}
