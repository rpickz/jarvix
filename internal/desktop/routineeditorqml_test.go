package desktop

import (
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/placement"
)

// Text guards over the routine editor (#181, ADR 0059), on the same terms as
// the other *qml_test.go files: QML cannot be parsed by anything in this
// module, so a scan of what the file DOES is what a Go test can hold it to.
//
// The rule being guarded is the one the whole ticket turns on. A preview
// diagram is a CLAIM about what a routine will do, and the user will believe
// it — more readily than they will believe the fields above it, because a
// picture reads as a fact. So every number in it has to come from the same
// arithmetic the run uses (ADR 0013): the window renders rectangles it was
// handed and words it was handed, and computes no placement of its own. A
// second copy of that arithmetic in QML would be the copy that goes stale,
// and the day it did the picture would be the lie.

// previewComponent is the diagram's own file.
func previewComponent(t *testing.T) string {
	t.Helper()
	return stripQMLComments(readPlugin(t, "JarvixLayoutPreview.qml"))
}

// previewSection returns the routine form's preview section — the part of the
// window that decides where the drawings go.
func previewSection(t *testing.T) string {
	t.Helper()
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))
	start := strings.Index(qml, `text: "What this will look like"`)
	if start < 0 {
		t.Fatal("the routine form has no preview section")
	}
	end := strings.Index(qml[start:], "JarvixLayoutPreview {")
	if end < 0 {
		t.Fatal("the preview section never renders the diagram component")
	}
	return qml[start : start+end]
}

// TestTheStepFormOffersTheWholePlacementVocabulary: every key the placement
// vocabulary owns has a control. A key editable only by hand is a key the
// standing no-config-files rule says should not exist — and until this ticket
// most of them were exactly that.
func TestTheStepFormOffersTheWholePlacementVocabulary(t *testing.T) {
	section := strings.Join(strings.Fields(stepFormSection(t)), " ")
	for _, key := range placement.Fields() {
		if !strings.Contains(section, "]."+key) {
			t.Errorf("the step form has no control bound to the vocabulary's %q key", key)
		}
	}
}

// TestTheStepFormPinsEveryPlacementProblemToItsField: each control asks the
// daemon for the problem on its own key, so "66% is more than the whole
// screen" lands under the width box rather than in a general error area the
// user has to map back to a field themselves.
func TestTheStepFormPinsEveryPlacementProblemToItsField(t *testing.T) {
	section := strings.Join(strings.Fields(stepFormSection(t)), " ")
	for _, key := range placement.Fields() {
		want := `automationProblemFor("steps[" + index + "].` + key + `"`
		alt := `automationProblemFor( "steps[" + positionRow.stepIndex + "].` + key + `"`
		if !strings.Contains(section, want) && !strings.Contains(section, alt) {
			t.Errorf("no control pins the daemon's problem for the step's %q key", key)
		}
	}
	// And nothing the daemon says about a step may be dropped: the keys with
	// no control of their own still land inside the step that owns them.
	if !strings.Contains(section, "automationStepExtraProblems(index)") {
		t.Error("a step's uncontrolled keys have no problem channel, so a message could vanish")
	}
}

// TestTheEditorInventsNoPlacementVocabulary: not one mode, direction, focus
// choice or launch policy is spelled in the window. They arrive from
// `placement.vocabulary`, so a mode added to the vocabulary appears in this
// form without anyone remembering — and a mode removed disappears, which a
// hard-coded list cannot do.
//
// Kept after the QML suite landed (#174). The executed suite drives the
// vocabulary the daemon sends and sees the editor render it, which is exactly
// what a hard-coded copy of the same list would also do. The failure only
// appears the day Go grows a mode this file has never heard of — by which
// time the copy is shipped. Absence of the literals is the only check that
// fires before that day.
func TestTheEditorInventsNoPlacementVocabulary(t *testing.T) {
	section := stepFormSection(t)
	var banned []string
	for _, name := range placement.ModeNames() {
		banned = append(banned, `"`+name+`"`)
	}
	for _, dir := range placement.PlaceNextValues() {
		banned = append(banned, `"`+dir+`"`)
	}
	banned = append(banned, `"`+string(placement.FocusFollow)+`"`,
		`"`+string(placement.FocusSilent)+`"`, `"always"`, `"if_missing"`)
	for _, word := range banned {
		if strings.Contains(section, word) {
			t.Errorf("the step form spells %s itself; the vocabulary is the daemon's", word)
		}
	}
	// And it renders the daemon's lists rather than lists of its own.
	for _, want := range []string{
		"options: win.placementModes",
		"options: win.placementPlaceNext",
		"options: win.placementFocusChoices",
		"options: win.placementLaunchChoices",
		"options: win.monitorPickerOptions()",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the step form is missing %s", want)
		}
	}
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))
	if n := strings.Count(qml, `"placement.vocabulary"`); n != 1 {
		t.Errorf("placement.vocabulary is called from %d places; one contract, one call site", n)
	}
	// The workspace bounds are the vocabulary's numbers too — a field claiming
	// a different range would be a field that refuses what the daemon accepts.
	if !strings.Contains(qml, "placementWorkspaceMin") ||
		!strings.Contains(qml, "placementWorkspaceMax") {
		t.Error("the workspace field does not take its bounds from the daemon")
	}
	// A state the vocabulary declines is shown WITH its reason, because an
	// option that is simply missing reads as an oversight.
	if !strings.Contains(qml, "placementUnsupported") {
		t.Error("the declined window states and their reasons are never shown")
	}
}

