// Package placement is the window-placement vocabulary (ADR 0056): the one
// definition of *where a window goes* that every surface which opens a window
// speaks — routine steps, the window-control tools, and whatever comes next.
//
// It exists because the answer used to be spelled twice and thinly. A routine
// step could say `float = true`, a pixel size and position that only applied
// while floating, and `tile = "split" | "master"`; the window tools could say
// a workspace number and nothing else. A user who set out to place their
// morning windows — a browser at two thirds of the top screen, two web apps
// stacked in the remaining third, a second browser on the bottom screen —
// could not, and worked around it with a shell script that had to reimplement
// launching, matching, monitor geometry and placement, and still could not
// express a tiled proportion. That script is the evidence this package is the
// answer to.
//
// Three properties are the design:
//
//   - One definition, many consumers. Modes, units, arrangement and targets
//     are declared here and nowhere else. The routine schema
//     (internal/config), the runner (internal/routine) and the window tools
//     (internal/tools) all derive their accepted values from these functions,
//     and a contract test pins that they cannot drift apart. Adding an option
//     here makes it available everywhere at once, which is the whole point.
//   - Nothing here talks to a compositor. This package is pure: values,
//     parsing, validation and arithmetic. The dialect seam (internal/desktop)
//     stays the only code that speaks to Hyprland, so a second compositor
//     changes one file rather than the vocabulary.
//   - What the compositor cannot do is written down, not omitted. Modes that
//     Hyprland offers but this vocabulary declines carry their reason
//     (Unsupported), so "why can't I tab these windows?" has an answer in the
//     product rather than in a maintainer's memory.
//
// The compositor mapping — which Lua verb each mode dispatches, probed against
// Hyprland 0.56.2 rather than assumed — is in ADR 0056 and implemented in
// internal/desktop/compositor.go.
package placement

import (
	"fmt"
	"strconv"
	"strings"
)

// Mode is how a window sits on its workspace. The set is closed: every value
// maps to a compositor verb the dialect seam can actually send, and a mode
// that could not be delivered as a *set* (rather than a toggle) is not in it —
// see Unsupported for those and why.
type Mode string

const (
	// ModeTiled puts the window in the workspace's tiling layout. With a
	// Width or Height it also claims a proportion of the workspace: on a
	// tiled window an exact resize moves the split it sits in rather than
	// lifting the window out of the layout.
	ModeTiled Mode = "tiled"
	// ModeFloating lifts the window out of the layout, where a size and a
	// position mean pixels on the monitor.
	ModeFloating Mode = "floating"
	// ModePinned is floating and pinned above everything, on every workspace
	// — Hyprland's "pin", which it only honours for floating windows, so this
	// mode floats first and pins second rather than making the user say both.
	ModePinned Mode = "pinned"
	// ModeFullscreen covers the whole output, decorations and bars included.
	ModeFullscreen Mode = "fullscreen"
	// ModeMaximised covers the workspace's usable area — everything the
	// tiling layout would have to share — while leaving the bars visible.
	// Hyprland spells it `maximized`; the vocabulary is in British English
	// like the rest of the product, and the seam translates.
	ModeMaximised Mode = "maximised"
)

// ModeSpec describes one mode for every consumer that has to explain it: the
// tool schema shown to the model, the form's dropdown, the docs, and the
// validator deciding whether a size means anything here.
type ModeSpec struct {
	// Name is the value written in configuration and sent by the tools.
	Name Mode
	// Summary is one sentence, written for someone choosing between modes.
	Summary string
	// Sized reports whether Width and Height mean anything in this mode. A
	// size on a mode that ignores it is a validation problem rather than a
	// silently dropped directive, because a user who wrote it expected it.
	Sized bool
	// Positioned reports whether an explicit pixel position applies.
	Positioned bool
	// Tiles reports whether the window joins the tiling layout, and so
	// whether PlaceNext (which decides where the *next* window lands) and
	// Master mean anything.
	Tiles bool
}

