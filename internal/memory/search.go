package memory

import (
	"sort"
	"strings"
)

// This file is memory.search's ranking (ADR 0037). The ranking is pure code
// — no model judgement, no randomness, no clock — so the same query against
// the same book always returns the same facts in the same order, and every
// property of it can be pinned by a test. It is deliberately shallow, the
// similar-matcher's philosophy applied to retrieval: token overlap plus a
// phrase bonus is enough to put the right fact first in a 200-fact book, and
// a scheme that simple can be reasoned about when it is wrong. Embeddings
// stay out of scope (the issue says so); the retrieval stats this feature
// records are exactly the evidence a future semantic layer would be judged
// against.

// maxSearchResults caps one search's answer. Ten facts is more than any
// question needs and keeps the tool result — which the model reads back into
// its context — from becoming a second injection block.
const maxSearchResults = 10

// Scoring weights. Integers on purpose: integer arithmetic has no rounding
// order to argue about, so determinism is structural.
const (
	// scoreExactWord is one query word found verbatim among a fact's
	// significant words — the strongest per-word signal.
	scoreExactWord = 2
	// scorePrefixWord is one query word that is a prefix of a fact word
	// ("stag" finds "staging"). Half an exact match, and only counted for
	// words of three letters or more — shorter prefixes match half the
	// dictionary.
	scorePrefixWord = 1
	// scorePhrase is the whole query appearing verbatim (case-insensitive)
	// inside the content: the user or model quoting the fact should beat
	// facts that merely share its vocabulary.
	scorePhrase = 3
)

// minPrefixLen is the shortest query word allowed to prefix-match.
const minPrefixLen = 3

// rankSearch scores every fact against query and returns copies of the
// matches, best first. Zero-score facts are excluded — an empty result is
// the honest answer, never padding. Ties break exactly like the injection
// order (most recently confirmed first, then stored, then id), so a query
// matching many facts equally prefers the ones the user touched last.
func rankSearch(query string, facts []Fact) []Fact {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	qWords := significantWords(query)

	type scored struct {
		fact  Fact
		score int
	}
	matches := make([]scored, 0, len(facts))
	for _, f := range facts {
		s := scoreFact(q, qWords, f.Content)
		if s > 0 {
			matches = append(matches, scored{fact: copyFact(f), score: s})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		a, b := matches[i].fact, matches[j].fact
		if !a.Updated.Equal(b.Updated) {
			return a.Updated.After(b.Updated)
		}
		if !a.Stored.Equal(b.Stored) {
			return a.Stored.After(b.Stored)
		}
		return a.ID < b.ID
	})
	out := make([]Fact, len(matches))
	for i, m := range matches {
		out[i] = m.fact
	}
	return out
}

// scoreFact scores one fact's content against the lowercased query q and its
// significant words. The per-word loop sums fixed contributions, so map
// iteration order cannot change the total — determinism does not depend on
// iteration order anywhere in this file.
func scoreFact(q string, qWords map[string]bool, content string) int {
	cWords := significantWords(content)
	s := 0
	for w := range qWords {
		switch {
		case cWords[w]:
			s += scoreExactWord
		case len(w) >= minPrefixLen && anyHasPrefix(cWords, w):
			s += scorePrefixWord
		}
	}
	if strings.Contains(strings.ToLower(content), q) {
		s += scorePhrase
	}
	return s
}

// anyHasPrefix reports whether any word in words starts with prefix.
func anyHasPrefix(words map[string]bool, prefix string) bool {
	for w := range words {
		if strings.HasPrefix(w, prefix) {
			return true
		}
	}
	return false
}
