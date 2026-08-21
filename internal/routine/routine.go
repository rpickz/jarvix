// Package routine turns one spoken sentence into a placed desktop (ADR 0026).
//
// A routine is a named, user-authored sequence of steps — launch this
// application, put its window on that workspace, optionally floated with a
// size and position or arranged into the workspace's master/split layout —
// triggered by a phrase the deterministic intent router recognises
// (internal/intent) and executed through the compositor seam
// (internal/desktop, ADR 0022).
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
)

// Tile arrangements a step may ask for. "split" is the ordinary tiled layout
// — two split steps on one workspace share it side by side — and "master"
// additionally promotes the window to the layout's master slot.
const (
	TileMaster = "master"
	TileSplit  = "split"
)

// Workspace bounds, matching the compositor seam's own.
const (
	minWorkspace = 1
	maxWorkspace = 99
)

// maxPixel bounds sizes and positions, matching the seam's defensive bound.
const maxPixel = 32768

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
type Step struct {
	// App is the program to launch when no matching window exists: a single
	// bare executable name or absolute path, the same rule the terminal
	// intent enforces, because it travels the same validated spawn path.
	App string
	// Match overrides how an existing window is recognised, for applications
	// whose window class is not their binary name ("google-chrome-stable"
	// launching a window classed "Google-chrome"). Empty matches on App.
	Match string
	// Workspace is where the window goes, 1–99.
	Workspace int
	// Float lifts the window out of the tiling layout; Size and position are
	// only meaningful then, because tiled geometry belongs to the layout.
	Float bool
	// Width and Height are the floating size in pixels; zero means no size
	// directive.
	Width, Height int
	// X and Y are the floating position in pixels; HasPosition distinguishes
	// "place at 0,0" from "no position directive".
	X, Y        int
	HasPosition bool
	// Tile arranges a tiled window: TileSplit tiles it into the workspace's
	// split, TileMaster additionally makes it the master. Empty leaves the
	// layout to the compositor. Mutually exclusive with Float.
	Tile string
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
	}
	return problems
}

func stepProblems(label string, s Step) []string {
	var problems []string
	switch {
	case strings.TrimSpace(s.App) == "":
		problems = append(problems, label+": app is empty; name the program this step launches")
	case !programToken.MatchString(s.App):
		problems = append(problems, fmt.Sprintf("%s: app %q must be a single executable name or absolute "+
			"path (letters, digits, . _ / + -); it is launched directly, never through a shell", label, s.App))
	}
	if s.Workspace < minWorkspace || s.Workspace > maxWorkspace {
		problems = append(problems, fmt.Sprintf("%s: workspace %d does not exist; workspaces are numbered %d to %d",
			label, s.Workspace, minWorkspace, maxWorkspace))
	}
	if s.Match != "" && strings.TrimSpace(s.Match) == "" {
		problems = append(problems, label+": match is blank; omit it to match on the app name")
	}
	hasSize := s.Width != 0 || s.Height != 0
	if hasSize {
		if s.Width <= 0 || s.Height <= 0 || s.Width > maxPixel || s.Height > maxPixel {
			problems = append(problems, fmt.Sprintf("%s: size %d by %d is not a window size in pixels",
				label, s.Width, s.Height))
		}
		if !s.Float {
			problems = append(problems, label+": size needs float = true; a tiled window's size belongs to the layout")
		}
	}
	if s.HasPosition {
		if s.X < -maxPixel || s.X > maxPixel || s.Y < -maxPixel || s.Y > maxPixel {
			problems = append(problems, fmt.Sprintf("%s: position %d,%d is not on any plausible monitor", label, s.X, s.Y))
		}
		if !s.Float {
			problems = append(problems, label+": position needs float = true; a tiled window's position belongs to the layout")
		}
	}
	switch s.Tile {
	case "", TileMaster, TileSplit:
	default:
		problems = append(problems, fmt.Sprintf("%s: tile %q is not an arrangement; use %q or %q",
			label, s.Tile, TileMaster, TileSplit))
	}
	if s.Tile != "" && s.Float {
		problems = append(problems, label+": float and tile are mutually exclusive; a window is floated or tiled, not both")
	}
	return problems
}