// modeSpecs is the vocabulary's mode table, in the order every consumer
// presents them: the two a user reaches for first, then the special states.
var modeSpecs = []ModeSpec{
	{
		Name:    ModeTiled,
		Summary: "Tiled into the workspace's layout, optionally claiming a share of it.",
		Sized:   true, Tiles: true,
	},
	{
		Name:    ModeFloating,
		Summary: "Floating above the layout at a size and position you choose.",
		Sized:   true, Positioned: true,
	},
	{
		Name:    ModePinned,
		Summary: "Floating and pinned above every workspace, so it follows you around.",
		Sized:   true, Positioned: true,
	},
	{
		Name:    ModeFullscreen,
		Summary: "Filling the whole screen, over the bars.",
	},
	{
		Name:    ModeMaximised,
		Summary: "Filling the workspace's usable area, leaving the bars visible.",
	},
}

// Modes returns the vocabulary's modes, in presentation order. Callers must
// treat the result as read-only; it is copied so a consumer cannot edit the
// vocabulary by editing what it was handed.
func Modes() []ModeSpec {
	return append([]ModeSpec(nil), modeSpecs...)
}

// ModeNames returns just the names, for a schema enum or a docs table.
func ModeNames() []string {
	names := make([]string, 0, len(modeSpecs))
	for _, spec := range modeSpecs {
		names = append(names, string(spec.Name))
	}
	return names
}

// Spec looks a mode up. The zero Mode ("") is not a mode: it means "the
// step said nothing", which every consumer treats as its own default rather
// than as an error, so it is not in the table.
func (m Mode) Spec() (ModeSpec, bool) {
	for _, spec := range modeSpecs {
		if spec.Name == m {
			return spec, true
		}
	}
	return ModeSpec{}, false
}

// Unsupported is one window state the compositor genuinely offers that this
// vocabulary declines, together with why. It is a value rather than a comment
// because the reason is owed to the user: an option that is simply absent
// reads as an oversight, and the same sentence has to appear in the error
// message, the docs and the ADR without being written three times.
type Unsupported struct {
	// Name is what a user would call it, and what they may well have typed.
	Name string
	// Reason is one sentence saying why it is not a mode, written to be read
	// by whoever asked for it.
	Reason string
}

// unsupportedModes is the "we looked, and here is why not" list. Each entry
// was probed against Hyprland 0.56.2 (ADR 0056 records the probes); none of
// them is a guess about what the compositor offers.
var unsupportedModes = []Unsupported{
	{
		Name: "grouped",
		Reason: "Hyprland groups windows with hl.dsp.group.toggle, which only toggles — " +
			"there is no \"be in a group\" verb — so a routine that ran twice would " +
			"group the window and then ungroup it, and placement must converge on a " +
			"re-run rather than oscillate",
	},
	{
		Name: "tabbed",
		Reason: "tabs are how Hyprland draws a group, not a separate window state, so it " +
			"is the grouped case with the same toggle-only problem",
	},
	{
		Name: "pseudotiled",
		Reason: "hl.dsp.window.pseudo takes an enable/disable action, but pseudotiling has " +
			"no size of its own — it keeps the window's last floating size inside its " +
			"tile — so it cannot express a proportion, which is what this vocabulary is for",
	},
	{
		Name: "scratchpad",
		Reason: "a scratchpad is Hyprland's special workspace, which is summoned rather " +
			"than placed; it is a target and not a mode, and this vocabulary's targets " +
			"are the numbered workspaces 1 to 99",
	},
	{
		Name: "fullscreen for the application only",
		Reason: "hl.dsp.window.fullscreen_state can tell an application it is fullscreen " +
			"without making it so (and the reverse), which is a compatibility shim for " +
			"misbehaving clients rather than a way to place a window",
	},
}

