package session

import (
	"regexp"
	"sort"
	"strings"
)

// The pronunciation lexicon: the one place a mispronounced word gets fixed.
//
// A neural TTS engine guesses at words it was not trained on, and technical
// vocabulary is exactly the vocabulary Jarvix says most: Kokoro reads "Golang"
// with the vowel of *posh* rather than *going*. Rather than patch the engine,
// the spoken form is respelled before synthesis — which also means the next
// mispronunciation is fixed by config, without touching code (issue #30).
//
// Matching is case-insensitive and on word boundaries, so a listed term never
// corrupts a longer word that contains it: "sudo" must not turn "sudoku" into
// "soo doo ku".

// defaultLexicon is the shipped respelling table: the words this assistant
// says on a normal day, in the phonetic spelling the engine reads correctly.
// User entries under [tts.lexicon] are merged over it, so any of these can be
// overridden — including back to the original word.
var defaultLexicon = map[string]string{
	"golang":     "go lang",
	"kubernetes": "koo ber net eez",
	"nginx":      "engine ex",
	"postgresql": "post gres queue ell",
	"hyprland":   "hyper land",
	"wayland":    "way land",
	"pipewire":   "pipe wire",
	"sudo":       "soo doo",
}

// speechLexicon is a compiled respelling table. One regexp holds every term,
// so a sentence costs a single pass however many terms are configured — this
// runs per sentence on the streaming path.
type speechLexicon struct {
	terms map[string]string // lower-cased term → spoken form
	re    *regexp.Regexp    // nil when there is nothing to match
}

// newSpeechLexicon merges user entries over the shipped defaults and compiles
// the matcher. Terms are compared case-insensitively, so "Golang", "golang"
// and "GoLang" are one entry — the last spelling written wins the value, and
// a user entry always beats a default.
//
// A user's spoken form is stripped of backticks and asterisks: the contract
// that markdown never reaches the engine holds regardless of what is in the
// config file.
func newSpeechLexicon(user map[string]string) *speechLexicon {
	terms := make(map[string]string, len(defaultLexicon)+len(user))
	for term, spoken := range defaultLexicon {
		terms[term] = spoken
	}
	for term, spoken := range user {
		term = strings.ToLower(strings.TrimSpace(term))
		if term == "" {
			continue // an empty term would match everywhere; ignore it
		}
		terms[term] = strings.TrimSpace(stripSpeechMarkers(spoken))
	}

	patterns := make([]string, 0, len(terms))
	for term := range terms {
		patterns = append(patterns, term)
	}
	// Longest first so an entry never shadows a longer one that starts the
	// same way; alphabetical within a length keeps the compiled form stable.
	sort.Slice(patterns, func(i, j int) bool {
		if len(patterns[i]) != len(patterns[j]) {
			return len(patterns[i]) > len(patterns[j])
		}
		return patterns[i] < patterns[j]
	})
	for i, term := range patterns {
		patterns[i] = boundedPattern(term)
	}

	lex := &speechLexicon{terms: terms}
	if len(patterns) == 0 {
		return lex
	}
	// QuoteMeta makes every term literal, so this cannot fail — but a broken
	// pattern must degrade to "no lexicon" rather than take the daemon down
	// with it.
	re, err := regexp.Compile(`(?i)` + strings.Join(patterns, "|"))
	if err != nil {
		return lex
	}
	lex.re = re
	return lex
}

// boundedPattern quotes a term and anchors it to word boundaries. The anchor
// is only added where the term's own edge is a word character: \b is defined
// against ASCII word characters, so anchoring a term that starts with "+" or
// an accented letter would make it unmatchable rather than precise.
func boundedPattern(term string) string {
	pattern := regexp.QuoteMeta(term)
	if isWordByte(term[0]) {
		pattern = `\b` + pattern
	}
	if isWordByte(term[len(term)-1]) {
		pattern += `\b`
	}
	return pattern
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// apply respells every listed term found in s.
func (l *speechLexicon) apply(s string) string {
	if l == nil || l.re == nil {
		return s
	}
	return l.re.ReplaceAllStringFunc(s, func(match string) string {
		if spoken, ok := l.terms[strings.ToLower(match)]; ok {
			return spoken
		}
		return match
	})
}

// speechNormalizer turns assistant text into what Jarvix actually says:
// markdown stripped, mispronounced terms respelled, numbers expanded. It is
// immutable once built and safe to share across sessions; a configuration
// change builds a new one (Engine.Reconfigure) rather than mutating this.
type speechNormalizer struct {
	lex *speechLexicon
}

// newSpeechNormalizer compiles a normalizer with the given user lexicon
// merged over the shipped defaults. A nil map is the shipped defaults alone.
func newSpeechNormalizer(lexicon map[string]string) *speechNormalizer {
	return &speechNormalizer{lex: newSpeechLexicon(lexicon)}
}

// text renders one piece of assistant text as its spoken form.
//
// Order matters. Markdown goes first, so a term inside `code` or **bold** is
// still respelled. The lexicon goes before numbers, so a user's respelling of
// a term that contains digits wins over the generic number rules. Nothing
// here touches the overlay or window text: this is the spoken form only.
func (n *speechNormalizer) text(s string) string {
	s = markdownProse(s)
	if n != nil {
		s = n.lex.apply(s)
	}
	s = spokenNumbers(s)
	return strings.TrimSpace(reMultiSpace.ReplaceAllString(s, " "))
}

// spokenForm is the engine's spoken-text entry point: the configured lexicon
// where there is one, the shipped defaults otherwise (an Engine built without
// NewEngine still speaks).
func (e *Engine) spokenForm(text string) string {
	if e.speech == nil {
		return speechText(text)
	}
	return e.speech.text(text)
}
