package intent

import (
	"strconv"
	"strings"
)

// Number handling for intent slots.
//
// Whisper transcribes spoken numbers inconsistently — "volume thirty" and
// "volume 30" are the same utterance said the same way — so the router has to
// understand both or the table would work only for whichever form the model
// happened to emit. Parsing is deliberately narrow: the ordinary English forms
// for 0–150 and plain digits, nothing else. A number the parser does not
// recognise is a miss, and a miss goes to the model, so being conservative
// costs nothing.

// maxParsedNumber caps what parseNumber will return at all. Per-slot bounds
// (volume 0–150, workspace 1–10) are applied by the pattern matcher; this
// bound only stops an absurd digit string from becoming an integer.
const maxParsedNumber = 999

// smallWords are the numbers with their own name, 0–19. "oh" and "nought" are
// there because that is how people read a zero aloud.
var smallWords = map[string]int{
	"zero": 0, "nought": 0, "oh": 0,
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
	"eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14,
	"fifteen": 15, "sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19,
}

// tensWords are the multiples of ten. "fourty" is accepted alongside "forty"
// because it is the commonest misspelling an STT engine emits.
var tensWords = map[string]int{
	"twenty": 20, "thirty": 30, "forty": 40, "fourty": 40, "fifty": 50,
	"sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
}

// smallNames and tensNames are the inverse, for spoken acknowledgements.
var smallNames = [20]string{
	"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine",
	"ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen",
	"seventeen", "eighteen", "nineteen",
}

var tensNames = [10]string{
	"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety",
}

// parseNumber reads a number from consecutive normalized words. It accepts a
// digit string ("30", "150") or English words ("thirty", "thirty five",
// "one hundred and fifty", "a hundred", "nine hundred and ninety nine"). ok is
// false for anything else — the caller treats that as no match.
//
// The word forms reach the same 0–maxParsedNumber the digit form does, which
// is what makes this and SpokenNumber inverse: anything Jarvix can say, it can
// read back. Per-slot bounds are the caller's job, not this function's.
func parseNumber(words []string) (int, bool) {
	if len(words) == 0 {
		return 0, false
	}
	if len(words) == 1 {
		if n, err := strconv.Atoi(words[0]); err == nil {
			if n < 0 || n > maxParsedNumber {
				return 0, false
			}
			return n, true
		}
	}

	rest := words
	hundreds := 0
	switch {
	case rest[0] == "hundred":
		hundreds, rest = 100, rest[1:]
	case len(rest) > 1 && rest[1] == "hundred":
		// "a hundred", "one hundred" … "nine hundred". The multiplier used to
		// stop at one, which left SpokenNumber and parseNumber — the "one copy
		// of words-to-numbers" this file exists to be — non-inverse over their
		// own declared range: SpokenNumber(999) says "nine hundred and
		// ninety-nine" and nothing here could read it back. The round-trip
		// property in when_property_test.go is what caught it (issue #172),
		// through a reminder confirmed as "in nine hundred and ninety-nine
		// hours" that the user could not repeat.
		//
		// Widening what is UNDERSTOOD cannot loosen any slot: every caller
		// bounds the value afterwards (volume 0–150, workspace 1–10), so
		// "volume nine hundred" is still a miss — it is now a miss for being
		// out of range rather than for being unreadable, which is the same
		// answer arrived at honestly.
		if n, ok := smallWords[rest[0]]; ok && n >= 1 && n <= 9 {
			hundreds, rest = n*100, rest[2:]
		} else if rest[0] == "a" {
			hundreds, rest = 100, rest[2:]
		}
	}
	if hundreds == 0 {
		return parseUnder100(words)
	}
	if len(rest) == 0 {
		return hundreds, true
	}
	if rest[0] == "and" {
		rest = rest[1:]
	}
	tail, ok := parseUnder100(rest)
	if !ok {
		return 0, false
	}
	return hundreds + tail, true
}

// parseUnder100 reads 0–99 as one or two words.
func parseUnder100(words []string) (int, bool) {
	switch len(words) {
	case 1:
		if n, ok := smallWords[words[0]]; ok {
			return n, true
		}
		if n, ok := tensWords[words[0]]; ok {
			return n, true
		}
	case 2:
		tens, ok := tensWords[words[0]]
		if !ok {
			return 0, false
		}
		units, ok := smallWords[words[1]]
		if !ok || units < 1 || units > 9 {
			return 0, false
		}
		return tens + units, true
	}
	return 0, false
}

// SpokenNumber renders an integer the way the acknowledgement should say it
// ("Volume thirty", not "Volume 30"). Speech engines do read numerals, but
// inconsistently — "150" comes out as "one five zero" in some voices — and
// the acknowledgement is the only feedback the user gets that the right value
// landed, so it is worth spelling out.
func SpokenNumber(n int) string {
	switch {
	case n < 0 || n > maxParsedNumber:
		return strconv.Itoa(n)
	case n < 20:
		return smallNames[n]
	case n < 100:
		if n%10 == 0 {
			return tensNames[n/10]
		}
		return tensNames[n/10] + "-" + smallNames[n%10]
	case n%100 == 0:
		return smallNames[n/100] + " hundred"
	}
	return smallNames[n/100] + " hundred and " + SpokenNumber(n%100)
}

// spokenWords is SpokenNumber split the way normalize would split it, used by
// tests to prove every value 0–150 round-trips.
func spokenWords(n int) []string {
	return strings.Fields(strings.ReplaceAll(SpokenNumber(n), "-", " "))
}
