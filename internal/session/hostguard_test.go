package session

import (
	"strings"
	"testing"
)

// These tests are the honesty guard's own (issue #161, ADR 0064). They are pure
// — no engine, no goroutine, no clock — because the guard is, and because this
// is the one component of the cascade whose failure mode is a sentence in
// Jarvix's voice stating something nobody checked.
//
// The fixture that matters most is hostThatTriesToAnswer below: a small model
// doing exactly what small models do when handed a question, in every register
// it does it in. Every one of those lines has to be thrown away, and the test
// says so line by line rather than in aggregate, so a regression names the
// sentence it would have spoken.

// TestTheHostMayHoldAndMayAsk is the permitted half: the shapes the system
// prompt asks for, accepted, and classified as what they are.
func TestTheHostMayHoldAndMayAsk(t *testing.T) {
	holding := []string{
		"Let me think about that properly.",
		"One moment.",
		"One second.",
		"Give me a second.",
		"Give me a moment.",
		"Bear with me.",
		"Hold on.",
		"Hang on.",
		"I'm thinking about that.",
		"Let me work that out.",
		"Still thinking.",
		"That's a good question.",
		"Working on that.",
		// Wrapped in the quotes a model adds when told to reply with one
		// sentence — whitespace and quotes are normalised, nothing else is.
		"\"Let me think about that properly.\"",
		// Leading and trailing whitespace, and an interior newline the model
		// used as a line wrap.
		"  Let me think\n about that properly.  ",
	}
	for _, line := range holding {
		text, kind, why := hostLineVerdict(line)
		if kind != hostLineHolding {
			t.Errorf("hostLineVerdict(%q) refused a holding line: %s", line, why)
			continue
		}
		if strings.TrimSpace(text) == "" {
			t.Errorf("hostLineVerdict(%q) accepted but returned nothing to say", line)
		}
	}

	asking := []string{
		"Do you mean the deploy script or the deploy thread?",
		"Which deploy do you mean?",
		"What did you want me to compare it with?",
		"Which of the two projects is that?",
		"Let me think. Which one do you mean?",
	}
	for _, line := range asking {
		_, kind, why := hostLineVerdict(line)
		if kind != hostLineClarifying {
			t.Errorf("hostLineVerdict(%q) refused a clarifying question: %s", line, why)
		}
	}
}

// TestTheAcceptedLineIsSpokenWordForWord pins that the guard judges rather than
// edits. A guard that rewrote a line could make one worse, and the sentence
// reaching the voice has to be the sentence that was checked.
func TestTheAcceptedLineIsSpokenWordForWord(t *testing.T) {
	const line = "Let me think about that properly."
	text, kind, _ := hostLineVerdict(line)
	if kind != hostLineHolding || text != line {
		t.Errorf("hostLineVerdict(%q) = %q/%v, want the line back unchanged", line, text, kind)
	}
}

