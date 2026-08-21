package session

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Numbers, said the way a person would.
//
// The TTS engine's own normalisation is not up to this job: Kokoro reads
// "9.2 million" as "nine two million" — the decimal point is simply dropped,
// which changes the *meaning* of an answer rather than merely sounding odd.
// An assistant that reports figures ("disk is 82.4% full", "the build took
// 4.7 seconds") is specifically in the business of saying numbers, so the
// expansion happens here, in our normalisation layer, where it is testable
// against a string instead of by ear (issue #30).
//
// Two rules shape what is in scope:
//
//   - Expand what the engine gets wrong or would read as symbols: decimals,
//     percentages, currency, versions, durations, byte sizes, ranges and
//     ordinals. A bare integer is left alone — every engine reads "1024" and
//     "1995" correctly, and expanding them turns a port number or a year into
//     a mouthful ("port eight thousand and eighty").
//   - Never touch a number that belongs to something else. A figure wedged
//     against letters, a slash, a colon or an underscore is part of a path,
//     a version-pinned image, a time, an address or an identifier
//     ("sail-8.5/app", "127.0.0.1:8080", "2026-08-21"), and is passed through
//     untouched. Anything unparseable is passed through too: a bad figure
//     should sound wrong at worst, never fail the session.

// numberPattern matches every number shape that gets expanded, in one pass.
// Alternation order is significance order — Go's regexp is leftmost-first, so
// the more specific shape has to come first, and "1.5GB" must be seen as a
// byte size before it is seen as a decimal.
//
// Word-form units end with \b so a unit is never matched as the prefix of a
// longer word: without it "9.2 million" matches "min" and the whole figure is
// then rejected as "number wedged against letters" — silently reintroducing
// the very bug this file exists to fix.
var numberPattern = regexp.MustCompile(strings.Join([]string{
	// v1.5.2 / 1.5.2 — three or more components is a version, never a decimal
	// and never a date.
	`(?P<version>v?\d+(?:\.\d+){2,})`,
	// v1.5 — the "v" is what makes two components a version rather than a
	// decimal. A bare "v1" is left alone: it is as likely to be a variable.
	`(?P<vprefix>v\d+\.\d+)`,
	// £3.50, $9.2m, €10
	`(?P<money>[£$€]\d+(?:\.\d+)?(?:bn|[kmb])?\b)`,
	`(?P<percent>\d+(?:\.\d+)?%)`,
	`(?P<size>\d+(?:\.\d+)? ?(?:[KMGT]i?B|kB)\b)`,
	// Abbreviations must be attached to the number ("4.7s"); spelled-out
	// units may be spaced ("4.7 seconds"). A spaced "s" is too easily the
	// first letter of the next word to be worth claiming.
	`(?P<duration>\d+(?:\.\d+)?(?: ?(?:milliseconds?|seconds?|minutes?|hours?|secs?|mins?|hrs?)\b|(?:ms|s|h)\b))`,
	`(?P<ordinal>\d+(?:st|nd|rd|th)\b)`,
	`(?P<span>\d+ ?[-\x{2013}] ?\d+)`,
	`(?P<decimal>\d+\.\d+)`,
}, "|"))

// The limits on how much number is worth saying. They are enforced in the
// expansion rather than in the pattern: bounded repeats ({1,15}) inflate the
// compiled automaton enough to cost real time on the streaming path, and a
// figure past these limits is passed through unchanged either way.
const (
	maxSpokenDigits   = 15 // "nine hundred and ninety nine trillion …" is the end of usefulness
	maxSpokenFraction = 10 // digits after the point, each spoken individually
)

