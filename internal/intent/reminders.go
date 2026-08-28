package intent

import (
	"fmt"
	"strings"
)

// This file is the router half of one-shot reminders (#141, ADR 0046): the
// fixed phrases that list, check, and read back reminders, and the free-text
// shapes that set and cancel one. Like the focus grammar (#123) the phrases
// are a short, literal list, and the two slots stay bounded: {when} admits
// only what ParseWhen (when.go) recognises — an expression it cannot parse
// is a miss, and the utterance goes to the model, where the reminder.set
// tool claims natural phrasings — and {text} is capped free text that goes
// nowhere but the reminder store: never an argv, never a shell, never a
// dispatch.
//
// The time is extracted by code, not the model: a set match carries the raw
// {when} words for the reminders service to parse again (the same one table)
// and resolve against its own clock, so the router needs no clock and stays
// the immutable, lock-free table it has always been.

// ReminderAction names what a matched reminder phrase asks for. The engine
// performs none of these itself — it hands the match to the reminder runner
// — so the constants are the entire contract between grammar and service.
type ReminderAction string

// The reminder actions the built-in table uses.
const (
	// ReminderNone is an intent that is not a reminder action.
	ReminderNone ReminderAction = ""
	// ReminderSet creates a one-shot reminder: Match.ReminderWhen holds the
	// raw time words (already known to parse), Match.ReminderText the text.
	ReminderSet ReminderAction = "set"
	// ReminderList speaks the pending reminders, soonest first.
	ReminderList ReminderAction = "list"
	// ReminderHistory speaks what fired today, from the capped history.
	ReminderHistory ReminderAction = "history"
	// ReminderDue claims and speaks every reminder whose moment has arrived —
	// the phrase the scheduler's delivery replays through the session path,
	// and an honest "no reminders are due" when a person says it themselves.
	ReminderDue ReminderAction = "due"
	// ReminderCancel cancels the reminder the free text names (fuzzy match;
	// ambiguity asks which).
	ReminderCancel ReminderAction = "cancel"
)

// ReminderCheckPhrase is the utterance a reminder delivery replays through
// the scheduled-session path — a sentence the user could equally have spoken,
// so the record reads the same whether the clock or the voice asked (the
// focus firing's rule, ADR 0041). Exported for the daemon's firing path.
const ReminderCheckPhrase = "reminder check"

// maxReminderWords bounds the {text} slot: a reminder is a sentence fragment
// ("call the pharmacy about the prescription"), the parked-thought bound.
const maxReminderWords = 12

// reminderFixedTable is the half of the reminder grammar with no free text.
// These compile with the built-ins and enter the collision set, so a routine
// or custom intent claiming "reminder check" is a config error naming both
// owners, never a coin toss.
func reminderFixedTable() []struct {
	name     string
	action   ReminderAction
	patterns []string
} {
	return []struct {
		name     string
		action   ReminderAction
		patterns []string
	}{
		{
			name: "reminder.list", action: ReminderList,
			patterns: []string{
				"what reminders do i have", "what are my reminders",
				"list my reminders", "do i have any reminders",
			},
		},
		{
			name: "reminder.history", action: ReminderHistory,
			patterns: []string{"what reminders fired today", "what fired today"},
		},
		{
			name: "reminder.due", action: ReminderDue,
			patterns: []string{ReminderCheckPhrase},
		},
	}
}

// reminderTextEntry is one free-text reminder pattern: literal words around a
// {text} slot, a {when} slot, or both.
type reminderTextEntry struct {
	name     string
	action   ReminderAction
	patterns []string
}

// reminderTextTable is the free-text half of the reminder grammar. It
// compiles last with the other slot patterns, so every literal phrase wins
// over the slots — including the focus grammar's own "remind me where i am
// every {minutes} minutes", which stays a focus check-in because it compiled
// with the built-ins and is tried first.
//
// The {when} slot is what keeps a mid-utterance split deterministic: the
// matcher tries the shortest reading and backtracks, and only a split whose
// when-words actually parse can win — so "remind me to pick up the kids at
// school at three" puts "at school" in the errand and "at three" on the
// clock, because "at school at three" is not a time.
func reminderTextTable() []reminderTextEntry {
	return []reminderTextEntry{
		{
			name: "reminder.set", action: ReminderSet,
			patterns: []string{
				"remind me {when} to {text}",
				"remind me {when} that {text}",
				"remind me to {text} {when}",
			},
		},
		{
			name: "reminder.cancel", action: ReminderCancel,
			patterns: []string{
				"cancel the {text} reminder",
				"cancel my {text} reminder",
				"cancel the reminder to {text}",
				"cancel the reminder about {text}",
			},
		},
	}
}

// compileReminder compiles one reminder pattern: literal words around at most
// one {text} slot and at most one {when} slot. Kept separate from compile so
// the slots stay unusable in custom intents and routine phrases (the
// compileCapture precedent) — free text and time words must never reach a
// command.
func compileReminder(raw string) (pattern, error) {
	words := strings.Fields(strings.ToLower(raw))
	if len(words) == 0 {
		return pattern{}, fmt.Errorf("pattern is empty")
	}
	p := pattern{raw: strings.Join(words, " "), tokens: make([]token, 0, len(words))}
	texts, whens := 0, 0
	for _, w := range words {
		switch w {
		case "{text}":
			texts++
			p.tokens = append(p.tokens, token{kind: slotText, min: 1, max: maxReminderWords})
			continue
		case "{when}":
			whens++
			p.tokens = append(p.tokens, token{kind: slotWhen, min: 2, max: maxWhenWords})
			continue
		}
		if strings.ContainsAny(w, "{}") {
			return pattern{}, fmt.Errorf("unknown placeholder %q in a reminder pattern", w)
		}
		norm := normalize(w)
		if len(norm) != 1 {
			return pattern{}, fmt.Errorf("word %q is not a plain spoken word", w)
		}
		p.tokens = append(p.tokens, token{word: norm[0]})
	}
	if texts > 1 || whens > 1 {
		return pattern{}, fmt.Errorf("reminder patterns carry at most one {text} and one {when} slot")
	}
	if texts == 0 && whens == 0 {
		return pattern{}, fmt.Errorf("reminder text patterns need a slot; fixed phrases belong in the fixed table")
	}
	if p.tokens[0].kind != slotNone {
		return pattern{}, fmt.Errorf("pattern must begin with a word, not a placeholder")
	}
	return p, nil
}
