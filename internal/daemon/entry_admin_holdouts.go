package daemon

// This file declares the last two config.toml families that still needed a
// text editor (issue #164): the user's own spoken phrases and the words the
// voice mispronounces.
//
//	[[intents.custom]] — a phrase, a shell command, and an acknowledgement
//	                     (ADR 0017). An ARRAY family like routines and scripts,
//	                     but the only one whose identity is not a `name`: a
//	                     custom intent IS its phrase, so the row declares
//	                     idKey "match" and the generic pipeline addresses it by
//	                     that. Collisions are the router's own — the whole
//	                     document is compiled by the real Router on every
//	                     validate — so the phrase field says who already owns a
//	                     taken phrase, in the loader's words.
//	[tts.lexicon]      — a written form and how to say it (#30). The first
//	                     SCALAR-MAP family: an entry is one `key = "value"`
//	                     line, the third document shape ADR 0052 anticipated.
//
// Both are registry rows and nothing else. Neither adds a verb, a handler, a
// write path or a validator of its own — which is the claim ADR 0033 made, ADR
// 0052 tested with a second shape, and this file tests with a third.
//
// Neither is reachable by the assistant, and the two reasons are different:
//
//   - A custom intent runs a shell command that the user never sees again once
//     the phrase is spoken. #109's exclusion wall has always named
//     `[[intents.custom]]` (configadmin.go says so in prose); this makes the
//     wall structural for the entry surface too, the way ADR 0052 did for [ai]
//     and [advisors].
//   - The lexicon is not walled off at all — the model can already respell a
//     word through the `tts.lexicon` SETTING, which is a whole-table write it
//     has had since #105. What it does not get is a second, per-entry route to
//     the same table, because two write paths to one table is the duplication
//     the registry exists to prevent. The reason says so rather than pretending
//     to a prohibition that is not there.

import (
	"fmt"
	"strings"
)

// customIntentFamily is the [[intents.custom]] row: one user-defined phrase.
//
// `run` is a shell command and is written verbatim from the form. That is not
// a hole in ADR 0030's "nothing spoken reaches an argv" rule but its other
// side: the rule is about SPEECH becoming an argument, and this is a command
// the user typed, which the permission gate classifies at run time exactly as
// it classifies one the model proposes (ADR 0017). The phrase is literal and
// has no slots, so there is nowhere for a spoken word to enter it.
var customIntentFamily = entryFamilySpec{
	family: "intents.custom", kind: "intent", shape: entryShapeArray,
	idKey: "match", phraseKeys: []string{"match"},
	keys: map[string]entryKeyKind{
		"match": entryKeyString, "run": entryKeyString, "say": entryKeyString,
	},
	keyOrder: []string{"match", "run", "say"},
	assistantReason: "the assistant may not add, change, or remove spoken commands; " +
		"[[intents.custom]] entries run shell commands and are edited in the window's " +
		"Automations tab",
}

// lexiconFamily is the [tts.lexicon] row: one written form and its spoken one.
var lexiconFamily = entryFamilySpec{
	family: "tts.lexicon", kind: "pronunciation", shape: entryShapeScalarMap,
	valueKey: "spoken",
	keys: map[string]entryKeyKind{
		"name": entryKeyString, "spoken": entryKeyString,
	},
	keyOrder: []string{"name", "spoken"},
	notes:    lexiconNotes,
	assistantReason: "the assistant changes pronunciations through the tts.lexicon setting, " +
		"which writes the whole table; the per-word form is the window's",
}