// parseSpokenUint reads a run of digits that is short enough to be worth
// saying. Anything longer (or unparseable) means "leave this text alone".
func parseSpokenUint(digits string) (uint64, bool) {
	if len(digits) == 0 || len(digits) > maxSpokenDigits {
		return 0, false
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// numberGroups maps each alternative's name to its submatch index, resolved
// once so the per-sentence path does no string comparison to find out which
// shape matched.
var numberGroups = func() map[string]int {
	groups := make(map[string]int)
	for i, name := range numberPattern.SubexpNames() {
		if name != "" {
			groups[name] = i
		}
	}
	return groups
}()

// spokenNumbers rewrites every number shape it recognises into words. Text
// with no digits at all is returned untouched without allocating — most
// sentences of an answer are exactly that.
func spokenNumbers(s string) string {
	if !strings.ContainsAny(s, "0123456789") {
		return s
	}
	matches := numberPattern.FindAllStringSubmatchIndex(s, -1)
	if matches == nil {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + len(s)/2)
	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		from, prefix, ok := numberPrefix(s, start)
		if !ok || !numberSuffixOK(s, end) {
			continue
		}
		words, ok := expandNumber(s, m)
		if !ok {
			continue
		}
		b.WriteString(s[last:from])
		b.WriteString(prefix)
		b.WriteString(words)
		last = end
	}
	if last == 0 {
		return s // every match was rejected: nothing was copied, nothing changed
	}
	b.WriteString(s[last:])
	return b.String()
}

// numberPrefixReject are the characters that make a following number part of
// something else — a path, an identifier, a version-pinned image, a fraction
// of an address. A currency symbol is in the list because the money rule owns
// those: reaching here with one in front means the money shape did not match,
// and half-expanding a price is worse than leaving it.
const numberPrefixReject = `/\_:.@#~=+*&£$€`

// numberSuffixReject are the characters that mean the number runs on into
// something else. A full stop is deliberately absent — it ends sentences far
// more often than it continues numbers, and a genuine continuation
// ("1.5.2") was already claimed by the version shape.
const numberSuffixReject = `/\_-:@#~=+*&%`

// numberPrefix decides whether what precedes a match lets it be read as a
// number, and returns the offset to copy up to plus any words the prefix
// itself contributes (a minus sign becomes "minus").
func numberPrefix(s string, start int) (from int, words string, ok bool) {
	if start == 0 {
		return start, "", true
	}
	r, size := utf8.DecodeLastRuneInString(s[:start])
	switch {
	case r == '-' || r == '−':
		// A negative number only when nothing word-like precedes the sign:
		// "-3.5" is minus three point five, "sail-8.5" is a name.
		before := start - size
		if before == 0 {
			return before, "minus ", true
		}
		if pr, _ := utf8.DecodeLastRuneInString(s[:before]); unicode.IsSpace(pr) || pr == '(' || pr == '[' {
			return before, "minus ", true
		}
		return 0, "", false
	case unicode.IsLetter(r) || unicode.IsDigit(r):
		return 0, "", false
	case strings.ContainsRune(numberPrefixReject, r):
		return 0, "", false
	}
	return start, "", true
}

// numberSuffixOK reports whether what follows a match lets it be read as a
// number.
func numberSuffixOK(s string, end int) bool {
	if end == len(s) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(s[end:])
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return false
	}
	return !strings.ContainsRune(numberSuffixReject, r)
}

// expandNumber renders one matched shape as words. A false return leaves the
// original text in place.
func expandNumber(s string, m []int) (string, bool) {
	text := s[m[0]:m[1]]
	switch {
	case matched(m, numberGroups["version"]), matched(m, numberGroups["vprefix"]):
		return spellVersion(text)
	case matched(m, numberGroups["money"]):
		return spellMoney(text)
	case matched(m, numberGroups["percent"]):
		n, ok := spellNumber(strings.TrimSuffix(text, "%"))
		return n + " percent", ok
	case matched(m, numberGroups["size"]):
		return spellUnit(text, byteUnits)
	case matched(m, numberGroups["duration"]):
		return spellUnit(text, durationUnits)
	case matched(m, numberGroups["ordinal"]):
		return spellOrdinalText(text)
	case matched(m, numberGroups["span"]):
		return spellSpan(text)
	case matched(m, numberGroups["decimal"]):
		return spellNumber(text)
	}
	return "", false
}

// matched reports whether submatch group i participated in the match.
func matched(m []int, i int) bool { return i > 0 && 2*i < len(m) && m[2*i] >= 0 }

// spellVersion reads a dotted number component by component — "1.5.2" is
// "one point five point two", never "one point fifty-two", never a date.
// The word "version" is only spoken when the text says so ("v1.5"), because
// the same shape is how an IP address is written, and "one hundred and
// twenty seven point nought point nought point one" is how one is read.
func spellVersion(text string) (string, bool) {
	words := make([]string, 0, 8)
	if rest, found := strings.CutPrefix(text, "v"); found {
		words, text = append(words, "version"), rest
	}
	parts := strings.Split(text, ".")
	for i, p := range parts {
		if i > 0 {
			words = append(words, "point")
		}
		n, ok := parseSpokenUint(p)
		if !ok {
			return "", false
		}
		words = append(words, spellCardinal(n))
	}
	return strings.Join(words, " "), true
}

// currency names one of the symbols we expand, with the name of its
// hundredth for the "three pounds fifty" form.
type currency struct{ major, majors, minors string }

var currencies = map[rune]currency{
	'£': {"pound", "pounds", "pence"},
	'$': {"dollar", "dollars", "cents"},
	'€': {"euro", "euros", "cents"},
}

