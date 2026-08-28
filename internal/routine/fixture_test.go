package routine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/placement"
)

// This file is the user's own case, made a fixture (issue #176): a personal
// browser at two thirds of the top monitor, X above ChatGPT in the remaining
// third at half each, a work browser filling the bottom monitor. It is the
// arrangement they set out to build, could not, and worked around with a
// shell script — so it is the arrangement the vocabulary has to be able to
// express, and the dispatch sequence it produces is asserted exactly.
//
// Exactly, not approximately, because the sequence IS the feature. A tiling
// compositor decides where a window lands the moment it maps, from the
// focused window and the preselection standing at that instant, so
// "launch, preselect, launch, preselect, launch, then size" is not one
// possible ordering of the work — it is the only ordering that produces this
// desktop. A test that counted the dispatches, or checked them as a set,
// would pass on a run that opened all three windows in a column.
//
// The launching half is deliberately thin here: a step still launches one
// bare program name (issue #175 gives steps arguments and desktop entries, at
// which point these three become two Chromium profiles and two web apps).
// What is being pinned is where the windows GO.

// theMonitors is the user's arrangement: a 3440-wide ultrawide above a
// 5120-wide one, each with a 26-pixel bar reserved at the top, and neither
// currently showing the workspace the routine wants on it.
func theMonitors() []placement.Monitor {
	return []placement.Monitor{
		{Name: "HDMI-A-1", X: 840, Y: 0, Width: 3440, Height: 1440, Scale: 1,
			Reserved: [4]int{0, 26, 0, 0}, Focused: true, ActiveWorkspace: 3},
		{Name: "DP-2", X: 0, Y: 1440, Width: 5120, Height: 1440, Scale: 1,
			Reserved: [4]int{0, 26, 0, 0}, ActiveWorkspace: 5},
	}
}

// morningSetup is the routine, written the way a user would write it.
func morningSetup() Definition {
	return Definition{
		Name:    "morning setup",
		Phrases: []string{"morning setup"},
		Steps: []Step{
			// Two thirds of the top screen, and the next window goes to its
			// right — which is what leaves the remaining third for the stack.
			{App: "chromium", Match: "chromium", Placement: placement.Placement{
				Workspace: 1, Monitor: "HDMI-A-1", Mode: placement.ModeTiled,
				Width: placement.Percent(66), Height: placement.Percent(100),
				PlaceNext: placement.PlaceNextRight,
			}},
			// X takes the remaining third, and the window after it goes below.
			{App: "x", Match: "x", Placement: placement.Placement{
				Workspace: 1, Monitor: "HDMI-A-1", Mode: placement.ModeTiled,
				PlaceNext: placement.PlaceNextBelow,
			}},
			// ChatGPT lands below X and takes half of what they share.
			{App: "chatgpt", Match: "chatgpt", Placement: placement.Placement{
				Workspace: 1, Monitor: "HDMI-A-1", Mode: placement.ModeTiled,
				Height: placement.Percent(50),
			}},
			// The work browser fills the bottom screen on its own.
			{App: "chromium-work", Match: "chromium-work", Placement: placement.Placement{
				Workspace: 2, Monitor: "DP-2", Mode: placement.ModeTiled,
			}},
		},
	}
}

// runMorningSetup drives the fixture against a fake compositor, making each
// launched window appear after one poll — the way a real launch lands while
// the routine is already waiting.
func runMorningSetup(t *testing.T) (*desktop.FakeCompositor, string) {
	t.Helper()
	comp := desktop.NewFakeCompositor()
	comp.Outputs = theMonitors()
	appearing := []desktop.Window{
		{Address: "0xa", Class: "chromium", Workspace: 1, Width: 3440, Height: 1414},
		{Address: "0xb", Class: "x", Workspace: 1, Width: 1170, Height: 1414},
		{Address: "0xc", Class: "chatgpt", Workspace: 1, Width: 1170, Height: 707},
		{Address: "0xd", Class: "chromium-work", Workspace: 2, Width: 5120, Height: 1414},
	}
	r, _ := newTestRunner(comp, []Definition{morningSetup()}, nil, func(poll int) {
		// One window per poll, in step order: the launches are serialised, so
		// step two's window cannot appear before step one's has been placed.
		if poll >= 1 && poll <= len(appearing) {
			comp.SetWindows(appearing[:poll]...)
		}
	})
	summary, err := r.Run(context.Background(), "morning setup")
	if err != nil {
		t.Fatal(err)
	}
	return comp, summary
}