// UnsupportedModes returns the declined states with their reasons.
func UnsupportedModes() []Unsupported {
	return append([]Unsupported(nil), unsupportedModes...)
}

// ParseMode reads a configured or spoken mode. A value that names something
// the compositor does offer but this vocabulary declines gets that reason
// back rather than a bare "unknown", because "grouped is not a mode" without
// the why is the sentence that makes someone file the same request again.
func ParseMode(s string) (Mode, error) {
	trimmed := strings.ToLower(strings.TrimSpace(s))
	if trimmed == "" {
		return "", nil
	}
	// American spellings of the two that have one. Accepted on input and
	// never emitted: a user who types what Hyprland's own documentation says
	// should not be told it does not exist.
	switch trimmed {
	case "maximized":
		trimmed = string(ModeMaximised)
	case "float":
		trimmed = string(ModeFloating)
	case "tile", "split":
		trimmed = string(ModeTiled)
	}
	if _, ok := Mode(trimmed).Spec(); ok {
		return Mode(trimmed), nil
	}
	for _, u := range unsupportedModes {
		if strings.EqualFold(u.Name, trimmed) {
			return "", fmt.Errorf("%q is not a placement mode: %s", s, u.Reason)
		}
	}
	return "", fmt.Errorf("%q is not a placement mode; use one of %s",
		s, strings.Join(ModeNames(), ", "))
}

// Unit is how a proportion is measured.
type Unit string

const (
	// UnitPercent is a share of the target monitor's usable area — what a
	// person means by "two thirds of the screen", and the only spelling that
	// survives a monitor being swapped for a bigger one.
	UnitPercent Unit = "%"
	// UnitPixels is an exact count of pixels, for the cases where a person
	// really does mean 1200 wide.
	UnitPixels Unit = "px"
)

// maxPixel bounds a pixel value, matching the compositor seam's own defensive
// bound. Values beyond it are not window sizes on any display that exists.
const maxPixel = 32768

// Extent is one axis of a size: a percentage of the usable area, or pixels.
// The zero value means "not said", which is distinct from zero pixels — a
// step that mentions no width must not be read as asking for a window nought
// pixels wide.
type Extent struct {
	Unit  Unit
	Value int
}

// Percent builds a percentage extent.
func Percent(v int) Extent { return Extent{Unit: UnitPercent, Value: v} }

// Pixels builds a pixel extent.
func Pixels(v int) Extent { return Extent{Unit: UnitPixels, Value: v} }

// Set reports whether the extent says anything at all.
func (e Extent) Set() bool { return e.Unit != "" }

// String renders the extent the way it is written in configuration, so a
// round trip through the file is byte-stable.
func (e Extent) String() string {
	if !e.Set() {
		return ""
	}
	return strconv.Itoa(e.Value) + string(e.Unit)
}

// ParseExtent reads "66%", "1200px" or a bare "1200" (pixels, because that is
// what every existing routine's `size` meant). An empty string is the unset
// extent and not an error: absence is a legitimate answer.
func ParseExtent(s string) (Extent, error) {
	trimmed := strings.ToLower(strings.TrimSpace(s))
	if trimmed == "" {
		return Extent{}, nil
	}
	unit := UnitPixels
	digits := trimmed
	switch {
	case strings.HasSuffix(trimmed, "%"):
		unit, digits = UnitPercent, strings.TrimSpace(strings.TrimSuffix(trimmed, "%"))
	case strings.HasSuffix(trimmed, "px"):
		digits = strings.TrimSpace(strings.TrimSuffix(trimmed, "px"))
	}
	v, err := strconv.Atoi(digits)
	if err != nil {
		return Extent{}, fmt.Errorf("%q is not a size; write a percentage of the screen "+
			"(\"66%%\") or a number of pixels (\"1200px\")", s)
	}
	e := Extent{Unit: unit, Value: v}
	if problem := e.problem(); problem != "" {
		return Extent{}, fmt.Errorf("%s", problem)
	}
	return e, nil
}

