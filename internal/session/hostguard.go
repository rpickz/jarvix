package session

import (
	"strings"
	"unicode"
)

// This file is the host's honesty guard (issue #161, ADR 0064): the check that
// decides whether the small model's line may be spoken at all.
//
// The host speaks *first*, before anything has been answered, which is exactly
// the shape that produced issue #71 — a model too small for what it was
// holding narrating facts and actions that were never true. A pinned system
// prompt tells the host what it may say; this file is what happens when the
// model ignores it, and it is not optional. A prompt is a request, and the
// whole premise of tiering is that the host is the *weakest* model in the
// house, so it is the least likely of the three to honour one.
//
// The guard is an ALLOWLIST, and that is the load-bearing decision. A blocklist
// over free prose cannot be made reliable: there is no finite list of ways to
// state a fact, and every one that is missed is spoken aloud in Jarvix's voice
// as though it were the answer. So instead of asking "does this line assert
// something?" the guard asks "is this line one of the two shapes a host line is
// allowed to have?", and refuses everything else:
//
//   - a HOLDING line — a single short clause, made only of letters, that begins
//     with one of a closed set of openers about waiting and thinking;
//   - a CLARIFYING question — a real question, opened by a question word, put
//     to the user.
//
// **The asymmetry is what makes this defensible.** A false refusal costs
// nothing: the line is discarded and the turn degrades to what it has always
// been, silence followed by the answer. A false acceptance costs a sentence in
// Jarvix's voice stating something nobody checked. So the guard is tuned hard
// towards refusal, and it will refuse plenty of lines a person would have been
// happy to hear. That is the trade, taken deliberately and in that direction.
//
// It is a pure function of the line — no engine, no session, no clock — so it
// can be, and is, tested exhaustively against a model that tries to answer.

// hostLineKind is what the guard made of a host's line.
type hostLineKind int

const (
	// hostLineRefused: the line is not one of the permitted shapes and is
	// discarded unspoken. The turn carries on exactly as it would have with no
	// host configured at all.
	hostLineRefused hostLineKind = iota
	// hostLineHolding: a holding line. It is spoken as an aside underneath the
	// answer, and it is dropped if the answer overtakes it.
	hostLineHolding
	// hostLineClarifying: the host asked the user for a detail. It abandons the
	// answer attempt and becomes this turn's reply (see host.go).
	hostLineClarifying
)

// The caps. Every one of them is a *maximum*, and a line that needs more than
// any of them is not a holding line — it is the small model starting to answer.
const (
	// maxHostWords bounds the whole line. A holding line is a breath, not a
	// paragraph, and the host's token budget is set below this so a model that
	// begins an essay is cut off mid-sentence and then refused here for having
	// no terminator.
	maxHostWords = 24
	// maxHostSentences allows a holding line followed by a question, and
	// nothing longer.
	maxHostSentences = 2
	// maxHoldingWords bounds one holding clause. "Let me think about that
	// properly" is six.
	maxHoldingWords = 10
	// maxQuestionWords bounds a clarifying question. "Do you mean the deploy
	// script or the deploy thread?" is ten.
	maxQuestionWords = 20
	// minQuestionWords refuses a question too vague to be worth interrupting
	// for. "What?" is not a clarification; it is the host admitting it was not
	// listening, and the user then has to say the whole thing again.
	minQuestionWords = 4
)

// holdingOpeners is the closed set of ways a holding line may begin. It is
// deliberately short, deliberately about *waiting and thinking*, and
// deliberately contains no verb of action: the host holds no tools, so a line
// beginning "checking" or "looking that up" would be a claim about work that is
// not happening anywhere. The heavier model is thinking; saying so is true.
//
// Matched as a whole-word prefix of the sentence, case-folded. The host's
// system prompt quotes several of these verbatim, so the common case is a model
// doing what it was asked and landing inside the list.
var holdingOpeners = []string{
	"a moment",
	"bear with me",
	"give me a moment",
	"give me a second",
	"give me a sec",
	"good question",
	"hang on",
	"hold on",
	"i am thinking",
	"i am working",
	"i'll get",
	"i'll have",
	"i'll think",
	"i'm thinking",
	"i'm working",
	"just a moment",
	"just a second",
	"let me get",
	"let me think",
	"let me work",
	"one moment",
	"one second",
	"still thinking",
	"still working",
	"that will take a moment",
	"that'll take a moment",
	"that's a good question",
	"that's a big question",
	"thinking about that",
	"working on that",
	"working that out",
}

// questionOpeners is the closed set of words a clarifying question may begin
// with. Every one of them opens a genuine request for information.
//
// The negated auxiliaries are deliberately absent, and their absence is a rule
// rather than an oversight: "isn't the deploy script the one that runs on
// merge?" is an assertion wearing a question mark, and it is exactly how a
// model that has decided on an answer smuggles it past a guard that only looks
// for full stops. They are refused explicitly in bannedPhrases as well, so a
// negation anywhere in the line refuses it, not only at the front.
var questionOpeners = []string{
	"am", "are", "can", "could", "did", "do", "does", "how", "is", "may",
	"shall", "should", "was", "were", "what", "when", "where", "which",
	"who", "whom", "whose", "why", "will", "would",
}

