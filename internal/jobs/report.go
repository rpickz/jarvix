package jobs

import (
	"fmt"
	"strings"
)

// What a job says about itself.
//
// **Every sentence in this file is composed from the ledger.** No model is
// consulted, at any state, ever — and that is a decision rather than an
// omission. The prose in a situation report is model-worded because its facts
// are fed to it one line at a time and a wrong headline is a wrong headline; a
// job's report is the account of unsupervised work, and #71 is the scar that
// says what a model does with an account it is asked to summarise. So the facts
// are gathered by the runner as each step ended, written to disk before the next
// one began, and read back here verbatim.
//
// The rule that follows from it, and the one worth stating out loud: **a job
// that cannot verify what it did says so, first.** An unverified step is not
// quietly counted as done and not quietly dropped. It leads the report, because
// "I did nine things and I can't tell you whether the tenth happened" is a
// different report from "I did ten things", and the listener has to be able to
// tell them apart without asking.

// Spoken is the short honest account of one job for the ear: what it is doing
// now, what it has done, and what it is waiting for. It is what the situation
// report carries and what "how's the tidy job?" answers with.
func (j Job) Spoken() string {
	switch j.State {
	case Parked:
		return j.parkedLine()
	case Ready, Running:
		return capitalise(j.Name) + " is running: " + j.Progress() + "."
	case Done:
		return capitalise(j.Name) + " has finished. " + j.Report()
	case Stopped:
		return capitalise(j.Name) + " stopped. " + j.Report()
	case Failed:
		return capitalise(j.Name) + " failed. " + j.Report()
	default:
		return capitalise(j.Name) + " is in a state I can't describe, which is a fault of mine."
	}
}

// parkedLine says what a parked job is waiting for, in the words of whoever
// parked it. The lead-in distinguishes a question the user can answer from a
// boundary they cannot, because those need different things from them.
func (j Job) parkedLine() string {
	ask := strings.TrimSpace(j.Question.Ask)
	if ask == "" {
		ask = "something I can no longer describe"
	}
	if j.Question.Why.Answerable() {
		return capitalise(j.Name) + " is waiting on you: " + ask +
			" It has done " + j.Progress() + " so far."
	}
	return capitalise(j.Name) + " has stopped and needs you: " + ask +
		" It had done " + j.Progress() + " before that."
}

// Title is the job's handle as a heading — the user typed it in whatever case
// they liked, and every surface that leads a sentence with it needs the same
// answer. One implementation, because two would disagree the first time
// somebody names a job "CI".
func (j Job) Title() string { return capitalise(j.Name) }

// Progress counts what actually happened, and never rounds up. A step that
// read something is a step; a step that changed something is a change; the two
// are counted separately because conflating them lets a job that looked at
// forty files describe itself as having done forty things.
//
// Exported since #221 because a listing needs the same count the spoken line
// carries, and a surface that counted the ledger itself would be the second
// place this arithmetic lives.
func (j Job) Progress() string {
	steps, acted, unsure := len(j.Ledger), j.Acted(), j.Unverified()
	if steps == 0 {
		return "nothing yet"
	}
	clause := fmt.Sprintf("%d %s", steps, plural(steps, "step", "steps"))
	if acted > 0 {
		clause += fmt.Sprintf(", %d of which changed something", acted)
	}
	if unsure > 0 {
		clause += fmt.Sprintf(", and %d I can't confirm either way", unsure)
	}
	return clause
}

// ---------------------------------------------------------------------------
// The offers
// ---------------------------------------------------------------------------

// What a surface may offer to do with this job, and — when it may not — the
// one sentence saying why.
//
// These exist for #221's window and are used by the Runner's own refusals, and
// that pairing is the whole point (ADR 0066's `Undoer.Offer`, restated for
// work rather than for reversal): a control the listing withholds and an
// action the runner declines must not explain the same rule two ways. One
// function, two callers — the surface asks before it draws a button, and the
// verb asks again when the button is pressed, because a listing composed a
// moment ago cannot promise what the job is doing now.

// StopOffer reports whether stopping this job would do anything, and the
// sentence to show when it would not.
func (j Job) StopOffer() (bool, string) {
	if j.State.Live() {
		return true, ""
	}
	return false, j.Title() + " has already " + endedWord(j.State) + "."
}