// TestTheMorningSetupProducesItsExactDispatchSequence is the acceptance
// criterion: the user's own example, expressed in the vocabulary, produces
// the placement they asked for.
func TestTheMorningSetupProducesItsExactDispatchSequence(t *testing.T) {
	comp, summary := runMorningSetup(t)
	if summary != "Morning setup: all four apps placed." {
		t.Errorf("summary = %q", summary)
	}

	want := []string{
		// Both workspaces are put on their screens before anything launches:
		// a workspace that arrives on the right monitor after its windows
		// have opened has already shown the user the wrong screen, and on a
		// tiling layout it re-tiles them on the way.
		"workspace_monitor", "workspace_monitor",
		// The personal browser. The view goes to its workspace first, because
		// a new window maps onto whatever is in view; then the window is
		// placed as a whole state (out of fullscreen, then tiled); then it is
		// focused and the layout is told the next window goes to its right.
		"workspace", "spawn", "move", "fullscreen", "float", "focus", "preselect",
		// X lands in that preselected space, and preselects downwards itself.
		"workspace", "spawn", "move", "fullscreen", "float", "focus", "preselect",
		// ChatGPT lands below X. Nothing follows it, so it preselects nothing.
		"workspace", "spawn", "move", "fullscreen", "float",
		// The work browser, alone on the bottom screen.
		"workspace", "spawn", "move", "fullscreen", "float",
		// The proportions, LAST — a tiled resize moves the split the window
		// sits in, so it only means anything once the windows it shares that
		// split with exist.
		"focus", "resize", "focus", "resize",
		// And the run ends on the first step's workspace.
		"workspace",
	}
	got := verbs(comp.Actions())
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dispatches =\n  %v\nwant\n  %v", got, want)
	}
}

// TestTheMorningSetupPutsEachWindowWhereItWasAsked reads the payloads of the
// sequence above: the screens, the preselections, and — the number this whole
// ticket exists for — the two-thirds share, resolved against the top
// monitor's USABLE area rather than its output size.
func TestTheMorningSetupPutsEachWindowWhereItWasAsked(t *testing.T) {
	comp, _ := runMorningSetup(t)
	actions := comp.Actions()

	byVerb := func(verb string) []desktop.FakeAction {
		var out []desktop.FakeAction
		for _, a := range actions {
			if a.Verb == verb {
				out = append(out, a)
			}
		}
		return out
	}

	screens := byVerb("workspace_monitor")
	if len(screens) != 2 ||
		screens[0].Workspace != 1 || screens[0].Monitor != "HDMI-A-1" ||
		screens[1].Workspace != 2 || screens[1].Monitor != "DP-2" {
		t.Errorf("workspaces went to %+v", screens)
	}

	arrangement := byVerb("preselect")
	if len(arrangement) != 2 ||
		arrangement[0].Direction != desktop.PreselectRight ||
		arrangement[1].Direction != desktop.PreselectDown {
		t.Errorf("arrangement = %+v, want right then down — two thirds on the left, "+
			"the rest stacked", arrangement)
	}

	sizes := byVerb("resize")
	if len(sizes) != 2 {
		t.Fatalf("resizes = %+v, want the browser's share and ChatGPT's half", sizes)
	}
	// 66% of the top monitor's usable width (3440, the bar takes height only)
	// is 2270; the height is the whole usable 1414, not the output's 1440.
	if sizes[0].Address != "0xa" || sizes[0].Width != 2270 || sizes[0].Height != 1414 {
		t.Errorf("the personal browser was sized %+v, want two thirds of the usable area", sizes[0])
	}
	// ChatGPT named only a height: the width it already has is sent, because
	// the compositor's resize verb wants both numbers and "leave it alone"
	// has to be spelled as the number it already is.
	if sizes[1].Address != "0xc" || sizes[1].Width != 1170 || sizes[1].Height != 707 {
		t.Errorf("ChatGPT was sized %+v, want half the usable height at its own width", sizes[1])
	}
}

