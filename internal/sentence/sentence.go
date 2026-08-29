// Package sentence holds the shape half of a model-sentence contract: the part
// that is about English rather than about any one feature's facts.
//
// Two features now ask a model for exactly one sentence and refuse it when it
// says more than the facts support — the return briefing's headline (#150, ADR
// 0050) and the situation report's (#196, ADR 0061). The *claims* half of each
// contract is different, because it is a question about that feature's own
// categories and counts. The shape half is identical, and it has to stay
// identical: "did the model answer with one sentence, and what number did it
// say?" has one right answer, and two copies of it would drift in the direction
// the honesty rules cannot afford — one feature tightening while the other
// quietly stopped catching something.
//
// So the vocabulary-free machinery lives here and the vocabularies stay with
// the features that own them. Nothing in this package knows what a category is.
package sentence

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// One extracts the single sentence a model was asked for: leading list markers
// and labels removed, everything after the first sentence dropped.
//
// It is tolerant rather than refusing, the recap contract's own stance (ADR
// 0043): a model that adds a bullet or a second sentence has still answered,
// and the first sentence is the answer. Refusal is for the claims, which is
// where a wrong answer actually costs something.
func One(reply string) string {
	return firstSentence(stripPreamble(stripListMarker(strings.TrimSpace(reply))))
}

// firstSentence keeps the first sentence and drops the rest.
func firstSentence(text string) string {
	for i, r := range text {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		// Not a sentence end if it is a decimal point or an initial.
		next := i + utf8.RuneLen(r)
		if next < len(text) {
			after, _ := utf8.DecodeRuneInString(text[next:])
			if !unicode.IsSpace(after) {
				continue
			}
		}
		return strings.TrimSpace(text[:next])
	}
	return strings.TrimSpace(text)
}

// stripListMarker removes a leading bullet or an enumerator like "1." — the
// most common way a model ignores "no lists" while otherwise complying. It is
// deliberately narrow: a bare leading digit is a *count*, and trimming it would
// turn "3 sessions finished" into a sentence about no sessions at all.
func stripListMarker(text string) string {
	rest := strings.TrimLeft(text, "-*• \t")
	if rest != text && rest != "" {
		return rest
	}
	digits := 0
	for digits < len(text) && text[digits] >= '0' && text[digits] <= '9' {
		digits++
	}
	if digits == 0 || digits >= len(text) {
		return text
	}
	if text[digits] != '.' && text[digits] != ')' {
		return text
	}
	trimmed := strings.TrimSpace(text[digits+1:])
	if trimmed == "" {
		return text
	}
	return trimmed
}

// stripPreamble removes a leading "Headline:"-style label. Only a short one: a
// colon deep in a real sentence is punctuation, not a label.
func stripPreamble(text string) string {
	idx := strings.Index(text, ":")
	if idx <= 0 || idx > 24 {
		return text
	}
	if strings.ContainsAny(text[:idx], ".!?") {
		return text
	}
	return strings.TrimSpace(text[idx+1:])
}

// Negators are the words that turn a claim into its denial. They are exported
// because a feature's tests want to reason about them, and they live here
// because they are English rather than anything about a briefing or a report.
//
// They exist because a guard has to let the model say the *true* thing about an
// empty category — "nothing has finished" is honest, and refusing it would
// leave the plain reading speaking on every quiet moment the model got right.
var Negators = []string{"nothing", "none", "no ", "not ", "n't", "never", "without", "neither"}

// negationWindow is how far back a negator counts. Long enough for "nothing of
// yours has finished", short enough that a denial in the first clause does not
// licence an invention in the second.
const negationWindow = 40

// ClaimedPositively reports whether the lower-cased sentence makes the given
// claim without denying it. Every occurrence is checked: a sentence that denies
// one and asserts another is still asserting one.
//
// Callers pass substrings rather than words on purpose — the point is to catch
// a claim however it is inflected ("finishes", "finished", "has finished") —
// and a false positive costs a duller sentence while a false negative costs a
// spoken untruth.
func ClaimedPositively(lower, claim string) bool {
	from := 0
	for {
		idx := strings.Index(lower[from:], claim)
		if idx < 0 {
			return false
		}
		at := from + idx
		start := at - negationWindow
		if start < 0 {
			start = 0
		}
		if !containsAny(lower[start:at], Negators) {
			return true
		}
		from = at + len(claim)
		if from >= len(lower) {
			return false
		}
	}
}

func containsAny(text string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

// Numbers extracts the integers a lower-cased sentence states, in digits or in
// words. words is the caller's number-word table, indexed by the value each
// word names ("zero", "one", …): only those are recognised, because a sentence
// that reaches for a number outside the table is describing something the
// caller's facts do not have.
func Numbers(lower string, words []string) []int {
	var found []int
	digits := 0
	inDigits := false
	for _, r := range lower + " " {
		if r >= '0' && r <= '9' {
			digits = digits*10 + int(r-'0')
			inDigits = true
			continue
		}
		if inDigits {
			found = append(found, digits)
			digits, inDigits = 0, false
		}
	}
	for _, word := range strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r)
	}) {
		for n, name := range words {
			if word == name {
				found = append(found, n)
			}
		}
	}
	return found
}