// problem reports what is wrong with the extent on its own — before any
// monitor is known — or "" when it is structurally fine. Percentages over a
// hundred and impossible pixel counts are caught here so the form and the
// config loader both refuse them at the moment they are written, rather than
// at the moment the routine runs.
func (e Extent) problem() string {
	switch {
	case !e.Set():
		return ""
	case e.Value <= 0:
		return fmt.Sprintf("%s is not a size; a window has to be bigger than nothing", e)
	case e.Unit == UnitPercent && e.Value > 100:
		return fmt.Sprintf("%s is more than the whole screen; a share cannot exceed 100%%", e)
	case e.Unit == UnitPixels && e.Value > maxPixel:
		return fmt.Sprintf("%s is not a window size in pixels", e)
	}
	return ""
}

// Resolve turns the extent into pixels against one axis of the target
// monitor's usable area. available is the usable extent of that axis — the
// monitor's own, minus whatever the bars reserved — because that is what a
// person means by "two thirds of the screen": two thirds of the part they can
// actually put a window in.
//
// A pixel extent larger than the axis is an error rather than a clamp. The
// clamp would place a window and quietly lie about it; the error names the
// field and the number, which is what someone needs to fix it.
func (e Extent) Resolve(available int) (int, error) {
	if !e.Set() {
		return 0, nil
	}
	if problem := e.problem(); problem != "" {
		return 0, fmt.Errorf("%s", problem)
	}
	if e.Unit == UnitPercent {
		if available <= 0 {
			// A share of nothing is not a size. A percentage is the one form
			// that CANNOT be honoured without knowing the screen, which is
			// why it fails here while pixels below do not.
			return 0, fmt.Errorf("the target screen reports no usable area, so %s cannot be worked out", e)
		}
		// Integer arithmetic, rounding to nearest: 33% of 3440 is 1135.2, and
		// a window one pixel narrower than asked for is invisible while a
		// float in a dispatch is a rendering question nobody wants.
		return (e.Value*available + 50) / 100, nil
	}
	if available > 0 && e.Value > available {
		return 0, fmt.Errorf("%s is wider than the screen's usable %d pixels", e, available)
	}
	// An unknown screen leaves a pixel size unchecked rather than refused:
	// there is nothing to check it against, and refusing would make "1200 by
	// 800" depend on whether the compositor felt like listing its outputs.
	return e.Value, nil
}

// PlaceNext says where the *next* tiled window on this workspace goes,
// relative to this one. It is the vocabulary's arrangement primitive, and it
// is this shape rather than a grid because it is what tiling compositors
// actually offer: Hyprland's dwindle layout decides where a new window lands
// from the focused window and a one-shot preselection, so "these two share
// the remaining third, stacked" is expressed as "the window after me goes to
// my right" followed by "the window after me goes below me".
//
// The consequence is worth stating rather than hiding: step order is part of
// the meaning. Reordering steps reorders the layout, which is why the editor
// (#181) has to draw it.
type PlaceNext string

const (
	// PlaceNextNone leaves the next window wherever the layout would put it.
	PlaceNextNone PlaceNext = ""
	// PlaceNextRight puts the next window to the right of this one.
	PlaceNextRight PlaceNext = "right"
	// PlaceNextLeft puts the next window to the left of this one.
	PlaceNextLeft PlaceNext = "left"
	// PlaceNextBelow puts the next window below this one.
	PlaceNextBelow PlaceNext = "below"
	// PlaceNextAbove puts the next window above this one.
	PlaceNextAbove PlaceNext = "above"
)

// placeNextValues is the closed set, in the order a form offers them.
var placeNextValues = []PlaceNext{PlaceNextRight, PlaceNextLeft, PlaceNextBelow, PlaceNextAbove}

// PlaceNextValues returns the arrangement directions, in presentation order.
func PlaceNextValues() []string {
	names := make([]string, 0, len(placeNextValues))
	for _, v := range placeNextValues {
		names = append(names, string(v))
	}
	return names
}