// bannedPhrases refuse a line wherever they appear in it, whatever shape the
// rest of it has. They are the second layer, and they exist for the case the
// shape check cannot see: a permitted opener with something else hung off the
// end of it.
//
// Three families, and each is a thing the host must never do:
//
//   - **Claiming an action.** The host has no tools — not "tools it chose not
//     to use", none at all — so every one of these is false by construction the
//     moment it is said. This is #71's exact sentence, and it is the reason the
//     family is here rather than trusted to the prompt.
//   - **Guessing.** "Probably", "I think", "it looks like" are the host doing
//     the answering tier's job badly, which is the one job it does not have.
//   - **Asserting through a negation.** The rhetorical question, above.
//
// Matched against a space-padded, punctuation-stripped, case-folded copy of the
// line, so every entry matches on word boundaries and "i've" is not found
// inside a longer word.
var bannedPhrases = []string{
	// Claimed actions.
	"i've", "i have", "i had", "i did", "i ran", "i run", "i opened",
	"i closed", "i set", "i sent", "i found", "i checked", "i looked",
	"i searched", "i created", "i made", "i wrote", "i saved", "i deleted",
	"i removed", "i added", "i changed", "i moved", "i started", "i stopped",
	"i installed", "i updated", "i fixed", "i copied", "i clicked",
	"i pressed", "i launched", "i read", "i got", "i took", "i put",
	"i called", "i turned", "i just", "just did", "already", "done",
	"here's", "here is", "there you go", "all set", "sorted", "for you now",
	// Guesses and openings of an answer.
	"i think", "i believe", "i'd say", "i would say", "probably", "likely",
	"apparently", "seems", "seem", "looks like", "actually", "in fact",
	"basically", "essentially", "the answer",
	// Assertions wearing a question mark.
	"isn't", "aren't", "wasn't", "weren't", "doesn't", "didn't", "don't",
	"can't", "couldn't", "shouldn't", "wouldn't", "won't", "haven't",
	"hasn't", "not really", "surely",
}

// assertionMarkers refuse a *holding* sentence that contains one. A holding
// line says that Jarvix is working; the moment it says that something IS
// something, it has stopped holding and started answering.
//
// Questions are exempt, and have to be: "is" and "does" are how a question
// about the world is opened, and refusing them would leave no way to ask one.
var assertionMarkers = []string{
	"is", "are", "was", "were", "means", "because", "refers", "has", "have",
	"does", "do",
}

// hostLineVerdict decides what, if anything, the host may say.
//
// It returns the exact text to speak (normalised only in whitespace — nothing
// is rewritten, because a guard that edits a line is a guard that can make one
// worse), the shape it was accepted as, and, when refused, the rule that
// refused it. The reason is for the log and the turn's record: "the host said
// something and it was thrown away" is a thing an operator has to be able to
// find out, and a silent discard would make the feature look broken rather than
// careful.
func hostLineVerdict(raw string) (text string, kind hostLineKind, why string) {
	line := normaliseHostLine(raw)
	switch {
	case line == "":
		return "", hostLineRefused, "empty"
	case len(strings.Fields(line)) > maxHostWords:
		return "", hostLineRefused, "too long"
	}
	if phrase, found := firstBannedPhrase(line); found {
		return "", hostLineRefused, "says " + quoted(phrase)
	}
	sentences := splitHostSentences(line)
	if len(sentences) == 0 {
		return "", hostLineRefused, "empty"
	}
	if len(sentences) > maxHostSentences {
		return "", hostLineRefused, "more than one thing"
	}
	kind = hostLineHolding
	for _, sentence := range sentences {
		switch {
		case isHostQuestion(sentence):
			if why := questionRefusal(sentence); why != "" {
				return "", hostLineRefused, why
			}
			if kind == hostLineClarifying {
				return "", hostLineRefused, "two questions"
			}
			kind = hostLineClarifying
		default:
			if why := holdingRefusal(sentence); why != "" {
				return "", hostLineRefused, why
			}
		}
	}
	return line, kind, ""
}

// quoted wraps a phrase for a refusal reason. Hand-rolled rather than
// strconv.Quote because these strings are read by people in a log line, and
// Quote's escaping of an apostrophe-free ASCII phrase only adds noise.
func quoted(s string) string { return "\"" + s + "\"" }

