package undo

import (
	"fmt"
	"time"
)

// How the account says when something happened (#210).
//
// It lives in this package rather than in whichever surface renders the
// account, and that is the whole of ADR 0013 applied to a timestamp: the
// window, the CLI and anything later all have to say the same thing about the
// same row, and a client that formatted its own would be the one place in this
// feature where two readers of one file could disagree about when an action
// took place. The window in particular has no clock it can trust — it renders
// what a daemon on the other side of a socket sent, and the difference between
// "just now" and "yesterday" is exactly the fact a person deciding whether to
// put something back is weighing.

// Ago phrases how long ago an action happened, for a surface that LISTS the
// account: "just now", "4 minutes ago", "yesterday", "3 weeks ago".
//
// Deliberately not knowledge.SpokenAge, which words the same idea for a speech
// engine and spells its numbers out because a synthesiser reads digits badly.
// This one is read rather than heard, and a person scanning a hundred rows is
// comparing the numbers between them — which is easier in digits, in a column
// of the same width.
func Ago(now, at time.Time) string {
	d := now.Sub(at)
	switch {
	case at.IsZero():
		// A row whose time did not survive a hand-edit. Saying "56 years ago"
		// would be arithmetic on a value that means "nobody wrote one".
		return "at a time I no longer have"
	case d < 0:
		// A hand-edited file, a restored backup, or a clock that has been put
		// back. "just now" would be the confident wrongness this whole feature
		// exists to remove, so it reports what it actually found instead.
		return "later than now, by this machine's clock"
	case d < time.Minute:
		return "just now"
	case d < 2*time.Minute:
		return "a minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d/time.Minute))
	case d < 2*time.Hour:
		return "an hour ago"
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d/time.Hour))
	case d < 48*time.Hour:
		return "yesterday"
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d/(24*time.Hour)))
	case d < 14*24*time.Hour:
		return "last week"
	default:
		return fmt.Sprintf("%d weeks ago", int(d/(7*24*time.Hour)))
	}
}
