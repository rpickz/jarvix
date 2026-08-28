package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/placement"
	"github.com/rpickz/jarvix/internal/routine"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// The routine editor's daemon half (#181, ADR 0059): the diagram arrives in
// the same config.validate_entry reply the field problems do, computed
// against the screens the compositor reports, and an arrangement that cannot
// happen comes back as a refusal keyed to the field that caused it rather
// than as a picture.
//
// Everything here is asserted on the socket, because that is where the
// contract is: the window renders what this reply contains and decides
// nothing (ADR 0013).

// previewTOML is a routine spanning both of the user's screens: a browser at
// two thirds of the top one with the next window to its right, two terminals
// stacked in the third that is left, and a second browser alone on the bottom
// one — the shape of the real "morning layout" this feature was written for.
//
// Every step names a bare program rather than a desktop entry, on purpose: an
// entry id is resolved out of the machine's own applications index and a test
// that named one would pass or fail according to what happens to be installed
// on the machine running it.
const previewTOML = `# hand-written
[context]
window = false
selection = false
clipboard = false

[[routines]]
name = "morning layout"
phrases = ["lay out my morning"]

  [[routines.steps]]
  app = "chromium"
  identity = "personal-browser"
  launch = "always"
  workspace = 1
  monitor = "HDMI-A-1"
  mode = "tiled"
  width = "66%"
  place_next = "right"

  [[routines.steps]]
  app = "kitty"
  identity = "notes"
  launch = "always"
  workspace = 1
  mode = "tiled"

  [[routines.steps]]
  app = "alacritty"
  identity = "logs"
  launch = "always"
  workspace = 1
  mode = "tiled"
  height = "50%"

  [[routines.steps]]
  app = "chromium"
  identity = "work-browser"
  launch = "always"
  workspace = 2
  monitor = "DP-2"
  mode = "tiled"
`

