package tools

import (
	"strings"
	"testing"
)

// The tests in this file exist because the mutation report asked for them
// (issue #172, docs/mutation.md).
//
// The properties beside them state what is true for every input, and that is
// the right shape for a classifier — but a property says nothing about the
// exact place a bound is drawn. Mutation testing does: it moves each bound by
// one and asks whether anything notices. On the first report that was read,
// six mutants in the classifier and the approval matrix survived, and every
// one of them was a boundary or a branch no example happened to sit on. These
// are the examples, written so that moving the bound back is a failing test
// rather than a discovery.
//
// Each test names the mutant it kills, because a boundary test with no stated
// reason is the first thing deleted in a refactor.

// TestAPatternWordOfExactlyTheMaximumLengthIsStillACommandWord kills
// CONDITIONALS_BOUNDARY at approvals.go:75 (`len(w) <= maxPatternWordLen`).
//
// The bound is the point: a token longer than this is a hash or an id wearing
// a letter as its first character, and baking one into a standing rule
// produces a pattern that matches once and then sits in the user's config
// forever. Off by one in the permissive direction lets one such token in; off
// by one in the strict direction refuses a legitimate long subcommand.
func TestAPatternWordOfExactlyTheMaximumLengthIsStillACommandWord(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{})
	atTheBound := strings.Repeat("z", maxPatternWordLen)
	if offer := p.VetAllowPattern(atTheBound); !offer.Offered {
		t.Errorf("a %d-character command word was refused: %s", maxPatternWordLen, offer.Reason)
	}
	oneOver := strings.Repeat("z", maxPatternWordLen+1)
	if offer := p.VetAllowPattern(oneOver); offer.Offered {
		t.Errorf("a %d-character token was accepted as %q", maxPatternWordLen+1, offer.Pattern)
	}
}

// TestACommandThatIsNothingButAssignmentsNamesNoCommand kills
// CONDITIONALS_BOUNDARY at approvals.go:238 and at policy.go:1023 — the two
// `for len(fields) > 0 && envAssignment...` loops that strip leading VAR=value
// words before reading the command.
//
// Both loops walk off the end of the slice if the bound moves, and the only
// input that reaches the end is a command made of nothing BUT assignments.
// `FOO=1` on its own is a real thing to type (it sets nothing and runs
// nothing), and until this test no case in the suite was one, so the classifier
// and the proposal both had an unexercised way to panic on a command a user
// could send.
func TestACommandThatIsNothingButAssignmentsNamesNoCommand(t *testing.T) {
	// A non-empty allow list is what makes the classifier reach
	// matchWordPrefix at all; with an empty one it never compares a pattern.
	p := mustPolicy(t, PolicyConfig{ShellAllow: []string{"zzprobe status"}})
	for _, command := range []string{"FOO=1", "FOO=1 BAR=2", "FOO=1   BAR=2"} {
		v := p.Decide(shellCall(command))
		if v.Decision != PolicyAsk {
			t.Errorf("%q was judged %s (%s); it names no command to allow",
				command, v.Decision, v.Rule)
		}
		if w := commandWord(command); w != "" {
			t.Errorf("commandWord(%q) = %q; there is no command there", command, w)
		}
		offer := p.RememberOfferFor(v)
		if offer.Offered {
			t.Errorf("%q was offered as the standing rule %q", command, offer.Pattern)
		}
		if !strings.Contains(offer.Reason, "command name") {
			t.Errorf("%q was refused with %q, which does not say there is no command",
				command, offer.Reason)
		}
	}
}

// TestATypedRuleSaysWhichWordItRefused kills CONDITIONALS_NEGATION at
// approvals.go:316 (`if w == words[0]`).
//
// VetAllowPattern gives two different refusals for a word that is not a
// command word, and which one it gives depends on WHERE the word is. A head
// that is a path is refused because a file's contents can change after the
// rule is remembered; a later word is refused because truncating to a shorter
// prefix would make the rule WIDER than the one the person typed. Swap the two
// and both sentences become nonsense — which no test noticed, because the
// tests looked at whether it refused, not at what it said. The sentence is the
// product here: it is read aloud.
func TestATypedRuleSaysWhichWordItRefused(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{})
	head := p.VetAllowPattern("./deploy.sh")
	if head.Offered || !strings.Contains(head.Reason, "path rather than a command name") {
		t.Errorf("a path-invoked head was refused with %q", head.Reason)
	}
	later := p.VetAllowPattern("zzprobe status --json")
	if later.Offered || !strings.Contains(later.Reason, "is not a command word") {
		t.Errorf("a flag in a later position was refused with %q", later.Reason)
	}
	if strings.Contains(later.Reason, "path rather than a command name") {
		t.Errorf("a flag was refused as a path: %q", later.Reason)
	}
}

