package config

import (
	"fmt"

	"github.com/rpickz/jarvix/internal/routine"
)

// Routine is one [[routines]] table (ADR 0026): a named, phrase-triggered
// sequence of app placements. The schema is deliberately flat — strings,
// integers, and two-element integer arrays — because the capture feature
// (#62) will write these tables programmatically, and a schema a program can
// emit plainly is one a person can read and edit.
//
// Routines are hand-edited TOML like [[intents.custom]] and [ai.<name>]:
// structured tables rather than single values, so they are outside the
// config.set surface and land on the next idle-class reload or restart. The
// daemon lists them read-only through `routines.list` (docs/ipc.md).
type Routine struct {
	// Name is what the summary opens with and what `jarvix routines run`
	// takes. Unique across routines, case-insensitively.
	Name string `toml:"name"`
	// Phrases are the literal trigger phrases the intent router matches, so
	// they follow intent grammar (plain spoken words, no placeholders) and
	// must not collide with built-in or custom intents — validated at load.
	Phrases []string `toml:"phrases"`
	// Steps run in order.
	Steps []RoutineStep `toml:"steps"`
}

// RoutineStep is one [[routines.steps]] table.
type RoutineStep struct {
	// App is the program to launch when no matching window exists: one bare
	// executable name or absolute path, launched through the compositor —
	// never a command line, never a shell.
	App string `toml:"app"`
	// Match optionally overrides how an already-running window is recognised
	// (for apps whose window class differs from their binary name). Empty
	// matches on App.
	Match string `toml:"match"`
	// Workspace is where the window goes, 1–99.
	Workspace int `toml:"workspace"`
	// Float lifts the window out of the tiling layout; size and position
	// apply only then.
	Float bool `toml:"float"`
	// Size is [width, height] in pixels, floating only.
	Size []int `toml:"size"`
	// Position is [x, y] in pixels, floating only.
	Position []int `toml:"position"`
	// Tile arranges a tiled window: "split" tiles it into the workspace's
	// split, "master" additionally makes it the master pane.
	Tile string `toml:"tile"`
}

// RoutineDefinitions converts the TOML tables into the routine package's
// definitions. Conversion is shape-preserving and order-preserving, so the
// labels routine.Problems produces line up with the file's own indices.
func (c Config) RoutineDefinitions() []routine.Definition {
	defs := make([]routine.Definition, 0, len(c.Routines))
	for _, r := range c.Routines {
		def := routine.Definition{
			Name:    r.Name,
			Phrases: append([]string(nil), r.Phrases...),
			Steps:   make([]routine.Step, 0, len(r.Steps)),
		}
		for _, s := range r.Steps {
			step := routine.Step{
				App: s.App, Match: s.Match, Workspace: s.Workspace,
				Float: s.Float, Tile: s.Tile,
			}
			if len(s.Size) == 2 {
				step.Width, step.Height = s.Size[0], s.Size[1]
			}
			if len(s.Position) == 2 {
				step.X, step.Y = s.Position[0], s.Position[1]
				step.HasPosition = true
			}
			def.Steps = append(def.Steps, step)
		}
		defs = append(defs, def)
	}
	return defs
}

// RoutineFromDefinition converts a derived definition into its TOML shape —
// the capture writer's direction (#62), inverse of RoutineDefinitions, kept
// beside it so the two conversions cannot drift apart.
func RoutineFromDefinition(d routine.Definition) Routine {
	r := Routine{
		Name:    d.Name,
		Phrases: append([]string(nil), d.Phrases...),
		Steps:   make([]RoutineStep, 0, len(d.Steps)),
	}
	for _, s := range d.Steps {
		step := RoutineStep{
			App: s.App, Match: s.Match, Workspace: s.Workspace,
			Float: s.Float, Tile: s.Tile,
		}
		if s.Width != 0 || s.Height != 0 {
			step.Size = []int{s.Width, s.Height}
		}
		if s.HasPosition {
			step.Position = []int{s.X, s.Y}
		}
		r.Steps = append(r.Steps, step)
	}
	return r
}

// Incomplete reports whether any step still carries the capture placeholder
// (#62) — the marker `jarvix routines` and routines.list surface until a
// human resolves the launch command.
func (r Routine) Incomplete() bool {
	for _, s := range r.Steps {
		if s.App == routine.PlaceholderApp {
			return true
		}
	}
	return false
}

// routineProblems validates the [[routines]] tables: the TOML shapes here,
// the structural rules in routine.Problems, and — through intentProblems,
// which compiles the real router — the phrase grammar and collisions. There
// is no second, weaker copy of any rule.
func (c Config) routineProblems() []string {
	if len(c.Routines) == 0 {
		return nil
	}
	var problems []string
	for i, r := range c.Routines {
		for j, s := range r.Steps {
			label := fmt.Sprintf("routines[%d] (%q) steps[%d]", i, r.Name, j)
			if n := len(s.Size); n != 0 && n != 2 {
				problems = append(problems, fmt.Sprintf(
					"%s: size has %d elements; write it as [width, height] in pixels", label, n))
			}
			if n := len(s.Position); n != 0 && n != 2 {
				problems = append(problems, fmt.Sprintf(
					"%s: position has %d elements; write it as [x, y] in pixels", label, n))
			}
		}
	}
	problems = append(problems, routine.Problems(c.RoutineDefinitions())...)
	if !c.Intents.Enabled {
		// The router is the only trigger there is: with it disabled a phrase
		// would fall through to the model, which must never be how a routine
		// "runs". Saying so at load beats a phrase that silently stops working.
		problems = append(problems,
			"routines are configured but intents.enabled is false; the intent router is what "+
				"triggers routines, so re-enable it or remove the [[routines]] tables")
	}
	return problems
}
