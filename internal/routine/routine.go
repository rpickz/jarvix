// Package routine turns one spoken sentence into a placed desktop (ADR 0026).
//
// A routine is a named, user-authored sequence of steps — launch this
// application and put its window *there* — triggered by a phrase the
// deterministic intent router recognises (internal/intent) and executed
// through the compositor seam (internal/desktop, ADR 0022).
//
// "There" is the window-placement vocabulary (internal/placement, ADR 0056),
// embedded in each Step rather than restated: a mode, a proportion in percent
// or pixels, an arrangement for what comes next, and a target workspace and
// monitor. The same value the window tools accept and the form edits, so a
// routine can express anything Jarvix can do to a window anywhere.
//
// Three properties are the design, and each answers a way this feature could
// annoy its owner:
//
//   - Dedupe before launch. A step whose application is already running
//     places the existing window instead of starting a second copy, matched
//     with the same identity logic desktop.focus_window uses. A morning
//     routine that opens a second browser every morning is worse than no
//     routine at all.
//   - Failure continues. One application that will not start, or whose
//     window never appears inside the bounded wait, is recorded and stepped
//     past; the remaining steps still run, and the single spoken summary at
//     the end names what failed. One dead app must not strand the other six.
//   - Nothing here is a command. Steps carry validated program names (the
//     same one-bare-token rule the terminal intent enforces) and integers.
//     There is no shell, no argument list, and deliberately no "run this
//     command" step kind — that would put a shell behind a single spoken
//     phrase, and it is excluded rather than gated.
package routine

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/rpickz/jarvix/internal/placement"
)

// Workspace bounds, matching the compositor seam's own and the vocabulary's.
const (
	minWorkspace = placement.MinWorkspace
	maxWorkspace = placement.MaxWorkspace
)

// Definition is one configured routine, converted from its [[routines]]
// table. The schema is deliberately boring — scalars and two-element integer
// arrays, nothing nested beyond the step list — because the capture ticket
// (#62) will write these entries programmatically, and a schema a program can
// emit without ceremony is a schema a person can read.
type Definition struct {
	// Name is what the user runs and what the summary opens with
	// ("Morning setup: five apps placed").
	Name string
	// Phrases trigger the routine through the intent router. Their grammar
	// (and collisions with built-in intents) are the router's to validate;
	// this package only requires that some exist.
	Phrases []string
	// Steps run in order.
	Steps []Step
}

// Step is one application placed on one workspace.
//
// The placement half is not spelled here: it is placement.Placement, embedded,
// which is the whole of ADR 0056's "defined once and used everywhere". A step
// is *what to launch* plus *where it goes*, and the second half is the same
// value the window tools take and the form edits, so an option added to the
// vocabulary is available in a routine the moment it exists.
type Step struct {
	// App is the program to launch when no matching window exists: a single
	// bare executable name or absolute path, the same rule the terminal
	// intent enforces, because it travels the same validated spawn path.
	App string
	// Match overrides how an existing window is recognised, for applications
	// whose window class is not their binary name ("google-chrome-stable"
	// launching a window classed "Google-chrome"). Empty matches on App.
	Match string
	// Placement is where the window goes: mode, proportion, arrangement and
	// target. Embedded so a step reads as one thing rather than two.
	placement.Placement
}

// matchQuery is what the dedupe matcher looks for.
func (s Step) matchQuery() string {
	if q := strings.TrimSpace(s.Match); q != "" {
		return q
	}
	return s.App
}

// programToken bounds what a step may launch: one bare executable name or
// absolute path. The same character set spawnPattern (internal/desktop) and
// the terminal intent enforce, restated here so a bad entry fails at config
// load with the step's own label instead of mid-routine with a compositor
// refusal.
var programToken = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)

// Problems reports everything structurally wrong with the definitions, one
// actionable message per problem, each naming the routine (and step) to fix.
// Phrase grammar and collisions are deliberately not checked here — the
// intent router owns the grammar, and configuration compiles the real router
// as its check, so there is no second, weaker copy of those rules.
func Problems(defs []Definition) []string {
	var problems []string
	seen := make(map[string]string, len(defs))
	for i, def := range defs {
		label := fmt.Sprintf("routines[%d]", i)
		name := strings.TrimSpace(def.Name)
		if name != "" {
			label = fmt.Sprintf("routines[%d] (%q)", i, name)
		}
		if name == "" {
			problems = append(problems, label+": name is empty; give the routine a name to trigger and log under")
		} else if owner, dup := seen[strings.ToLower(name)]; dup {
			problems = append(problems, fmt.Sprintf("%s: name %q is already %s; routine names must be unique",
				label, name, owner))
		} else {
			seen[strings.ToLower(name)] = fmt.Sprintf("routines[%d]", i)
		}
		if len(def.Phrases) == 0 {
			problems = append(problems, label+": it has no phrases; add at least one trigger phrase")
		}
		if len(def.Steps) == 0 {
			problems = append(problems, label+": it has no steps; add at least one [[routines.steps]] table")
		}
		for j, step := range def.Steps {
			problems = append(problems, stepProblems(fmt.Sprintf("%s steps[%d]", label, j), step)...)
		}
		problems = append(problems, danglingArrangements(label, def)...)
	}
	return problems
}

// danglingArrangements catches a `place_next` with nothing after it on the
// same workspace.
//
// It is a whole-routine rule rather than a step rule because it can only be
// seen from the whole routine, and it is worth catching: a preselection is
// ONE-SHOT — the compositor holds it until a window maps and then spends it —
// so a routine that sets one and never opens another window on that workspace
// leaves it lying there for whatever the user opens by hand next. They would
// experience it as their terminal opening in a strange place ten minutes
// later, with nothing on screen connecting it to the routine.
func danglingArrangements(label string, def Definition) []string {
	var problems []string
	for i, step := range def.Steps {
		if step.PlaceNext == placement.PlaceNextNone {
			continue
		}
		followed := false
		for _, later := range def.Steps[i+1:] {
			if later.Workspace == step.Workspace {
				followed = true
				break
			}
		}
		if !followed {
			problems = append(problems, fmt.Sprintf(
				"%s steps[%d]: place_next = %q has no step after it on workspace %d, so the "+
					"arrangement would be spent on whatever you open next by hand; remove it, "+
					"or add the step it is making room for",
				label, i, step.PlaceNext, step.Workspace))
		}
	}
	return problems
}

// stepProblems validates one step: the launching half here, the placement
// half through the vocabulary. There is deliberately no second copy of the
// placement rules — the form, the tools and this loader all run
// placement.Problems, so a value refused when a routine is saved is refused
// identically when a tool sends it.
func stepProblems(label string, s Step) []string {
	var problems []string
	switch {
	case strings.TrimSpace(s.App) == "":
		problems = append(problems, label+": app is empty; name the program this step launches")
	case !programToken.MatchString(s.App):
		problems = append(problems, fmt.Sprintf("%s: app %q must be a single executable name or absolute "+
			"path (letters, digits, . _ / + -); it is launched directly, never through a shell", label, s.App))
	}
	if s.Match != "" && strings.TrimSpace(s.Match) == "" {
		problems = append(problems, label+": match is blank; omit it to match on the app name")
	}
	// A routine step must name a workspace: it is describing a desktop, and
	// "wherever the compositor felt like" is not a description. The tools,
	// which place the window already in front of the user, pass false.
	for _, p := range s.Problems(true) {
		problems = append(problems, label+": "+p.String())
	}
	return problems
}
