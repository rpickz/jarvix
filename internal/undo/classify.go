package undo

import (
	"sort"
	"strings"
)

// This file answers one question, before the fact: **is this decision
// one-way?**
//
// It exists because of the half of #201 that is easy to skip. Recording what
// happened is the obvious feature; saying which decisions cannot be taken
// back *at the moment the user makes them* is the one that changes anything.
// A manager who learns after the fact that a choice was irreversible has
// learned something useless. So the confirmation card carries the warning
// before approval, and this table is where it comes from.
//
// It is keyed by string literal rather than by the internal/tools constants
// for internal/desktop/pending.go's reason, inverted: internal/tools imports
// this package, so naming the constants here would be an import cycle.
// classify_tools_test.go is an external test in internal/tools that pins
// every literal to its constant, the same guard the toolPhrases table
// carries, so a tool rename cannot silently demote a one-way action to an
// unclassified one.

// Nature is what a tool does to the machine.
type Nature int

const (
	// NatureUnknown is a tool this table does not classify. It says nothing
	// on the card, and that silence is deliberate: a guess about whether an
	// unfamiliar capability can be undone is worse than no claim at all, and
	// the failure mode of guessing "reversible" is a user who approved a
	// one-way change believing otherwise.
	NatureUnknown Nature = iota
	// NatureReadOnly changes nothing, so there is nothing to undo and nothing
	// to warn about. Listed rather than left unknown so that adding a
	// capability without thinking about this is visible as a gap.
	NatureReadOnly
	// NatureReversible can be put back, and the account will carry what would
	// do it.
	NatureReversible
	// NatureIrreversible cannot. The card says so before approval; the
	// account says so afterwards, in the same words.
	NatureIrreversible
)

// natures classifies every registered tool.
//
// The read-only entries are here for the reason stated at NatureReadOnly: a
// table with holes in it cannot tell "we decided this changes nothing" from
// "nobody looked". classify_tools_test.go fails when a tool the registry can
// hold is missing from this map.
var natures = map[string]Nature{
	// Reads.
	"desktop.list_windows":   NatureReadOnly,
	"desktop.list_apps":      NatureReadOnly,
	"desktop.list_managed":   NatureReadOnly,
	"memory.search":          NatureReadOnly,
	"conversations.search":   NatureReadOnly,
	"config.list_entries":    NatureReadOnly,
	"config.get_entry":       NatureReadOnly,
	"config.read_settings":   NatureReadOnly,
	"reminder.list":          NatureReadOnly,
	"briefing.get":           NatureReadOnly,
	"situation.get":          NatureReadOnly,
	"knowledge.get":          NatureReadOnly,
	"advisor.ask":            NatureReadOnly,
	"thinking.ask_deep":      NatureReadOnly,
	"desktop.release_window": NatureReadOnly,

	// Changes Jarvix can put back.
	"config.write_entry":    NatureReversible,
	"config.delete_entry":   NatureReversible,
	"config.write_setting":  NatureReversible,
	"memory.remember":       NatureReversible,
	"memory.forget":         NatureReversible,
	"vocabulary.teach":      NatureReversible,
	"vocabulary.forget":     NatureReversible,
	"reminder.set":          NatureReversible,
	"reminder.cancel":       NatureReversible,
	"desktop.move_window":   NatureReversible,
	"desktop.name_window":   NatureReversible,
	"desktop.manage_window": NatureReversible,
	"artifact.create":       NatureReversible,

	// One-way.
	"shell.run":            NatureIrreversible,
	"script.run":           NatureIrreversible,
	"routine.run":          NatureIrreversible,
	"intent.run":           NatureIrreversible,
	"typing.type_text":     NatureIrreversible,
	"typing.press_key":     NatureIrreversible,
	"desktop.close_window": NatureIrreversible,
	"desktop.launch_app":   NatureIrreversible,
	"knowledge.refresh":    NatureIrreversible,
}

