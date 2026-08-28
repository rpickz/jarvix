package config

import (
	"fmt"
	"strings"

	"github.com/rpickz/jarvix/internal/placement"
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
	// Enabled parks the routine without deleting it (issue #93, the one
	// `enabled` convention shared with [[knowledge.feeds]] and [[scripts]]):
	// false takes its phrases out of the intent grammar on the standard
	// reload and its schedule off the clock, while the entry — steps,
	// comments, everything — stays listed and validated. A pointer because
	// absent means true: the key only appears in config.toml when someone
	// (hand or window) chose to write it.
	Enabled *bool `toml:"enabled"`
	// Phrases are the literal trigger phrases the intent router matches, so
	// they follow intent grammar (plain spoken words, no placeholders) and
	// must not collide with built-in or custom intents — validated at load.
	Phrases []string `toml:"phrases"`
	// Schedule optionally fires the routine on a clock (ADR 0032): a time of
	// day with optional days — "08:30", "08:30 mon-fri". Empty means
	// phrase-triggered only. The clockfire travels the same gated session
	// path as the phrase; only allow-tier entries execute unattended.
	Schedule string `toml:"schedule"`
	// Announce opts a scheduled firing's summary into speech. Off by default
	// on purpose: an unattended run reports through the activity feed and a
	// notification, never a voice at whatever hour the schedule names.
	Announce bool `toml:"announce"`
	// Steps run in order.
	Steps []RoutineStep `toml:"steps"`
}

// RoutineStep is one [[routines.steps]] table: what to launch, and the
// window-placement vocabulary (ADR 0056) spelled as TOML scalars.
//
// The placement keys are the vocabulary's own field names (placement.Fields),
// which is what makes the file, the form and the window tools one surface
// rather than three: a validation message written once lands on the right
// control in the window and reads correctly in a config-load error.
//
// Three keys are SUPERSEDED and still accepted — `float`, `size` and `tile`,
// the whole placement vocabulary before ADR 0056. Routines written against
// them keep working, unchanged, and translate into the new mode on load; the
// window and the capture writer emit only the new spelling, so an entry
// migrates the first time anyone saves it. Refusing them at load would have
// broken every routine in the field to make a schema tidier, which is not a
// trade worth making.
type RoutineStep struct {
	// App is the program to launch: one bare executable name or absolute
	// path, started directly — never a command line, never a shell. Exactly
	// one of App and DesktopEntry says what a step opens.
	App string `toml:"app"`
	// DesktopEntry names an XDG desktop entry instead — "ChatGPT" or
	// "ChatGPT.desktop", as it appears in the applications menu (#175). Its
	// own Exec is what runs, which is the only way to launch the web apps and
	// the several applications on this desktop that have no binary on PATH.
	// A name this machine has no entry for is refused when the routine is
	// loaded or saved, not discovered as an eight-second silence at run time.
	DesktopEntry string `toml:"desktop_entry"`
	// Args are handed to the program as a literal argv: no shell, no word
	// splitting, no expansion, no globbing. `args = ["--profile-directory=Profile 3"]`
	// is one argument containing a space, and a value containing `;`, `&&` or
	// `$(` is one argument containing those characters. They are the user's
	// own configuration — the model cannot write here (ADR 0022).
	Args []string `toml:"args"`
	// Identity gives the launched window a class of the routine's own
	// choosing, for programs that accept such a flag (Chromium's `--class=`).
	// It is how two steps launching one binary with different arguments are
	// told apart: Chromium runs every profile in a single process, so two
	// profile windows are identical in class, PID and cmdline, and only an
	// identity chosen before the launch can distinguish them.
	Identity string `toml:"identity"`
	// Match optionally overrides how an already-running window is recognised
	// (for apps whose window class differs from their binary name). Empty
	// matches on Identity when there is one, and on App otherwise.
	Match string `toml:"match"`
	// Launch says what to do when a matching window is already open:
	// "if_missing" (the default — adopt it) or "always" (start a fresh one
	// every run). Per step, because both answers are right for different
	// steps of the same routine.
	Launch string `toml:"launch"`
	// Workspace is where the window goes, 1–99.
	Workspace int `toml:"workspace"`
	// Monitor is which screen the workspace belongs on: a connector name
	// ("HDMI-A-1"), or "current" for the one holding focus. Empty leaves the
	// workspace where the compositor has it.
	Monitor string `toml:"monitor"`
	// Mode is how the window sits: "tiled", "floating", "pinned",
	// "fullscreen" or "maximised".
	Mode string `toml:"mode"`
	// Width and Height are the share of the screen: a percentage of the
	// monitor's usable area ("66%") or pixels ("1200px"). On a tiled window
	// they move the split; on a floating one they are the window's own size.
	Width  string `toml:"width"`
	Height string `toml:"height"`
	// Position is [x, y] in pixels, floating modes only.
	Position []int `toml:"position"`
	// PlaceNext says where the NEXT tiled window on this workspace goes
	// relative to this one: "right", "left", "below" or "above". Step order
	// is therefore part of the layout's meaning.
	PlaceNext string `toml:"place_next"`
	// Master promotes a tiled window into the layout's big pane. Only
	// master-family layouts have one; on any other the run says so.
	Master bool `toml:"master"`
	// Focus is "silent" (the default: the window is placed and the view stays
	// put) or "follow".
	Focus string `toml:"focus"`
	// Float is the superseded spelling of mode = "floating".
	Float bool `toml:"float"`
	// Size is the superseded spelling of width/height in pixels.
	Size []int `toml:"size"`
	// Tile is the superseded spelling of mode = "tiled" ("split") and
	// mode = "tiled" with master = true ("master").
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
			// Launch is parsed leniently here for the same reason the
			// placement keys are: this conversion feeds the RUNNER, and a
			// value the parser refuses must reach it as "not said" rather
			// than as a wrong value. stepLaunchProblems refuses the document
			// outright, so no bad value ever reaches a run.
			policy, _ := routine.ParseLaunchPolicy(s.Launch)
			def.Steps = append(def.Steps, routine.Step{
				App: s.App, DesktopEntry: s.DesktopEntry,
				Args:     append([]string(nil), s.Args...),
				Identity: s.Identity, Match: s.Match, Launch: policy,
				Placement: s.placement(),
			})
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
			App: s.App, DesktopEntry: s.DesktopEntry,
			Args:     append([]string(nil), s.Args...),
			Identity: s.Identity, Match: s.Match,
			// The default policy is written as absence, not as the word: a
			// step that never asked for anything must come back out of a
			// round trip looking exactly as it went in, or the window would
			// add a `launch = "if_missing"` line to every step it touched.
			Launch:    launchWord(s.Launch),
			Workspace: s.Workspace,
			Monitor:   string(s.Monitor), Mode: string(s.Mode),
			Width: s.Width.String(), Height: s.Height.String(),
			PlaceNext: string(s.PlaceNext), Master: s.Master, Focus: string(s.Focus),
		}
		if s.HasPosition {
			step.Position = []int{s.X, s.Y}
		}
		r.Steps = append(r.Steps, step)
	}
	return r
}