// hostThatTriesToAnswer is the fixture the ticket asks for: a model that
// ignores its instructions. Each line is something a 1.7B model asked "what is
// recursion?" or "is the deploy broken?" will produce sooner or later, and not
// one of them may be said.
//
// They are grouped by what makes them unsayable, because the groups are the
// argument: assertions, guesses, and claims of action are three different
// failures, and the guard has to catch all three whatever the sentence around
// them looks like.
var hostThatTriesToAnswer = []struct {
	name string
	line string
}{
	// Plain assertions — the model simply answering.
	{"answers outright", "Recursion is a function that calls itself."},
	{"answers in two sentences", "Recursion is a function calling itself. Let me think about that properly."},
	{"defines the subject", "The deploy script runs on every merge to main."},
	{"answers after a permitted opener",
		"Let me think about that properly. Recursion is a function that calls itself."},
	{"hangs the answer off the opener with a dash",
		"Let me think — recursion is a function that calls itself."},
	{"hangs the answer off the opener with a comma",
		"Let me think, the deploy runs on merge."},
	{"hangs the answer off the opener with a colon",
		"One moment: the answer is forty two."},
	{"smuggles a number into a holding line", "Let me think about that 42."},

	// Guesses — the model doing the answering tier's job badly.
	{"guesses", "I think it's the deploy script."},
	{"hedges a guess", "It's probably the deploy thread."},
	{"guesses inside a holding line", "Let me think, it looks like the deploy thread."},
	{"asserts through a rhetorical question", "Isn't the deploy script the one that runs on merge?"},
	{"asserts through a negation", "Don't you mean the deploy thread?"},

	// Claimed actions — the #71 sentence, in every tense a model reaches for.
	{"claims an action", "I've opened the deploy script for you."},
	{"claims an action in the past tense", "I checked the deploy log."},
	{"claims an action after a permitted opener", "Let me think. I've had a look at it."},
	{"claims completion", "Done."},
	{"presents a result", "Here's what I found."},

	// Shapes that are not holding lines at all.
	{"is empty", ""},
	{"is whitespace", "   \n  "},
	{"chats", "Sure thing, boss."},
	{"apologises", "Sorry about that."},
	{"is three sentences", "One moment. Let me think. Give me a second."},
	{"asks two questions", "Which one? Do you mean the deploy script or the thread?"},
	{"asks a question too vague to help", "What?"},
	{"runs on past the token budget",
		"Let me think about that properly because recursion is one of those ideas that " +
			"sounds circular until you see the base case, which is where"},
	{"emits markdown", "**Let me think about that properly.**"},
	{"emits a code fence", "`let me think about that properly`"},
	{"narrates a tool it does not have", "Checking the deploy script now."},
}

// TestAHostThatTriesToAnswerIsRefusedLineByLine is the fixture test the ticket
// calls for. Every line above is discarded, and the refusal names a reason —
// silence with no reason on the record is how a guard comes to look like a bug.
func TestAHostThatTriesToAnswerIsRefusedLineByLine(t *testing.T) {
	for _, tc := range hostThatTriesToAnswer {
		t.Run(tc.name, func(t *testing.T) {
			text, kind, why := hostLineVerdict(tc.line)
			if kind != hostLineRefused {
				t.Fatalf("hostLineVerdict(%q) would have SPOKEN %q as %v", tc.line, text, kind)
			}
			if text != "" {
				t.Errorf("a refused line still returned text to say: %q", text)
			}
			if why == "" {
				t.Error("refused without saying why")
			}
		})
	}
}

// TestTheGuardIsTotal runs the guard over every fixture in this file plus a
// handful of shapes chosen to break a hand-rolled splitter — an interior
// terminator run, a bare terminator, a very long line, multibyte punctuation —
// and asserts only that it returns rather than panicking or looping. Purity is
// no use if some input escapes it.
func TestTheGuardIsTotal(t *testing.T) {
	lines := []string{
		".", "?", "!", "...", "?!?!", "\x00", "…", "“Let me think about that properly.”",
		strings.Repeat("a", 5000), strings.Repeat(". ", 500), "Let me think" + strings.Repeat("?", 50),
	}
	for _, tc := range hostThatTriesToAnswer {
		lines = append(lines, tc.line)
	}
	for _, line := range lines {
		text, kind, why := hostLineVerdict(line)
		if kind == hostLineRefused && (text != "" || why == "") {
			t.Errorf("hostLineVerdict(%q) refused inconsistently: text %q, why %q", line, text, why)
		}
	}
}

// TestTheSystemPromptTeachesShapesTheGuardAccepts pins the two halves together.
// The prompt quotes example lines; if it ever quoted one the guard refuses, the
// host would be instructed to say something that could never be spoken, and the
// feature would look broken while working exactly as written.
func TestTheSystemPromptTeachesShapesTheGuardAccepts(t *testing.T) {
	examples := []string{
		"Let me think about that properly.",
		"One moment.",
		"Give me a second.",
		"Bear with me.",
		"Do you mean the deploy script or the deploy thread?",
	}
	for _, example := range examples {
		if !strings.Contains(hostSystemPrompt, example) {
			t.Errorf("the host prompt no longer quotes %q; keep the prompt and the guard in step", example)
			continue
		}
		if _, kind, why := hostLineVerdict(example); kind == hostLineRefused {
			t.Errorf("the host prompt asks for %q and the guard refuses it: %s", example, why)
		}
	}
}