// TestAPreApprovedRunNamesTheRuleTheUserGranted kills CONDITIONALS_NEGATION at
// policy.go:781 (`if worst == PolicyAllow`).
//
// This is the audit promise of issue #162, and it is a promise about a STRING:
// when a line runs unprompted because of a rule the user added, the activity
// row names that rule rather than whichever shipped pattern happened to match
// first. The existing tests all put the granted segment first, where the
// distinction cannot show — so the branch that reaches back and renames the
// rule was free to be inverted.
func TestAPreApprovedRunNamesTheRuleTheUserGranted(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{ShellAllow: []string{"zzprobe status"}})

	// The granted segment is SECOND, behind a shipped allow pattern.
	v := p.Decide(shellCall("ls; zzprobe status"))
	if v.Decision != PolicyAllow || !v.PreApproved {
		t.Fatalf("ran as %s (pre-approved %v): %s", v.Decision, v.PreApproved, v.Rule)
	}
	if v.Rule != `configured allow pattern "zzprobe status"` {
		t.Errorf("the audit row names %q; the rule the user added is the fact it carries", v.Rule)
	}
	if v.Pattern != "zzprobe status" {
		t.Errorf("pattern = %q", v.Pattern)
	}

	// And when something else asks, the ask's rule wins: the row is about why
	// the question was put, and there is no unprompted run to audit.
	asked := p.Decide(shellCall("zzprobe status; rm -rf ./build"))
	if asked.Decision != PolicyAsk {
		t.Fatalf("ran as %s: %s", asked.Decision, asked.Rule)
	}
	if asked.PreApproved || asked.Pattern != "" {
		t.Errorf("an ask was marked pre-approved by %q", asked.Pattern)
	}
	if !strings.Contains(asked.Rule, "rm") {
		t.Errorf("the question names %q rather than the segment that caused it", asked.Rule)
	}
}

// TestTheSpokenCommandIsShortenedOnlyWhenItIsTooLong kills
// CONDITIONALS_BOUNDARY at policy.go:812 (`len(runes) <= maxSpoken`).
//
// The confirmation is generated daemon-side from the command precisely so a
// model cannot describe `rm -rf ~` as tidying up (ADR 0014), and the spoken
// form is where that guarantee is delivered. A command exactly at the bound
// must be read out whole: an ellipsis one character early is a user approving
// a command they were not told the end of. The count is in RUNES, so the
// second half uses multi-byte characters — a byte-counted bound would cut a
// 120-character command that is 240 bytes long.
func TestTheSpokenCommandIsShortenedOnlyWhenItIsTooLong(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{})
	const spoken = 120 // maxSpoken, restated so moving it fails here too
	for _, filler := range []string{"a", "é"} {
		atTheBound := "zzprobe " + strings.Repeat(filler, spoken-len("zzprobe "))
		if n := len([]rune(atTheBound)); n != spoken {
			t.Fatalf("the fixture is %d runes, not %d", n, spoken)
		}
		v := p.Decide(shellCall(atTheBound))
		if v.Decision != PolicyAsk {
			t.Fatalf("%q was judged %s", atTheBound, v.Decision)
		}
		if !strings.Contains(v.Summary, atTheBound) {
			t.Errorf("a %d-rune command was shortened before it had to be: %q", spoken, v.Summary)
		}
		if strings.Contains(v.Summary, "…") {
			t.Errorf("a %d-rune command was given an ellipsis: %q", spoken, v.Summary)
		}

		oneOver := atTheBound + filler
		over := p.Decide(shellCall(oneOver))
		if !strings.Contains(over.Summary, "…") {
			t.Errorf("a %d-rune command was read out whole: %q", spoken+1, over.Summary)
		}
		if strings.Contains(over.Summary, oneOver) {
			t.Errorf("a %d-rune command was not shortened: %q", spoken+1, over.Summary)
		}
	}
}

// TestOutputExactlyAtTheCapIsNotTruncated kills CONDITIONALS_BOUNDARY at
// shell.go:101 (`len(result) > maxOutput`).
//
// Not a classifier, but it is in the same package and it is the same kind of
// mistake: a cap applied one byte early appends "[output truncated]" to output
// that was complete, and the model then reasons about a reply it has been told
// is partial when it is not.
func TestOutputExactlyAtTheCapIsNotTruncated(t *testing.T) {
	s := &Shell{MaxOutput: 100}
	// Exactly 100 bytes, no trailing newline: `printf` writes what it is given
	// and nothing else, so the fixture's length is not the shell's opinion.
	out := runShell(t, s, "printf '%0100d' 0")
	if strings.Contains(out, "truncated") {
		t.Errorf("output of exactly MaxOutput bytes was reported as truncated: %q", out)
	}
}
