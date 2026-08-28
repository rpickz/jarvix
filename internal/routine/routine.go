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
//   - Dedupe is the step's own decision (issue #175). A step says whether it
//     adopts a matching window that is already open or starts a fresh one,
//     because both answers are right for different steps of the same routine:
//     the browser left open all week should be adopted, the scratch terminal
//     should be new. Adopting is the default and what every routine written
//     before the key existed did — a morning routine that opens a second
//     browser every morning is worse than no routine at all.
//   - Failure continues, and says which failure it was. One application that
//     will not start, or whose window never appears inside the bounded wait,
//     is recorded and stepped past; the remaining steps still run, and the
//     single spoken summary at the end names what failed and how — not
//     installed, opened nothing, or opened something that did not match. One
//     dead app must not strand the other six, and a step that launched
//     nothing must not report as placed.
//   - Nothing here is a command line. A step names a program or a desktop
//     entry and, since #175, a list of arguments — a literal argv handed to
//     execve, never a string handed to a shell. There is deliberately no "run
//     this command" step kind: that would put a shell behind a single spoken
//     phrase, and it is excluded rather than gated. The arguments come from
//     the user's own configuration file; ADR 0022's refusal to give the
//     model-facing launch tool arguments is unchanged.
package routine

import (
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	"github.com/rpickz/jarvix/internal/desktopentry"
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
	// App is the program to launch: a single bare executable name or
	// absolute path, the same rule the terminal intent enforces. Exactly one
	// of App and DesktopEntry says what a step opens.
	App string
	// DesktopEntry names an XDG desktop entry to launch instead — "ChatGPT"
	// or "ChatGPT.desktop", the same name the application menu shows. The
	// entry's own Exec is what runs (issue #175): on this desktop the web
	// apps and several installed applications exist only as entries, so a
	// step limited to bare binaries could not open them at all.
	DesktopEntry string
	// Args are passed to the program as a literal argv — no shell, no word
	// splitting, no expansion, no globbing. They come from the user's own
	// configuration file, never from the model (see launch.go), which is what
	// makes them safe in a way a model-supplied argument would not be.
	Args []string
	// Identity gives the launched window a class of the routine's choosing,
	// for programs that accept such a flag. It is how two steps launching one
	// binary with different arguments are told apart — the two Chromium
	// profiles, which are otherwise identical in class, PID and cmdline
	// because Chromium runs every profile in one process. Empty means the
	// window is recognised by whatever class it happens to take.
	Identity string
	// Match overrides how an existing window is recognised, for applications
	// whose window class is not their binary name ("google-chrome-stable"
	// launching a window classed "Google-chrome"). Empty matches on Identity
	// when there is one, and on App otherwise.
	Match string
	// Launch decides what happens when a matching window is already open:
	// adopt it, or start a fresh one anyway. Empty adopts, which is what
	// every routine written before this key existed did.
	Launch LaunchPolicy
	// Placement is where the window goes: mode, proportion, arrangement and
	// target. Embedded so a step reads as one thing rather than two.
	placement.Placement
}

// matchQuery is what the dedupe matcher looks for.
//
// Identity comes before App because it is the stronger statement: a step that
// launched its window with `--class=work` produces a window classed exactly
// that, and matching on the binary name instead would find the OTHER profile's
// window just as readily — which is the bug identity exists to fix.
func (s Step) matchQuery() string {
	if q := strings.TrimSpace(s.Match); q != "" {
		return q
	}
	if id := strings.TrimSpace(s.Identity); id != "" {
		return id
	}
	if entry := strings.TrimSpace(s.DesktopEntry); entry != "" && strings.TrimSpace(s.App) == "" {
		return entry
	}
	return s.App
}

// Launches reports what the step opens, in the user's own words, for a
// sentence that has to name it.
func (s Step) Launches() string {
	if app := strings.TrimSpace(s.App); app != "" {
		return app
	}
	return strings.TrimSpace(s.DesktopEntry)
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
	return ProblemsWith(defs, resolverFor(defs))
}

// resolverFor builds the resolver Problems judges launch targets with, and
// reads the machine's desktop entries only when some step actually names one.
//
// The laziness is not an optimisation, it is a promise: a configuration
// written before this change contains no `desktop_entry`, so validating it
// touches no applications directory and behaves exactly as it did.
func resolverFor(defs []Definition) Resolver {
	r := Resolver{LookPath: exec.LookPath}
	for _, def := range defs {
		for _, s := range def.Steps {
			if strings.TrimSpace(s.DesktopEntry) != "" {
				r.Entries = desktopentry.Default()
				return r
			}
		}
	}
	return r
}

// MachineResolver is the resolver for asking what THIS machine can launch:
// PATH through exec.LookPath, and the desktop entries — read only when some
// step names one.
func MachineResolver(defs []Definition) Resolver { return resolverFor(defs) }