// ParsePlaceNext reads an arrangement direction. Compass words are accepted
// because half the world says "down" and the other half says "below", and
// refusing one of them teaches nothing.
func ParsePlaceNext(s string) (PlaceNext, error) {
	trimmed := strings.ToLower(strings.TrimSpace(s))
	switch trimmed {
	case "":
		return PlaceNextNone, nil
	case "down", "under", "beneath":
		return PlaceNextBelow, nil
	case "up", "over":
		return PlaceNextAbove, nil
	}
	for _, v := range placeNextValues {
		if string(v) == trimmed {
			return v, nil
		}
	}
	return "", fmt.Errorf("%q is not a direction for the next window; use one of %s",
		s, strings.Join(PlaceNextValues(), ", "))
}

// Focus says whether the user's view follows the placed window.
type Focus string

const (
	// FocusSilent places the window without taking the user anywhere. It is
	// the default because a routine places several windows and following each
	// one in turn would drag the screen around six times.
	FocusSilent Focus = "silent"
	// FocusFollow focuses the window once it is placed — "and put me there".
	FocusFollow Focus = "follow"
)

// FocusValues returns the focus choices, in presentation order.
func FocusValues() []string { return []string{string(FocusSilent), string(FocusFollow)} }

// ParseFocus reads a focus choice; empty means the silent default.
func ParseFocus(s string) (Focus, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", nil
	case string(FocusSilent), "silently", "no":
		return FocusSilent, nil
	case string(FocusFollow), "yes":
		return FocusFollow, nil
	}
	return "", fmt.Errorf("%q is not a focus choice; use %s or %s", s, FocusSilent, FocusFollow)
}

// Workspace bounds. Hyprland numbers workspaces from 1; its negative ids are
// the special workspaces, which are summoned rather than placed (see
// UnsupportedModes) and so are not a target this vocabulary can name.
const (
	MinWorkspace = 1
	MaxWorkspace = 99
)

// Placement is the whole vocabulary for one window: what mode it sits in,
// what share of the screen it takes, where the next window goes, and which
// screen and workspace all of that happens on.
//
// It is a value, and deliberately flat. The routine schema stores it as TOML
// scalars, the tools accept it as JSON scalars, and the form renders one
// control per field — three surfaces that must agree, which they can only do
// if there is nothing nested to disagree about.
type Placement struct {
	// Mode is how the window sits. Empty means the step said nothing, and
	// every consumer leaves the compositor's own choice alone.
	Mode Mode
	// Width and Height are the proportion, in percent or pixels. Meaningful
	// in the modes whose ModeSpec.Sized is true.
	Width, Height Extent
	// X and Y are an explicit pixel position for a floating window;
	// HasPosition distinguishes "place at 0,0" from "no position directive".
	X, Y        int
	HasPosition bool
	// PlaceNext arranges what comes after this window. Tiled modes only.
	PlaceNext PlaceNext
	// Master promotes the window to the master pane. It is a separate flag
	// rather than a mode because it is a property OF being tiled, and because
	// only master-family layouts have such a pane — see MasterUnsupported.
	Master bool
	// Workspace is which workspace, 1 to 99. Zero means the step said
	// nothing; the routine schema requires one, the tools do not.
	Workspace int
	// Monitor is which screen, as a MonitorRef: a connector name, "current",
	// or (once #180 lands) a nickname. Empty leaves the workspace where it is.
	Monitor MonitorRef
	// Focus says whether the view follows. Empty means FocusSilent.
	Focus Focus
}

// MasterUnsupported is the sentence a run reports when a master promotion was
// asked for on a layout that has no master pane. It is here rather than in the
// runner because the docs, the ADR and the message must say the same thing.
const MasterUnsupported = "this workspace's layout has no master pane, so nothing was promoted"

