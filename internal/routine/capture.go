package routine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/placement"
	"github.com/rpickz/jarvix/internal/tools"
)

// This file derives a routine from the live window inventory — the heart of
// "save this as my morning setup" (#62). It is deliberately pure: one
// inventory in, one Definition plus its accounting out, no compositor call,
// no file, no clock. Reading state and writing configuration belong to the
// caller (the daemon's capture service), which is what keeps this logic
// hermetic under test and incapable of moving a window by construction.

// PlaceholderApp is written as a step's app when no launch command could be
// derived from the window's class. It is a valid program token on purpose —
// the entry must load, list, and run (the step dedupes onto the live window
// through its match; only a cold launch fails, named in the run's summary) —
// and it is conspicuous on purpose: `jarvix routines` marks the routine
// incomplete until a human replaces it.
const PlaceholderApp = "CHANGE-ME"

// CaptureOptions configure a snapshot.
type CaptureOptions struct {
	// LookPath reports whether a candidate launch command resolves to an
	// installed binary — exec.LookPath in production, a canned map in tests.
	// It is the difference between deriving a command and guessing one: a
	// browser web-app window's class ("chrome-web.whatsapp.com__-Default")
	// is a perfectly legal program token that starts nothing. Nil derives
	// nothing, so every step is a placeholder.
	LookPath func(name string) (string, error)
}

// Capture is one derived routine together with the accounting the spoken
// confirmation, the listing, and the logs need. Counts and app names only —
// deliberately no titles, matching the observability rule that window titles
// never leave the daemon through this feature.
type Capture struct {
	// Definition is the derived routine, ready for Problems and the config
	// writer. Phrases is the generated trigger: the name itself, exactly as
	// the worked examples in docs/configuration.md write it.
	Definition Definition
	// Kept is how many windows became steps; Workspaces how many distinct
	// workspaces they span. These are the spoken confirmation's numbers.
	Kept, Workspaces int
	// Excluded counts the windows the rules below dropped, for the log line.
	Excluded int
	// Placeholders names — in the spoken form, desktop.AppName — the windows
	// whose launch command could not be derived, in step order. Non-empty
	// means the capture is partial: saved, never dropped, and named aloud.
	Placeholders []string
	// Notes carries one comment line per step ("" for most): the TODO the
	// config writer places above a placeholder step so the file explains
	// itself to the human who opens it.
	Notes []string
}

// jarvixClasses are the window classes that are Jarvix's own surfaces. The
// conversation window lives inside the Omarchy shell plugin, so both names
// are excluded: a routine that re-placed the assistant's own window every
// morning would be capturing the mirror in the photograph.
var jarvixClasses = map[string]bool{"jarvix": true, "omarchy-shell": true}

// Snapshot derives a routine named name from one window inventory.
//
// The exclusion rules, each a documented decision (they are also the
// docs/configuration.md list — change both together):
//
//   - The Jarvix window itself (jarvixClasses). Capturing the assistant's
//     own surface would make every routine re-open the conversation window.
//   - Windows that accept no input. On the compositors this seam models,
//     that is what a transient surface — a splash screen, a tooltip layer, a
//     dialog that exists only relative to its parent — looks like in the
//     inventory; an application window the user works in accepts input.
//   - Windows with no class at all. Nothing can be derived, matched, or
//     launched from them, and an empty class is itself the mark of a
//     transient X11 surface rather than an application.
//   - Windows outside the numbered workspaces 1–99 (Hyprland's special
//     workspaces are negative). Special workspaces are summoned, not placed;
//     a routine step cannot express them, so capturing one would write an
//     entry that fails validation.
//
// Unmapped windows never reach this function: the compositor seam drops them
// when parsing the inventory, because a window that is not on screen is not
// part of the layout the user is looking at.
func Snapshot(name string, windows []desktop.Window, opts CaptureOptions) Capture {
	kept := make([]desktop.Window, 0, len(windows))
	excluded := 0
	for _, w := range windows {
		if captureExcluded(w) {
			excluded++
			continue
		}
		kept = append(kept, w)
	}

	// Steps are ordered by workspace, tiled before floating within one, and
	// otherwise as the inventory came (focus recency) — so the written file
	// reads in the order the desktop does, and the same inventory always
	// produces byte-identical configuration.
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].Workspace != kept[j].Workspace {
			return kept[i].Workspace < kept[j].Workspace
		}
		return !kept[i].Floating && kept[j].Floating
	})

	c := Capture{Definition: Definition{
		Name:    strings.TrimSpace(name),
		Phrases: []string{strings.TrimSpace(name)},
		Steps:   make([]Step, 0, len(kept)),
	}, Excluded: excluded}
	seenWorkspace := make(map[int]bool, len(kept))
	for _, w := range kept {
		step, note := captureStep(w, opts)
		c.Definition.Steps = append(c.Definition.Steps, step)
		c.Notes = append(c.Notes, note)
		if note != "" {
			c.Placeholders = append(c.Placeholders, desktop.AppName(w.Class))
		}
		seenWorkspace[w.Workspace] = true
	}
	c.Kept = len(kept)
	c.Workspaces = len(seenWorkspace)
	return c
}

