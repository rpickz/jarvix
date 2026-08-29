package config

import (
	"fmt"
	"strings"
)

// Launch is the user's own say in how a program is started (#194).
//
// Jarvix works out for itself whether a program opens a window or needs a
// terminal: a graphical application ships an XDG desktop entry and a
// command-line tool does not, so a program on PATH with no entry anywhere is
// a command. That rule identifies `claude`, `opencode`, `codex` and `grok`
// on the machine it was written for, and it is right almost everywhere — but
// it is an inference, and an inference is exactly the thing that ought to be
// overridable by the person who owns the machine.
//
// So these two lists outrank it. They are the highest-precedence source in
// the classification, above the entry's own `Terminal` key, because a user
// correcting Jarvix about their own computer is not offering a hint.
//
// Neither list needs to exist for the feature to work, and on most machines
// neither will: they are for the program the rule gets wrong, said once, in
// the settings screen like every other family (ADR 0054).
type Launch struct {
	// TerminalPrograms are the programs to open inside the terminal whatever
	// this machine's entries suggest.
	TerminalPrograms []string `toml:"terminal_programs"`
	// GraphicalPrograms are the programs to start directly, expecting a
	// window of their own.
	GraphicalPrograms []string `toml:"graphical_programs"`
}

// launchProblems validates the override lists.
//
// The one substantive check is the contradiction: a name in both lists is not
// a preference expressed twice, it is two incompatible instructions, and
// resolving it silently would mean picking one of the user's own sentences to
// ignore. Everything else is shape — an override names a program, so it is
// bounded by what a program name may be, the same bound the launcher applies
// to the model.
func (c Config) launchProblems() []string {
	var problems []string
	terminal := map[string]bool{}
	for _, name := range c.Tools.Launch.TerminalPrograms {
		problems = append(problems, launchNameProblems("tools.launch.terminal_programs", name)...)
		terminal[strings.ToLower(strings.TrimSpace(name))] = true
	}
	for _, name := range c.Tools.Launch.GraphicalPrograms {
		problems = append(problems, launchNameProblems("tools.launch.graphical_programs", name)...)
		if terminal[strings.ToLower(strings.TrimSpace(name))] {
			problems = append(problems, fmt.Sprintf(
				"%q is in both tools.launch.terminal_programs and tools.launch.graphical_programs; "+
					"a program starts one way or the other, so name it in one list only",
				strings.TrimSpace(name)))
		}
	}
	return problems
}

// launchNameProblems bounds one override entry. It is a program name, not a
// path and not a command line: what it overrides is a classification, and a
// classification is keyed on the name the launcher resolves.
func launchNameProblems(key, name string) []string {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		return []string{key + " contains an empty entry; each one must be a program name such as \"claude\""}
	case strings.ContainsAny(trimmed, " \t/"):
		return []string{fmt.Sprintf(
			"%s entry %q must be a program name such as \"claude\" — not a path and not a command line",
			key, trimmed)}
	}
	return nil
}