// Sized reports whether the placement asks for a proportion.
func (p Placement) Sized() bool { return p.Width.Set() || p.Height.Set() }

// Tiles reports whether the placement puts the window in the tiling layout.
func (p Placement) Tiles() bool {
	spec, ok := p.Mode.Spec()
	return ok && spec.Tiles
}

// Problem is one thing wrong with a placement, keyed to the field a form
// would show it on. The field name is the configuration key and the tool
// argument name — one spelling, so a message written once lands on the right
// control in the window and reads correctly in a config-load error.
type Problem struct {
	Field   string
	Message string
}

// String renders the problem the way the config loader words every other one:
// the field first, so the daemon's field-keyed classifier (entry_admin.go)
// finds the control it belongs to by reading the leading token.
func (p Problem) String() string {
	if p.Field == "" {
		return p.Message
	}
	return p.Field + " " + p.Message
}

// Fields are the configuration keys and tool argument names this vocabulary
// owns. Declared as constants because three packages spell them and a typo in
// any one of them silently unkeys a form error.
const (
	FieldMode      = "mode"
	FieldWidth     = "width"
	FieldHeight    = "height"
	FieldPosition  = "position"
	FieldPlaceNext = "place_next"
	FieldMaster    = "master"
	FieldWorkspace = "workspace"
	FieldMonitor   = "monitor"
	FieldFocus     = "focus"
)

// Fields returns every key the vocabulary contributes, in the order a form
// presents them. The routine schema's step keys are this list plus the
// launching half's, and a contract test pins that.
func Fields() []string {
	return []string{
		FieldWorkspace, FieldMonitor, FieldMode, FieldWidth, FieldHeight,
		FieldPosition, FieldPlaceNext, FieldMaster, FieldFocus,
	}
}