// captureExcluded applies the documented exclusion rules to one window.
func captureExcluded(w desktop.Window) bool {
	class := strings.TrimSpace(w.Class)
	switch {
	case class == "":
		return true
	case jarvixClasses[strings.ToLower(desktop.AppName(class))]:
		return true
	case !w.AcceptsInput:
		return true
	case w.Workspace < minWorkspace || w.Workspace > maxWorkspace:
		return true
	case strings.HasPrefix(strings.ToLower(strings.TrimSpace(w.WorkspaceName)), "special"):
		// Belt and braces: special workspaces are negative on Hyprland, but
		// the name is the documented contract and survives a compositor that
		// numbers them differently.
		return true
	}
	return false
}

// captureStep derives one step from one kept window. note is the TODO line
// for a placeholder step, "" when the command derived.
func captureStep(w desktop.Window, opts CaptureOptions) (Step, string) {
	step := Step{Placement: placement.Placement{Workspace: w.Workspace}}
	if w.Floating {
		step.Mode = placement.ModeFloating
		// Geometry is recorded only when it would replay. A size that cannot
		// be honoured must not be written as if it could (the #177 lesson:
		// captured sizes were recorded for two years against a resize verb
		// that had never worked), so each axis is validated through the
		// vocabulary's own parser and dropped if it would not load.
		width, wErr := placement.ParseExtent(strconv.Itoa(w.Width) + "px")
		height, hErr := placement.ParseExtent(strconv.Itoa(w.Height) + "px")
		if wErr == nil && hErr == nil {
			step.Width, step.Height = width, height
		}
		if p := (placement.Placement{Mode: placement.ModeFloating,
			X: w.X, Y: w.Y, HasPosition: true}); len(p.Problems(false)) == 0 {
			step.X, step.Y, step.HasPosition = w.X, w.Y, true
		}
	} else {
		// Tiled windows are captured as tiled, and without a proportion. The
		// inventory reports a tiled window's geometry, but replaying it would
		// move splits in an order that is not the order they were made in, so
		// a captured share is a promise the replay cannot keep; the honest
		// capture is the arrangement, and the proportion is a hand edit or a
		// form edit. Master is not guessed either — the inventory does not
		// say which window holds the pane, and promoting the wrong one on
		// every run is worse than promoting none.
		step.Mode = placement.ModeTiled
	}

	app, derived := deriveCommand(w.Class, opts.LookPath)
	if !derived {
		step.App = PlaceholderApp
		step.Match = w.Class
		return step, fmt.Sprintf("TODO: no installed command matched this window's class (%q); "+
			"set app to the program that launches it", w.Class)
	}
	step.App = app
	if !strings.EqualFold(app, w.Class) {
		// The class is not the binary's name, so the dedupe matcher needs
		// telling what an already-running window looks like — otherwise every
		// run would launch a second copy, the exact annoyance dedupe exists
		// to prevent.
		step.Match = w.Class
	}
	return step, ""
}

// deriveCommand turns a window class into a launch command, sharing identity
// logic with the dedupe matcher rather than keeping a second table: the
// candidate is desktop.AppName's spoken form of the class (which collapses
// reverse-DNS classes to their app segment), and each candidate must both be
// a valid program token and resolve to an installed binary. The candidate
// spellings come from tools.LaunchCandidates — the one copy of the "-desktop"
// packaging convention ("signal" → "signal-desktop"), shared with the
// launcher's near-match suggestions (issue #71).
//
// A candidate that resolves but differs from the class is handled by the
// caller writing a match override — derivation never loosens what dedupe
// will later search for.
func deriveCommand(class string, lookPath func(string) (string, error)) (string, bool) {
	if lookPath == nil {
		return "", false
	}
	base := strings.ToLower(strings.TrimSpace(desktop.AppName(class)))
	for _, candidate := range tools.LaunchCandidates(base) {
		if candidate == "" || candidate == "-desktop" || !programToken.MatchString(candidate) {
			continue
		}
		if _, err := lookPath(candidate); err == nil {
			return candidate, true
		}
	}
	return "", false
}

// IncompleteSteps returns the indices of steps still carrying the capture
// placeholder — the "needs a human hand" marker `jarvix routines` and the
// routines.list surface show until the user resolves them.
func (d Definition) IncompleteSteps() []int {
	var incomplete []int
	for i, s := range d.Steps {
		if s.App == PlaceholderApp {
			incomplete = append(incomplete, i)
		}
	}
	return incomplete
}

// Claims reports whether the step's dedupe query would claim the window,
// using the same matcher the runner uses (tools.FindWindow). Exposed so the
// capture round-trip can assert — with the real matcher, not a copy of its
// rules — that every derived step resolves back to the window it came from.
func (s Step) Claims(w desktop.Window) bool {
	got, ok := tools.FindWindow(s.matchQuery(), []desktop.Window{w})
	return ok && got.Address == w.Address
}