// spellMoney reads a price the way it is said aloud: "£3.50" is "three pounds
// fifty", not "three point five zero pounds". Two decimal places are the
// minor unit; anything else is a plain decimal with the unit after it
// ("$9.2m" → "nine point two million dollars").
func spellMoney(text string) (string, bool) {
	sym, size := utf8.DecodeRuneInString(text)
	cur, ok := currencies[sym]
	if !ok {
		return "", false
	}
	rest := text[size:]

	// Quoted shorthand: "$9.2m" is nine point two million dollars. Longest
	// suffix first, so "bn" is never read as "b" with a stray "n" left over.
	magnitude := ""
	switch lower := strings.ToLower(rest); {
	case strings.HasSuffix(lower, "bn"):
		magnitude, rest = "billion", rest[:len(rest)-2]
	case strings.HasSuffix(lower, "k"):
		magnitude, rest = "thousand", rest[:len(rest)-1]
	case strings.HasSuffix(lower, "m"):
		magnitude, rest = "million", rest[:len(rest)-1]
	case strings.HasSuffix(lower, "b"):
		magnitude, rest = "billion", rest[:len(rest)-1]
	}

	whole, frac, hasFrac := strings.Cut(rest, ".")
	major, ok := parseSpokenUint(whole)
	if !ok {
		return "", false
	}

	if hasFrac && len(frac) == 2 && magnitude == "" {
		minor, ok := parseSpokenUint(frac)
		if !ok {
			return "", false
		}
		switch {
		case major == 0 && minor == 0:
			return "zero " + cur.majors, true
		case major == 0:
			return spellCardinal(minor) + " " + cur.minors, true
		case minor == 0:
			return spellCardinal(major) + " " + pluralise(major, cur.major, cur.majors), true
		default:
			return spellCardinal(major) + " " + pluralise(major, cur.major, cur.majors) +
				" " + spellCardinal(minor), true
		}
	}

	words, spelled := spellNumber(rest)
	if !spelled {
		return "", false
	}
	unit := cur.majors
	if magnitude != "" {
		words += " " + magnitude
	} else if major == 1 && !hasFrac {
		unit = cur.major
	}
	return words + " " + unit, true
}

// byteUnits and durationUnits map a written unit to its spoken singular and
// plural. Keys are matched case-sensitively for byte sizes (kB and KB are
// both kilobytes; "b" alone is left alone, being as often a variable as a
// byte) and case-insensitively for durations.
var byteUnits = map[string][2]string{
	"kB": {"kilobyte", "kilobytes"}, "KB": {"kilobyte", "kilobytes"},
	"MB": {"megabyte", "megabytes"}, "GB": {"gigabyte", "gigabytes"},
	"TB":  {"terabyte", "terabytes"},
	"KiB": {"kibibyte", "kibibytes"}, "MiB": {"mebibyte", "mebibytes"},
	"GiB": {"gibibyte", "gibibytes"}, "TiB": {"tebibyte", "tebibytes"},
}

var durationUnits = map[string][2]string{
	"ms": {"millisecond", "milliseconds"}, "millisecond": {"millisecond", "milliseconds"},
	"milliseconds": {"millisecond", "milliseconds"},
	"s":            {"second", "seconds"}, "sec": {"second", "seconds"},
	"secs": {"second", "seconds"}, "second": {"second", "seconds"},
	"seconds": {"second", "seconds"},
	"min":     {"minute", "minutes"}, "mins": {"minute", "minutes"},
	"minute": {"minute", "minutes"}, "minutes": {"minute", "minutes"},
	"h": {"hour", "hours"}, "hr": {"hour", "hours"}, "hrs": {"hour", "hours"},
	"hour": {"hour", "hours"}, "hours": {"hour", "hours"},
}

// spellUnit expands "<number><unit>" against a unit table, pluralising on the
// value so "1GB" is one gigabyte and "1.5GB" is one point five gigabytes.
func spellUnit(text string, units map[string][2]string) (string, bool) {
	split := lastDigit(text)
	number := strings.TrimSpace(text[:split])
	unit := strings.TrimSpace(text[split:])
	names, ok := units[unit]
	if !ok {
		if names, ok = units[strings.ToLower(unit)]; !ok {
			return "", false
		}
	}
	words, ok := spellNumber(number)
	if !ok {
		return "", false
	}
	name := names[1]
	if number == "1" {
		name = names[0]
	}
	return words + " " + name, true
}

// spellOrdinalText reads "21st" as "twenty first".
func spellOrdinalText(text string) (string, bool) {
	n, ok := parseSpokenUint(text[:lastDigit(text)])
	if !ok {
		return "", false
	}
	return spellOrdinal(n), true
}

