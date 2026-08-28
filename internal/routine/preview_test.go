package routine

import (
	"errors"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/placement"
)

// The routine these tests preview is the one on the machine this feature was
// written for: a browser at two thirds of the top screen with the next window
// to its right, two web apps stacked in the third that is left, and a second
// browser alone on the bottom screen. Nothing here runs it.

func previewMonitors() []placement.Monitor {
	return []placement.Monitor{
		{Name: "HDMI-A-1", X: 840, Y: 0, Width: 3440, Height: 1440,
			Scale: 1, Reserved: [4]int{0, 26, 0, 0}, Focused: true, ActiveWorkspace: 1},
		{Name: "DP-2", X: 0, Y: 1440, Width: 5120, Height: 1440,
			Scale: 1, Reserved: [4]int{0, 26, 0, 0}, ActiveWorkspace: 2},
	}
}

func morningRoutine() Definition {
	return Definition{
		Name:    "morning layout",
		Phrases: []string{"lay out my morning"},
		Steps: []Step{
			{App: "chromium", Identity: "personal-browser", Launch: LaunchAlways,
				Placement: placement.Placement{Workspace: 1, Monitor: "HDMI-A-1",
					Mode: placement.ModeTiled, Width: placement.Percent(66),
					PlaceNext: placement.PlaceNextRight}},
			{DesktopEntry: "X", Launch: LaunchAlways,
				Placement: placement.Placement{Workspace: 1, Mode: placement.ModeTiled,
					PlaceNext: placement.PlaceNextBelow}},
			{DesktopEntry: "ChatGPT", Launch: LaunchAlways,
				Placement: placement.Placement{Workspace: 1, Mode: placement.ModeTiled,
					Height: placement.Percent(50)}},
			{App: "chromium", Identity: "work-browser", Launch: LaunchAlways,
				Placement: placement.Placement{Workspace: 2, Monitor: "DP-2",
					Mode: placement.ModeTiled}},
		},
	}
}

// nicknameResolver is the resolver every consumer builds: the vocabulary's
// own, with a nickname table behind it.
func nicknameResolver(names map[string]string) placement.Resolver {
	return placement.Resolver{Nicknames: func(name string) (string, bool) {
		connector, ok := names[strings.ToLower(name)]
		return connector, ok
	}}
}

// workspaceIn finds one workspace's preview.
func workspaceIn(t *testing.T, p Preview, workspace int) PreviewWorkspace {
	t.Helper()
	for _, ws := range p.Workspaces {
		if ws.Workspace == workspace {
			return ws
		}
	}
	t.Fatalf("the preview has no workspace %d", workspace)
	return PreviewWorkspace{}
}

// TestThePreviewDrawsOneArrangementPerWorkspace: a routine spanning two
// screens is two pictures, each against its own screen, because that is what
// the user is going to see.
func TestThePreviewDrawsOneArrangementPerWorkspace(t *testing.T) {
	preview := Describe(morningRoutine(), previewMonitors(), nil, nicknameResolver(nil), nil)
	if len(preview.Workspaces) != 2 {
		t.Fatalf("the preview has %d workspaces; the routine names two", len(preview.Workspaces))
	}
	top := workspaceIn(t, preview, 1)
	if !top.Drawable {
		t.Fatalf("workspace 1 does not draw: %s %v", top.Unavailable, top.Problems)
	}
	if top.Monitor != "HDMI-A-1" {
		t.Errorf("workspace 1 is drawn against %q; its first step names HDMI-A-1", top.Monitor)
	}
	if len(top.Panels) != 3 {
		t.Errorf("workspace 1 has %d rectangles; three steps open on it", len(top.Panels))
	}
	bottom := workspaceIn(t, preview, 2)
	if bottom.Monitor != "DP-2" || len(bottom.Panels) != 1 {
		t.Errorf("workspace 2 is %q with %d rectangles; one browser on DP-2",
			bottom.Monitor, len(bottom.Panels))
	}
	if !strings.Contains(top.Heading, "Workspace 1") || !strings.Contains(top.Heading, "3440") {
		t.Errorf("the heading does not name the workspace and its screen: %q", top.Heading)
	}
}

