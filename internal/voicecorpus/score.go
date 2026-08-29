package voicecorpus

import (
	"strings"
	"unicode"
)

// Score reports how much of a phrase survived into its transcript: the
// fraction of the spoken words that appear in what whisper wrote, ignoring
// order, case and punctuation.
//
// It is a TRACKING number, not an assertion, and the distinction is the whole
// reason it can exist at all. Nothing in this package ever requires a score of
// 1, or of anything in particular: the pass/fail verdict comes from the
// downstream outcome (Evaluate), and a phrase can score 0.6 and still route to
// exactly the right intent, which is a perfectly good day. What the score is
// for is drift — a bias prompt that got worse, a model swap that costs a few
// words a phrase, a microphone that has started clipping. Those show up as a
// number falling before they show up as an outcome flipping, and a baseline
// that records only pass/fail cannot see them coming.
//
// Recall rather than an edit distance, and unordered rather than aligned, for
// the same reason the outcome assertions avoid exact transcripts: whisper is
// entitled to write "9.2 million" for "nine point two million", to add a full
// stop, and to capitalise what it likes. A metric that punished any of that
// would be a metric people learned to ignore. Words the transcript adds are
// not counted against it either — an inserted "um" is not a recognition
// failure worth a red number.
//
// Score is 0 for an empty transcript and 1 for an empty phrase (nothing was
// asked for, nothing was lost).
func Score(say, transcript string) float64 {
	want := scoreWords(say)
	if len(want) == 0 {
		return 1
	}
	got := make(map[string]int, len(want))
	for _, w := range scoreWords(transcript) {
		got[w]++
	}
	matched := 0
	for _, w := range want {
		// Multiset, not set: "no no" only scores full marks against a
		// transcript that heard the word twice.
		if got[w] > 0 {
			got[w]--
			matched++
		}
	}
	return float64(matched) / float64(len(want))
}

// scoreWords folds text into the comparable words Score counts.
//
// Apostrophes survive inside a word so "don't" stays one token and matches
// whisper's own "don't"; every other punctuation mark is a separator, which is
// what makes "later, reply" and "later reply" the same two words. Digits are
// kept as they are — no attempt is made to reconcile "four" with "4", because
// that reconciliation is the intent router's job and it is already asserted
// there, as a slot value, where getting it wrong actually matters.
func scoreWords(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' && r != '’'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.Trim(strings.ReplaceAll(f, "’", "'"), "'"); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// containsWord reports whether text contains word as a whole word, folded the
// same way Score folds. This is what an expect.words entry means: the taught
// term, the nickname or the scale word came out of whisper intact — not that
// the transcript looks like anything in particular.
func containsWord(text, word string) bool {
	want := scoreWords(word)
	if len(want) != 1 {
		// Validate refuses multi-word entries, so this is unreachable from a
		// valid manifest; refusing rather than substring-matching keeps it
		// that way if it ever becomes reachable.
		return false
	}
	for _, w := range scoreWords(text) {
		if w == want[0] {
			return true
		}
	}
	return false
}