// TestTheDiagramComputesNoPlacement is the ADR 0013 guard proper, and the
// test the acceptance criteria name outright. The window may scale a fraction
// it was handed to the width of a box; it may not work out a share, read a
// monitor's geometry, or decide what fits.
//
// Kept after the QML suite landed (#174). A running test can only ask whether
// the drawing is right for the numbers it was given, and it would be — the
// duplicated arithmetic starts out agreeing with the daemon's. What this
// guards against is the second implementation existing at all, so that it can
// drift later against a placement rule nobody thought to re-test. "No
// arithmetic here" is a property of the source and has no runtime form.
func TestTheDiagramComputesNoPlacement(t *testing.T) {
	component := previewComponent(t)
	for _, banned := range []string{
		"Math.",       // any arithmetic beyond scaling a fraction to a box
		`"%"`,         // a percentage composed or parsed here
		`"px"`,        // a pixel unit composed here
		"parseInt",    // a number read out of a string the daemon wrote
		"parseFloat",  //
		"/ 100",       // a percentage turned into a fraction
		"* 100",       // a fraction turned into a percentage
		"reserved",    // the bars' reservation, which is the daemon's arithmetic
		"scale",       // the output scale, likewise
		"connector",   // a screen identified here rather than named by the reply
		"placement.",  // any daemon verb: the diagram is handed its data
		"monitors.",   //
		"JSON.String", // and it never sends anything at all
	} {
		if strings.Contains(component, banned) {
			t.Errorf("the diagram spells %q; every number it draws is the daemon's", banned)
		}
	}
	// What it DOES read is the reply's own geometry, unchanged.
	for _, want := range []string{
		"root.workspace.aspect",
		"root.usable.x", "root.usable.y", "root.usable.width", "root.usable.height",
		"panelBox.panel.x", "panelBox.panel.y",
		"panelBox.panel.width", "panelBox.panel.height",
		"panelBox.panel.share", "panelBox.panel.label",
	} {
		if !strings.Contains(component, want) {
			t.Errorf("the diagram does not draw from %s", want)
		}
	}
	// The section that places the drawings decides only their order, which is
	// the order the daemon sent them in.
	section := previewSection(t)
	if !strings.Contains(section, "win.automationPreviewWorkspaces()") {
		t.Error("the preview section builds its own list of workspaces")
	}
	if strings.Contains(section, "sort") || strings.Contains(section, "workspace <") {
		t.Error("the preview section reorders the daemon's workspaces")
	}
}

// TestTheDiagramIsDrawnToTheScreensOwnShape: the aspect ratio comes from the
// reply and sets the height of the drawing. Drawn to any other proportions,
// every share read off the picture would be wrong.
func TestTheDiagramIsDrawnToTheScreensOwnShape(t *testing.T) {
	component := strings.Join(strings.Fields(previewComponent(t)), " ")
	if !strings.Contains(component, "height: parent.width / root.aspect") {
		t.Error("the drawing's height is not the daemon's aspect ratio, so it is not the " +
			"shape of the screen it claims to show")
	}
}

// TestAnArrangementThatCannotHappenIsNotDrawn: the criterion in one test. The
// daemon says whether a workspace is drawable; when it is not there is no
// picture, and the daemon's own sentences appear in its place.
func TestAnArrangementThatCannotHappenIsNotDrawn(t *testing.T) {
	component := strings.Join(strings.Fields(previewComponent(t)), " ")
	if !strings.Contains(component, "visible: root.workspace.drawable === true") {
		t.Error("the drawing is not gated on the daemon's drawable verdict")
	}
	if !strings.Contains(component, `text: "Not drawn: " + String(root.workspace.unavailable`) {
		t.Error("a workspace with no drawing never says why")
	}
	if !strings.Contains(component, `"Problem: " + String((root.problems[index] || {}).message`) {
		t.Error("the daemon's refusals are not shown where the drawing would have been")
	}
	// The refusals are rendered as problems and the notes are not: a caution
	// dressed as a refusal reads as a draft that cannot be saved.
	if strings.Contains(component, `"Problem: " + String((root.panels[index]`) {
		t.Error("a panel's note is rendered in the problem channel")
	}
}

