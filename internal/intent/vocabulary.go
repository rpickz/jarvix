package intent

import (
	"fmt"
	"strings"
)

// This file is the router's half of the taught vocabulary (issue #129): the
// deterministic phrases that teach a word ("when I say quid I mean pounds"),
// flag one as hard to hear ("listen for the word quid"), and list what has
// been taught. Like the capture and window-name rules, only the raw spoken
// words travel: the router decides *whether* an utterance teaches, and the
// engine hands the words to the vocabulary seam, which owns storage,
// supersede, and every spoken sentence.
//
// What is deliberately NOT here: taught phrases never enter this grammar.
// A taught word must not rewrite deterministic command matching — "quid"
// meaning "pounds" cannot make "volume quid" mean anything — so the
// collision guarantees of this table are untouched by teaching (the stated
// out-of-scope of #129). Synonym substitution, if ever wanted, is its own
// ticket against this comment.

// Intent names for logs and the intent.executed event.
const (
	// VocabTeachIntentName identifies "when i say X i mean Y".
	VocabTeachIntentName = "vocabulary.teach"
	// VocabListenIntentName identifies "listen for the word X".
	VocabListenIntentName = "vocabulary.listen"
	// VocabListIntentName identifies "what words have i taught you".
	VocabListIntentName = "vocabulary.list"
)

// maxVocabMeaningWords bounds the meaning slot. A little wider than a name
// slot — "pounds sterling, the currency" is a legitimate meaning — but still
// tight: past eight words the utterance is far more likely a sentence for
// the model than a definition, and an unbounded slot would claim it.
const maxVocabMeaningWords = 8

// vocabTeachPatterns are the teach utterances: literal lead words, the
// phrase slot, a literal separator, the meaning slot. A short list on the
// built-in table's terms — every entry is a sentence a person actually says,
// and a near-synonym is a code change with a test. All of them open with
// "when i say": that anchor is what keeps ordinary sentences ("I mean it")
// out of the grammar entirely.
var vocabTeachPatterns = []struct {
	lead string
	sep  string
}{
	{"when i say", "i mean"},
	{"when i say", "it means"},
	{"when i say", "that means"},
	{"if i say", "i mean"},
	{"if i say", "it means"},
}

// vocabListenPatterns flag a taught phrase as hard to hear (the STT-bias
// half of #129). Trailing-text like the capture patterns, and all of them
// name "the word"/"the phrase": a bare "listen for {x}" is far more likely a
// sentence for the model ("listen for a moment") than a flag, and ambiguity
// belongs to the model, never this table.
var vocabListenPatterns = []string{
	"listen for the word {name}",
	"listen for the phrase {name}",
	"listen out for the word {name}",
}

// vocabListPatterns list the taught vocabulary. Fully literal — owned, so a
// custom intent or routine wanting one is refused naming this owner, exactly
// like the window-name listing phrases.
var vocabListPatterns = []string{
	"what words have i taught you",
	"what words did i teach you",
	"which words have i taught you",
	"what vocabulary have i taught you",
	"list my taught words",
}

// compileVocabTeach compiles one teach pattern: the literal lead as ordinary
// tokens, plus the separator the two free-text slots hinge on. Kept apart
// from compile so the two-slot shape stays unusable in custom intents and
// routine phrases, where free text would have to be interpolated into a
// command.
func compileVocabTeach(lead, sep string) (pattern, error) {
	p, err := compile(lead)
	if err != nil {
		return pattern{}, err
	}
	sepWords := strings.Fields(strings.ToLower(sep))
	if len(sepWords) == 0 {
		return pattern{}, fmt.Errorf("teach pattern %q has an empty separator", lead)
	}
	p.raw = lead + " {phrase} " + sep + " {meaning}"
	p.vocabSep = sepWords
	return p, nil
}

// matchVocab matches a teach pattern: the literal lead, then a phrase of one
// to maxNameWords words, the separator, and a meaning of one to
// maxVocabMeaningWords words. The separator must occur exactly once in the
// words after the lead — "when I say I mean it I mean I am serious" has two
// readings, and ambiguity always belongs to the model, so the router
// declines to claim it (the strictness that makes every claim certain).
func (p pattern) matchVocab(fields []string) (phrase, meaning string, ok bool) {
	if len(fields) < len(p.tokens) {
		return "", "", false
	}
	for i, t := range p.tokens {
		if fields[i] != t.word {
			return "", "", false
		}
	}
	rest := fields[len(p.tokens):]
	at := -1
	for i := 0; i+len(p.vocabSep) <= len(rest); i++ {
		if !sepAt(rest, i, p.vocabSep) {
			continue
		}
		if at >= 0 {
			return "", "", false // two separators: two readings, not ours
		}
		at = i
	}
	if at < 0 {
		return "", "", false
	}
	phraseWords := rest[:at]
	meaningWords := rest[at+len(p.vocabSep):]
	if len(phraseWords) < 1 || len(phraseWords) > maxNameWords {
		return "", "", false
	}
	if len(meaningWords) < 1 || len(meaningWords) > maxVocabMeaningWords {
		return "", "", false
	}
	return strings.Join(phraseWords, " "), strings.Join(meaningWords, " "), true
}

// sepAt reports whether sep occurs in fields at offset i.
func sepAt(fields []string, i int, sep []string) bool {
	for j, w := range sep {
		if fields[i+j] != w {
			return false
		}
	}
	return true
}