// normaliseHostLine collapses the line to a single line of single-spaced words
// and strips the quotes a model wraps its answer in when it has been told to
// reply with one sentence. Whitespace only: no word is added, removed or
// changed, so what is spoken is what the model said.
func normaliseHostLine(raw string) string {
	line := strings.Join(strings.Fields(raw), " ")
	for _, quote := range []string{"\"", "'", "“", "”", "‘", "’"} {
		line = strings.TrimPrefix(line, quote)
		line = strings.TrimSuffix(line, quote)
	}
	return strings.TrimSpace(line)
}

// splitHostSentences breaks the line at terminators, keeping each terminator
// with the sentence it ends. A trailing run with no terminator is a sentence
// too — a model cut off by the token budget produces exactly that, and it is
// refused below on shape rather than silently dropped.
func splitHostSentences(line string) []string {
	var out []string
	start := 0
	for i := 0; i < len(line); i++ {
		if c := line[i]; c != '.' && c != '!' && c != '?' {
			continue
		}
		// Consume any run of terminators ("...", "?!") as one boundary.
		end := i + 1
		for end < len(line) && (line[end] == '.' || line[end] == '!' || line[end] == '?') {
			end++
		}
		if s := strings.TrimSpace(line[start:end]); s != "" {
			out = append(out, s)
		}
		start = end
		i = end - 1
	}
	if s := strings.TrimSpace(line[start:]); s != "" {
		out = append(out, s)
	}
	return out
}

// isHostQuestion reports whether this sentence is put to the user. A question
// mark is necessary but not sufficient — questionRefusal decides the rest.
func isHostQuestion(sentence string) bool { return strings.HasSuffix(sentence, "?") }

// questionRefusal reports why a clarifying question may not be asked, or "" if
// it may.
func questionRefusal(sentence string) string {
	words := strings.Fields(sentence)
	switch {
	case len(words) < minQuestionWords:
		return "question too vague"
	case len(words) > maxQuestionWords:
		return "question too long"
	case !hasWord(questionOpeners, strings.ToLower(strings.Trim(words[0], ",'"))):
		return "not a question the host may ask"
	case strings.ContainsAny(sentence, ":;—–\"()[]{}*`#$%&+=<>/\\|~^"):
		return "punctuation a question does not need"
	case strings.Count(sentence, "?") != 1:
		return "more than one question"
	}
	return ""
}

// holdingRefusal reports why a holding sentence may not be spoken, or "" if it
// may.
//
// The character rule is the strict half and it does most of the work: a holding
// clause is made of letters, spaces and apostrophes, and nothing else. No
// digits, because a number in a holding line is a fact. No comma, because a
// comma is where the second clause hides ("Let me think, the deploy runs on
// merge"). No colon and no dash, for the same reason with different
// punctuation. There is no phrasing this refuses that a person would miss.
func holdingRefusal(sentence string) string {
	body := strings.TrimRight(sentence, ".!")
	words := strings.Fields(body)
	switch {
	case len(words) == 0:
		return "empty"
	case len(words) > maxHoldingWords:
		return "not a holding line"
	}
	for _, r := range body {
		if unicode.IsLetter(r) || r == ' ' || r == '\'' || r == '’' {
			continue
		}
		return "punctuation a holding line does not need"
	}
	if !hasHoldingOpener(strings.ToLower(body)) {
		return "not a holding line or a question"
	}
	padded := " " + strings.ToLower(body) + " "
	for _, marker := range assertionMarkers {
		if strings.Contains(padded, " "+marker+" ") {
			return "asserts with " + quoted(marker)
		}
	}
	return ""
}

// hasHoldingOpener reports whether the (lower-cased) sentence begins with one
// of the permitted openers, on a word boundary — so "one moment" opens a
// holding line and "one momentous decision" does not.
func hasHoldingOpener(body string) bool {
	for _, opener := range holdingOpeners {
		if !strings.HasPrefix(body, opener) {
			continue
		}
		if len(body) == len(opener) || body[len(opener)] == ' ' {
			return true
		}
	}
	return false
}

// firstBannedPhrase finds the first banned phrase in the line, matched on word
// boundaries against a punctuation-stripped copy. Apostrophes survive the strip
// so "i've" is one word rather than two.
func firstBannedPhrase(line string) (string, bool) {
	var b strings.Builder
	b.WriteByte(' ')
	for _, r := range strings.ToLower(line) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '\'' || r == '’':
			b.WriteByte('\'')
		default:
			b.WriteByte(' ')
		}
	}
	b.WriteByte(' ')
	// Collapsed so a stripped punctuation run does not leave a double space a
	// padded phrase would fail to match across.
	padded := " " + strings.Join(strings.Fields(b.String()), " ") + " "
	for _, phrase := range bannedPhrases {
		if strings.Contains(padded, " "+phrase+" ") {
			return phrase, true
		}
	}
	return "", false
}

// hasWord reports whether word is in the set.
func hasWord(set []string, word string) bool {
	for _, w := range set {
		if w == word {
			return true
		}
	}
	return false
}
