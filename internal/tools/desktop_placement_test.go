package tools

import (
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/placement"
)

// The window tools are the vocabulary's second consumer (ADR 0056): "put this
// on the top monitor, two thirds" has to dispatch what the equivalent routine
// step would, be refused by the same rules, and stay inside the same
// no-addresses-in-the-answer discipline everything else in this file keeps.

// placementHarness is newHarness plus a monitor inventory and window
// geometry, both of which a placement needs and a plain move does not: a
// share resolves against a screen, and an axis the caller did not mention is
// filled in from the window's own current extent.
func placementHarness(t *testing.T) *harness {
	t.Helper()
	windows := testWindows()
	for i := range windows {
		windows[i].Width, windows[i].Height = 1720, 1414
	}
	h := newHarness(t, windows...)
	h.comp.Outputs = []placement.Monitor{
		{Name: "HDMI-A-1", X: 840, Y: 0, Width: 3440, Height: 1440, Scale: 1,
			Reserved: [4]int{0, 26, 0, 0}, Focused: true, ActiveWorkspace: 1},
		{Name: "DP-2", X: 0, Y: 1440, Width: 5120, Height: 1440, Scale: 1,
			Reserved: [4]int{0, 26, 0, 0}, ActiveWorkspace: 2},
	}
	return h
}

// TestMoveWindowSpeaksTheWholeVocabulary: a spoken "put the terminal on the
// bottom screen, tiled, two thirds across" reaches the compositor as the same
// dispatches a routine step would make, with the share resolved against the
// screen it was sent to — not the one the window was on.
func TestMoveWindowSpeaksTheWholeVocabulary(t *testing.T) {
	h := placementHarness(t)
	out := h.run(t, MoveWindowToolName, map[string]any{
		"window": "alacritty", "workspace": 2, "monitor": "DP-2",
		"mode": "tiled", "width": "66%",
	})
	if !strings.Contains(out, "Moved") || !strings.Contains(out, "DP-2") {
		t.Errorf("result = %q, want it to confirm the screen", out)
	}
	got := make([]string, 0, len(h.comp.Actions()))
	for _, a := range h.comp.Actions() {
		got = append(got, a.Verb)
	}
	// Workspace 2 already lives on DP-2 in this inventory, so no workspace is
	// moved between screens: the window is sent to the workspace, put into
	// its mode as a whole state, then given its share of the split.
	want := "move,fullscreen,float,focus,resize"
	if strings.Join(got, ",") != want {
		t.Fatalf("dispatches = %v, want %v", got, want)
	}
	var resize desktop.FakeAction
	for _, a := range h.comp.Actions() {
		if a.Verb == "resize" {
			resize = a
		}
	}
	// 66% of DP-2's usable 5120, not of the 3440-wide screen the window was
	// on: a share is of the screen it lands on.
	if resize.Width != 3379 {
		t.Errorf("resize = %+v, want two thirds of DP-2's usable width (3379)", resize)
	}
}

// TestMoveWindowMovesTheWorkspaceWhenItIsOnTheWrongScreen: naming both a
// workspace and a monitor is a statement about where that WORKSPACE lives,
// because the windows of one workspace belong together.
func TestMoveWindowMovesTheWorkspaceWhenItIsOnTheWrongScreen(t *testing.T) {
	h := placementHarness(t)
	h.run(t, MoveWindowToolName, map[string]any{
		"window": "alacritty", "workspace": 2, "monitor": "HDMI-A-1",
	})
	first, _ := h.comp.Actions()[0], 0
	if first.Verb != "workspace_monitor" || first.Workspace != 2 || first.Monitor != "HDMI-A-1" {
		t.Errorf("first dispatch = %+v, want workspace 2 moved to HDMI-A-1", first)
	}
}