// AnswerOffer reports whether the user saying something can resume this job,
// and the sentence to show when it cannot.
//
// Two refusals, and they are different facts a reader acts on differently: a
// job that is not parked is not waiting for anybody, and a job parked on a
// boundary or a denial is waiting for a decision an answer cannot supply — the
// way out of those is a new job with a scope that admits the work, which is a
// thing the user does deliberately rather than a yes they nod through.
func (j Job) AnswerOffer() (bool, string) {
	if j.State != Parked {
		return false, j.Title() + " is not waiting on anything."
	}
	if !j.Question.Why.Answerable() {
		return false, j.Title() + " stopped because " +
			strings.TrimSuffix(strings.TrimSpace(j.Question.Ask), ".") +
			", which isn't something I can carry on from."
	}
	return true, ""
}

// endedWord words a finished state for the refusals above.
func endedWord(s State) string {
	switch s {
	case Done:
		return "finished"
	case Stopped:
		return "been stopped"
	default:
		return "ended"
	}
}

// Report is the account of a finished job: what was done, what was not, and
// what it could not do.
//
// All three halves, always, because a report that named only the successes
// would read as if the whole direction had been carried out — the same argument
// the job-scoped undo makes about its two halves (ADR 0064), applied to the
// work rather than to its reversal.
func (j Job) Report() string {
	var parts []string
	if unsure := j.Unverified(); unsure > 0 {
		// First, deliberately. It is the one thing a listener cannot recover
		// from being told late.
		parts = append(parts, fmt.Sprintf("There %s %d %s I started and never saw the end of, so I can't tell you whether %s happened",
			was(unsure), unsure, plural(unsure, "step", "steps"), theyIt(unsure)))
	}
	done := j.summaries(func(e Entry) bool { return e.Verified && !e.Failed })
	failed := j.summaries(func(e Entry) bool { return e.Verified && e.Failed })
	switch {
	case len(done) == 0 && len(failed) == 0 && len(parts) == 0:
		return "It did nothing at all."
	case len(done) == 0:
		parts = append(parts, "Nothing I tried worked")
	default:
		parts = append(parts, "I did "+joinNaturally(done, "and"))
	}
	if len(failed) > 0 {
		parts = append(parts, "I couldn't "+joinNaturally(failed, "or"))
	}
	if closing := strings.TrimSpace(j.Closing); closing != "" {
		parts = append(parts, strings.TrimSuffix(closing, "."))
	}
	return strings.Join(parts, ". ") + "."
}

// summaries lists what the matching ledger entries say they did, in the words
// the step itself carried.
//
// It uses the model's Intent line rather than the tool's raw output, and that
// is the one place a job's own words survive into a report — but only for steps
// the runner VERIFIED, and only as a label on a fact it independently holds.
// The claim being made is "this step ran and reported success"; the intent is
// how it is named, not the evidence that it happened. An intent with no verified
// step behind it never reaches this function.
func (j Job) summaries(keep func(Entry) bool) []string {
	seen := make(map[string]bool, len(j.Ledger))
	out := make([]string, 0, len(j.Ledger))
	for _, e := range j.Ledger {
		if !keep(e) {
			continue
		}
		label := strings.TrimSpace(e.Intent)
		if label == "" {
			label = "run " + e.Tool
		}
		label = strings.TrimSuffix(label, ".")
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, label)
	}
	return out
}

// Stated is the whole job read back before it starts: the goal in the user's
// own words and the boundary it will be held to. It is what the confirmation
// shows, and it is deliberately one sentence a person can hear and judge.
func (j Job) Stated() string {
	return fmt.Sprintf("I'll work on %q as a job called %s, %s. "+
		"I'll stop and come back to you before anything I can't undo.",
		j.Goal, j.Name, j.Scope.Stated())
}

// plural picks a word for a count.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// was picks the verb for a count.
func was(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// theyIt picks the pronoun for a count.
func theyIt(n int) string {
	if n == 1 {
		return "it"
	}
	return "they"
}

// capitalise upper-cases the first letter of a sentence that starts with a
// job's name, which the user typed in whatever case they liked.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