// lexiconNotes states what a lexicon entry will actually do to ordinary speech,
// on the field that decides it.
//
// The lexicon matches on word boundaries and case-insensitively, over every
// sentence Jarvix says — so an entry for a technical term nobody else uses is
// invisible, and an entry for a word that appears in ordinary English rewrites
// that word everywhere, forever. "Read" respelled to help with a product name
// changes every "I read your note". The form cannot know which the user meant,
// so it does not guess: it says what the entry will do and lets them save it.
func lexiconNotes(name string, draft map[string]any) []entryNote {
	written := strings.TrimSpace(name)
	if written == "" {
		written = strings.TrimSpace(entryDraftString(draft, "name"))
	}
	folded := strings.ToLower(written)
	if folded == "" {
		return nil
	}
	switch {
	case commonEnglishWords[folded]:
		return []entryNote{{Field: "name", Message: fmt.Sprintf(
			"%q is an ordinary English word, and the lexicon respells EVERY whole word it "+
				"matches, in every sentence — so this changes how %q is said even when you "+
				"meant something else by it. Keep it only if that is what you want.",
			written, written)}}
	case len(folded) <= 2 && isPlainWord(folded):
		return []entryNote{{Field: "name", Message: fmt.Sprintf(
			"%q is very short, and the lexicon respells every whole word it matches — a "+
				"one- or two-letter written form will turn up in ordinary sentences far more "+
				"often than you expect.", written)}}
	}
	return nil
}

// entryDraftString reads a string key out of a loosely-typed draft.
func entryDraftString(draft map[string]any, key string) string {
	s, _ := draft[key].(string)
	return s
}

// isPlainWord reports whether s is letters only — the shape the short-word
// warning is about. "k9s" is short but is nobody's ordinary word.
func isPlainWord(s string) bool {
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return s != ""
}

// commonEnglishWords is the warning's list: words frequent enough in ordinary
// English that respelling one is a decision rather than a typo fix.
//
// It is deliberately a SHORT list of very common words rather than a
// dictionary. A dictionary would warn about "kubernetes" the day it is added to
// one, and a warning that fires on the entries this feature exists for is a
// warning people learn to click past. Everything here would appear in a normal
// sentence within a paragraph or two, which is exactly the claim the note
// makes.
var commonEnglishWords = map[string]bool{
	"a": true, "about": true, "after": true, "again": true, "all": true,
	"also": true, "an": true, "and": true, "any": true, "are": true,
	"as": true, "at": true, "back": true, "be": true, "because": true,
	"been": true, "before": true, "but": true, "by": true, "call": true,
	"came": true, "can": true, "come": true, "could": true, "day": true,
	"did": true, "do": true, "does": true, "done": true, "down": true,
	"each": true, "even": true, "every": true, "find": true, "first": true,
	"for": true, "from": true, "get": true, "give": true, "go": true,
	"going": true, "good": true, "got": true, "had": true, "has": true,
	"have": true, "he": true, "her": true, "here": true, "him": true,
	"his": true, "how": true, "i": true, "if": true, "in": true,
	"into": true, "is": true, "it": true, "its": true, "just": true,
	"keep": true, "know": true, "last": true, "left": true, "let": true,
	"like": true, "little": true, "long": true, "look": true, "made": true,
	"make": true, "man": true, "many": true, "may": true, "me": true,
	"mean": true, "might": true, "more": true, "most": true, "much": true,
	"must": true, "my": true, "need": true, "never": true, "new": true,
	"next": true, "no": true, "not": true, "now": true, "of": true,
	"off": true, "old": true, "on": true, "once": true, "one": true,
	"only": true, "open": true, "or": true, "other": true, "our": true,
	"out": true, "over": true, "own": true, "part": true, "people": true,
	"place": true, "play": true, "put": true, "read": true, "right": true,
	"said": true, "same": true, "say": true, "see": true, "set": true,
	"she": true, "should": true, "show": true, "side": true, "so": true,
	"some": true, "still": true, "such": true, "take": true, "tell": true,
	"than": true, "that": true, "the": true, "their": true, "them": true,
	"then": true, "there": true, "these": true, "they": true, "thing": true,
	"think": true, "this": true, "those": true, "through": true, "time": true,
	"to": true, "today": true, "too": true, "two": true, "under": true,
	"up": true, "us": true, "use": true, "used": true, "very": true,
	"want": true, "was": true, "way": true, "we": true, "well": true,
	"went": true, "were": true, "what": true, "when": true, "where": true,
	"which": true, "while": true, "who": true, "why": true, "will": true,
	"with": true, "work": true, "would": true, "year": true, "yes": true,
	"you": true, "your": true,
}