// TestMoveWindowMovesOneWindowWhenNoWorkspaceIsNamed: "put this on the other
// screen" is about one window, and moves only that one.
func TestMoveWindowMovesOneWindowWhenNoWorkspaceIsNamed(t *testing.T) {
	h := placementHarness(t)
	h.run(t, MoveWindowToolName, map[string]any{"window": "alacritty", "monitor": "DP-2"})
	first := h.comp.Actions()[0]
	if first.Verb != "window_monitor" || first.Address != "0x4" || first.Monitor != "DP-2" {
		t.Errorf("first dispatch = %+v, want the window itself moved to DP-2", first)
	}
	for _, a := range h.comp.Actions() {
		if a.Verb == "workspace_monitor" {
			t.Errorf("a whole workspace was moved for a one-window request: %+v", a)
		}
	}
}

// TestMoveWindowRefusesBeforeItDispatches: a value the vocabulary will not
// honour comes back as a sentence, and nothing has moved. The refusal happens
// before the window is even resolved, so a bad call cannot half-place a window.
func TestMoveWindowRefusesBeforeItDispatches(t *testing.T) {
	tests := []struct {
		name, want string
		args       map[string]any
	}{
		{"a share bigger than the screen", "more than the whole screen",
			map[string]any{"window": "alacritty", "mode": "tiled", "width": "150%"}},
		{"a mode nobody has", "is not a placement mode",
			map[string]any{"window": "alacritty", "mode": "sideways"}},
		{"a mode the compositor cannot deliver as a set", "which only toggles",
			map[string]any{"window": "alacritty", "mode": "grouped"}},
		{"a share on a mode that always fills the screen", "means nothing in fullscreen mode",
			map[string]any{"window": "alacritty", "mode": "fullscreen", "width": "50%"}},
		{"a size that is not a size", "write a percentage of the screen",
			map[string]any{"window": "alacritty", "mode": "floating", "width": "two thirds"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := placementHarness(t)
			out := h.run(t, MoveWindowToolName, tt.args)
			if !strings.Contains(out, tt.want) {
				t.Errorf("result = %q, want it to contain %q", out, tt.want)
			}
			if _, acted := h.comp.LastAction(); acted {
				t.Fatal("something was dispatched despite the refusal")
			}
		})
	}
}

// TestMoveWindowNamesAScreenThatIsNotThere: the #180 contract on the tool
// side — the reason, and what is plugged in, rather than "it did not work".
func TestMoveWindowNamesAScreenThatIsNotThere(t *testing.T) {
	h := placementHarness(t)
	out := h.run(t, MoveWindowToolName, map[string]any{"window": "alacritty", "monitor": "DP-9"})
	if !strings.Contains(out, `no monitor is called "DP-9"`) {
		t.Errorf("result = %q, want it to name the screen", out)
	}
	if !strings.Contains(out, "DP-2, HDMI-A-1") {
		t.Errorf("result = %q, want it to list the screens that are plugged in", out)
	}
	// And the activity feed hears the same reason, never the diagnostics.
	refusals := h.firedRefusals()
	if len(refusals) != 1 || !strings.Contains(refusals[0], "no monitor is called") {
		t.Errorf("refusals = %v", refusals)
	}
}

// TestMoveWindowConfirmationQuotesTheWholePlacement: the ADR 0014 discipline
// — the user approves what will happen, not the part of it that fitted in the
// old sentence.
func TestMoveWindowConfirmationQuotesTheWholePlacement(t *testing.T) {
	h := placementHarness(t)
	tool, ok := h.tool(t, MoveWindowToolName).(Confirmable)
	if !ok {
		t.Fatal("the move tool cannot describe its own confirmation")
	}
	input := []byte(`{"window":"alacritty","workspace":2,"monitor":"DP-2","mode":"tiled","width":"66%"}`)
	summary, question, confirm := tool.Confirmation(input)
	if !confirm {
		t.Fatal("the move tool declined to confirm a placement")
	}
	for _, want := range []string{"workspace 2", "DP-2", "tiled", "66%"} {
		if !strings.Contains(summary, want) || !strings.Contains(question, want) {
			t.Errorf("confirmation %q / %q omits %q", summary, question, want)
		}
	}
}