// TestTheMorningSetupConvergesOnASecondRun is ADR 0026's set-not-toggle rule
// on the whole fixture: run it again with the windows already open and the
// same desktop comes out — no second copy launched, and every directive still
// a set, so nothing oscillates.
func TestTheMorningSetupConvergesOnASecondRun(t *testing.T) {
	comp := desktop.NewFakeCompositor(
		desktop.Window{Address: "0xa", Class: "chromium", Workspace: 1, Width: 2270, Height: 1414},
		desktop.Window{Address: "0xb", Class: "x", Workspace: 1, Width: 1170, Height: 1414},
		desktop.Window{Address: "0xc", Class: "chatgpt", Workspace: 1, Width: 1170, Height: 707},
		desktop.Window{Address: "0xd", Class: "chromium-work", Workspace: 2, Width: 5120, Height: 1414},
	)
	comp.Outputs = theMonitors()
	r, _ := newTestRunner(comp, []Definition{morningSetup()}, nil, nil)
	if _, err := r.Run(context.Background(), "morning setup"); err != nil {
		t.Fatal(err)
	}
	for _, a := range comp.Actions() {
		if a.Verb == "spawn" {
			t.Fatalf("a re-run launched %q; every window was already open", a.Program)
		}
		if a.Verb == "float" && a.Floating {
			t.Fatalf("a re-run floated a tiled window: %+v", a)
		}
	}
	// The sizes are re-sent, and re-sent exactly: an exact resize applied
	// twice lands in the same place, which is the whole reason the seam never
	// sends a delta.
	var resizes []desktop.FakeAction
	for _, a := range comp.Actions() {
		if a.Verb == "resize" {
			resizes = append(resizes, a)
		}
	}
	if len(resizes) != 2 || resizes[0].Width != 2270 || resizes[0].Height != 1414 {
		t.Errorf("second-run resizes = %+v", resizes)
	}
}

// TestARefusedDispatchIsNotAPlacedStep is issue #177's defect, pinned: the
// compositor declines part of a placement, and the run says so instead of
// counting the step. Before this, a refused resize left the move and the
// float succeeding, and the routine reported the step placed — which is how a
// resize verb that had never worked survived two years of green tests.
func TestARefusedDispatchIsNotAPlacedStep(t *testing.T) {
	comp := desktop.NewFakeCompositor(
		desktop.Window{Address: "0xa", Class: "chromium", Workspace: 1, Width: 3440, Height: 1414},
	)
	comp.Outputs = theMonitors()
	// The shape a real refusal takes: hyprctl exits 0 and the compositor
	// explains itself on stdout, so the seam judges the reply (runDispatch).
	comp.FailVerb = map[string]error{
		"resize": errors.New("hyprctl dispatch: unrecognized arguments"),
	}
	def := Definition{Name: "morning setup", Phrases: []string{"morning setup"},
		Steps: []Step{morningSetup().Steps[0]}}
	def.Steps[0].PlaceNext = placement.PlaceNextNone // one step, so nothing follows it
	r, _ := newTestRunner(comp, []Definition{def}, nil, nil)
	summary, err := r.Run(context.Background(), "morning setup")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "could not be sized") {
		t.Errorf("summary = %q, want it to name the resize as what failed", summary)
	}
	if strings.Contains(summary, "placed") && !strings.Contains(summary, "nothing could be placed") {
		t.Errorf("summary = %q, want the step NOT reported as placed", summary)
	}
}

// TestAnUnpluggedMonitorIsNamedAndTheRunContinues is the #180 contract this
// vocabulary already has to honour: a step naming a screen that is not there
// fails with THAT reason, and the rest of the routine still runs.
func TestAnUnpluggedMonitorIsNamedAndTheRunContinues(t *testing.T) {
	comp := desktop.NewFakeCompositor(
		desktop.Window{Address: "0xa", Class: "chromium", Workspace: 1, Width: 3440, Height: 1414},
		desktop.Window{Address: "0xd", Class: "chromium-work", Workspace: 2, Width: 5120, Height: 1414},
	)
	// The bottom screen was unplugged since the routine was written.
	comp.Outputs = theMonitors()[:1]
	def := morningSetup()
	def.Steps = []Step{def.Steps[0], def.Steps[3]}
	def.Steps[0].PlaceNext = placement.PlaceNextNone
	r, _ := newTestRunner(comp, []Definition{def}, nil, nil)
	summary, err := r.Run(context.Background(), "morning setup")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, `no monitor is called "DP-2" right now`) {
		t.Errorf("summary = %q, want it to name the screen that is not there", summary)
	}
	if !strings.Contains(summary, "HDMI-A-1") {
		t.Errorf("summary = %q, want it to say which screens are plugged in", summary)
	}
	if !strings.Contains(summary, "one app placed") {
		t.Errorf("summary = %q, want the other step still placed", summary)
	}
}
