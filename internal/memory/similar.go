package memory

import "strings"

// This file is the supersede matcher. "Actually the staging server is
// helios" must *update* the stored "the staging server is called atlas", not
// sit beside it as a contradiction — and the first step is noticing the two
// statements are about the same thing.
//
// The matcher is deliberately shallow: lowercase significant words, count
// the overlap. Embeddings and fuzzy matching are out of scope (the issue
// says so), and a deterministic rule has a property they lack — it can be
// tested in both directions and never drifts. It also does not need to be
// clever, because it does not decide anything: a match only means the model
// is *shown* the candidates and asked to choose update-or-new deliberately
// (see the memory.remember tool). A false positive costs one extra tool
// round; a false negative costs a duplicate the user can still correct.

// stopwords are the glue words that connect facts without being about
// anything: matching on them would relate every fact to every other. "user"
// is here because the model phrases facts as "the user's ..." — it appears
// in most facts and distinguishes none.
var stopwords = map[string]bool{
	"a": true, "an": true, "the": true,
	"is": true, "are": true, "was": true, "were": true, "be": true, "been": true,
	"it": true, "its": true, "this": true, "that": true, "these": true, "those": true,
	"my": true, "your": true, "our": true, "their": true, "his": true, "her": true,
	"i": true, "im": true, "me": true, "we": true, "you": true, "they": true,
	"of": true, "to": true, "in": true, "on": true, "at": true, "for": true,
	"and": true, "or": true, "not": true, "no": true, "with": true, "as": true,
	"by": true, "from": true, "now": true, "actually": true,
	"called": true, "named": true, "user": true, "users": true,
}

// significantWords reduces a fact to the words that carry its subject:
// lowercased, punctuation stripped, stopwords and single characters dropped.
func significantWords(s string) map[string]bool {
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		letter := 'a' <= r && r <= 'z'
		digit := '0' <= r && r <= '9'
		return !letter && !digit
	})
	out := make(map[string]bool, len(words))
	for _, w := range words {
		if len(w) < 2 || stopwords[w] {
			continue
		}
		out[w] = true
	}
	return out
}

// similar reports whether two statements look like they are about the same
// thing: two significant words in common, or one when either statement has
// only a couple to offer ("my terminal is ghostty" vs "my terminal is
// alacritty"). Symmetric by construction.
func similar(a, b string) bool {
	wa, wb := significantWords(a), significantWords(b)
	shared := 0
	for w := range wa {
		if wb[w] {
			shared++
		}
	}
	if shared == 0 {
		return false
	}
	min := len(wa)
	if len(wb) < min {
		min = len(wb)
	}
	if min <= 2 {
		return true // shared >= 1 already established
	}
	return shared >= 2
}

// matchesQuery reports whether a recall query finds a fact: a
// case-insensitive substring, or any significant word in common. Looser than
// similar on purpose — "what do you know about the staging server" must find
// the fact, and a listing that over-matches costs a line, not a contradiction.
func matchesQuery(query, content string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return true
	}
	if strings.Contains(strings.ToLower(content), q) {
		return true
	}
	words := significantWords(content)
	for w := range significantWords(query) {
		if words[w] {
			return true
		}
	}
	return false
}
