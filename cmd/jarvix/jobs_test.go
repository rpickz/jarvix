package main

import (
	"strings"
	"testing"
)

// The CLI half of the jobs surface (#221, ADR 0067).
//
// What can be tested here without a daemon is the argument grammar, the help,
// and the two decisions this file is actually allowed to make: which glyph a
// row gets and what to type next. Everything else is a sentence the daemon
// wrote, which is exactly why there is nothing else here to test.

// TestJobsRefusesAnInvocationItCannotRead. `jarvix jobs stop` with no name must
// not look like it worked, and `jarvix jobs answer tidy` with nothing to say
// must not send an empty answer to a job waiting on a decision.
func TestJobsRefusesAnInvocationItCannotRead(t *testing.T) {
	hermeticEnv(t)
	cases := [][]string{
		{"jobs", "stop"},
		{"jobs", "stop", "tidy", "extra"},
		{"jobs", "answer"},
		{"jobs", "answer", "tidy"},
		{"jobs", "--json"},
		{"jobs", "list"},
	}
	for _, args := range cases {
		var code int
		_, stderr := capture(t, func() { code = run(args) })
		if code != 1 {
			t.Errorf("run(%v) exit = %d, want the usage refusal (1)", args, code)
		}
		if !strings.Contains(stderr, "jarvix jobs") {
			t.Errorf("run(%v) stderr = %q, want the usage line", args, stderr)
		}
	}
}

// TestTheJobCommandsAreInTheHelp. A command nobody can find is a command that
// does not exist, and "what are you doing" is the one a user reaches for when
// something has gone wrong — which is the worst possible moment to have to
// guess the verb.
func TestTheJobCommandsAreInTheHelp(t *testing.T) {
	hermeticEnv(t)
	stdout, _ := capture(t, func() { run([]string{"help"}) })
	for _, want := range []string{
		"jarvix jobs", "jarvix jobs stop <name>", "jarvix jobs answer <name> yes|no",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the help does not mention %q", want)
		}
	}
}

// The hint is built from the controls the daemon offered, never from the CLI's
// idea of what is possible — so a job with nothing on offer is told nothing to
// type, which is the same withholding the window's missing button is. An
// instruction that would be refused is worse than none.
func TestTheHintOffersOnlyWhatTheDaemonOffered(t *testing.T) {
	parked := jobRow{Name: "tidy", Controls: []jobControl{
		{ID: "approve", Label: "Approve"},
		{ID: "decline", Label: "Say no"},
		{ID: "stop", Label: "Stop"},
	}}
	hint := jobHint(parked)
	for _, want := range []string{
		"jarvix jobs answer tidy yes", "jarvix jobs answer tidy no", "jarvix jobs stop tidy",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("the hint for a parked approval = %q, missing %q", hint, want)
		}
	}
	if strings.Contains(hint, "<your answer>") {
		t.Errorf("a gate approval was offered a free-text answer: %q", hint)
	}

	decision := jobRow{Name: "tidy", Controls: []jobControl{
		{ID: "answer", Label: "Send your answer"},
		{ID: "stop", Label: "Stop"},
	}}
	if !strings.Contains(jobHint(decision), "jarvix jobs answer tidy <your answer>") {
		t.Errorf("a job waiting on a decision is not told to answer it in words: %q",
			jobHint(decision))
	}

	if got := jobHint(jobRow{Name: "tidy"}); got != "" {
		t.Errorf("a job with nothing on offer was told to type %q", got)
	}
}

// The mark is a shortcut for the eye and never the only place a fact lives —
// every row's own sentence already says where the job stands, so the glyph is
// derived from the same controls the hint is and carries nothing extra.
func TestTheMarkFollowsTheControlsAndNeverStandsAlone(t *testing.T) {
	waiting := jobRow{Controls: []jobControl{{ID: "approve"}, {ID: "stop"}}}
	running := jobRow{Controls: []jobControl{{ID: "stop"}}}
	over := jobRow{}

	if jobMark(waiting) == jobMark(running) || jobMark(running) == jobMark(over) {
		t.Errorf("two different standings share a mark: %q / %q / %q",
			jobMark(waiting), jobMark(running), jobMark(over))
	}
	if strings.TrimSpace(jobMark(over)) != "" {
		t.Errorf("a job that has ended is marked %q; the quiet state is the blank one",
			jobMark(over))
	}
}

// yes and no are the whole vocabulary for an approval; anything else is the
// answer to a decision and travels verbatim. A CLI that turned "no thanks, use
// the other one" into a boolean would be answering a question nobody asked.
func TestAnAnswersWordsAreReadTheWayTheJobNeedsThem(t *testing.T) {
	for _, tc := range []struct {
		words    []string
		approved bool
		answer   any
	}{
		{[]string{"yes"}, true, nil},
		{[]string{"YES"}, true, nil},
		{[]string{"no"}, false, nil},
		{[]string{"the", "one", "in", "Documents"}, true, "the one in Documents"},
	} {
		params := answerParams("tidy", tc.words)
		if params["approved"] != tc.approved {
			t.Errorf("%v read as approved=%v, want %v", tc.words, params["approved"], tc.approved)
		}
		if tc.answer == nil {
			if _, carried := params["answer"]; carried {
				t.Errorf("%v invented words the user did not type: %#v", tc.words, params["answer"])
			}
		} else if params["answer"] != tc.answer {
			t.Errorf("%v carried %#v, want %#v", tc.words, params["answer"], tc.answer)
		}
	}
}
