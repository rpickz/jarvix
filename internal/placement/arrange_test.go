package placement

import (
	"strings"
	"testing"
)

// The arrangement these tests measure is the one the feature was written for
// and the one the editor's worked example shows: a browser at two thirds of
// the top screen with the next window to its right, two web apps stacked in
// the remaining third, and a second browser alone on the bottom screen.
//
// The numbers are the top monitor's: 3440 by 1440 with 26 pixels of bar
// reserved, so 3440 by 1414 usable.

// morningWindows is the worked example, in the order the routine opens them.
func morningWindows() []Arranged {
	return []Arranged{
		{Step: 0, Label: "chromium", Placement: Placement{
			Workspace: 1, Monitor: "HDMI-A-1", Mode: ModeTiled,
			Width: Percent(66), PlaceNext: PlaceNextRight}},
		{Step: 1, Label: "X", Placement: Placement{
			Workspace: 1, Mode: ModeTiled, PlaceNext: PlaceNextBelow}},
		{Step: 2, Label: "ChatGPT", Placement: Placement{
			Workspace: 1, Mode: ModeTiled, Height: Percent(50)}},
	}
}

// panelFor finds one step's rectangle, which is the only way to assert about
// a drawing without depending on the order it happens to be built in.
func panelFor(t *testing.T, a Arrangement, step int) Panel {
	t.Helper()
	for _, p := range a.Panels {
		if p.Step == step {
			return p
		}
	}
	t.Fatalf("step %d has no panel; got %d panels", step, len(a.Panels))
	return Panel{}
}

// near reports whether two fractions agree to within half a pixel of a
// 3440-wide screen, which is the precision a drawing has.
func near(a, b float64) bool { return a-b < 0.0002 && b-a < 0.0002 }

// TestTheWorkedExampleLaysOutTheWayTheRoutineDescribesIt: the acceptance
// criterion's own arrangement, checked rectangle by rectangle. Two thirds and
// a third across, and the third split in half down.
func TestTheWorkedExampleLaysOutTheWayTheRoutineDescribesIt(t *testing.T) {
	arrangement := Arrange(topMonitor(), morningWindows())
	if !arrangement.Drawable() {
		t.Fatalf("the worked example does not draw: %v", arrangement.Problems)
	}
	usable := topMonitor().Usable()
	logical := topMonitor().Logical()
	// 66% of 3440 usable pixels is 2270; the top of the usable area is the
	// bar's 26 pixels down the glass.
	browser := panelFor(t, arrangement, 0)
	if !near(browser.Width, 2270.0/float64(logical.Width)) {
		t.Errorf("the browser is %.4f of the screen wide; two thirds of %d usable pixels is 2270",
			browser.Width, usable.Width)
	}
	if !near(browser.Height, float64(usable.Height)/float64(logical.Height)) {
		t.Errorf("the browser is %.4f tall; a lone column fills the usable height", browser.Height)
	}
	// The two web apps share what is left, stacked.
	entry := panelFor(t, arrangement, 1)
	chat := panelFor(t, arrangement, 2)
	if !near(entry.X, browser.X+browser.Width) {
		t.Errorf("the first web app starts at %.4f, not where the browser ends (%.4f)",
			entry.X, browser.X+browser.Width)
	}
	if !near(entry.Width, chat.Width) {
		t.Errorf("the stacked web apps are %.4f and %.4f wide; they share one column",
			entry.Width, chat.Width)
	}
	if !near(chat.Y, entry.Y+entry.Height) {
		t.Errorf("the second web app starts at %.4f, not below the first (%.4f)",
			chat.Y, entry.Y+entry.Height)
	}
	// The one that asked for half the height got it.
	if !near(chat.Height, 707.0/float64(logical.Height)) {
		t.Errorf("the web app that asked for 50%% of the height is %.4f of the screen tall; "+
			"half of %d usable pixels is 707", chat.Height, usable.Height)
	}
}

// TestTheDrawingIsTheShapeOfTheScreen: the aspect ratio and the usable inset
// come from the monitor, so a preview of an ultrawide is not a preview of a
// square. Drawing to the wrong proportions would make every share look wrong.
func TestTheDrawingIsTheShapeOfTheScreen(t *testing.T) {
	arrangement := Arrange(topMonitor(), morningWindows())
	if !near(arrangement.Aspect, 3440.0/1440.0) {
		t.Errorf("the drawing's aspect is %.4f; the screen is 3440 by 1440", arrangement.Aspect)
	}
	// The bar's 26 pixels are an inset at the top, not lost.
	if !near(arrangement.Usable.Y, 26.0/1440.0) {
		t.Errorf("the usable area starts %.4f down; the bar took 26 of 1440 pixels",
			arrangement.Usable.Y)
	}
	if !near(arrangement.Usable.Height, 1414.0/1440.0) {
		t.Errorf("the usable area is %.4f of the glass tall; the bar took 26 of 1440",
			arrangement.Usable.Height)
	}
}