// startPreviewDaemon boots a daemon on previewTOML with the user's own two
// screens behind a fake compositor.
func startPreviewDaemon(t *testing.T) *ipc.Client {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock")}
	if err := os.WriteFile(paths.ConfigFile(), []byte(previewTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	comp := desktop.NewFakeCompositor()
	comp.Outputs = []placement.Monitor{
		{Name: "HDMI-A-1", Width: 3440, Height: 1440, Scale: 1,
			Reserved: [4]int{0, 26, 0, 0}, Focused: true, ActiveWorkspace: 1},
		{Name: "DP-2", Y: 1440, Width: 5120, Height: 1440, Scale: 1,
			Reserved: [4]int{0, 26, 0, 0}, ActiveWorkspace: 2},
	}
	d, err := New(cfg, paths, nil, Deps{
		Provider:    &ai.Fake{Response: "ok"},
		Transcriber: &stt.Fake{Text: "hello"},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
		Compositor:  comp,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	return dialDaemon(t, paths.Socket)
}

// previewDraft is the routine as the form would ship it, with one field
// changed per call so a test can say "and now the width is this".
func previewDraft(edit func(steps []map[string]any)) map[string]any {
	steps := []map[string]any{
		{"app": "chromium", "identity": "personal-browser", "launch": "always",
			"workspace": 1, "monitor": "HDMI-A-1", "mode": "tiled",
			"width": "66%", "place_next": "right"},
		{"app": "kitty", "identity": "notes", "launch": "always",
			"workspace": 1, "mode": "tiled"},
		{"app": "alacritty", "identity": "logs", "launch": "always",
			"workspace": 1, "mode": "tiled", "height": "50%"},
		{"app": "chromium", "identity": "work-browser", "launch": "always",
			"workspace": 2, "monitor": "DP-2", "mode": "tiled"},
	}
	if edit != nil {
		edit(steps)
	}
	return map[string]any{"name": "morning layout",
		"phrases": []string{"lay out my morning"}, "steps": steps}
}

// validatePreview validates one draft and returns the preview half of the
// reply.
func validatePreview(t *testing.T, client *ipc.Client, draft map[string]any) map[string]any {
	t.Helper()
	out := entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "routines", "name": "morning layout", "entry": draft})
	preview, ok := out["preview"].(map[string]any)
	if !ok {
		t.Fatalf("the validate reply carries no preview: %v", out)
	}
	return preview
}

// previewWorkspace pulls one workspace out of a preview.
func previewWorkspace(t *testing.T, preview map[string]any, workspace float64) map[string]any {
	t.Helper()
	rows, _ := preview["workspaces"].([]any)
	for _, row := range rows {
		ws, _ := row.(map[string]any)
		if ws["workspace"] == workspace {
			return ws
		}
	}
	t.Fatalf("the preview has no workspace %v: %v", workspace, preview)
	return nil
}

// previewPanels returns one workspace's rectangles.
func previewPanels(ws map[string]any) []map[string]any {
	raw, _ := ws["panels"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, p := range raw {
		if panel, ok := p.(map[string]any); ok {
			out = append(out, panel)
		}
	}
	return out
}

// TestTheValidateReplyCarriesTheArrangement: the acceptance path. One drawing
// per workspace, drawn against the screen each one targets, with a
// proportional rectangle per window labelled by what it launches and the
// share it gets — and a sentence for every step beside it.
func TestTheValidateReplyCarriesTheArrangement(t *testing.T) {
	client := startPreviewDaemon(t)
	preview := validatePreview(t, client, previewDraft(nil))

	steps, _ := preview["steps"].([]any)
	if len(steps) != 4 {
		t.Fatalf("the preview describes %d steps; the routine has four", len(steps))
	}
	first, _ := steps[0].(map[string]any)
	summary, _ := first["summary"].(string)
	for _, want := range []string{"chromium opens", "workspace 1", "HDMI-A-1",
		"66% of the width", "to its right"} {
		if !strings.Contains(summary, want) {
			t.Errorf("the first step's sentence omits %q: %s", want, summary)
		}
	}

	top := previewWorkspace(t, preview, 1)
	if top["drawable"] != true {
		t.Fatalf("workspace 1 is not drawable: %v", top)
	}
	if top["monitor"] != "HDMI-A-1" {
		t.Errorf("workspace 1 is drawn against %v; its first step names HDMI-A-1", top["monitor"])
	}
	aspect, _ := top["aspect"].(float64)
	if aspect < 2.38 || aspect > 2.39 {
		t.Errorf("the drawing's aspect is %v; a 3440 by 1440 screen is about 2.389", aspect)
	}
	panels := previewPanels(top)
	if len(panels) != 3 {
		t.Fatalf("workspace 1 has %d rectangles; three steps open on it", len(panels))
	}
	if panels[0]["label"] != "chromium" || panels[0]["share"] != "66% × 100%" {
		t.Errorf("the browser's rectangle = %v, want it labelled and at two thirds", panels[0])
	}
	// The two terminals share the third that is left, stacked, because the
	// last of them asked for half the height.
	if panels[1]["label"] != "kitty" || panels[1]["share"] != "34% × 50%" {
		t.Errorf("the first terminal's rectangle = %v, want half of the remaining third", panels[1])
	}
	if panels[2]["label"] != "alacritty" || panels[2]["share"] != "34% × 50%" {
		t.Errorf("the second terminal's rectangle = %v, want the other half", panels[2])
	}
	// The bar's 26 pixels are an inset rather than lost, which is what makes
	// "two thirds of the screen" two thirds of the part a window can use.
	usable, _ := top["usable"].(map[string]any)
	if height, _ := usable["height"].(float64); height > 0.9985 {
		t.Errorf("the usable area is %v of the glass tall; a bar reserved 26 of 1440", height)
	}

	bottom := previewWorkspace(t, preview, 2)
	if bottom["drawable"] != true || bottom["monitor"] != "DP-2" {
		t.Errorf("workspace 2 = %v, want one browser drawn on DP-2", bottom)
	}
}

// TestReorderingTheStepsRedrawsThePreview: insertion order is the layout, so
// the reply has to change when two steps swap. If it did not, the editor's
// picture would be hiding the property that makes routines hard.
func TestReorderingTheStepsRedrawsThePreview(t *testing.T) {
	client := startPreviewDaemon(t)
	before := previewPanels(previewWorkspace(t,
		validatePreview(t, client, previewDraft(nil)), 1))
	after := previewPanels(previewWorkspace(t,
		validatePreview(t, client, previewDraft(func(steps []map[string]any) {
			steps[1], steps[2] = steps[2], steps[1]
		})), 1))
	if before[1]["label"] == after[1]["label"] {
		t.Errorf("swapping two steps left the drawing's second rectangle as %v; "+
			"step order decides the layout", after[1]["label"])
	}
}

// TestAnImpossibleShareIsRefusedWhereTheProblemIs: the criterion the preview
// exists for. Two thirds beside a half is more screen than there is, so there
// is no picture and the message names both windows, on the field that has to
// change.
func TestAnImpossibleShareIsRefusedWhereTheProblemIs(t *testing.T) {
	client := startPreviewDaemon(t)
	preview := validatePreview(t, client, previewDraft(func(steps []map[string]any) {
		steps[1]["width"] = "50%"
	}))
	top := previewWorkspace(t, preview, 1)
	if top["drawable"] == true {
		t.Fatal("66% beside 50% was drawn")
	}
	problems, _ := top["problems"].([]any)
	if len(problems) == 0 {
		t.Fatalf("the workspace is not drawn and says nothing about why: %v", top)
	}
	problem, _ := problems[0].(map[string]any)
	if problem["step"] != float64(1) || problem["field"] != placement.FieldWidth {
		t.Errorf("the refusal = %v, want it on the second step's width", problem)
	}
	message, _ := problem["message"].(string)
	if !strings.Contains(message, "chromium") || !strings.Contains(message, "kitty") {
		t.Errorf("the refusal does not name both windows: %s", message)
	}
	// The other workspace is untouched: one bad share must not blank a routine.
	if previewWorkspace(t, preview, 2)["drawable"] != true {
		t.Error("a problem on workspace 1 took workspace 2's drawing away too")
	}
}

// TestAModeTheVocabularyDeclinesIsRefusedRatherThanDrawn: `grouped` never
// reaches a Placement — the loader reads a refused value as "not said" — so a
// preview built from the converted step would draw a tiled window for it. The
// blocks the validation hands back are what stop that.
func TestAModeTheVocabularyDeclinesIsRefusedRatherThanDrawn(t *testing.T) {
	client := startPreviewDaemon(t)
	out := entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "routines", "name": "morning layout",
		"entry": previewDraft(func(steps []map[string]any) { steps[1]["mode"] = "grouped" })})
	message, ok := problemOn(entryProblemList(t, out), "steps[1].mode")
	if !ok || !strings.Contains(message, "grouped") {
		t.Fatalf("the refused mode did not land on its field: %v", out["problems"])
	}
	preview, _ := out["preview"].(map[string]any)
	top := previewWorkspace(t, preview, 1)
	if top["drawable"] == true {
		t.Error("a mode the vocabulary declines was drawn as though it were tiled")
	}
	problems, _ := top["problems"].([]any)
	if len(problems) == 0 {
		t.Fatal("the refusal did not travel to the drawing")
	}
	if first, _ := problems[0].(map[string]any); first["field"] != "steps[1].mode" {
		t.Errorf("the drawing's refusal = %v, want it keyed to the mode field", problems[0])
	}
}

// TestAScreenInABagLeavesTheWordsBehind: the graceful-degradation criterion.
// A routine written for a monitor that is not plugged in must still open,
// still be editable, and still say what it would do.
func TestAScreenInABagLeavesTheWordsBehind(t *testing.T) {
	client := startPreviewDaemon(t)
	preview := validatePreview(t, client, previewDraft(func(steps []map[string]any) {
		steps[3]["monitor"] = "DP-9"
	}))
	bottom := previewWorkspace(t, preview, 2)
	if bottom["drawable"] == true {
		t.Fatal("a workspace targeting an unplugged screen was drawn anyway")
	}
	unavailable, _ := bottom["unavailable"].(string)
	if !strings.Contains(unavailable, "DP-9") {
		t.Errorf("the reason does not name the missing screen: %q", unavailable)
	}
	summaries, _ := bottom["summaries"].([]any)
	if len(summaries) != 1 {
		t.Errorf("the step's sentence went with the drawing: %v", bottom)
	}
}

// TestACapturedRoutineOpensAndPreviewsLikeAnyOther: a routine captured from
// the live desktop (ADR 0026) is a [[routines]] entry like any other, written
// in the same placement vocabulary — so it edits through the same verbs and
// draws through the same preview, including the steps the capture could not
// derive a launch command for and left marked for a human to finish.
func TestACapturedRoutineOpensAndPreviewsLikeAnyOther(t *testing.T) {
	client := startPreviewDaemon(t)
	preview := validatePreview(t, client, previewDraft(func(steps []map[string]any) {
		steps[1]["app"] = routine.PlaceholderApp
		delete(steps[1], "identity")
	}))
	top := previewWorkspace(t, preview, 1)
	if top["drawable"] != true {
		t.Fatalf("a capture's unfinished step took the drawing away: %v", top)
	}
	panels := previewPanels(top)
	if panels[1]["label"] != routine.PlaceholderApp {
		t.Errorf("the unfinished step is drawn as %v; it should be named as what it is, "+
			"so the picture shows the gap the capture left", panels[1]["label"])
	}
}

// TestOnlyTheRoutinesFamilyGetsAPreview: the registry hook is a declared
// property of one family, not a branch in the pipeline. A script draft comes
// back exactly as it did before this existed.
func TestOnlyTheRoutinesFamilyGetsAPreview(t *testing.T) {
	client := startPreviewDaemon(t)
	out := entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "intents.custom",
		"entry":  map[string]any{"name": "lock the screen", "run": "hyprlock"}})
	if _, ok := out["preview"]; ok {
		t.Errorf("a family that declares no preview got one: %v", out)
	}
}