// launchWord renders a launch policy back into its TOML value, writing
// nothing for the default.
func launchWord(p routine.LaunchPolicy) string {
	if p == routine.LaunchAlways {
		return string(routine.LaunchAlways)
	}
	return ""
}

// placement reads one step's placement out of its TOML keys, translating the
// superseded spellings on the way.
//
// Parse errors are swallowed here and reported by stepPlacementProblems,
// which runs over the same keys: this conversion feeds the *runner*, and a
// half-parsed value must reach it as "not said" rather than as a wrong value,
// while the loader refuses the document outright. Two functions rather than
// one because a Config is converted in places where returning an error would
// mean threading one through code that has nothing to do with placement — and
// no document with a bad value ever reaches a runner, because a load carrying
// problems is a load that failed.
func (s RoutineStep) placement() placement.Placement {
	p := placement.Placement{
		Workspace: s.Workspace,
		Monitor:   placement.MonitorRef(strings.TrimSpace(s.Monitor)),
		Master:    s.Master,
	}
	p.Mode, _ = placement.ParseMode(s.Mode)
	p.Width, _ = placement.ParseExtent(s.Width)
	p.Height, _ = placement.ParseExtent(s.Height)
	p.PlaceNext, _ = placement.ParsePlaceNext(s.PlaceNext)
	p.Focus, _ = placement.ParseFocus(s.Focus)
	if len(s.Position) == 2 {
		p.X, p.Y, p.HasPosition = s.Position[0], s.Position[1], true
	}
	// The superseded spellings, applied only where the new ones said nothing,
	// so a step carrying both is decided by the new key — and told to pick
	// one by stepPlacementProblems rather than left to guess.
	if p.Mode == "" {
		switch {
		case s.Float:
			p.Mode = placement.ModeFloating
		case strings.EqualFold(strings.TrimSpace(s.Tile), "master"):
			p.Mode, p.Master = placement.ModeTiled, true
		case strings.TrimSpace(s.Tile) != "":
			p.Mode = placement.ModeTiled
		}
	}
	if !p.Width.Set() && !p.Height.Set() && len(s.Size) == 2 {
		p.Width = placement.Pixels(s.Size[0])
		p.Height = placement.Pixels(s.Size[1])
	}
	return p
}