// InstallProblem is one step this machine cannot currently launch, keyed to
// the step and the field a form would show it on.
type InstallProblem struct {
	Step    int
	Field   string
	Message string
}

// InstallProblems reports what one routine's steps cannot launch here and
// now: a program that is not on PATH, a desktop entry whose program is gone.
//
// It is separate from Problems for the reason Resolver.Describe explains — it
// is a question about the machine rather than about the file — and the
// separation is load-bearing rather than tidy. Its answer is shown, never
// used to refuse: the entry surface turns each of these into a NOTE beside
// the field, so a routine can be written for an application that is not
// installed yet. Authoring the routine first and installing the program
// second is ordinary, and a routine written on a desktop has to stay editable
// from a laptop that has none of it.
//
// The place this answer is enforced is the run, which asks the same question
// through the same resolver a moment before launching and reports
// FailureNotInstalled by name.
func InstallProblems(def Definition, resolver Resolver) []InstallProblem {
	var problems []InstallProblem
	for i, step := range def.Steps {
		if strings.TrimSpace(step.App) == PlaceholderApp {
			continue
		}
		if _, err := resolver.Resolve(step); err != nil {
			problems = append(problems, InstallProblem{
				Step: i, Field: launchField(step), Message: err.Error()})
		}
	}
	return problems
}

// ProblemsWith is Problems against a given resolver, so a test — and the
// window's form, which validates a draft against the same machine the run
// will use — can say what is installed without installing anything.
func ProblemsWith(defs []Definition, resolver Resolver) []string {
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
			problems = append(problems, stepProblems(fmt.Sprintf("%s steps[%d]", label, j), step, resolver)...)
		}
		problems = append(problems, danglingArrangements(label, def)...)
		problems = append(problems, indistinguishableSteps(label, def)...)
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

// indistinguishableSteps catches the failure this ticket was reported for:
// two steps that launch the same thing with DIFFERENT arguments and then look
// for their windows with the same query.
//
// On a running desktop those two windows are the same window as far as
// anything observable goes — Chromium runs every profile in one process, so
// class, PID and /proc/<pid>/cmdline are identical for the personal profile
// and the work one. Whichever step runs first claims whichever window the
// compositor happens to list first, and the routine places the wrong browser
// on the wrong screen roughly half the time. There is no run-time fix for
// that, only a launch-time one — give the windows different identities — so
// the pairing is refused when the routine is saved, with the mechanism named
// in the message.
//
// Same arguments is deliberately NOT a problem: two terminals, two editors,
// two of anything identical are legitimately interchangeable, and the runner
// already claims windows one at a time so each step gets its own.
func indistinguishableSteps(label string, def Definition) []string {
	var problems []string
	for i, step := range def.Steps {
		if !step.Launch.Adopts() {
			// A step that always launches never adopts, so it cannot adopt
			// the wrong window. It still needs its own window afterwards,
			// which is what the identity mechanism is for — but that is the
			// user's call to make, not a refusal to save.
			continue
		}
		for j := i + 1; j < len(def.Steps); j++ {
			other := def.Steps[j]
			if !strings.EqualFold(step.matchQuery(), other.matchQuery()) {
				continue
			}
			if slices.Equal(step.Args, other.Args) &&
				strings.EqualFold(step.Launches(), other.Launches()) {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"%s steps[%d] and steps[%d] both look for a window matching %q but launch different "+
					"things, so whichever runs first takes whichever window is listed first; give one of "+
					"them %s (a window class of its own) or a distinct %s",
				label, i, j, step.matchQuery(), FieldIdentity, FieldMatch))
		}
	}
	return problems
}

// argProblems validates a step's argument list.
//
// What is NOT checked here is the point: an argument containing `;`, `&&`, a
// backtick or `$(` is a perfectly good argument and is left alone. Those
// characters are only dangerous to a shell, and there is no shell on this
// path — the list becomes an argv and reaches execve as a list. Refusing them
// would be cargo-cult validation that removed a real capability (a URL with a
// query string, a `--flag=a;b`) to defend against a thing that cannot happen.
// The only refusals are what execve itself cannot carry, and bounds.
func argProblems(label string, s Step) []string {
	var problems []string
	if len(s.Args) > maxStepArgs {
		problems = append(problems, fmt.Sprintf("%s: %s has %d entries; a step takes at most %d",
			label, FieldArgs, len(s.Args), maxStepArgs))
	}
	for i, arg := range s.Args {
		switch {
		case strings.ContainsRune(arg, 0):
			problems = append(problems, fmt.Sprintf(
				"%s: %s[%d] contains a null byte, which no argument can carry", label, FieldArgs, i))
		case len(arg) > maxStepArgLen:
			problems = append(problems, fmt.Sprintf("%s: %s[%d] is %d characters; the limit is %d",
				label, FieldArgs, i, len(arg), maxStepArgLen))
		}
	}
	return problems
}