// TestTheDiagramFollowsTheStepOrder: reordering is a structural change, and
// every structural change goes through reassignAutomationDraft, which
// revalidates — which is where the new drawing comes from. If the reorder
// buttons stopped doing that the picture would keep showing the old layout.
func TestTheDiagramFollowsTheStepOrder(t *testing.T) {
	section := strings.Join(strings.Fields(stepFormSection(t)), " ")
	for _, want := range []string{`name: "Move step " + (index + 1) + " up"`,
		`name: "Move step " + (index + 1) + " down"`} {
		if !strings.Contains(section, want) {
			t.Errorf("the step form is missing %s", want)
		}
	}
	if n := strings.Count(section, "win.reassignAutomationDraft()"); n < 3 {
		t.Errorf("reassignAutomationDraft is called %d times in the step form; the two "+
			"reorder buttons and the remove button all have to redraw", n)
	}
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))
	// And the step delegates have to be REBUILT by that reassignment. A model
	// that was only the length does not change when two steps swap, so the
	// inputs — filled on completion — would keep showing the other step's
	// values while the draft and the diagram had both moved on.
	if !strings.Contains(qml, "model: (win.automationDraft.steps || []).slice()") {
		t.Error("the steps repeater is not driven by a fresh array, so a reorder would " +
			"leave every step's inputs showing the step it used to be")
	}
	if !strings.Contains(qml, "automationDraft = clone\n    validateAutomationDraft()") {
		t.Error("a structural change does not revalidate, so the diagram would keep " +
			"showing the arrangement before the move")
	}
	if !strings.Contains(qml, "automationFormPreview = result.preview || {}") {
		t.Error("the validate reply's preview is never stored, so nothing could be drawn")
	}
	if strings.Count(qml, "automationFormPreview = {}") < 2 {
		t.Error("the preview is not cleared when a form opens; the last routine's " +
			"drawing would be attached to this one")
	}
}

// TestTheArrangementIsAlsoInWords: the accessibility criterion. A per-step
// sentence beside the fields, the same sentences under the drawing, and the
// drawing itself announced rather than left as an unlabelled rectangle.
func TestTheArrangementIsAlsoInWords(t *testing.T) {
	section := stepFormSection(t)
	if !strings.Contains(section, "win.automationStepSummary(index)") {
		t.Error("a step has no sentence, so the arrangement is conveyed by the diagram alone")
	}
	component := previewComponent(t)
	if !strings.Contains(component, "root.summaries") {
		t.Error("the diagram is not accompanied by the daemon's sentences")
	}
	for _, want := range []string{
		"Accessible.role: Accessible.Graphic",
		"Accessible.description: root.describeArrangement()",
	} {
		if !strings.Contains(component, want) {
			t.Errorf("the drawing is missing %s", want)
		}
	}
	// The sentences are the daemon's, verbatim — a summary assembled here
	// would be a second description of the same placement.
	if strings.Contains(component, "opens on workspace") {
		t.Error("the diagram composes a placement sentence of its own")
	}
}

// TestACapturedRoutineOpensInTheSameEditor: a routine captured from the live
// desktop (ADR 0026) is a [[routines]] entry like any other, so it opens
// through the same config.get_entry and is edited with the same controls.
// What it must NOT do is arrive with keys the form silently drops — the
// superseded spellings a hand edit or an old capture left behind are carried
// through and removed only when the user says so.
func TestACapturedRoutineOpensInTheSameEditor(t *testing.T) {
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))
	if !strings.Contains(qml, `readonly property var automationPlacementKeys: ["size", "tile"]`) {
		t.Error("the superseded placement keys are no longer the ones carried through; " +
			"a key with a control must be written by that control and not twice")
	}
	if !strings.Contains(qml, "automationClearSuperseded") {
		t.Error("there is no way to remove a superseded key from the window, so a routine " +
			"carrying one could only be fixed by editing config.toml")
	}
	section := stepFormSection(t)
	if !strings.Contains(section, "win.automationStepSuperseded(index)") {
		t.Error("a step never says which older keys it is still carrying")
	}
	if strings.Contains(section, "edit config.toml") {
		t.Error("the step form still sends the user to a text editor for a placement key")
	}
}