// TestEveryStepIsAlsoASentence: the accessibility criterion. The arrangement
// has to be conveyed in text as well as by the picture, in step order, one
// sentence each.
func TestEveryStepIsAlsoASentence(t *testing.T) {
	preview := Describe(morningRoutine(), previewMonitors(), nil, nicknameResolver(nil), nil)
	if len(preview.Steps) != 4 {
		t.Fatalf("the preview describes %d steps; the routine has four", len(preview.Steps))
	}
	if preview.Steps[0].Launches != "chromium" || preview.Steps[1].Launches != "X" {
		t.Errorf("the rows do not name what each step launches: %+v", preview.Steps)
	}
	if !strings.Contains(preview.Steps[0].Summary, "66% of the width") {
		t.Errorf("the first step's sentence omits its share: %s", preview.Steps[0].Summary)
	}
	if !strings.Contains(preview.Steps[1].Summary, "X opens") {
		t.Errorf("a desktop-entry step is not named by its entry: %s", preview.Steps[1].Summary)
	}
	// And the same sentences travel with the workspace they belong to, so the
	// text beside a drawing is the text FOR that drawing.
	top := workspaceIn(t, preview, 1)
	if len(top.Summaries) != 3 {
		t.Errorf("workspace 1 carries %d sentences for three steps", len(top.Summaries))
	}
}

// TestReorderingTheStepsReordersTheDrawing: insertion order is the layout, so
// the editor has to redraw when a step moves. Hiding that would make the
// preview a lie about the one thing routines get wrong most often.
func TestReorderingTheStepsReordersTheDrawing(t *testing.T) {
	before := Describe(morningRoutine(), previewMonitors(), nil, nicknameResolver(nil), nil)
	def := morningRoutine()
	def.Steps[1], def.Steps[2] = def.Steps[2], def.Steps[1]
	after := Describe(def, previewMonitors(), nil, nicknameResolver(nil), nil)
	same := true
	for i, panel := range workspaceIn(t, before, 1).Panels {
		if workspaceIn(t, after, 1).Panels[i].Label != panel.Label {
			same = false
		}
	}
	if same {
		t.Error("swapping two steps left the drawing identical; step order decides the layout")
	}
}

// TestAScreenInABagDegradesToWords: the graceful-degradation criterion. A
// routine written for a monitor that is not plugged in right now must still
// open, still be editable, and still say what it would do.
func TestAScreenInABagDegradesToWords(t *testing.T) {
	only := previewMonitors()[:1] // the bottom screen is unplugged
	preview := Describe(morningRoutine(), only, nil, nicknameResolver(nil), nil)
	bottom := workspaceIn(t, preview, 2)
	if bottom.Drawable {
		t.Fatal("a workspace targeting an unplugged screen was drawn anyway")
	}
	if !strings.Contains(bottom.Unavailable, "DP-2") {
		t.Errorf("the reason does not name the missing screen: %q", bottom.Unavailable)
	}
	if len(bottom.Summaries) != 1 {
		t.Error("the step's sentence went with the drawing; the words are what is left")
	}
}

// TestNoCompositorAtAllStillDescribesTheRoutine: the window manager being
// unreachable is a reason to omit the pictures, never the answer.
func TestNoCompositorAtAllStillDescribesTheRoutine(t *testing.T) {
	preview := Describe(morningRoutine(), nil, errors.New("hyprctl is not answering"),
		nicknameResolver(nil), nil)
	if len(preview.Steps) != 4 {
		t.Fatalf("the sentences went missing with the screens: %+v", preview.Steps)
	}
	for _, ws := range preview.Workspaces {
		if ws.Drawable {
			t.Errorf("workspace %d drew without a compositor", ws.Workspace)
		}
		if !strings.Contains(ws.Unavailable, "hyprctl is not answering") {
			t.Errorf("the reason is not the compositor's own: %q", ws.Unavailable)
		}
	}
}

