package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rpickz/jarvix/internal/placement"
)

// This file is the compositor seam (ADR 0022): the interface through which
// Jarvix reads the window inventory and acts on it, plus the one shipped
// implementation, Hyprland.
//
// Three properties shape it, and each is a requirement rather than a nicety:
//
//   - The inventory is the only source of window identity. Every dispatch
//     names a window by the address the inventory reported; nothing the model
//     says ever reaches argv. That is what makes "close Firefox" safe — the
//     model chooses *which entry*, never *what to run*.
//   - Unavailability is an ordinary outcome. No compositor, no hyprctl, no
//     Wayland session: Windows returns an error, the tool speaks one sentence
//     about it, and the daemon is otherwise untouched (ADR 0002's trade
//     again).
//   - It is an interface because a second compositor is a plausible future,
//     not because tests need one — though they need one too: no test in this
//     tree may require a running Hyprland.

// Window is one window as the compositor sees it. Everything the matcher,
// the spoken summaries, and layout capture (#62) need, and nothing else:
// monitors, groups, and the dozens of other fields hyprctl reports are
// deliberately not modelled.
type Window struct {
	// Address is the compositor's stable handle for this window and the only
	// thing ever dispatched against. Opaque: it is never spoken, and never
	// built by anything but the compositor.
	Address string
	// Class is the application identity ("firefox", "md.obsidian.Obsidian").
	Class string
	// Title is the window title, which changes as the user works.
	Title string
	// Workspace is the numeric workspace id. Negative for Hyprland's special
	// workspaces, which is why WorkspaceName exists alongside it.
	Workspace int
	// WorkspaceName is the workspace's display name ("3", "special:magic").
	WorkspaceName string
	// Floating marks a window outside the tiling layout.
	Floating bool
	// AcceptsInput is false for windows that cannot be typed into. Reported
	// so #37 (typing) can refuse them without a second inventory shape.
	AcceptsInput bool
	// Focused marks the window the user is currently in — the one "this" and
	// "the current window" resolve to.
	Focused bool
	// StableID is the compositor's own per-window identifier, used together
	// with Address to prove a resolved window is still *that* window before
	// a state-changing dispatch. Empty on compositors that do not report one.
	StableID string
	// PID is the owning process, logged for the audit trail.
	PID int
	// X and Y are the window's top-left corner in global pixels, and Width
	// and Height its size. They exist for layout capture (#62), which must
	// record a floating window's geometry to reproduce it, and for the window
	// overlays (#127), which pin a chip to the top-right corner; tiled
	// geometry belongs to the layout and is carried for those readers. All
	// four are zero on a compositor that does not report geometry.
	X, Y          int
	Width, Height int
	// Fullscreen marks a window covering its whole output — or maximised
	// over the tiling area, which covers its siblings just as completely.
	// Read for the window overlays (#127): an overlay must never float over
	// a window that is itself covering the window it annotates, so a
	// workspace with a fullscreen window gets no overlays at all.
	Fullscreen bool
}

// Describe renders a window for humans: "Firefox — GitHub". The em dash is
// the same shape the desktop-context gatherer uses, so one window reads the
// same wherever it appears.
func (w Window) Describe() string {
	app, title := AppName(w.Class), strings.TrimSpace(w.Title)
	switch {
	case app != "" && title != "" && !strings.EqualFold(app, title):
		return app + " — " + title
	case app != "":
		// A title that only repeats the application's name adds nothing but
		// length to a sentence someone has to listen to.
		return app
	default:
		return title
	}
}

// AppName turns a window class into something worth saying aloud. Classes are
// developer-facing identifiers, and reverse-DNS ones ("md.obsidian.Obsidian",
// "org.gnome.Nautilus") read as nonsense in speech, so their last segment
// wins.
//
// The test for "reverse-DNS" is strict — three or more segments, every one of
// them letters and digits only — because the interesting failure is a class
// that merely contains dots. A browser profile's class
// ("chrome-web.whatsapp.com__-Default") would otherwise be spoken as
// "com__-Default", which is worse than the honest, ugly whole: a wrong guess
// here is a wrong name in the user's ear.
func AppName(class string) string {
	class = strings.TrimSpace(class)
	parts := strings.Split(class, ".")
	if len(parts) < 3 {
		return class
	}
	for _, part := range parts {
		if part == "" || !isAlphanumeric(part) {
			return class
		}
	}
	return parts[len(parts)-1]
}