// Problems reports everything structurally wrong with the placement, one
// actionable message per problem. It is the whole of the validation: the
// config loader, the form's validate call and the tools all run this, so a
// value that is refused in one surface is refused identically in the others.
//
// requireWorkspace distinguishes the two consumers. A routine step must name
// a workspace — it is describing a desktop, and "wherever" is not a
// description — while a tool call placing the window the user is looking at
// legitimately leaves it where it is.
//
// Monitor and percentage validation against a *real* monitor happens later,
// in ResolveSize, because it needs an inventory this pure function has no way
// to obtain. What is checked here is everything that can be known from the
// value alone, which is what makes it a load-time and form-time answer rather
// than an eight-second silence at run time.
func (p Placement) Problems(requireWorkspace bool) []Problem {
	var problems []Problem
	spec, known := p.Mode.Spec()
	if p.Mode != "" && !known {
		// Unreachable through ParseMode, which refuses an unknown value; it
		// is here because a Placement can also be built in Go, and a mode
		// nobody can honour must be a sentence rather than a silent no-op.
		problems = append(problems, Problem{FieldMode, fmt.Sprintf(
			"%q is not a placement mode; use one of %s", p.Mode, strings.Join(ModeNames(), ", "))})
	}
	for _, axis := range []struct {
		field string
		e     Extent
	}{{FieldWidth, p.Width}, {FieldHeight, p.Height}} {
		if problem := axis.e.problem(); problem != "" {
			problems = append(problems, Problem{axis.field, problem})
			continue
		}
		if !axis.e.Set() {
			continue
		}
		if p.Mode == "" {
			problems = append(problems, Problem{axis.field, fmt.Sprintf(
				"%s needs a mode; say mode = %q for a share of the layout, or %q to float",
				axis.field, ModeTiled, ModeFloating)})
			continue
		}
		if known && !spec.Sized {
			problems = append(problems, Problem{axis.field, fmt.Sprintf(
				"%s means nothing in %s mode, which always fills the screen", axis.field, p.Mode)})
		}
	}
	if p.HasPosition {
		switch {
		case p.X < -maxPixel || p.X > maxPixel || p.Y < -maxPixel || p.Y > maxPixel:
			problems = append(problems, Problem{FieldPosition, fmt.Sprintf(
				"position %d,%d is not on any plausible monitor", p.X, p.Y)})
		case p.Mode == "":
			problems = append(problems, Problem{FieldPosition, fmt.Sprintf(
				"position needs a mode; say mode = %q to place a window by pixels", ModeFloating)})
		case known && !spec.Positioned:
			problems = append(problems, Problem{FieldPosition, fmt.Sprintf(
				"position means nothing in %s mode; the layout owns where a tiled window sits", p.Mode)})
		}
	}
	if p.PlaceNext != PlaceNextNone {
		if _, err := ParsePlaceNext(string(p.PlaceNext)); err != nil {
			problems = append(problems, Problem{FieldPlaceNext, err.Error()})
		} else if p.Mode != "" && known && !spec.Tiles {
			problems = append(problems, Problem{FieldPlaceNext, fmt.Sprintf(
				"place_next arranges tiled windows, and this one is %s", p.Mode)})
		}
	}
	if p.Master && p.Mode != "" && known && !spec.Tiles {
		problems = append(problems, Problem{FieldMaster, fmt.Sprintf(
			"master promotes a tiled window into the layout's big pane, and this one is %s", p.Mode)})
	}
	switch {
	case p.Workspace == 0 && requireWorkspace:
		problems = append(problems, Problem{FieldWorkspace, fmt.Sprintf(
			"workspace %d does not exist; workspaces are numbered %d to %d",
			p.Workspace, MinWorkspace, MaxWorkspace)})
	case p.Workspace != 0 && (p.Workspace < MinWorkspace || p.Workspace > MaxWorkspace):
		problems = append(problems, Problem{FieldWorkspace, fmt.Sprintf(
			"workspace %d does not exist; workspaces are numbered %d to %d",
			p.Workspace, MinWorkspace, MaxWorkspace)})
	}
	if problem := p.Monitor.problem(); problem != "" {
		problems = append(problems, Problem{FieldMonitor, problem})
	}
	if p.Focus != "" {
		if _, err := ParseFocus(string(p.Focus)); err != nil {
			problems = append(problems, Problem{FieldFocus, err.Error()})
		}
	}
	return problems
}

// Size is a placement's proportion resolved into pixels against a real
// monitor: what the compositor is actually told.
type Size struct {
	// Width and Height are pixels. Zero means the axis was not specified, in
	// which case the compositor keeps whatever it already had for it.
	Width, Height int
}

// Set reports whether either axis resolved to anything.
func (s Size) Set() bool { return s.Width > 0 || s.Height > 0 }

// ResolveSize turns the placement's proportion into pixels against a
// monitor's usable area, naming the field when it cannot.
//
// This is the second half of the "percentages and pixels are both accepted,
// validated against the target monitor's usable area" requirement: Problems
// catches everything knowable from the value, and this catches the rest —
// 1200px asked for on a 1024-wide screen — at the moment a monitor is known,
// which for the form is when it renders the preview and for a run is before
// any window has moved.
func (p Placement) ResolveSize(m Monitor) (Size, []Problem) {
	usable := m.Usable()
	var size Size
	var problems []Problem
	if p.Width.Set() {
		v, err := p.Width.Resolve(usable.Width)
		if err != nil {
			problems = append(problems, Problem{FieldWidth, err.Error()})
		}
		size.Width = v
	}
	if p.Height.Set() {
		v, err := p.Height.Resolve(usable.Height)
		if err != nil {
			problems = append(problems, Problem{FieldHeight, err.Error()})
		}
		size.Height = v
	}
	// An axis the placement did not mention keeps the window's current
	// extent, and the compositor's resize verb wants both numbers. The caller
	// supplies the missing one from the live window; here it stays zero so
	// "not said" survives all the way out of this package.
	return size, problems
}