// TestANicknamedScreenPreviewsAgainstTheScreenItMeans: a routine saying
// `monitor = "top"` must be drawn on the output that name points at, or the
// preview and the run would be describing different desktops (#180).
func TestANicknamedScreenPreviewsAgainstTheScreenItMeans(t *testing.T) {
	def := morningRoutine()
	def.Steps[0].Monitor = "top"
	preview := Describe(def, previewMonitors(), nil,
		nicknameResolver(map[string]string{"top": "HDMI-A-1"}), nil)
	top := workspaceIn(t, preview, 1)
	if top.Monitor != "HDMI-A-1" {
		t.Errorf("the nickname resolved to %q; \"top\" is HDMI-A-1", top.Monitor)
	}
}

// TestARefusedValueTakesTheDrawingAway: the criterion the preview exists to
// honour. A value the loader turned down must not be drawn around — the
// message shows where the problem is and there is no picture until it is
// fixed.
func TestARefusedValueTakesTheDrawingAway(t *testing.T) {
	preview := Describe(morningRoutine(), previewMonitors(), nil, nicknameResolver(nil),
		[]PreviewBlock{{Step: 1, Field: "steps[1].mode",
			Message: `"grouped" is not a placement mode: Hyprland groups windows with a toggle`}})
	top := workspaceIn(t, preview, 1)
	if top.Drawable {
		t.Fatal("a workspace with a refused mode was drawn")
	}
	if len(top.Problems) != 1 || top.Problems[0].Field != "steps[1].mode" {
		t.Errorf("the refusal did not travel to the drawing keyed to its field: %+v", top.Problems)
	}
	if !strings.Contains(top.Problems[0].Message, "grouped") {
		t.Errorf("the daemon's own words were not carried through: %q", top.Problems[0].Message)
	}
	// The OTHER workspace is untouched: one bad step must not blank a routine.
	if !workspaceIn(t, preview, 2).Drawable {
		t.Error("a problem on workspace 1 took workspace 2's drawing away too")
	}
}

// TestAnImpossibleShareIsReportedOnTheStepThatCausedIt: two thirds beside a
// half, keyed to the second window's width so the editor can pin it to the
// control the user has to change.
func TestAnImpossibleShareIsReportedOnTheStepThatCausedIt(t *testing.T) {
	def := morningRoutine()
	def.Steps[1].Width = placement.Percent(50)
	preview := Describe(def, previewMonitors(), nil, nicknameResolver(nil), nil)
	top := workspaceIn(t, preview, 1)
	if top.Drawable {
		t.Fatal("66% beside 50% was drawn")
	}
	if len(top.Problems) == 0 || top.Problems[0].Step != 1 ||
		top.Problems[0].Field != placement.FieldWidth {
		t.Errorf("the refusal is not keyed to the second step's width: %+v", top.Problems)
	}
}

// TestAHalfWrittenStepStillPreviews: a form's normal state is incomplete, and
// a preview that only appeared once every field was filled in would be
// missing exactly while the routine is being built.
func TestAHalfWrittenStepStillPreviews(t *testing.T) {
	preview := Describe(Definition{Name: "new one", Steps: []Step{{Placement: placement.Placement{
		Workspace: 1}}}}, previewMonitors(), nil, nicknameResolver(nil), nil)
	if len(preview.Steps) != 1 || !strings.Contains(preview.Steps[0].Summary, "Step 1 opens") {
		t.Errorf("an empty step has no sentence: %+v", preview.Steps)
	}
	if !workspaceIn(t, preview, 1).Drawable {
		t.Error("an empty step took the drawing away; it is a window like any other")
	}
}