func isAlphanumeric(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// Compositor is the window-manager seam. Implementations are short-lived
// subprocesses in production and fakes in tests; every method must honour ctx,
// because the tool timeouts are enforced through it.
//
// The action methods take an address rather than a Window on purpose: the
// caller resolved a window at some earlier moment, and the address is the
// entirety of what it is allowed to carry forward. There is no way to ask this
// interface to "close whatever matches firefox".
type Compositor interface {
	// Describe names the compositor and how it is being driven, for
	// `jarvix doctor`. The error is what "unavailable" means here: no
	// compositor, no binary, or one that would not answer.
	Describe(ctx context.Context) (string, error)
	// Windows returns the current inventory, most-recently-focused first.
	Windows(ctx context.Context) ([]Window, error)
	// Focus raises and focuses the window at address.
	Focus(ctx context.Context, address string) error
	// MoveToWorkspace sends the window at address to a numbered workspace
	// without following it there.
	MoveToWorkspace(ctx context.Context, address string, workspace int) error
	// Close asks the window at address to close, as its own close button
	// would — the application may still refuse or prompt.
	Close(ctx context.Context, address string) error
	// SwitchWorkspace takes the *user* to a numbered workspace. It is the
	// mirror image of MoveToWorkspace: that one sends a window away and
	// leaves the view alone, this one moves the view and leaves the windows
	// alone. Both exist because "move this to three" and "go to three" are
	// different requests.
	SwitchWorkspace(ctx context.Context, workspace int) error
	// Spawn starts a program as a child of the compositor, so it lands on the
	// active workspace with the graphical session's environment and outlives
	// the daemon that asked for it. program must be a single bare executable
	// name or absolute path — see spawnPattern for why that is a rule and not
	// a convention.
	Spawn(ctx context.Context, program string) error
	// SetFloating puts the window at address into (true) or back out of
	// (false) the floating layer. It is a set, not a toggle, on purpose:
	// routines re-run (ADR 0026), and a toggle applied twice undoes itself
	// while a set applied twice converges.
	SetFloating(ctx context.Context, address string, floating bool) error
	// ResizeWindow sets a window's size in pixels, exactly.
	//
	// On a floating window that is the window's own size. On a TILED window
	// it is how a proportion is asked for: the compositor moves the split the
	// window sits in, so "this one takes two thirds" is an exact resize of a
	// tiled window and not a float (ADR 0056). Callers therefore no longer
	// float first — floating first would produce a window hovering over the
	// layout, which is a different arrangement entirely.
	ResizeWindow(ctx context.Context, address string, width, height int) error
	// PositionWindow moves a floating window's top-left corner to x,y in
	// pixels. Coordinates may be negative: on a multi-monitor layout the
	// global origin is wherever the user arranged it to be.
	PositionWindow(ctx context.Context, address string, x, y int) error
	// SetPinned pins (true) or unpins (false) the window at address, so it
	// stays above everything on every workspace. Hyprland honours pinning
	// only for floating windows, so callers float first; the pair is one mode
	// in the vocabulary (placement.ModePinned) rather than two knobs.
	SetPinned(ctx context.Context, address string, pinned bool) error
	// SetFullscreen puts the window at address into (true) or out of (false)
	// a fullscreen mode: covering the whole output, or maximised over the
	// workspace's usable area. A set with an explicit mode, never a toggle,
	// for the reason SetFloating is.
	SetFullscreen(ctx context.Context, address string, mode FullscreenMode, on bool) error
	// Preselect tells the tiling layout where the NEXT window mapped on the
	// focused workspace should go, relative to the focused window. It is how
	// tiled arrangement is expressed on a dwindle-family layout, which
	// decides a new window's place when the window maps and never afterwards
	// — so it is dispatched between launches, not after them.
	//
	// It acts on whatever holds focus, because that is the only thing the
	// compositor's layout message takes; the caller focuses the window it
	// means first. Layouts with no such message report so (see
	// Hyprland.Preselect), which is how "your layout cannot do this" reaches
	// the user instead of a step counted as placed.
	Preselect(ctx context.Context, direction PreselectDirection) error
	// PromoteMaster makes the window at address its workspace's master
	// window — the big pane of a master/stack layout. Implementations may
	// need to focus the window to do it (see Hyprland.PromoteMaster), so
	// callers should expect the user's view to follow. On a layout with no
	// master pane it reports that rather than pretending.
	PromoteMaster(ctx context.Context, address string) error
	// MoveToMonitor sends one window to a named output without following it
	// there. It is what "put this on the other screen" means for a single
	// window the user is looking at; a routine placing a layout moves the
	// whole workspace instead (MoveWorkspaceToMonitor), because the windows
	// of one workspace belong together.
	MoveToMonitor(ctx context.Context, address string, monitor string) error
	// MoveWorkspaceToMonitor puts a whole workspace on a named output. This
	// is what "put this on my top monitor" means for a routine step: the
	// windows of one workspace belong together, and moving them individually
	// would scatter a layout across two screens. Naming an output that is not
	// plugged in is an error, not a silent no-op.
	MoveWorkspaceToMonitor(ctx context.Context, workspace int, monitor string) error
	// Monitors returns the outputs the compositor is driving, with the
	// geometry a percentage resolves against.
	Monitors(ctx context.Context) ([]placement.Monitor, error)
	// LayoutName is the tiling layout in force ("dwindle", "master"), for the
	// two directives whose availability depends on it: preselection is a
	// dwindle-family message and master promotion a master-family one. Empty
	// with no error means the compositor would not say.
	LayoutName(ctx context.Context) (string, error)
}

// FullscreenMode is which of the compositor's two covering states is meant.
type FullscreenMode int

const (
	// FullscreenWhole covers the entire output, bars included.
	FullscreenWhole FullscreenMode = iota
	// FullscreenMaximised covers the workspace's usable area, leaving the
	// bars on screen. Hyprland spells it "maximized".
	FullscreenMaximised
)

// PreselectDirection is where the next tiled window goes relative to the
// focused one. The values are the vocabulary's (placement.PlaceNext),
// translated once here into the compositor's single-letter spelling.
type PreselectDirection string

const (
	PreselectRight PreselectDirection = "r"
	PreselectLeft  PreselectDirection = "l"
	PreselectDown  PreselectDirection = "d"
	PreselectUp    PreselectDirection = "u"
)

// Compositor call bounds. A window action is a local IPC round trip: if it has
// not answered in a second, something is wrong and the user is owed a sentence
// rather than a longer silence.
const (
	// DefaultCompositorTimeout bounds one hyprctl call.
	DefaultCompositorTimeout = time.Second
	// maxDispatchOutput caps a dispatch's captured output, which is
	// diagnostics and nothing else — the compositor answers "ok" or explains
	// itself in a line.
	maxDispatchOutput = 8 * 1024
	// maxInventoryOutput caps the window inventory. Hyprland reports roughly
	// a kilobyte per window, so this holds a desktop nobody has; the cap is
	// there because a truncated JSON document parses as no compositor at all,
	// and that failure must be impossible rather than merely unlikely.
	maxInventoryOutput = 512 * 1024
	// minWorkspace and maxWorkspace bound a workspace number before it may be
	// rendered into a dispatch. Hyprland numbers workspaces from 1; the
	// negative ids belong to special workspaces, which are named rather than
	// numbered and are not what anyone means by "workspace four".
	minWorkspace = 1
	maxWorkspace = 99
)

// Hyprland drives the Hyprland compositor through hyprctl, the same
// short-lived-subprocess trade the context gatherers make (ADR 0002): no IPC
// library, no protocol version to track, and a missing binary degrades to a
// sentence instead of a broken daemon.
type Hyprland struct {
	// Binary overrides the hyprctl executable (tests, unusual installs).
	// Empty means "hyprctl" from PATH.
	Binary string
	// Timeout bounds one hyprctl call. Zero means DefaultCompositorTimeout.
	Timeout time.Duration

	// mu guards the discovered dispatch dialect, which is probed once and
	// reused. Compositor methods are called from session goroutines.
	mu      sync.Mutex
	dialect dispatchDialect
}

// dispatchDialect is how this Hyprland wants dispatches written.
//
// Hyprland 0.55 moved configuration to Lua, and with it `hyprctl dispatch`:
// the argument is now evaluated as a Lua expression, so the syntax every
// script on the internet uses — `hyprctl dispatch focuswindow address:0x…` —
// is a parse error on a Lua-configured compositor, while the Lua form is an
// unknown dispatcher on an hyprlang-configured one. Neither is a version
// question alone: a 0.56 user still running hyprlang keeps the legacy syntax.
//
// So the dialect is *discovered*, never assumed, with a dispatch that does
// nothing at all (`hl.dsp.no_op()`). The probe is safe on both: on a legacy
// compositor it is an unrecognised dispatcher, which changes nothing.
type dispatchDialect int

const (
	dialectUnknown dispatchDialect = iota
	// dialectLua is Hyprland ≥ 0.55 with a Lua configuration.
	dialectLua
	// dialectLegacy is the pre-Lua `dispatch <dispatcher> <args>` syntax.
	dialectLegacy
)

// Describe implements Compositor.
func (h *Hyprland) Describe(ctx context.Context) (string, error) {
	out, err := h.run(ctx, versionArgs()...)
	if err != nil {
		return "", err
	}
	var v struct {
		Version string `json:"version"`
		Tag     string `json:"tag"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", fmt.Errorf("hyprctl version: %w", err)
	}
	name := strings.TrimSpace(v.Version)
	if name == "" {
		name = strings.TrimSpace(v.Tag)
	}
	described := strings.TrimSpace("Hyprland " + name + " (" + h.probeDialect(ctx).String() + " dispatch")
	// The tiling layout belongs in the diagnostic because two of the
	// placement vocabulary's directives depend on it (ADR 0056):
	// `place_next` is a dwindle-family message and `master` a master-family
	// one, so "my routine says the layout cannot arrange windows that way" is
	// answered by this line rather than by reading someone's hyprland.conf.
	// Best-effort — a compositor that will not answer still gets described.
	if layout, err := h.LayoutName(ctx); err == nil && layout != "" {
		described += ", " + layout + " layout"
	}
	return described + ")", nil
}

// String names a dialect for logs and doctor output.
func (d dispatchDialect) String() string {
	switch d {
	case dialectLua:
		return "lua"
	case dialectLegacy:
		return "legacy"
	default:
		return "unknown"
	}
}

// Windows implements Compositor.
func (h *Hyprland) Windows(ctx context.Context) ([]Window, error) {
	out, err := h.runCapped(ctx, maxInventoryOutput, clientsArgs()...)
	if err != nil {
		return nil, err
	}
	return parseClients(out)
}

// Focus implements Compositor.
func (h *Hyprland) Focus(ctx context.Context, address string) error {
	return h.dispatch(ctx, focusArgs, address, 0)
}

// Close implements Compositor.
func (h *Hyprland) Close(ctx context.Context, address string) error {
	return h.dispatch(ctx, closeArgs, address, 0)
}

// MoveToWorkspace implements Compositor.
func (h *Hyprland) MoveToWorkspace(ctx context.Context, address string, workspace int) error {
	return h.dispatch(ctx, moveArgs, address, workspace)
}

// SwitchWorkspace implements Compositor.
func (h *Hyprland) SwitchWorkspace(ctx context.Context, workspace int) error {
	if workspace < minWorkspace || workspace > maxWorkspace {
		return fmt.Errorf("workspace %d does not exist; workspaces are numbered %d to %d",
			workspace, minWorkspace, maxWorkspace)
	}
	return h.dispatchProbed(ctx, func(d dispatchDialect) []string {
		return workspaceArgs(d, workspace)
	})
}

// Spawn implements Compositor.
func (h *Hyprland) Spawn(ctx context.Context, program string) error {
	if !spawnPattern.MatchString(program) {
		return fmt.Errorf("refusing to start %q: it is not a program name", program)
	}
	return h.dispatchProbed(ctx, func(d dispatchDialect) []string {
		return spawnArgs(d, program)
	})
}

// maxPixel bounds any pixel value before it may be rendered into a dispatch.
// It is defensive rather than load-bearing — every caller's value came from
// validated configuration and is rendered with strconv, never interpolated as
// syntax — but a bound means a corrupted value fails as a sentence instead of
// as a window teleported somewhere no monitor will ever be.
const maxPixel = 32768

// SetFloating implements Compositor.
func (h *Hyprland) SetFloating(ctx context.Context, address string, floating bool) error {
	if !addressPattern.MatchString(address) {
		return fmt.Errorf("refusing to dispatch to malformed window address %q", address)
	}
	return h.dispatchProbed(ctx, func(d dispatchDialect) []string {
		return floatArgs(d, address, floating)
	})
}

// ResizeWindow implements Compositor.
func (h *Hyprland) ResizeWindow(ctx context.Context, address string, width, height int) error {
	if !addressPattern.MatchString(address) {
		return fmt.Errorf("refusing to dispatch to malformed window address %q", address)
	}
	if width <= 0 || height <= 0 || width > maxPixel || height > maxPixel {
		return fmt.Errorf("refusing to resize a window to %d by %d pixels", width, height)
	}
	return h.dispatchProbed(ctx, func(d dispatchDialect) []string {
		return resizeArgs(d, address, width, height)
	})
}

// PositionWindow implements Compositor.
func (h *Hyprland) PositionWindow(ctx context.Context, address string, x, y int) error {
	if !addressPattern.MatchString(address) {
		return fmt.Errorf("refusing to dispatch to malformed window address %q", address)
	}
	if x < -maxPixel || x > maxPixel || y < -maxPixel || y > maxPixel {
		return fmt.Errorf("refusing to move a window to %d,%d", x, y)
	}
	return h.dispatchProbed(ctx, func(d dispatchDialect) []string {
		return positionArgs(d, address, x, y)
	})
}

// SetPinned implements Compositor.
func (h *Hyprland) SetPinned(ctx context.Context, address string, pinned bool) error {
	if !addressPattern.MatchString(address) {
		return fmt.Errorf("refusing to dispatch to malformed window address %q", address)
	}
	return h.dispatchProbed(ctx, func(d dispatchDialect) []string {
		return pinArgs(d, address, pinned)
	})
}

// SetFullscreen implements Compositor.
func (h *Hyprland) SetFullscreen(ctx context.Context, address string, mode FullscreenMode, on bool) error {
	if !addressPattern.MatchString(address) {
		return fmt.Errorf("refusing to dispatch to malformed window address %q", address)
	}
	return h.dispatchProbed(ctx, func(d dispatchDialect) []string {
		return fullscreenArgs(d, address, mode, on)
	})
}

// Preselect implements Compositor.
//
// The refusal this turns into a sentence is the whole reason the method
// exists: `preselect` is a dwindle layout message, and on a master-family
// layout the compositor answers "Unknown master layoutmsg: preselect" with
// exit status zero. Left alone that is a step reported as placed whose
// arrangement never happened — the #177 shape — so the seam rewrites it into
// something a run can report.
func (h *Hyprland) Preselect(ctx context.Context, direction PreselectDirection) error {
	switch direction {
	case PreselectRight, PreselectLeft, PreselectDown, PreselectUp:
	default:
		return fmt.Errorf("refusing to preselect in unknown direction %q", direction)
	}
	err := h.dispatchProbed(ctx, func(d dispatchDialect) []string {
		return preselectArgs(d, direction)
	})
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "layoutmsg") {
		return fmt.Errorf("this workspace's layout cannot arrange windows that way "+
			"(preselection is a dwindle-family feature): %w", err)
	}
	return err
}

// PromoteMaster implements Compositor.
//
// The window is focused first, and that is a workaround stated openly rather
// than hidden: the layout message carries no window selector on either
// dialect — it acts on whatever has focus — so naming the window means
// becoming it for a moment. `hl.dsp.layout` is a *function* taking a plain
// string (probed; ADR 0056), not a table of named messages, which is why the
// Lua form has no selector to offer either.
//
// A layout with no master pane answers "Unknown dwindle layoutmsg", and that
// is rewritten into the sentence the vocabulary promises rather than being
// reported as a placement that happened.
func (h *Hyprland) PromoteMaster(ctx context.Context, address string) error {
	if err := h.dispatch(ctx, focusArgs, address, 0); err != nil {
		return err
	}
	err := h.dispatchProbed(ctx, func(d dispatchDialect) []string {
		return masterArgs(d, address)
	})
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "layoutmsg") {
		return fmt.Errorf("%s: %w", placement.MasterUnsupported, err)
	}
	return err
}

// MoveToMonitor implements Compositor.
func (h *Hyprland) MoveToMonitor(ctx context.Context, address string, monitor string) error {
	if !addressPattern.MatchString(address) {
		return fmt.Errorf("refusing to dispatch to malformed window address %q", address)
	}
	if !monitorPattern.MatchString(monitor) {
		return fmt.Errorf("refusing to dispatch to malformed monitor name %q", monitor)
	}
	return h.dispatchProbed(ctx, func(d dispatchDialect) []string {
		return windowMonitorArgs(d, address, monitor)
	})
}

// MoveWorkspaceToMonitor implements Compositor.
func (h *Hyprland) MoveWorkspaceToMonitor(ctx context.Context, workspace int, monitor string) error {
	if workspace < minWorkspace || workspace > maxWorkspace {
		return fmt.Errorf("workspace %d does not exist; workspaces are numbered %d to %d",
			workspace, minWorkspace, maxWorkspace)
	}
	if !monitorPattern.MatchString(monitor) {
		return fmt.Errorf("refusing to dispatch to malformed monitor name %q", monitor)
	}
	return h.dispatchProbed(ctx, func(d dispatchDialect) []string {
		return workspaceMonitorArgs(d, workspace, monitor)
	})
}

// Monitors implements Compositor.
func (h *Hyprland) Monitors(ctx context.Context) ([]placement.Monitor, error) {
	out, err := h.runCapped(ctx, maxInventoryOutput, monitorsArgs()...)
	if err != nil {
		return nil, err
	}
	return parseMonitors(out)
}

// LayoutName implements Compositor.
//
// `hyprctl getoption` is a read, not a dispatch, so it is the same shape as
// the inventory: no dialect question, no state changed, and a compositor that
// will not answer degrades to "" rather than to an error the caller has to
// decide what to do with.
func (h *Hyprland) LayoutName(ctx context.Context) (string, error) {
	out, err := h.run(ctx, layoutOptionArgs()...)
	if err != nil {
		return "", err
	}
	var v struct {
		Str string `json:"str"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil {
		return "", fmt.Errorf("hyprctl getoption: %w", err)
	}
	return strings.TrimSpace(v.Str), nil
}

// clientsArgs builds the inventory invocation. JSON rather than the human
// format, for the same reason the context gatherer chooses it: `-j` is the
// documented machine interface, the human one is a display surface.
func clientsArgs() []string { return []string{"clients", "-j"} }

// versionArgs asks which Hyprland this is, for `jarvix doctor`.
func versionArgs() []string { return []string{"version", "-j"} }

// probeArgs discovers the dispatch dialect by dispatching a dispatcher that
// does nothing, so finding out how to talk to the compositor can never move a
// window. On a legacy compositor it is an unrecognised dispatcher, which is
// equally harmless — and is itself the answer.
func probeArgs() []string { return []string{"dispatch", "hl.dsp.no_op()"} }

// addressPattern is what an address must look like before it may become an
// argument. The inventory is the only producer of addresses, so this can only
// fail if the compositor itself returned something unexpected — which is
// exactly when a defensive check is worth having, because the value is about
// to be handed to a program that acts on windows.
var addressPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{1,32}$`)

// spawnPattern is what a program name must look like before Spawn will send
// it. This one is load-bearing rather than defensive: the Lua dialect spells a
// spawn as `hl.dsp.exec_cmd("…")`, so the name is interpolated into a Lua
// string literal, and the compositor runs that string through a shell. A
// quote, a backslash, a space or a semicolon would therefore be *syntax* at
// two levels rather than a program that does not exist. Bounding the value to
// one bare executable name or absolute path removes both, and it costs
// nothing: every caller's value is a configured setting, never a spoken or
// model-chosen string.
var spawnPattern = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)

// monitorPattern is what a monitor name must look like before it may be
// interpolated into a dispatch. Load-bearing for the same reason spawnPattern
// is: the Lua dialect wraps it in a string literal, so a quote or a backslash
// would be syntax rather than a name that does not resolve. Connector names
// are letters, digits, dashes and underscores on every driver there is, and a
// monitor nickname (#180) is bounded to the same set by the vocabulary.
var monitorPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// dispatchArgs builds one dispatch invocation in the given dialect. Split out
// per verb, and pure, so the argv guarantees are asserted in table tests
// without a compositor anywhere.
type dispatchArgs func(dialect dispatchDialect, address string, workspace int) []string

// focusArgs, closeArgs and moveArgs are the three shipped verbs, in both
// dialects. The address is always one whole argv element and always in
// selector form ("address:0x…"): it is a value the compositor looks up, never
// syntax it parses.
func focusArgs(d dispatchDialect, address string, _ int) []string {
	if d == dialectLegacy {
		return []string{"dispatch", "focuswindow", "address:" + address}
	}
	return []string{"dispatch", `hl.dsp.focus({ window = "address:` + address + `" })`}
}

func closeArgs(d dispatchDialect, address string, _ int) []string {
	if d == dialectLegacy {
		return []string{"dispatch", "closewindow", "address:" + address}
	}
	return []string{"dispatch", `hl.dsp.window.close({ window = "address:` + address + `" })`}
}

// moveArgs sends a window away without following it. "Move this to workspace
// three" is a tidying gesture: the user is talking about a window, not asking
// to be taken somewhere, and having the screen change under them would be the
// surprise. Legacy spells that `movetoworkspacesilent`; Lua spells it
// `follow = false`.
func moveArgs(d dispatchDialect, address string, workspace int) []string {
	ws := strconv.Itoa(workspace)
	if d == dialectLegacy {
		return []string{"dispatch", "movetoworkspacesilent", ws + ",address:" + address}
	}
	return []string{"dispatch",
		`hl.dsp.window.move({ workspace = ` + ws + `, window = "address:` + address + `", follow = false })`}
}

// workspaceArgs moves the user to a workspace. Legacy spells it with the
// bare `workspace` dispatcher — the form every script and every keybinding on
// the internet uses, and a Lua parse error on a Lua-configured compositor,
// which is the whole of issue #47. Lua spells the same thing as focusing a
// workspace rather than a window: `hl.dsp.focus` takes whichever of `window`
// or `workspace` it is given.
func workspaceArgs(d dispatchDialect, workspace int) []string {
	ws := strconv.Itoa(workspace)
	if d == dialectLegacy {
		return []string{"dispatch", "workspace", ws}
	}
	return []string{"dispatch", `hl.dsp.focus({ workspace = ` + ws + ` })`}
}

// spawnArgs starts a program.
//
// ADR 0022 declined `hl.dsp.exec_cmd` for the *launch tool*, and that stands:
// there the program name comes from the model, and handing a model-chosen
// string to a shell is the thing the whole tool family exists to prevent, so
// the tool starts a detached child directly instead. Here the name comes from
// the `[intents] terminal` setting, validated as one bare token when the
// router compiles and again by spawnPattern before it is rendered — a shell
// with nothing to chew on. What it buys is the reason "open a terminal" used
// this route in the first place: the terminal is a child of the compositor,
// so it lands on the active workspace with the graphical session's
// environment and survives a daemon restart. Starting it from jarvixd would
// regress all three, most sharply for a daemon started by systemd outside the
// session.
func spawnArgs(d dispatchDialect, program string) []string {
	if d == dialectLegacy {
		return []string{"dispatch", "exec", program}
	}
	return []string{"dispatch", `hl.dsp.exec_cmd("` + program + `")`}
}

// floatArgs sets a window's floating state. Legacy spells the two directions
// as separate dispatchers (`setfloating` / `settiled`) — deliberately not
// `togglefloating`, whose second application undoes the first. Lua takes the
// direction as an `action`, and the two spellings that mean "set" are
// `enable` and `disable`.
//
// The spelling was probed, and the probe is the reason this comment is long.
// `hl.dsp.window.set_floating`, which this code sent until issue #177, does
// not exist at all: Lua answers "attempt to call a nil value (field
// 'set_floating')" and hyprctl reports the failure, so every routine that
// asked to float a window has been failing. Worse, the replacement validates
// nothing — `action = "nonsense"` is accepted and silently falls back to
// TOGGLE (Hyprland's Internal::parseToggleStr), so a typo here would not
// error, it would oscillate on every re-run. That is why the two action words
// are constants in the argv and not something a caller can influence.
func floatArgs(d dispatchDialect, address string, floating bool) []string {
	if d == dialectLegacy {
		verb := "settiled"
		if floating {
			verb = "setfloating"
		}
		return []string{"dispatch", verb, "address:" + address}
	}
	action := "disable"
	if floating {
		action = "enable"
	}
	return []string{"dispatch",
		`hl.dsp.window.float({ window = "address:` + address + `", action = "` + action + `" })`}
}

// resizeArgs sets a window's size exactly.
//
// The Lua verb takes **`x` and `y` as the target size**, not `width` and
// `height` — the whole of issue #177. Probed with a deliberately bogus
// address, which is rejected on argument shape before the window is looked
// up, so the reply distinguishes a wrong shape from a missing window:
//
//	$ hyprctl dispatch 'hl.dsp.window.resize({ window = "address:0xdeadbeef", width = 100, height = 100 })'
//	error: hl.window.resize: unrecognized arguments. Expected positions (x & y) or keep_aspect_ratio
//	$ hyprctl dispatch 'hl.dsp.window.resize({ window = "address:0xdeadbeef", x = 100, y = 100 })'
//	ok
//
// `exact = true`, which this code used to send, is not a key the verb reads
// at all; exactness is the *absence* of `relative`, which defaults to false
// (Hyprland's hlWindowResize). It is written explicitly because a default
// that silently changed would turn every routine into a window that grows by
// its own size on each run, and routines re-run.
func resizeArgs(d dispatchDialect, address string, width, height int) []string {
	w, h := strconv.Itoa(width), strconv.Itoa(height)
	if d == dialectLegacy {
		return []string{"dispatch", "resizewindowpixel", "exact " + w + " " + h + ",address:" + address}
	}
	return []string{"dispatch",
		`hl.dsp.window.resize({ window = "address:` + address + `", x = ` + w + `, y = ` + h + `, relative = false })`}
}

// positionArgs moves a window's top-left corner, exactly.
//
// There is no `hl.dsp.window.position` — probing it answers "attempt to call
// a nil value (field 'position')", so this was the second verb #177's defect
// class had wrong. Positioning is the same `move` verb the workspace and
// monitor directives use, distinguished by carrying `x` and `y`; Hyprland
// reads the keys in the order direction, position, workspace, monitor, so a
// table must carry exactly one of those groups.
func positionArgs(d dispatchDialect, address string, x, y int) []string {
	xs, ys := strconv.Itoa(x), strconv.Itoa(y)
	if d == dialectLegacy {
		return []string{"dispatch", "movewindowpixel", "exact " + xs + " " + ys + ",address:" + address}
	}
	return []string{"dispatch",
		`hl.dsp.window.move({ window = "address:` + address + `", x = ` + xs + `, y = ` + ys + `, relative = false })`}
}

// pinArgs pins or unpins a window. The same enable/disable action floatArgs
// uses, and the same reason for spelling it out: an unrecognised action word
// falls back to toggling.
func pinArgs(d dispatchDialect, address string, pinned bool) []string {
	if d == dialectLegacy {
		// Legacy has only `pin`, which toggles. Sending it for "unpin" would
		// pin an unpinned window, so the seam declines to guess: the Lua
		// dialect is where pinning is a set, and on legacy the vocabulary's
		// pinned mode reports what it could not do (see routine's placer).
		return []string{"dispatch", "pin", "address:" + address}
	}
	action := "disable"
	if pinned {
		action = "enable"
	}
	return []string{"dispatch",
		`hl.dsp.window.pin({ window = "address:` + address + `", action = "` + action + `" })`}
}

// fullscreenArgs covers or uncovers a window.
//
// This verb is the well-behaved one: it validates both of its enumerations
// and says which value it did not like, and it reports a missing window
// ("hl.window.fullscreen: no target") instead of answering "ok". Hyprland's
// spelling of the maximised mode is American; the vocabulary's is not, and
// this is the one place the two meet.
func fullscreenArgs(d dispatchDialect, address string, mode FullscreenMode, on bool) []string {
	if d == dialectLegacy {
		// `fullscreen 0` is whole-output, `fullscreen 1` maximised; there is
		// no "unset" spelling, so leaving the state is the same dispatcher
		// with the mode the window is already in, which toggles it off.
		arg := "0"
		if mode == FullscreenMaximised {
			arg = "1"
		}
		return []string{"dispatch", "fullscreen", arg}
	}
	name := "fullscreen"
	if mode == FullscreenMaximised {
		name = "maximized"
	}
	action := "unset"
	if on {
		action = "set"
	}
	return []string{"dispatch",
		`hl.dsp.window.fullscreen({ window = "address:` + address + `", mode = "` + name +
			`", action = "` + action + `" })`}
}

// preselectArgs tells the dwindle layout where the next window goes.
//
// `hl.dsp.layout` is a FUNCTION taking a plain string, not a table of named
// messages: `hl.dsp.layout({ message = "preselect r" })` answers "layout: bad
// argument 1: expected string, got table". The string is the same layout
// message the legacy dialect passes to `layoutmsg`, so the two dialects carry
// identical text and differ only in how it is wrapped.
func preselectArgs(d dispatchDialect, direction PreselectDirection) []string {
	msg := "preselect " + string(direction)
	if d == dialectLegacy {
		return []string{"dispatch", "layoutmsg", msg}
	}
	return []string{"dispatch", `hl.dsp.layout("` + msg + `")`}
}

// masterArgs promotes a window to its workspace's master slot. The layout
// message carries no window selector on either dialect — PromoteMaster
// focuses the window first, which is why the address goes unused. The Lua
// form this code used to send, `hl.dsp.layout.swap_with_master({ window = …
// })`, indexes a function and fails outright ("attempt to index a function
// value"); there is no such table.
func masterArgs(d dispatchDialect, _ string) []string {
	if d == dialectLegacy {
		return []string{"dispatch", "layoutmsg", "swapwithmaster"}
	}
	return []string{"dispatch", `hl.dsp.layout("swapwithmaster")`}
}

// windowMonitorArgs sends one window to a named output, without following it.
// The same `move` verb positioning and the workspace directive use,
// distinguished by carrying `monitor`; an output that is not plugged in
// answers "Invalid monitor / monitor doesn't exist", which the seam reports.
func windowMonitorArgs(d dispatchDialect, address, monitor string) []string {
	if d == dialectLegacy {
		return []string{"dispatch", "movewindow", "mon:" + monitor + ",silent,address:" + address}
	}
	return []string{"dispatch",
		`hl.dsp.window.move({ monitor = "` + monitor + `", window = "address:` + address +
			`", follow = false })`}
}

// workspaceMonitorArgs moves a whole workspace onto a named output.
//
// `hl.dsp.workspace.move` requires `monitor` (it answers "'monitor' is
// required" without it) and accepts the workspace as a number or a string; a
// monitor that is not plugged in answers "Monitor not found", which is a
// refusal the seam reports rather than an "ok" nobody can check.
func workspaceMonitorArgs(d dispatchDialect, workspace int, monitor string) []string {
	ws := strconv.Itoa(workspace)
	if d == dialectLegacy {
		return []string{"dispatch", "moveworkspacetomonitor", ws + " " + monitor}
	}
	return []string{"dispatch",
		`hl.dsp.workspace.move({ workspace = ` + ws + `, monitor = "` + monitor + `" })`}
}

// monitorsArgs asks for the outputs and their geometry. A read, like the
// inventory: JSON, no dialect, nothing changed.
func monitorsArgs() []string { return []string{"monitors", "-j"} }

// layoutOptionArgs asks which tiling layout is in force, so the two
// layout-dependent directives can say "your layout cannot do this" before
// trying rather than after.
func layoutOptionArgs() []string { return []string{"getoption", "general:layout", "-j"} }

// dispatch performs one window action.
func (h *Hyprland) dispatch(ctx context.Context, args dispatchArgs, address string, workspace int) error {
	if !addressPattern.MatchString(address) {
		return fmt.Errorf("refusing to dispatch to malformed window address %q", address)
	}
	return h.dispatchProbed(ctx, func(d dispatchDialect) []string {
		return args(d, address, workspace)
	})
}

// dispatchProbed renders one dispatch in the discovered dialect and runs it.
// Every dispatch in Jarvix goes through here — the window verbs and the
// deterministic workspace and terminal intents alike — so the dialect is
// probed once for all of them and a refusal is judged the same way for all of
// them.
//
// The retry deserves its explanation: a dispatch can fail because the dialect
// changed underneath us (the user switched their configuration and restarted
// the compositor), and re-probing costs one call on a path that has already
// failed. Retrying is only safe because a dispatch in the wrong dialect is a
// syntax error, which does nothing at all — there is no state to have half
// changed.
func (h *Hyprland) dispatchProbed(ctx context.Context, argv func(dispatchDialect) []string) error {
	d := h.probeDialect(ctx)
	err := h.runDispatch(ctx, argv(d))
	if err == nil {
		return nil
	}
	if redetected := h.reprobeDialect(ctx); redetected != d {
		if retryErr := h.runDispatch(ctx, argv(redetected)); retryErr == nil {
			return nil
		}
	}
	return err
}

// runDispatch runs one dispatch and decides whether it worked. hyprctl exits 0
// for a dispatch the compositor refused — "window not found" comes back on
// stdout with a zero status — so the exit code alone is not an answer: success
// is the compositor saying "ok" and nothing else.
func (h *Hyprland) runDispatch(ctx context.Context, argv []string) error {
	out, err := h.run(ctx, argv...)
	if err != nil {
		return err
	}
	if trimmed := strings.TrimSpace(out); !strings.EqualFold(trimmed, "ok") && trimmed != "" {
		return fmt.Errorf("hyprctl %s: %s", argv[0], firstLine(trimmed))
	}
	return nil
}

// probeDialect returns the discovered dispatch dialect, probing once.
func (h *Hyprland) probeDialect(ctx context.Context) dispatchDialect {
	h.mu.Lock()
	known := h.dialect
	h.mu.Unlock()
	if known != dialectUnknown {
		return known
	}
	return h.reprobeDialect(ctx)
}

// reprobeDialect re-runs the probe and stores the result.
func (h *Hyprland) reprobeDialect(ctx context.Context) dispatchDialect {
	found := dialectLegacy
	if out, err := h.run(ctx, probeArgs()...); err == nil && strings.EqualFold(strings.TrimSpace(out), "ok") {
		found = dialectLua
	}
	h.mu.Lock()
	h.dialect = found
	h.mu.Unlock()
	return found
}

// run executes one hyprctl call and returns its combined output. It reuses
// runCapture's process discipline (no shell, own process group, killed as a
// group, output capped) and adds stderr, because a dispatch's diagnostics are
// the only explanation of a refusal.
func (h *Hyprland) run(ctx context.Context, args ...string) (string, error) {
	return h.runCapped(ctx, maxDispatchOutput, args...)
}

func (h *Hyprland) runCapped(ctx context.Context, maxBytes int, args ...string) (string, error) {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = DefaultCompositorTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return runCaptureBoth(callCtx, binaryOr(h.Binary, "hyprctl"), maxBytes, args...)
}

// runCaptureBoth executes one compositor call and returns stdout and stderr
// together.
//
// It is runCapture (gatherers.go) with two differences, both required here and
// wrong there: stderr is kept, because a refused dispatch explains itself on
// it and that explanation is the only material for the sentence the user
// hears; and the cap is small, because nothing this runs can legitimately
// produce more than an inventory. Everything else is the same discipline —
// no shell, its own process group, killed as a group, stdin closed.
func runCaptureBoth(ctx context.Context, binary string, maxBytes int, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // binary and argv are package constants, config, or an inventory address validated against addressPattern
	out := &capped{max: maxBytes}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid: the whole group, so a helper the tool spawned dies too.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second

	if err := cmd.Run(); err != nil {
		if text := firstLine(out.String()); text != "" {
			return "", fmt.Errorf("%s %s: %s", binary, args[0], text)
		}
		return "", fmt.Errorf("%s %s: %w", binary, args[0], err)
	}
	return out.String(), nil
}

// firstLine bounds a compositor diagnostic to something a log line — or, once
// a tool has rewritten it, a sentence — can carry.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const maxLogged = 200
	if len(s) > maxLogged {
		s = s[:maxLogged] + "…"
	}
	return s
}

// hyprClient is the subset of `hyprctl clients -j` this package reads.
// Everything else hyprctl reports (geometry, monitors, groups, tags) is
// deliberately not decoded: unread fields are fields that cannot go stale.
type hyprClient struct {
	Address      string `json:"address"`
	Class        string `json:"class"`
	Title        string `json:"title"`
	Floating     bool   `json:"floating"`
	AcceptsInput bool   `json:"acceptsInput"`
	Mapped       bool   `json:"mapped"`
	PID          int    `json:"pid"`
	// FocusHistoryID is 0 for the focused window and counts upwards through
	// the focus history, which is exactly the order a summary wants.
	FocusHistoryID int        `json:"focusHistoryID"`
	StableID       flexString `json:"stableId"`
	Workspace      struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"workspace"`
	// At and Size are [x, y] and [width, height] in pixels, decoded for
	// layout capture (#62). Slices rather than [2]int so an inventory from a
	// compositor that omits them still parses.
	At   []int `json:"at"`
	Size []int `json:"size"`
	// Fullscreen has been both a JSON bool and a JSON number across Hyprland
	// versions (a plain flag originally, a mode bitfield — 1 maximised, 2
	// fullscreen — since the fullscreen-state rework), so it gets the same
	// tolerant decoding as stableId: either encoding, any non-zero meaning
	// "this window is covering the others".
	Fullscreen flexBool `json:"fullscreen"`
}

// flexString decodes a field Hyprland has emitted as both a JSON string and a
// JSON number across versions (stableId). Refusing to decode it would fail the
// whole inventory over an identifier used only for a sanity check, which is a
// bad trade.
type flexString string

// UnmarshalJSON implements json.Unmarshaler.
func (f *flexString) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	switch {
	case s == "" || s == "null":
		*f = ""
	case s[0] == '"':
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*f = flexString(v)
	default:
		*f = flexString(s)
	}
	return nil
}

// flexBool decodes a field Hyprland has emitted as both a JSON bool and a
// JSON number across versions (fullscreen). The number form is a mode — 0
// none, 1 maximised, 2 fullscreen — and every non-zero mode means the window
// is drawn over its siblings, which is the only fact this package models.
// Refusing to decode it would fail the whole inventory, the flexString trade
// again.
type flexBool bool

// UnmarshalJSON implements json.Unmarshaler.
func (f *flexBool) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	switch s {
	case "", "null", "false":
		*f = false
	case "true":
		*f = true
	default:
		var n float64
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
		*f = n != 0
	}
	return nil
}

// hyprMonitor is the subset of `hyprctl monitors -j` the placement vocabulary
// reads: where the output is, how big it is, what the bars took, and which
// workspace it is showing. The other forty-odd fields — modes, colour
// management, the tearing diagnostics — are deliberately not decoded.
type hyprMonitor struct {
	Name   string  `json:"name"`
	X      int     `json:"x"`
	Y      int     `json:"y"`
	Width  int     `json:"width"`
	Height int     `json:"height"`
	Scale  float64 `json:"scale"`
	// Reserved is the layer-shell reservation in Hyprland's own order —
	// left, top, right, bottom — and is what makes a percentage resolve
	// against the part of the screen a window can actually occupy.
	Reserved        []int `json:"reserved"`
	Focused         bool  `json:"focused"`
	Disabled        bool  `json:"disabled"`
	ActiveWorkspace struct {
		ID int `json:"id"`
	} `json:"activeWorkspace"`
}

// parseMonitors turns hyprctl's output list into the vocabulary's monitors.
// Disabled outputs are dropped: they have geometry in the JSON but nothing
// can be placed on them, and resolving "DP-3" to a screen that is switched
// off would put the user's morning windows somewhere they cannot see.
func parseMonitors(out string) ([]placement.Monitor, error) {
	var raw []hyprMonitor
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &raw); err != nil {
		return nil, fmt.Errorf("hyprctl monitors: %w", err)
	}
	monitors := make([]placement.Monitor, 0, len(raw))
	for _, m := range raw {
		if m.Disabled || strings.TrimSpace(m.Name) == "" {
			continue
		}
		mon := placement.Monitor{
			Name: strings.TrimSpace(m.Name), X: m.X, Y: m.Y,
			Width: m.Width, Height: m.Height, Scale: m.Scale,
			Focused: m.Focused, ActiveWorkspace: m.ActiveWorkspace.ID,
		}
		// A compositor that reports a reservation of some other length is
		// reporting something this code does not understand, and guessing
		// which edges it meant would shrink windows by the wrong amount.
		if len(m.Reserved) == 4 {
			mon.Reserved = [4]int{m.Reserved[0], m.Reserved[1], m.Reserved[2], m.Reserved[3]}
		}
		monitors = append(monitors, mon)
	}
	return monitors, nil
}

// parseClients turns hyprctl's inventory into Windows, most-recently-focused
// first. Unmapped windows are dropped: they are not on screen, so focusing or
// closing one would act on something the user cannot see.
func parseClients(out string) ([]Window, error) {
	var clients []hyprClient
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &clients); err != nil {
		return nil, fmt.Errorf("hyprctl clients: %w", err)
	}
	type ranked struct {
		win  Window
		rank int
	}
	found := make([]ranked, 0, len(clients))
	for _, c := range clients {
		if !c.Mapped || strings.TrimSpace(c.Address) == "" {
			continue
		}
		w := Window{
			Address:       c.Address,
			Class:         strings.TrimSpace(c.Class),
			Title:         strings.TrimSpace(c.Title),
			Workspace:     c.Workspace.ID,
			WorkspaceName: strings.TrimSpace(c.Workspace.Name),
			Floating:      c.Floating,
			AcceptsInput:  c.AcceptsInput,
			Focused:       c.FocusHistoryID == 0,
			StableID:      string(c.StableID),
			PID:           c.PID,
			Fullscreen:    bool(c.Fullscreen),
		}
		if len(c.At) == 2 {
			w.X, w.Y = c.At[0], c.At[1]
		}
		if len(c.Size) == 2 {
			w.Width, w.Height = c.Size[0], c.Size[1]
		}
		found = append(found, ranked{rank: c.FocusHistoryID, win: w})
	}
	// Most-recently-focused first: it is the order the user thinks in ("the
	// one I was just in"), and it makes a long inventory's truncation drop
	// the windows they care least about. Stable, so windows the compositor
	// gave the same rank keep its order rather than shuffling between calls.
	sort.SliceStable(found, func(i, j int) bool { return found[i].rank < found[j].rank })
	windows := make([]Window, 0, len(found))
	for _, f := range found {
		windows = append(windows, f.win)
	}
	return windows, nil
}