// spellSpan reads "3-5" as "three to five" — the range an assistant means
// when it says a build takes 3-5 seconds, not a subtraction.
func spellSpan(text string) (string, bool) {
	sep := strings.IndexAny(text, "-–")
	if sep < 0 {
		return "", false
	}
	_, size := utf8.DecodeRuneInString(text[sep:])
	lo, okLo := spellNumber(strings.TrimSpace(text[:sep]))
	hi, okHi := spellNumber(strings.TrimSpace(text[sep+size:]))
	if !okLo || !okHi {
		return "", false
	}
	return lo + " to " + hi, true
}

// spellNumber reads a plain integer or decimal. Digits after the point are
// read one at a time, which is how a person says them: "3.14" is "three point
// one four", not "three point fourteen".
func spellNumber(text string) (string, bool) {
	whole, frac, hasFrac := strings.Cut(text, ".")
	n, ok := parseSpokenUint(whole)
	if !ok || len(frac) > maxSpokenFraction {
		return "", false
	}
	words := spellCardinal(n)
	if !hasFrac {
		return words, true
	}
	var b strings.Builder
	b.WriteString(words)
	b.WriteString(" point")
	for i := 0; i < len(frac); i++ {
		if !isDigitByte(frac[i]) {
			return "", false
		}
		b.WriteByte(' ')
		b.WriteString(smallNumbers[frac[i]-'0'])
	}
	return b.String(), true
}

var smallNumbers = [...]string{
	"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine",
	"ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen",
	"seventeen", "eighteen", "nineteen",
}

var tensNumbers = [...]string{
	"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety",
}

// scales are the magnitude groups, largest first. uint64 covers every value a
// 15-digit match can hold, so no scale beyond a trillion is reachable.
var scales = []struct {
	value uint64
	name  string
}{
	{1_000_000_000_000, "trillion"},
	{1_000_000_000, "billion"},
	{1_000_000, "million"},
	{1_000, "thousand"},
}

// spellCardinal writes a whole number in British English, where the last
// group under a hundred takes an "and": 1024 is "one thousand and twenty
// four", 1234 is "one thousand two hundred and thirty four".
func spellCardinal(n uint64) string {
	if n < 20 {
		return smallNumbers[n]
	}
	var words []string
	for _, s := range scales {
		if q := n / s.value; q > 0 {
			words = append(words, spellCardinal(q), s.name)
			n %= s.value
		}
	}
	if n > 0 {
		if len(words) > 0 && n < 100 {
			words = append(words, "and")
		}
		words = append(words, spellUnderThousand(n)...)
	}
	return strings.Join(words, " ")
}

// spellUnderThousand writes 1..999.
func spellUnderThousand(n uint64) []string {
	var words []string
	if h := n / 100; h > 0 {
		words = append(words, smallNumbers[h], "hundred")
		n %= 100
		if n > 0 {
			words = append(words, "and")
		}
	}
	switch {
	case n == 0:
	case n < 20:
		words = append(words, smallNumbers[n])
	default:
		words = append(words, tensNumbers[n/10])
		if u := n % 10; u > 0 {
			words = append(words, smallNumbers[u])
		}
	}
	return words
}

// ordinalWords are the cardinal words whose ordinal is not simply "+th".
var ordinalWords = map[string]string{
	"one": "first", "two": "second", "three": "third", "five": "fifth",
	"eight": "eighth", "nine": "ninth", "twelve": "twelfth",
	"twenty": "twentieth", "thirty": "thirtieth", "forty": "fortieth",
	"fifty": "fiftieth", "sixty": "sixtieth", "seventy": "seventieth",
	"eighty": "eightieth", "ninety": "ninetieth",
}

// spellOrdinal writes the position: 1 is first, 21 is twenty first, 100 is
// one hundredth. Only the final word changes, which is exactly how English
// builds an ordinal from a cardinal.
func spellOrdinal(n uint64) string {
	words := strings.Split(spellCardinal(n), " ")
	last := len(words) - 1
	if o, ok := ordinalWords[words[last]]; ok {
		words[last] = o
	} else {
		words[last] += "th"
	}
	return strings.Join(words, " ")
}

// pluralise picks the singular only for exactly one.
func pluralise(n uint64, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

func isDigitByte(b byte) bool { return b >= '0' && b <= '9' }

// lastDigit returns the offset just past the last digit of s — the seam
// between a figure and the unit written after it.
func lastDigit(s string) int {
	end := len(s)
	for end > 0 && !isDigitByte(s[end-1]) {
		end--
	}
	return end
}
