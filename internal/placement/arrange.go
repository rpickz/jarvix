package placement

import (
	"fmt"
	"math"
	"strings"
)

// This file is the preview arithmetic (issue #181, ADR 0059): given one
// monitor and the windows a routine opens on one workspace, IN ORDER, what
// rectangles result — plus one sentence per window saying the same thing in
// words, for the readers who will never see the picture.
//
// It lives in the vocabulary rather than in the window for the reason
// everything else here does (ADR 0013, ADR 0056). A diagram is a claim about
// what will happen; a diagram drawn from arithmetic of the QML's own would be
// a SECOND claim, and the day the two disagreed the picture would be the one
// the user believed. So every number the editor draws is computed here, from
// the same Extent.Resolve a run resolves against the same Monitor.Usable, and
// the window multiplies fractions by the width of a rectangle and nothing
// else.
//
// What this models, precisely, is the routine's own ASK — "this window takes
// two thirds; the one after it goes to its right" — laid out the way a
// dwindle layout lays it out: each new window splits the tile of the window
// before it, in the direction that one's place_next named, at whichever of
// the two declared a share of that axis. Where the asks are consistent that
// is what the compositor delivers. Where they are not — two windows claiming
// more of an axis than it has — Arrange refuses to draw rather than picking a
// winner, because a preview that quietly resolved a contradiction would be
// showing a layout that cannot happen, which is the one thing it must never
// do.

// Arranged is one window handed to the preview, in the order the routine
// opens them. Order is not a presentation detail here: insertion order is
// what decides the tiling structure, which is why the editor has to draw it
// (see PlaceNext).
type Arranged struct {
	// Step is the window's position in the routine, so a problem this file
	// finds can be keyed back to the form control that caused it.
	Step int
	// Label is what the step launches, in the user's own words — the app name
	// or the desktop entry id, never a resolved path.
	Label string
	// Placement is where the step said the window goes.
	Placement Placement
}

// Rect is a rectangle expressed as fractions of the monitor it is drawn on:
// 0,0 is the top-left of the glass and 1,1 the bottom-right.
//
// Fractions rather than pixels because the consumer is a drawing of unknown
// size, and a fraction is the one form that cannot be misread as a promise
// about pixels. The monitor's real proportions survive in Arrangement.Aspect,
// which is what makes the picture the shape of the screen rather than the
// shape of the panel it is drawn in.
type Rect struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// PanelKind is how one window sits in the drawing, for a renderer that has to
// tell a tile from something floating over it without re-deciding which is
// which.
type PanelKind string

const (
	// PanelTiled is a window in the layout, sharing the usable area.
	PanelTiled PanelKind = "tiled"
	// PanelFloating is a window lifted out of the layout, drawn over the
	// tiles at its own size and position.
	PanelFloating PanelKind = "floating"
	// PanelCovering is a window that covers everything under it —
	// fullscreen over the whole glass, maximised over the usable area.
	PanelCovering PanelKind = "covering"
)

// Panel is one window's rectangle in the previewed arrangement, with the
// words to label it by. Everything a drawing needs is here and nothing a
// drawing would have to work out is left to it.
type Panel struct {
	Rect
	// Step is the routine step this window is, zero-based, so the drawing can
	// number the rectangles in the order they open.
	Step int `json:"step"`
	// Label is what the step launches, in the user's words.
	Label string `json:"label"`
	// Kind is how the window sits.
	Kind PanelKind `json:"kind"`
	// Share is the proportion this window ACTUALLY ends up with, as whole
	// percentages of the usable area ("66% × 100%") — what the rectangle is,
	// not what was asked for. The two differ exactly when the layout had the
	// final say, and the difference is the thing worth seeing.
	Share string `json:"share"`
	// Size is the same proportion in logical pixels ("2270 × 1400").
	Size string `json:"size"`
	// Note is something true about this window the rectangle cannot show —
	// that it asked for the master pane, or for a share nothing came along to
	// take the rest of. "" when there is nothing to add.
	Note string `json:"note,omitempty"`
}