// stepPlacementProblems reports what is wrong with the placement keys as
// TOML: values the vocabulary's parsers refuse, arrays of the wrong length,
// and a step that spells one directive two ways. Everything about the
// resulting placement — a percentage over a hundred, a size on a mode that
// has no size — is routine.Problems' through placement.Problems, so there is
// exactly one copy of those rules.
func stepPlacementProblems(label string, s RoutineStep) []string {
	var problems []string
	for _, field := range []struct {
		key, value string
		parse      func(string) error
	}{
		{placement.FieldMode, s.Mode, func(v string) error { _, err := placement.ParseMode(v); return err }},
		{placement.FieldWidth, s.Width, func(v string) error { _, err := placement.ParseExtent(v); return err }},
		{placement.FieldHeight, s.Height, func(v string) error { _, err := placement.ParseExtent(v); return err }},
		{placement.FieldPlaceNext, s.PlaceNext, func(v string) error { _, err := placement.ParsePlaceNext(v); return err }},
		{placement.FieldFocus, s.Focus, func(v string) error { _, err := placement.ParseFocus(v); return err }},
	} {
		if err := field.parse(field.value); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %s %s", label, field.key, err.Error()))
		}
	}
	if n := len(s.Position); n != 0 && n != 2 {
		problems = append(problems, fmt.Sprintf(
			"%s: position has %d elements; write it as [x, y] in pixels", label, n))
	}
	if n := len(s.Size); n != 0 && n != 2 {
		problems = append(problems, fmt.Sprintf(
			"%s: size has %d elements; write it as [width, height] in pixels", label, n))
	}
	// One directive, one spelling. A step saying both `float = true` and
	// `mode = "tiled"` has contradicted itself, and picking a winner quietly
	// is how a routine ends up doing something nobody wrote.
	if s.Mode != "" {
		for _, legacy := range []struct{ key, value string }{
			{"float", boolWord(s.Float)}, {"tile", strings.TrimSpace(s.Tile)},
		} {
			if legacy.value == "" {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"%s: mode = %q and %s = %s say the same thing two ways; %s is the superseded "+
					"spelling, so delete it", label, s.Mode, legacy.key, legacy.value, legacy.key))
		}
	}
	if (s.Width != "" || s.Height != "") && len(s.Size) == 2 {
		problems = append(problems, fmt.Sprintf(
			"%s: width/height and size say the same thing two ways; size is the superseded "+
				"spelling, so delete it", label))
	}
	return problems
}

// stepLaunchProblems reports what is wrong with the launching keys as TOML —
// the values whose parsers this file reads leniently, so a refusal is not
// swallowed by the conversion that feeds the runner. Everything about the
// resulting step (what it launches, whether this machine has it, whether two
// steps could be confused for one another) is routine.Problems'.
func stepLaunchProblems(label string, s RoutineStep) []string {
	var problems []string
	if _, err := routine.ParseLaunchPolicy(s.Launch); err != nil {
		problems = append(problems, fmt.Sprintf("%s: %s %s", label, routine.FieldLaunch, err.Error()))
	}
	return problems
}

// boolWord renders a superseded boolean for a message, or "" when it is not
// set — absent and false are the same thing in TOML, and a message
// complaining about `float = false` would name a key nobody wrote.
func boolWord(v bool) string {
	if v {
		return "true"
	}
	return ""
}

// IsEnabled reads the enabled switch with its default applied (absent means
// true) — the same reading KnowledgeFeed.IsEnabled gives the shared
// convention.
func (r Routine) IsEnabled() bool {
	return r.Enabled == nil || *r.Enabled
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
		problems = append(problems,
			scheduleProblems(fmt.Sprintf("routines[%d] (%q)", i, r.Name), r.Schedule, r.Announce)...)
		for j, s := range r.Steps {
			label := fmt.Sprintf("routines[%d] (%q) steps[%d]", i, r.Name, j)
			problems = append(problems, stepPlacementProblems(label, s)...)
			problems = append(problems, stepLaunchProblems(label, s)...)
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