// identityProblems validates a step's chosen window identity, and — the part
// that matters — refuses one this machine cannot actually impose.
//
// An identity is only real if the program accepts a flag that sets its
// window's class. Accepting `identity` for a program with no such flag would
// produce a step that launches correctly, matches nothing, and reports its
// own window as never having appeared: a silent lie, dressed as a feature.
// Saying so at save time is the whole difference.
func identityProblems(label string, s Step, id string) []string {
	var problems []string
	if !identityToken.MatchString(id) {
		problems = append(problems, fmt.Sprintf(
			"%s: %s %q must be a window class (letters, digits, . _ -, up to 64 characters)",
			label, FieldIdentity, id))
		return problems
	}
	if entry := strings.TrimSpace(s.DesktopEntry); entry != "" {
		problems = append(problems, fmt.Sprintf(
			"%s: %s cannot be set on a %s step — the entry's own Exec decides what runs, and a class "+
				"flag appended to it would be an argument the wrapper never passes on",
			label, FieldIdentity, FieldDesktopEntry))
		return problems
	}
	if app := strings.TrimSpace(s.App); app != "" && app != PlaceholderApp {
		if _, ok := IdentityFlag(app); !ok {
			problems = append(problems, fmt.Sprintf(
				"%s: %s cannot be set for %s — it takes no flag for choosing its window class. "+
					"The programs that do are %s; for anything else, tell the two windows apart with "+
					"a distinct %s",
				label, FieldIdentity, app, strings.Join(IdentityCapablePrograms(), ", "), FieldMatch))
		}
	}
	return problems
}

// launchField names the key a launch failure belongs to, so the form pins the
// message to the control the user has to change.
func launchField(s Step) string {
	if strings.TrimSpace(s.DesktopEntry) != "" {
		return FieldDesktopEntry
	}
	return FieldApp
}

// stepProblems validates one step: the launching half here, the placement
// half through the vocabulary. There is deliberately no second copy of the
// placement rules — the form, the tools and this loader all run
// placement.Problems, so a value refused when a routine is saved is refused
// identically when a tool sends it.
func stepProblems(label string, s Step, resolver Resolver) []string {
	var problems []string
	app, entry := strings.TrimSpace(s.App), strings.TrimSpace(s.DesktopEntry)
	switch {
	case app == "" && entry == "":
		problems = append(problems, label+": "+FieldApp+" is empty; name the program this step launches, "+
			"or name a desktop entry in "+FieldDesktopEntry)
	case app != "" && entry != "":
		problems = append(problems, fmt.Sprintf(
			"%s: %s = %q and %s = %q both say what to launch; keep one of them",
			label, FieldApp, s.App, FieldDesktopEntry, s.DesktopEntry))
	case app != "" && app != PlaceholderApp && !programToken.MatchString(app):
		problems = append(problems, fmt.Sprintf("%s: %s %q must be a single executable name or absolute "+
			"path (letters, digits, . _ / + -); it is launched directly, never through a shell",
			label, FieldApp, s.App))
	case entry != "" && !entryID.MatchString(strings.TrimSuffix(entry, desktopentry.Suffix)):
		problems = append(problems, fmt.Sprintf(
			"%s: %s %q is not an entry name; write it as it appears in the applications menu "+
				"(\"ChatGPT\" or \"ChatGPT.desktop\"), never as a path",
			label, FieldDesktopEntry, s.DesktopEntry))
	}
	problems = append(problems, argProblems(label, s)...)
	if id := strings.TrimSpace(s.Identity); id != "" {
		problems = append(problems, identityProblems(label, s, id)...)
	}
	if _, err := ParseLaunchPolicy(string(s.Launch)); err != nil {
		problems = append(problems, fmt.Sprintf("%s: %s %s", label, FieldLaunch, err.Error()))
	}
	if s.Match != "" && strings.TrimSpace(s.Match) == "" {
		problems = append(problems, label+": "+FieldMatch+" is blank; omit it to match on the app name")
	}
	// Whether the step is launchable AT ALL — the desktop entry exists, is an
	// application, has an Exec, does not want a terminal; the identity flag
	// can be applied to what it resolves to. Describe rather than Resolve, so
	// this asks nothing of PATH: whether the machine currently HAS the
	// program is a different question, asked where it can be acted on (see
	// Resolver.Describe). A capture placeholder is exempt: #62 writes entries
	// deliberately incomplete for a human to finish, and refusing to load one
	// would break the feature that wrote it.
	if len(problems) == 0 && app != PlaceholderApp {
		if _, err := resolver.Describe(s); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %s: %s", label, launchField(s), err.Error()))
		}
	}
	// A routine step must name a workspace: it is describing a desktop, and
	// "wherever the compositor felt like" is not a description. The tools,
	// which place the window already in front of the user, pass false.
	for _, p := range s.Problems(true) {
		problems = append(problems, label+": "+p.String())
	}
	return problems
}