// TestReorderingTheStepsRedrawsTheLayout: insertion order decides the tiling
// structure, so moving a step must move a rectangle. A preview that looked
// the same either way would be hiding the one thing about routines that is
// easiest to get wrong.
func TestReorderingTheStepsRedrawsTheLayout(t *testing.T) {
	before := Arrange(topMonitor(), morningWindows())
	swapped := morningWindows()
	swapped[1], swapped[2] = swapped[2], swapped[1]
	after := Arrange(topMonitor(), swapped)
	if !before.Drawable() || !after.Drawable() {
		t.Fatalf("one of the orders does not draw: %v / %v", before.Problems, after.Problems)
	}
	// Step 2 asked for half the height and, opened second, is now the window
	// the third one splits — so it no longer gets what it asked for, and the
	// rectangle has to say so.
	moved := panelFor(t, after, 2)
	original := panelFor(t, before, 2)
	if near(moved.Y, original.Y) && near(moved.Height, original.Height) {
		t.Error("swapping two steps left every rectangle where it was; step order is the layout")
	}
}

// TestSharesOverTheWholeScreenAreRefused: the acceptance criterion. Two
// thirds beside a half is more screen than there is, and the preview must say
// so on a field rather than draw one of the two possible outcomes.
func TestSharesOverTheWholeScreenAreRefused(t *testing.T) {
	arrangement := Arrange(topMonitor(), []Arranged{
		{Step: 0, Label: "chromium", Placement: Placement{
			Mode: ModeTiled, Width: Percent(66), PlaceNext: PlaceNextRight}},
		{Step: 1, Label: "kitty", Placement: Placement{Mode: ModeTiled, Width: Percent(50)}},
	})
	if arrangement.Drawable() {
		t.Fatal("66% beside 50% drew a layout; together they are more than the screen")
	}
	problem := arrangement.Problems[0]
	if problem.Step != 1 || problem.Field != FieldWidth {
		t.Errorf("the refusal is keyed to step %d field %q; the second window's width is what "+
			"was just typed", problem.Step, problem.Field)
	}
	for _, want := range []string{"chromium", "kitty", "2270"} {
		if !strings.Contains(problem.Message, want) {
			t.Errorf("the refusal does not name %q: %s", want, problem.Message)
		}
	}
}

// TestAWindowTakingEverythingLeavesNothingForTheNext: the other half of the
// same refusal, where only one of the two said a number.
func TestAWindowTakingEverythingLeavesNothingForTheNext(t *testing.T) {
	arrangement := Arrange(topMonitor(), []Arranged{
		{Step: 0, Label: "chromium", Placement: Placement{
			Mode: ModeTiled, Width: Percent(100), PlaceNext: PlaceNextRight}},
		{Step: 1, Label: "kitty", Placement: Placement{Mode: ModeTiled}},
	})
	if arrangement.Drawable() {
		t.Fatal("a window taking the whole width still left room for another")
	}
	if !strings.Contains(arrangement.Problems[0].Message, "nothing left for kitty") {
		t.Errorf("the refusal does not say who gets nothing: %s", arrangement.Problems[0].Message)
	}
}

// TestAModeTheVocabularyDeclinesIsNotDrawn: a Placement can be built in Go
// with a mode ParseMode would have refused, and the drawing must refuse it
// too rather than fall through to the tiled default.
func TestAModeTheVocabularyDeclinesIsNotDrawn(t *testing.T) {
	arrangement := Arrange(topMonitor(), []Arranged{
		{Step: 0, Label: "kitty", Placement: Placement{Mode: Mode("grouped")}},
	})
	if arrangement.Drawable() {
		t.Fatal("a mode outside the vocabulary was drawn as though it were tiled")
	}
	if arrangement.Problems[0].Field != FieldMode {
		t.Errorf("the refusal is keyed to %q, not the mode field", arrangement.Problems[0].Field)
	}
}

// TestAPixelSizeBiggerThanTheScreenIsRefusedInTheVocabularysOwnWords: the
// preview asks Extent.Resolve, so "1200px is wider than the screen's usable
// 1024 pixels" is written once and shows up here unchanged.
func TestAPixelSizeBiggerThanTheScreenIsRefusedInTheVocabularysOwnWords(t *testing.T) {
	small := Monitor{Name: "eDP-1", Width: 1024, Height: 768, Scale: 1}
	arrangement := Arrange(small, []Arranged{
		{Step: 0, Label: "kitty", Placement: Placement{Mode: ModeTiled, Width: Pixels(1200)}},
	})
	if arrangement.Drawable() {
		t.Fatal("a window wider than the screen was drawn")
	}
	if !strings.Contains(arrangement.Problems[0].Message, "1024") {
		t.Errorf("the refusal is not the vocabulary's own: %s", arrangement.Problems[0].Message)
	}
}