// StepProblem is one thing wrong with an arrangement, keyed to the step and
// the field a form would show it on — placement.Problem plus which window it
// belongs to, because a preview covers a whole workspace and "width is more
// than the screen" is useless without saying whose width.
type StepProblem struct {
	Step    int    `json:"step"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Arrangement is one workspace's previewed layout: the shape of the screen,
// the part of it windows may occupy, the rectangles, and — when the routine
// asks for something the compositor cannot deliver — why there is nothing to
// draw.
type Arrangement struct {
	// Aspect is the monitor's logical width divided by its height. A drawing
	// that ignores it is a drawing of a different screen.
	Aspect float64 `json:"aspect"`
	// Usable is where windows may go, as a fraction of the glass. It is drawn
	// as an inset so the bars are visible as the margin they are — which is
	// what explains why "66% of the screen" is not 66% of the glass.
	Usable Rect `json:"usable"`
	// Panels are the windows, in the order the routine opens them.
	Panels []Panel `json:"panels"`
	// Problems is why this arrangement cannot happen. Non-empty means the
	// drawing must not be shown: the panels computed so far are the state
	// before the contradiction, and showing them would be showing a layout
	// that will not happen.
	Problems []StepProblem `json:"problems,omitempty"`
}

// Drawable reports whether the arrangement can be shown.
func (a Arrangement) Drawable() bool { return len(a.Problems) == 0 && a.Aspect > 0 }

// rectF is a rectangle in logical pixels inside the usable area, origin at
// its top-left corner. The arithmetic happens here and is converted to
// fractions once, at the end.
type rectF struct{ x, y, w, h float64 }

// Arrange lays out one workspace's windows on one monitor.
//
// The monitor is the caller's business to resolve (Resolver, ForWorkspace):
// this function takes a real output and does arithmetic against it, so a
// screen that is not plugged in never reaches here as a guess.
func Arrange(m Monitor, windows []Arranged) Arrangement {
	logical, usable := m.Logical(), m.Usable()
	out := Arrangement{}
	if logical.Width <= 0 || logical.Height <= 0 || usable.Width <= 0 || usable.Height <= 0 {
		out.Problems = append(out.Problems, StepProblem{Step: -1, Field: FieldMonitor,
			Message: fmt.Sprintf("%s reports no usable area, so there is nothing to draw against",
				monitorName(m))})
		return out
	}
	out.Aspect = float64(logical.Width) / float64(logical.Height)
	out.Usable = Rect{
		X:      float64(usable.X-logical.X) / float64(logical.Width),
		Y:      float64(usable.Y-logical.Y) / float64(logical.Height),
		Width:  float64(usable.Width) / float64(logical.Width),
		Height: float64(usable.Height) / float64(logical.Height),
	}

	layout := &tiling{usable: usable}
	for _, w := range windows {
		panel, problems := placePanel(m, logical, usable, layout, w)
		out.Problems = append(out.Problems, problems...)
		if len(out.Problems) > 0 {
			// One contradiction is enough. Carrying on would lay the rest of
			// the workspace out against a split that could not have happened,
			// and every rectangle after it would be fiction.
			return out
		}
		if panel != nil {
			out.Panels = append(out.Panels, *panel)
		}
	}
	// The tiles' rectangles are only final once every window has been placed,
	// because each split resizes the one before it.
	layout.finish(&out, logical, usable)
	return out
}

// monitorName is the screen's own name, or a phrase for the inventory that
// gave us an output with none.
func monitorName(m Monitor) string {
	if strings.TrimSpace(m.Name) == "" {
		return "that screen"
	}
	return m.Name
}

// tiling is the layout as it is built: the tiles in insertion order, and
// which one the next window splits.
type tiling struct {
	usable Area
	tiles  []*tile
	// last is the tile a new window splits — the most recently tiled window,
	// which is what a dwindle layout does with the focus a fresh window
	// inherits.
	last *tile
}

// tile is one tiled window while the layout is still being built.
type tile struct {
	win  Arranged
	rect rectF
	// appliedW and appliedH record whether the window's declared share was
	// ever honoured by a split. A share nothing came along to take the rest of
	// is not an error — the layout simply gives a lone window everything — but
	// it is the difference between what was asked for and what is drawn, and
	// the panel says so rather than leaving the user to spot it.
	appliedW, appliedH bool
}

// placePanel puts one window somewhere, returning its panel (nil when there
// is nothing to draw for it) and anything it makes impossible.
func placePanel(m Monitor, logical, usable Area, layout *tiling, w Arranged) (*Panel, []StepProblem) {
	spec, known := w.Placement.Mode.Spec()
	switch {
	case w.Placement.Mode != "" && !known:
		// Unreachable through ParseMode, which refuses an unknown value with
		// the reason it is not a mode. Here for the case a Placement was built
		// in Go: a mode nobody can honour has no rectangle, and inventing one
		// would be the preview lying.
		return nil, []StepProblem{{Step: w.Step, Field: FieldMode, Message: fmt.Sprintf(
			"%q is not a placement mode; use one of %s",
			w.Placement.Mode, strings.Join(ModeNames(), ", "))}}
	case known && !spec.Sized && !spec.Tiles:
		// Fullscreen covers the glass; maximised covers the usable area.
		rect := logical
		if w.Placement.Mode == ModeMaximised {
			rect = usable
		}
		panel := Panel{
			Rect: fractionOf(rect, logical),
			Step: w.Step, Label: w.Label, Kind: PanelCovering,
			Share: sharePhrase(float64(rect.Width), float64(rect.Height), usable),
			Size:  sizePhrase(float64(rect.Width), float64(rect.Height)),
			Note:  string(w.Placement.Mode) + ", so it covers whatever is under it",
		}
		return &panel, nil
	case known && spec.Positioned:
		return floatingPanel(m, logical, usable, w)
	}
	// Tiled, and the unset mode with it: a window nobody said anything about
	// joins the layout, because that is what a tiling compositor does with it.
	return layout.insert(m, usable, w)
}

// floatingPanel draws a window that was lifted out of the layout.
//
// A float with no size is not drawn, and that is deliberate: its size is
// whatever the application opens at, which is a fact about the application
// rather than about the routine, and drawing a rectangle for it would be
// inventing the one number the routine did not give. The step's sentence says
// so instead.
func floatingPanel(m Monitor, logical, usable Area, w Arranged) (*Panel, []StepProblem) {
	size, problems := w.Placement.ResolveSize(m)
	if len(problems) > 0 {
		return nil, stepProblems(w.Step, problems)
	}
	if size.Width <= 0 || size.Height <= 0 {
		return nil, nil
	}
	x, y := w.Placement.X-logical.X, w.Placement.Y-logical.Y
	note := ""
	if !w.Placement.HasPosition {
		// Centred, and said so: the routine named no position, so where it
		// lands is the compositor's choice and the drawing must not pretend
		// otherwise.
		x = (logical.Width - size.Width) / 2
		y = (logical.Height - size.Height) / 2
		note = "no position given, so it is drawn centred; the compositor decides where it lands"
	}
	panel := Panel{
		Rect: fractionOf(Area{X: logical.X + x, Y: logical.Y + y,
			Width: size.Width, Height: size.Height}, logical),
		Step: w.Step, Label: w.Label, Kind: PanelFloating,
		Share: sharePhrase(float64(size.Width), float64(size.Height), usable),
		Size:  sizePhrase(float64(size.Width), float64(size.Height)),
		Note:  note,
	}
	return &panel, nil
}

// insert puts one window into the tiling layout, splitting the tile of the
// window before it.
func (t *tiling) insert(m Monitor, usable Area, w Arranged) (*Panel, []StepProblem) {
	if _, problems := w.Placement.ResolveSize(m); len(problems) > 0 {
		// The share is refused before it is used, and by the same function a
		// run resolves it with: "1200px is wider than the screen's usable
		// 1024 pixels" is one sentence written once.
		return nil, stepProblems(w.Step, problems)
	}
	entering := &tile{win: w}
	if t.last == nil {
		entering.rect = rectF{0, 0, float64(usable.Width), float64(usable.Height)}
		t.tiles = append(t.tiles, entering)
		t.last = entering
		return nil, nil
	}
	keeper := t.last
	dir := keeper.win.Placement.PlaceNext
	if dir == PlaceNextNone {
		// A dwindle layout with no preselection splits the longer axis, which
		// is why a routine that never says place_next still produces a sane
		// picture — and why one that cares has to say it.
		dir = longerAxis(keeper.rect)
	}
	horizontal := dir == PlaceNextRight || dir == PlaceNextLeft
	span := keeper.rect.h
	if horizontal {
		span = keeper.rect.w
	}
	keep, keepSet := axisExtent(keeper.win.Placement, horizontal, usable)
	enter, enterSet := axisExtent(w.Placement, horizontal, usable)
	kept, problem := splitAt(span, keeper, entering, horizontal, keep, keepSet, enter, enterSet)
	if problem != nil {
		return nil, []StepProblem{*problem}
	}
	keeper.rect, entering.rect = cut(keeper.rect, dir, kept)
	if keepSet {
		mark(keeper, horizontal)
	}
	if enterSet {
		mark(entering, horizontal)
	}
	t.tiles = append(t.tiles, entering)
	t.last = entering
	return nil, nil
}

// splitAt decides how much of the shared axis the window already there keeps,
// and refuses the split when the two windows together ask for more of that
// axis than it has.
//
// That refusal is the acceptance criterion this whole file exists for. Two
// thirds beside a half is 116% of a screen: a compositor asked for it will
// deliver something — one of the two resizes will simply lose — and a preview
// that showed either outcome would be teaching the user that the routine
// works. Saying "these two do not fit" names both windows and both numbers,
// which is what someone needs in order to change one of them.
func splitAt(span float64, keeper, entering *tile, horizontal bool,
	keep float64, keepSet bool, enter float64, enterSet bool) (float64, *StepProblem) {
	field := FieldHeight
	axis := "down"
	if horizontal {
		field, axis = FieldWidth, "across"
	}
	switch {
	case keepSet && enterSet:
		if keep+enter > span+0.5 {
			return 0, &StepProblem{Step: entering.win.Step, Field: field, Message: fmt.Sprintf(
				"%s takes %d of the %d pixels %s here and %s asks for %d more, so together they "+
					"are bigger than the screen; give one of them a smaller share",
				label(keeper.win), round(keep), round(span), axis, label(entering.win), round(enter))}
		}
		return keep, nil
	case keepSet:
		if keep >= span {
			return 0, &StepProblem{Step: keeper.win.Step, Field: field, Message: fmt.Sprintf(
				"%s asks for the whole %d pixels %s here, so there is nothing left for %s; "+
					"give it a smaller share", label(keeper.win), round(span), axis, label(entering.win))}
		}
		return keep, nil
	case enterSet:
		if enter >= span {
			return 0, &StepProblem{Step: entering.win.Step, Field: field, Message: fmt.Sprintf(
				"%s asks for the whole %d pixels %s here, so there is nothing left for %s; "+
					"give it a smaller share", label(entering.win), round(span), axis, label(keeper.win))}
		}
		return span - enter, nil
	}
	// Neither said anything: the layout halves the tile, which is what
	// dwindle does.
	return span / 2, nil
}

// mark records that a window's declared share was honoured on one axis.
func mark(t *tile, horizontal bool) {
	if horizontal {
		t.appliedW = true
		return
	}
	t.appliedH = true
}

// label is what to call a window in a sentence about the layout.
func label(w Arranged) string {
	if trimmed := strings.TrimSpace(w.Label); trimmed != "" {
		return trimmed
	}
	return fmt.Sprintf("step %d", w.Step+1)
}

// axisExtent resolves one window's declared share on the axis being split,
// against the usable area — Extent.Resolve, the same arithmetic a run uses.
func axisExtent(p Placement, horizontal bool, usable Area) (float64, bool) {
	e, available := p.Height, usable.Height
	if horizontal {
		e, available = p.Width, usable.Width
	}
	if !e.Set() {
		return 0, false
	}
	px, err := e.Resolve(available)
	if err != nil || px <= 0 {
		// Refused shares never reach here: insert resolves the whole size
		// first and returns the vocabulary's own message for it.
		return 0, false
	}
	return float64(px), true
}

// longerAxis is the direction a layout with no preselection splits in.
func longerAxis(r rectF) PlaceNext {
	if r.w >= r.h {
		return PlaceNextRight
	}
	return PlaceNextBelow
}

// cut divides a rectangle, returning the part the window already there keeps
// and the part the arriving window takes. kept is measured on the split axis,
// from whichever side the keeper stays on.
func cut(r rectF, dir PlaceNext, kept float64) (keeper, entering rectF) {
	switch dir {
	case PlaceNextRight:
		return rectF{r.x, r.y, kept, r.h}, rectF{r.x + kept, r.y, r.w - kept, r.h}
	case PlaceNextLeft:
		return rectF{r.x + r.w - kept, r.y, kept, r.h}, rectF{r.x, r.y, r.w - kept, r.h}
	case PlaceNextAbove:
		return rectF{r.x, r.y + r.h - kept, r.w, kept}, rectF{r.x, r.y, r.w, r.h - kept}
	default: // PlaceNextBelow
		return rectF{r.x, r.y, r.w, kept}, rectF{r.x, r.y + kept, r.w, r.h - kept}
	}
}

// finish converts the finished tiles into panels, in insertion order.
func (t *tiling) finish(out *Arrangement, logical, usable Area) {
	if len(t.tiles) == 0 {
		return
	}
	panels := make([]Panel, 0, len(t.tiles)+len(out.Panels))
	for _, tile := range t.tiles {
		panels = append(panels, Panel{
			Rect: fractionOf(Area{
				X: usable.X + round(tile.rect.x), Y: usable.Y + round(tile.rect.y),
				Width: round(tile.rect.w), Height: round(tile.rect.h),
			}, logical),
			Step: tile.win.Step, Label: tile.win.Label, Kind: PanelTiled,
			Share: sharePhrase(tile.rect.w, tile.rect.h, usable),
			Size:  sizePhrase(tile.rect.w, tile.rect.h),
			Note:  tileNote(tile),
		})
	}
	// The floating and covering panels were appended as they were met and
	// belong ON TOP of the tiles; the tiles go first so a renderer drawing in
	// order gets the stacking right without deciding anything.
	out.Panels = append(panels, out.Panels...)
}

// tileNote says what a tile's rectangle cannot: a share the layout never had
// occasion to honour, and a promotion whose destination belongs to the
// workspace's layout rather than to this arithmetic.
func tileNote(t *tile) string {
	var notes []string
	for _, axis := range []struct {
		e       Extent
		applied bool
		word    string
	}{
		{t.win.Placement.Width, t.appliedW, "width"},
		{t.win.Placement.Height, t.appliedH, "height"},
	} {
		if axis.e.Set() && !axis.applied {
			notes = append(notes, fmt.Sprintf(
				"it asks for %s of the %s, but nothing is tiled beside it on that axis, so the "+
					"layout gives it everything", axis.e, axis.word))
		}
	}
	if t.win.Placement.Master {
		notes = append(notes, "it asks for the master pane; which pane that is belongs to the "+
			"workspace's layout, so this drawing shows it where the order puts it")
	}
	return strings.Join(notes, " ")
}

// stepProblems keys the vocabulary's own problems to a step.
func stepProblems(step int, problems []Problem) []StepProblem {
	out := make([]StepProblem, 0, len(problems))
	for _, p := range problems {
		out = append(out, StepProblem{Step: step, Field: p.Field, Message: p.Message})
	}
	return out
}

// fractionOf expresses a rectangle as fractions of the monitor's glass.
func fractionOf(r, logical Area) Rect {
	return Rect{
		X:      float64(r.X-logical.X) / float64(logical.Width),
		Y:      float64(r.Y-logical.Y) / float64(logical.Height),
		Width:  float64(r.Width) / float64(logical.Width),
		Height: float64(r.Height) / float64(logical.Height),
	}
}

// sharePhrase is the proportion a window ends up with, as whole percentages
// of the usable area — the label on the rectangle. Whole percentages because
// the number is being read off a picture, and "66%" is the fact while
// "66.02%" is noise.
func sharePhrase(w, h float64, usable Area) string {
	return fmt.Sprintf("%d%% × %d%%",
		round(w*100/float64(usable.Width)), round(h*100/float64(usable.Height)))
}

// sizePhrase is the same proportion in logical pixels.
func sizePhrase(w, h float64) string {
	return fmt.Sprintf("%d × %d", round(w), round(h))
}

// round is the one rounding rule in this file: nearest, so a rectangle a
// pixel narrow never appears as a gap in the drawing.
func round(v float64) int { return int(math.Round(v)) }

// Sentence describes a placement in words — the accessibility channel the
// diagram cannot be, and the channel that still works when the target screen
// is unplugged and there is nothing to draw at all.
//
// It is composed here rather than in the window for the reason the drawing is:
// the vocabulary owns what its values MEAN, and a sentence assembled in QML
// would be a second, quietly diverging description of the same placement.
//
// what is the thing being placed, in the user's own words.
func (p Placement) Sentence(what string) string {
	subject := strings.TrimSpace(what)
	if subject == "" {
		subject = "the window"
	}
	var b strings.Builder
	b.WriteString(subject + " opens")
	if p.Workspace > 0 {
		fmt.Fprintf(&b, " on workspace %d", p.Workspace)
	}
	switch p.Monitor.Kind() {
	case RefCurrent:
		b.WriteString(", on the screen you are on")
	case RefConnector, RefNickname:
		b.WriteString(", on " + strings.TrimSpace(string(p.Monitor)))
	}
	b.WriteString(modePhrase(p))
	if clause := sizeClause(p); clause != "" {
		b.WriteString(", " + clause)
	}
	if p.HasPosition {
		fmt.Fprintf(&b, ", at %d,%d on the screen", p.X, p.Y)
	}
	if p.Master {
		b.WriteString(", promoted into the layout's master pane")
	}
	b.WriteString(".")
	if phrase, ok := placeNextPhrase[p.PlaceNext]; ok {
		b.WriteString(" The next window on this workspace goes " + phrase + ".")
	}
	if p.Focus == FocusFollow {
		b.WriteString(" The view follows it.")
	}
	return b.String()
}

// modePhrase words how the window sits, including the answer for a step that
// said nothing — which is a real answer and not a gap: the compositor's own
// choice is to tile it.
func modePhrase(p Placement) string {
	switch p.Mode {
	case ModeTiled:
		return ", tiled into the layout"
	case ModeFloating:
		return ", floating above the layout"
	case ModePinned:
		return ", floating and pinned above every workspace"
	case ModeFullscreen:
		return ", filling the whole screen over the bars"
	case ModeMaximised:
		return ", filling the workspace's usable area"
	}
	return ", however the layout opens it"
}

// sizeClause words the share, or "" when the placement asks for none.
func sizeClause(p Placement) string {
	var parts []string
	if phrase := extentPhrase(p.Width, true); phrase != "" {
		parts = append(parts, phrase)
	}
	if phrase := extentPhrase(p.Height, false); phrase != "" {
		parts = append(parts, phrase)
	}
	if len(parts) == 0 {
		return ""
	}
	return "taking " + strings.Join(parts, " and ")
}

// extentPhrase words one axis of a share.
func extentPhrase(e Extent, horizontal bool) string {
	if !e.Set() {
		return ""
	}
	axis, dimension := "height", "tall"
	if horizontal {
		axis, dimension = "width", "wide"
	}
	if e.Unit == UnitPercent {
		return fmt.Sprintf("%d%% of the %s", e.Value, axis)
	}
	return fmt.Sprintf("%d pixels %s", e.Value, dimension)
}

// placeNextPhrase is how each arrangement direction reads in a sentence.
var placeNextPhrase = map[PlaceNext]string{
	PlaceNextRight: "to its right",
	PlaceNextLeft:  "to its left",
	PlaceNextBelow: "below it",
	PlaceNextAbove: "above it",
}
