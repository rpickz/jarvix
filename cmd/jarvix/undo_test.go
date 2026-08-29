package main

import (
	"strings"
	"testing"
)

// The CLI half of review and undo (#201, ADR 0064).
//
// What can be tested here without a daemon is the argument grammar and the
// help — which is more worth pinning than it looks. `jarvix undo` is a
// command that changes files, and a flag it silently ignored would be a user
// who thought they had scoped a reversal and had not.

// TestUndoRefusesAnInvocationItCannotRead: an unrecognised flag is a usage
// error, never a silently-dropped one. `jarvix undo --jobb deploy` must not
// look like it worked by reversing the last action instead.
func TestUndoRefusesAnInvocationItCannotRead(t *testing.T) {
	hermeticEnv(t)
	cases := [][]string{
		{"undo", "--jobb", "deploy"},
		{"undo", "--job"},
		{"undo", "--job", "deploy", "extra"},
		{"undo", "a1", "a2"},
		{"undo", "--all"},
	}
	for _, args := range cases {
		var code int
		_, stderr := capture(t, func() { code = run(args) })
		// 1, not 2: `fail` is this CLI's usage refusal for a known command
		// with arguments it cannot read; 2 is reserved for a command that
		// does not exist at all.
		if code != 1 {
			t.Errorf("run(%v) exit = %d, want the usage refusal (1)", args, code)
		}
		if !strings.Contains(stderr, "jarvix undo") {
			t.Errorf("run(%v) stderr = %q, want the usage line", args, stderr)
		}
	}
}

// TestActionsTakesNoArguments: the account is a listing, and a filter nobody
// implemented must refuse rather than quietly print everything.
func TestActionsTakesNoArguments(t *testing.T) {
	hermeticEnv(t)
	var code int
	_, stderr := capture(t, func() { code = run([]string{"actions", "--json"}) })
	if code != 1 {
		t.Errorf("exit = %d, want the usage refusal (1)", code)
	}
	if !strings.Contains(stderr, "usage: jarvix actions") {
		t.Errorf("stderr = %q", stderr)
	}
}

// TestBothCommandsAreInTheHelp. A command nobody can find is a command that
// does not exist, and "what did you do in my name" is the one a user reaches
// for when something has gone wrong — which is the worst possible moment to
// have to guess the verb.
func TestBothCommandsAreInTheHelp(t *testing.T) {
	hermeticEnv(t)
	stdout, _ := capture(t, func() { run([]string{"help"}) })
	for _, want := range []string{"jarvix actions", "jarvix undo [id]", "jarvix undo --job"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the help does not mention %q", want)
		}
	}
}

// TestShortTimeRendersTheColumnAndNeverEatsTheValue. A daemon whose timestamp
// format changed should look like a formatting bug, not like a row with no
// time at all.
func TestShortTimeRendersTheColumnAndNeverEatsTheValue(t *testing.T) {
	if got, want := shortTime("2026-08-29T09:41:00Z"), "2026-08-29 09:41"; got != want {
		t.Errorf("shortTime = %q, want %q", got, want)
	}
	for _, raw := range []string{"", "yesterday", "2026-08-29"} {
		if got := shortTime(raw); got != raw {
			t.Errorf("shortTime(%q) = %q, want the raw value back", raw, got)
		}
	}
}
