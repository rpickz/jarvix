package routine

import (
	"context"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/placement"
)

// These tests are #180's contract where it matters most: in a routine, which
// is the thing that used to break silently when a cable moved.
//
// The fixture is the user's own arrangement (fixture_test.go's theMonitors),
// written the way they talk about it — `monitor = "top"` and `monitor =
// "bottom"` — with a nickname table standing in for the store.

// nicknamed builds a routine of two steps, one per screen, named the way the
// user names them.
func nicknamed() Definition {
	return Definition{
		Name:    "screens",
		Phrases: []string{"screens"},
		Steps: []Step{
			{App: "chromium", Match: "chromium", Placement: placement.Placement{
				Workspace: 1, Monitor: "top", Mode: placement.ModeTiled}},
			{App: "chromium-work", Match: "chromium-work", Placement: placement.Placement{
				Workspace: 2, Monitor: "bottom", Mode: placement.ModeTiled}},
		},
	}
}

// runNicknamed drives the two-step routine against an inventory, with the
// given nickname table, and returns the compositor and the spoken summary.
func runNicknamed(t *testing.T, outputs []placement.Monitor,
	names func(string) (string, bool)) (*desktop.FakeCompositor, string) {
	t.Helper()
	comp := desktop.NewFakeCompositor()
	comp.Outputs = outputs
	appearing := []desktop.Window{
		{Address: "0xa", Class: "chromium", Workspace: 1, Width: 3440, Height: 1414},
		{Address: "0xb", Class: "chromium-work", Workspace: 2, Width: 5120, Height: 1414},
	}
	r, _ := newTestRunner(comp, []Definition{nicknamed()}, nil, func(poll int) {
		if poll >= 1 && poll <= len(appearing) {
			comp.SetWindows(appearing[:poll]...)
		}
	})
	r.monitorNames = names
	summary, err := r.Run(context.Background(), "screens")
	if err != nil {
		t.Fatal(err)
	}
	return comp, summary
}

// theUsersNames is what "top" and "bottom" mean on their desk.
func theUsersNames(name string) (string, bool) {
	connector, ok := map[string]string{"top": "HDMI-A-1", "bottom": "DP-2"}[name]
	return connector, ok
}

// TestARoutineNamesItsScreensTheWayTheUserDoes: the acceptance case. The
// workspaces land on the screens the nicknames point at, which is proved by
// the dispatches the compositor received rather than by anything internal.
func TestARoutineNamesItsScreensTheWayTheUserDoes(t *testing.T) {
	comp, summary := runNicknamed(t, theMonitors(), theUsersNames)
	if summary != "Screens: all two apps placed." {
		t.Errorf("summary = %q", summary)
	}
	var moves []string
	for _, a := range comp.Actions() {
		if a.Verb == "workspace_monitor" {
			moves = append(moves, a.Monitor)
		}
	}
	if len(moves) != 2 || moves[0] != "HDMI-A-1" || moves[1] != "DP-2" {
		t.Fatalf("workspaces went to %v; want HDMI-A-1 then DP-2", moves)
	}
}

// TestACableMoveNeedsNoEditToTheRoutine is the whole promise: the definition
// is byte-for-byte the same one, and swapping what the names mean is what
// sends the workspaces to the other screens.
func TestACableMoveNeedsNoEditToTheRoutine(t *testing.T) {
	swapped := func(name string) (string, bool) {
		connector, ok := map[string]string{"top": "DP-2", "bottom": "HDMI-A-1"}[name]
		return connector, ok
	}
	comp, _ := runNicknamed(t, theMonitors(), swapped)
	var moves []string
	for _, a := range comp.Actions() {
		if a.Verb == "workspace_monitor" {
			moves = append(moves, a.Monitor)
		}
	}
	if len(moves) != 2 || moves[0] != "DP-2" || moves[1] != "HDMI-A-1" {
		t.Fatalf("workspaces went to %v; the routine was not re-resolved", moves)
	}
}

// TestAnUnpluggedScreenFailsItsOwnStepAndTheRunGoesOn is the disappearance
// contract: the step naming the absent screen says why, in those words, and
// every other step still lands.
func TestAnUnpluggedScreenFailsItsOwnStepAndTheRunGoesOn(t *testing.T) {
	onlyTop := theMonitors()[:1]
	comp, summary := runNicknamed(t, onlyTop, theUsersNames)

	for _, want := range []string{`no monitor is called "bottom" right now`,
		"it means DP-2, which is not plugged in"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q is missing %q", summary, want)
		}
	}
	// The other step ran to completion, and both apps still launched — an
	// application the user asked for is not withheld because a screen is in a
	// bag. What the absent screen costs is exactly its own placement: one
	// workspace moved, and one window put into a mode rather than two.
	var moved, spawned, placed int
	for _, a := range comp.Actions() {
		switch a.Verb {
		case "workspace_monitor":
			moved++
		case "spawn":
			spawned++
		case "float":
			placed++
		}
	}
	if moved != 1 || spawned != 2 || placed != 1 {
		t.Errorf("after the disappearance: %d workspace moves, %d spawns, %d placements",
			moved, spawned, placed)
	}
}

// TestWithoutNicknamesTheRunnerIsExactlyAsItWas is the pinned baseline: a
// runner built with no nickname table resolves connector names and "current"
// only, and a name nothing holds is the same honest refusal as before #180.
func TestWithoutNicknamesTheRunnerIsExactlyAsItWas(t *testing.T) {
	_, summary := runNicknamed(t, theMonitors(), nil)
	for _, want := range []string{`no monitor is called "top" right now`,
		`no monitor is called "bottom" right now`} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q is missing %q", summary, want)
		}
	}
	// And the forms that always worked still do, through the same runner.
	comp := desktop.NewFakeCompositor()
	comp.Outputs = theMonitors()
	def := nicknamed()
	def.Steps[0].Monitor = "HDMI-A-1"
	def.Steps[1].Monitor = placement.MonitorCurrent
	r, _ := newTestRunner(comp, []Definition{def}, nil, func(poll int) {
		windows := []desktop.Window{
			{Address: "0xa", Class: "chromium", Workspace: 1},
			{Address: "0xb", Class: "chromium-work", Workspace: 2},
		}
		if poll >= 1 && poll <= len(windows) {
			comp.SetWindows(windows[:poll]...)
		}
	})
	summary, err := r.Run(context.Background(), "screens")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(summary, "no monitor is called") {
		t.Errorf("a connector or \"current\" stopped resolving: %q", summary)
	}
}