// TestALoneWindowIsToldItsShareWasNotUsed: a tiled window with nothing beside
// it gets the whole workspace whatever it asked for. Drawing it at 66% would
// be a lie; drawing it full width without saying why would be a mystery.
func TestALoneWindowIsToldItsShareWasNotUsed(t *testing.T) {
	arrangement := Arrange(topMonitor(), []Arranged{
		{Step: 0, Label: "chromium", Placement: Placement{Mode: ModeTiled, Width: Percent(66)}},
	})
	if !arrangement.Drawable() {
		t.Fatalf("a lone tiled window does not draw: %v", arrangement.Problems)
	}
	panel := panelFor(t, arrangement, 0)
	if !strings.Contains(panel.Note, "nothing is tiled beside it") {
		t.Errorf("the panel does not explain why its share was not used: %q", panel.Note)
	}
	if panel.Share != "100% × 100%" {
		t.Errorf("the lone window is labelled %q; it has the whole workspace", panel.Share)
	}
}

// TestFullscreenCoversTheGlassAndMaximisedCoversTheUsableArea: the difference
// between the two is the bars, which is exactly what the drawing exists to
// make visible.
func TestFullscreenCoversTheGlassAndMaximisedCoversTheUsableArea(t *testing.T) {
	full := Arrange(topMonitor(), []Arranged{
		{Step: 0, Label: "mpv", Placement: Placement{Mode: ModeFullscreen}}})
	panel := panelFor(t, full, 0)
	if !near(panel.Y, 0) || !near(panel.Height, 1) {
		t.Errorf("fullscreen is drawn at %.4f+%.4f; it covers the bars too", panel.Y, panel.Height)
	}
	max := Arrange(topMonitor(), []Arranged{
		{Step: 0, Label: "mpv", Placement: Placement{Mode: ModeMaximised}}})
	panel = panelFor(t, max, 0)
	if !near(panel.Y, 26.0/1440.0) {
		t.Errorf("maximised is drawn at %.4f down; it leaves the bar visible", panel.Y)
	}
}

// TestAFloatingWindowWithNoSizeIsNotDrawn: its size is the application's
// business, and a rectangle for it would be the preview inventing the one
// number the routine did not give.
func TestAFloatingWindowWithNoSizeIsNotDrawn(t *testing.T) {
	arrangement := Arrange(topMonitor(), []Arranged{
		{Step: 0, Label: "kitty", Placement: Placement{Mode: ModeFloating}}})
	if !arrangement.Drawable() {
		t.Fatalf("an unsized float refused to draw the workspace: %v", arrangement.Problems)
	}
	if len(arrangement.Panels) != 0 {
		t.Errorf("an unsized float was drawn as a rectangle at %v", arrangement.Panels[0].Rect)
	}
}

// TestAScreenWithNoUsableAreaSaysSo: the graceful degradation criterion, at
// the arithmetic's own level.
func TestAScreenWithNoUsableAreaSaysSo(t *testing.T) {
	arrangement := Arrange(Monitor{Name: "DP-9"}, morningWindows())
	if arrangement.Drawable() {
		t.Fatal("a screen with no size drew an arrangement")
	}
	if !strings.Contains(arrangement.Problems[0].Message, "DP-9") {
		t.Errorf("the refusal does not name the screen: %s", arrangement.Problems[0].Message)
	}
}

// TestTheSentenceSaysTheWholePlacement: the accessibility channel. Every part
// of the placement a user chose has to be in the words, because the diagram
// is not available to everyone who has to check the routine.
func TestTheSentenceSaysTheWholePlacement(t *testing.T) {
	sentence := Placement{
		Workspace: 1, Monitor: "HDMI-A-1", Mode: ModeTiled,
		Width: Percent(66), PlaceNext: PlaceNextRight, Focus: FocusFollow,
	}.Sentence("chromium")
	for _, want := range []string{
		"chromium opens", "workspace 1", "HDMI-A-1", "tiled",
		"66% of the width", "to its right", "The view follows it.",
	} {
		if !strings.Contains(sentence, want) {
			t.Errorf("the sentence omits %q: %s", want, sentence)
		}
	}
}

// TestTheSentenceStillReadsForAHalfWrittenStep: a form's normal state is
// half-filled, and a summary that only worked once every field was set would
// be blank exactly when it was most wanted.
func TestTheSentenceStillReadsForAHalfWrittenStep(t *testing.T) {
	sentence := Placement{}.Sentence("")
	if !strings.HasPrefix(sentence, "the window opens") {
		t.Errorf("an empty placement reads as %q", sentence)
	}
	if !strings.Contains(sentence, "however the layout opens it") {
		t.Errorf("a step naming no mode does not say what that means: %s", sentence)
	}
}

// TestTheSentenceNamesTheCurrentScreenByItsMeaning: "current" is a reserved
// word, and reading it back as a connector name would be nonsense.
func TestTheSentenceNamesTheCurrentScreenByItsMeaning(t *testing.T) {
	sentence := Placement{Workspace: 2, Monitor: MonitorCurrent,
		Mode: ModeFloating, Width: Pixels(1200), Height: Pixels(800),
		X: 100, Y: 200, HasPosition: true}.Sentence("kitty")
	for _, want := range []string{
		"on the screen you are on", "floating", "1200 pixels wide",
		"800 pixels tall", "at 100,200",
	} {
		if !strings.Contains(sentence, want) {
			t.Errorf("the sentence omits %q: %s", want, sentence)
		}
	}
}