// oneWayReasons is why each one-way tool is one way, in the clause the user
// hears on the card. One sentence each, and each says the actual reason
// rather than "this is irreversible" — a warning that does not say what it is
// warning about is a warning people learn to click past.
var oneWayReasons = map[string]string{
	"shell.run":            "a command that has run has run — I can't take it back",
	"script.run":           "a script that has run has run — I can't take it back",
	"routine.run":          "a routine that has run has run — I can't take back what its steps did",
	"intent.run":           "a command that has run has run — I can't take it back",
	"typing.type_text":     "I can't un-type this; whatever it lands in has it",
	"typing.press_key":     "I can't un-press a key; whatever it lands in has it",
	"desktop.close_window": "I can't reopen a window with what was in it",
	"desktop.launch_app":   "I can close it again, but I can't undo it having started",
	"knowledge.refresh":    "the old contents of the feed are gone once it refetches",
}

// Classify reports what a tool does to the machine.
func Classify(tool string) Nature {
	n, ok := natures[strings.TrimSpace(tool)]
	if !ok {
		return NatureUnknown
	}
	return n
}

// CardNote is the clause the confirmation card and the spoken question carry
// for a one-way action, and "" for everything else — reversible, read-only,
// and, deliberately, unknown.
//
// It rides the card's existing summary sentence rather than a new field, and
// that is a decision rather than an expedient (ADR 0064): the daemon owns the
// wording (ADR 0013), the summary is what every surface already renders — the
// window's card, the overlay, the spoken question, the CLI's `jarvix confirm`
// — and a new field would have reached exactly one of them and left the
// others quietly silent about the thing that matters most.
func CardNote(tool string) string {
	if Classify(tool) != NatureIrreversible {
		return ""
	}
	reason, ok := oneWayReasons[strings.TrimSpace(tool)]
	if !ok {
		return oneWayLead + "."
	}
	return oneWayLead + ": " + reason + "."
}

// oneWayLead is the clause both notes below open with, so the short spoken
// form is literally a prefix of the written one and a user who hears one and
// then reads the other is not told the same thing two ways.
const oneWayLead = "This can't be undone"

// SpokenNote is the same warning for the abbreviated spoken question — the
// lead clause, without the reason.
//
// Two forms, one table, and the split is deliberate. A screen can afford the
// reason and is better for having it: a warning that does not say what it is
// warning about is a warning people learn to click past. Audio cannot. The
// default spoken prompt exists to be SHORT (issue #119) — "May I run a shell
// command? The details are on screen." — and shell.run is the most-asked
// question there is, so a full clause appended to every one of them would be
// a sentence users learn to talk over, which is the same failure in a
// different medium.
//
// What is never abbreviated away is the fact itself, because it is the one
// part of the question a person cannot recover by looking at the screen
// afterwards: by then they have already answered.
func SpokenNote(tool string) string {
	if Classify(tool) != NatureIrreversible {
		return ""
	}
	return oneWayLead + "."
}

// Annotate appends the one-way clause to a confirmation summary. A summary
// that already carries it is returned untouched, so a caller that annotates
// twice — the spoken question and the event are built from the same string —
// does not say it twice.
func Annotate(tool, summary string) string {
	note := CardNote(tool)
	if note == "" {
		return summary
	}
	if strings.Contains(summary, note) {
		return summary
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return note
	}
	return summary + " " + note
}

// OneWay is the Restore for an action that cannot be undone, worded from the
// same table the confirmation card read.
//
// One table, two moments: the card says "this can't be undone: a command that
// has run has run" before the user answers, and the account says the same
// words afterwards. Two tables would eventually disagree, and the
// disagreement would be a user told one thing at approval and another at
// review — which is precisely the failure the whole feature is written
// against.
func OneWay(tool string) Restore {
	reason, ok := oneWayReasons[strings.TrimSpace(tool)]
	if !ok {
		reason = "I didn't keep anything that would restore it"
	}
	return Restore{Kind: KindNone, Because: reason}
}

// ClassifiedTools lists every tool the table knows, sorted, for the test that
// pins it against the registry.
func ClassifiedTools() []string {
	out := make([]string, 0, len(natures))
	for name := range natures {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
