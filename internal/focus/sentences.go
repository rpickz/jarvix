package focus

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/knowledge"
)

// Every sentence the focus feature speaks is composed here, daemon-side, from
// the thread's own record (ADR 0013, restated by ADR 0041): a recap can be
// wrong only if the record is, never because something was invented. The
// AI-session summary (#124) lives in recap.go behind its own gates — these
// templated sentences remain the floor every recap falls back to, fast,
// predictable, and honest.
//
// Numbers are rendered with intent.SpokenNumber and ages with
// knowledge.SpokenAge, so a thread recap counts and dates things on exactly
// the scale every other Jarvix surface uses.

// maxStatusThreads bounds how many threads "where am I on everything" names.
// One short line each at a spoken ~150 words a minute puts six lines safely
// inside the ~15-second budget; the rest are counted, not read.
const maxStatusThreads = 6

// maxSpokenParked bounds how many parked thoughts one answer reads aloud.
const maxSpokenParked = 5

// createAck confirms a new thread and what it is anchored to. anchorNote is
// the disclosed degradation when windows were asked for but could not be
// seen — the thread exists either way, and saying so plainly beats implying
// an anchor that is not there.
func createAck(th Thread, wanted int, anchorNote string) string {
	ack := "New thread: " + th.Name + "."
	switch {
	case len(th.Anchors) > 0:
		ack += " Anchored to " + anchorList(th.Anchors) + "."
	case wanted > 0 && anchorNote != "":
		ack += " " + capitalise(anchorNote) + "."
	}
	return ack
}

// anchorAck confirms a re-anchor of the active thread.
func anchorAck(th Thread) string {
	return capitalise(th.Name) + " is anchored to " + anchorList(th.Anchors) + "."
}

// switchRecap is the re-entry sentence pair: where this thread stands, read
// from its record as it was before this switch touched it. At most two
// sentences — a recap that takes longer to hear than the re-orientation it
// replaces has failed at its one job.
func switchRecap(prior Thread, alive map[string]bool, now time.Time) string {
	if prior.LastSwitched.IsZero() && len(prior.Parked) == 0 {
		// No history to recap: an honest fresh start, never a fabricated one.
		return "The " + prior.Name + " thread — fresh thread, nothing parked yet."
	}
	first := "Back on " + prior.Name
	if !prior.LastSwitched.IsZero() {
		first += " — last here " + knowledge.SpokenAge(now, prior.LastSwitched)
	}
	if clause := anchorClause(prior.Anchors, alive); clause != "" {
		first += clause
	}
	first += "."
	if len(prior.Parked) == 0 {
		return first
	}
	newest := prior.Parked[len(prior.Parked)-1]
	if len(prior.Parked) == 1 {
		return first + " One parked: " + newest.Text + "."
	}
	return first + " " + capitalise(intent.SpokenNumber(len(prior.Parked))) +
		" parked; newest: " + newest.Text + "."
}

// checkRecap is the same story without the switch — what a check-in reminder
// speaks, and what "where am I on X" answers.
func checkRecap(th Thread, alive map[string]bool, now time.Time) string {
	if th.LastSwitched.IsZero() && len(th.Parked) == 0 {
		return capitalise(th.Name) + ": fresh thread, nothing parked yet."
	}
	line := capitalise(th.Name) + ": last touched " + knowledge.SpokenAge(now, th.LastActivity)
	if clause := anchorClause(th.Anchors, alive); clause != "" {
		line += clause
	}
	line += "."
	if len(th.Parked) == 0 {
		return line
	}
	newest := th.Parked[len(th.Parked)-1]
	if len(th.Parked) == 1 {
		return line + " One parked: " + newest.Text + "."
	}
	return line + " " + capitalise(intent.SpokenNumber(len(th.Parked))) +
		" parked; newest: " + newest.Text + "."
}

// anchorClause reads the anchor state against the live inventory: named when
// present, named as gone when the window no longer exists. alive is nil when
// the desktop could not be read, and then the clause stays silent — an
// unreadable inventory must never be spoken as a vanished window.
func anchorClause(anchors []Anchor, alive map[string]bool) string {
	if len(anchors) == 0 {
		return ""
	}
	if alive == nil {
		return ", anchored to " + anchorList(anchors)
	}
	var here, gone []Anchor
	for _, a := range anchors {
		if alive[a.Address] {
			here = append(here, a)
		} else {
			gone = append(gone, a)
		}
	}
	switch {
	case len(gone) == 0:
		return ", anchored to " + anchorList(here)
	case len(here) == 0 && len(gone) == 1:
		return "; its " + gone[0].App + " window is gone"
	case len(here) == 0:
		return "; its anchored windows are gone"
	default:
		return ", anchored to " + anchorList(here) + "; its " + gone[0].App + " window is gone"
	}
}

// anchorList names one or two anchors: "Alacritty", "Alacritty and Firefox".
func anchorList(anchors []Anchor) string {
	names := make([]string, 0, len(anchors))
	for _, a := range anchors {
		name := a.App
		if name == "" {
			name = a.Title
		}
		if name == "" {
			name = "a window"
		}
		names = append(names, name)
	}
	return strings.Join(names, " and ")
}