// TestThePlacementVocabularyIsServedWhole: the editor's pickers are the
// daemon's lists, so a mode added to the vocabulary appears in the form
// without anyone remembering to add it — and a state the vocabulary declines
// is served WITH its reason rather than quietly missing.
func TestThePlacementVocabularyIsServedWhole(t *testing.T) {
	client := startPreviewDaemon(t)
	var reply struct {
		Modes []struct {
			Value string `json:"value"`
			Label string `json:"label"`
		} `json:"modes"`
		PlaceNext []struct {
			Value string `json:"value"`
		} `json:"place_next"`
		Focus []struct {
			Value string `json:"value"`
		} `json:"focus"`
		Launch []struct {
			Value string `json:"value"`
		} `json:"launch"`
		Unsupported []struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"unsupported"`
		Workspace struct {
			Min int `json:"min"`
			Max int `json:"max"`
		} `json:"workspace"`
	}
	if err := client.Call("placement.vocabulary", nil, &reply); err != nil {
		t.Fatal(err)
	}
	// Every mode the vocabulary declares, plus the "not said" option, each
	// carrying words to choose by rather than a bare name.
	if len(reply.Modes) != len(placement.ModeNames())+1 {
		t.Fatalf("modes = %+v, want the vocabulary's %v plus the unset option",
			reply.Modes, placement.ModeNames())
	}
	if reply.Modes[0].Value != "" || reply.Modes[0].Label == "" {
		t.Errorf("the first mode option = %+v, want the unset one with its own words",
			reply.Modes[0])
	}
	for _, spec := range placement.Modes() {
		found := false
		for _, option := range reply.Modes {
			if option.Value == string(spec.Name) && strings.Contains(option.Label, spec.Summary) {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is not offered with its own summary", spec.Name)
		}
	}
	if len(reply.PlaceNext) != len(placement.PlaceNextValues())+1 {
		t.Errorf("place_next = %+v, want %v plus the unset option",
			reply.PlaceNext, placement.PlaceNextValues())
	}
	if len(reply.Focus) != 2 || len(reply.Launch) != 2 {
		t.Errorf("focus = %+v and launch = %+v, want two options each", reply.Focus, reply.Launch)
	}
	// The default launch policy is served as absence, so saving a step that
	// never asked for one does not add a line to the file.
	if reply.Launch[0].Value != "" || reply.Launch[1].Value != string(routine.LaunchAlways) {
		t.Errorf("launch = %+v, want the default as \"\" and the other by name", reply.Launch)
	}
	if len(reply.Unsupported) != len(placement.UnsupportedModes()) {
		t.Errorf("unsupported = %+v, want the vocabulary's declined states", reply.Unsupported)
	}
	for _, u := range reply.Unsupported {
		if strings.TrimSpace(u.Reason) == "" {
			t.Errorf("%q is declined without a reason", u.Name)
		}
	}
	if reply.Workspace.Min != placement.MinWorkspace || reply.Workspace.Max != placement.MaxWorkspace {
		t.Errorf("workspace bounds = %+v, want the vocabulary's", reply.Workspace)
	}
}