// parkedSpoken reads a thread's parked list, newest first — the newest is the
// one most likely still relevant, and a capped read must not hide it.
func parkedSpoken(th Thread) string {
	n := len(th.Parked)
	if n == 0 {
		return "Nothing parked on " + th.Name + "."
	}
	items := make([]string, 0, n)
	for i := n - 1; i >= 0 && len(items) < maxSpokenParked; i-- {
		items = append(items, th.Parked[i].Text)
	}
	line := capitalise(intent.SpokenNumber(n)) + " parked on " + th.Name + ": " +
		strings.Join(items, "; ")
	if extra := n - len(items); extra > 0 {
		line += fmt.Sprintf("; and %s older", intent.SpokenNumber(extra))
	}
	return line + "."
}

// statusSpoken is "where am I on everything": one short line per thread,
// active thread first, the rest by most recent activity, bounded to
// maxStatusThreads lines with the remainder counted.
func statusSpoken(p persisted, now time.Time) string {
	if len(p.threads) == 0 {
		return "No threads yet. Say new thread, then a name, to start one."
	}
	ordered := append([]Thread(nil), p.threads...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if (ordered[i].ID == p.active) != (ordered[j].ID == p.active) {
			return ordered[i].ID == p.active
		}
		return ordered[i].LastActivity.After(ordered[j].LastActivity)
	})
	lines := make([]string, 0, len(ordered))
	for _, th := range ordered {
		if len(lines) == maxStatusThreads {
			break
		}
		lines = append(lines, statusLine(th, th.ID == p.active, now))
	}
	out := strings.Join(lines, " ")
	if extra := len(ordered) - len(lines); extra > 0 {
		out += " And " + intent.SpokenNumber(extra) + " more " + plural(extra, "thread", "threads") + "."
	}
	return out
}

// statusLine is one thread's line of the cross-thread status.
func statusLine(th Thread, active bool, now time.Time) string {
	var b strings.Builder
	if active {
		b.WriteString("You're on ")
		b.WriteString(th.Name)
	} else {
		b.WriteString(capitalise(th.Name))
	}
	switch n := len(th.Parked); n {
	case 0:
		b.WriteString(": nothing parked")
	case 1:
		b.WriteString(": one parked")
	default:
		b.WriteString(": " + intent.SpokenNumber(n) + " parked")
	}
	b.WriteString(", touched " + knowledge.SpokenAge(now, th.LastActivity) + ".")
	return b.String()
}

// endAck says what ended and what went with it — the deletion is honest
// about its cost, because parked thoughts die with their thread.
func endAck(th Thread) string {
	ack := "Ended " + th.Name + "."
	switch n := len(th.Parked); n {
	case 0:
	case 1:
		ack += " Its one parked thought went with it."
	default:
		ack += " Its " + intent.SpokenNumber(n) + " parked thoughts went with it."
	}
	return ack
}

// The timebox sentences.

func sessionStartAck(th Thread, minutes int) string {
	return "Focusing on " + th.Name + " for " + intent.SpokenNumber(minutes) + " minutes."
}

func continueAck(th Thread, minutes int) string {
	return "Another " + intent.SpokenNumber(minutes) + " minutes on " + th.Name + "."
}

func midpointLine(th Thread, now, end time.Time) string {
	return "Halfway — " + spokenMinutesLeft(now, end) + " left on " + th.Name + "."
}

func remainingLine(th Thread, now, end time.Time) string {
	return capitalise(spokenMinutesLeft(now, end)) + " left on " + th.Name + "."
}

func closePrompt(th Thread, minutes int) string {
	return "Time — " + intent.SpokenNumber(minutes) + " minutes on " + th.Name +
		". Keep focusing, or take a break?"
}

func breakAck(th Thread) string {
	return "Break. " + capitalise(th.Name) + " is where you left it."
}

func endSessionAck(th Thread, elapsed int) string {
	if elapsed < 1 {
		return "Ended the focus session on " + th.Name + "."
	}
	return "Ended the focus session — " + intent.SpokenNumber(elapsed) +
		" " + plural(elapsed, "minute", "minutes") + " on " + th.Name + "."
}

func remindAck(th Thread, minutes int) string {
	return "I'll check in on " + th.Name + " every " + intent.SpokenNumber(minutes) + " minutes."
}

func remindStopAck(th Thread) string {
	return "No more check-ins on " + th.Name + "."
}

// spokenMinutesLeft rounds the remaining time up to whole minutes — "one
// minute" is the floor, because "zero minutes left" is a close, not a count.
func spokenMinutesLeft(now, end time.Time) string {
	left := end.Sub(now)
	minutes := int((left + time.Minute - 1) / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	return intent.SpokenNumber(minutes) + " " + plural(minutes, "minute", "minutes")
}

// capitalise upper-cases the first letter for sentence starts. ASCII is
// enough: everything it receives is either a template word or a user's name
// for a thread, and a name that starts with a non-ASCII rune passes through
// unchanged rather than mangled.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	if c := s[0]; c >= 'a' && c <= 'z' {
		return string(c-32) + s[1:]
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
