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

// Window is one window as the compositor sees it. Everything the matcher and
// the spoken summaries need, and nothing else: geometry, monitors, and the
// dozens of other fields hyprctl reports are deliberately not modelled.
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
	// ResizeWindow sets a floating window's size in pixels. On a tiled window
	// the compositor's answer is its own — the layout owns tiled geometry,
	// which is why callers float first.
	ResizeWindow(ctx context.Context, address string, width, height int) error
	// PositionWindow moves a floating window's top-left corner to x,y in
	// pixels. Coordinates may be negative: on a multi-monitor layout the
	// global origin is wherever the user arranged it to be.
	PositionWindow(ctx context.Context, address string, x, y int) error
	// PromoteMaster makes the window at address its workspace's master
	// window — the big pane of a master/stack layout. Implementations may
	// need to focus the window to do it (see Hyprland.PromoteMaster), so
	// callers should expect the user's view to follow.
	PromoteMaster(ctx context.Context, address string) error
}

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
	return strings.TrimSpace("Hyprland " + name + " (" + h.probeDialect(ctx).String() + " dispatch)"), nil
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

// PromoteMaster implements Compositor.
//
// The window is focused first, and that is a workaround stated openly rather
// than hidden: the legacy dialect's layout message (`layoutmsg
// swapwithmaster`) takes no window selector — it acts on whatever has focus —
// so naming the window means becoming it for a moment. The Lua form does take
// a selector, but focusing on both dialects keeps the two behaviourally
// identical, which matters more than saving the Lua users one dispatch: a
// routine must place windows the same way on every machine it is written for.
func (h *Hyprland) PromoteMaster(ctx context.Context, address string) error {
	if err := h.dispatch(ctx, focusArgs, address, 0); err != nil {
		return err
	}
	return h.dispatchProbed(ctx, func(d dispatchDialect) []string {
		return masterArgs(d, address)
	})
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
// `togglefloating`, whose second application undoes the first; Lua takes the
// state as a value. Both are sets, which is what makes a re-run routine
// converge instead of oscillate (ADR 0026).
func floatArgs(d dispatchDialect, address string, floating bool) []string {
	if d == dialectLegacy {
		verb := "settiled"
		if floating {
			verb = "setfloating"
		}
		return []string{"dispatch", verb, "address:" + address}
	}
	return []string{"dispatch",
		`hl.dsp.window.set_floating({ window = "address:` + address + `", floating = ` + strconv.FormatBool(floating) + ` })`}
}

// resizeArgs sets a floating window's size. `exact` in both dialects: a
// relative resize applied twice keeps growing, and routines re-run.
func resizeArgs(d dispatchDialect, address string, width, height int) []string {
	w, h := strconv.Itoa(width), strconv.Itoa(height)
	if d == dialectLegacy {
		return []string{"dispatch", "resizewindowpixel", "exact " + w + " " + h + ",address:" + address}
	}
	return []string{"dispatch",
		`hl.dsp.window.resize({ window = "address:` + address + `", width = ` + w + `, height = ` + h + `, exact = true })`}
}

// positionArgs moves a floating window's top-left corner. Exact for the same
// reason resizeArgs is.
func positionArgs(d dispatchDialect, address string, x, y int) []string {
	xs, ys := strconv.Itoa(x), strconv.Itoa(y)
	if d == dialectLegacy {
		return []string{"dispatch", "movewindowpixel", "exact " + xs + " " + ys + ",address:" + address}
	}
	return []string{"dispatch",
		`hl.dsp.window.position({ window = "address:` + address + `", x = ` + xs + `, y = ` + ys + `, exact = true })`}
}

// masterArgs promotes a window to its workspace's master slot. The legacy
// layout message carries no window selector — PromoteMaster focuses the
// window first, which is why the address goes unused there; the Lua form
// names it directly, and PromoteMaster still focuses first so the two
// dialects behave identically.
func masterArgs(d dispatchDialect, address string) []string {
	if d == dialectLegacy {
		return []string{"dispatch", "layoutmsg", "swapwithmaster"}
	}
	return []string{"dispatch", `hl.dsp.layout.swap_with_master({ window = "address:` + address + `" })`}
}

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
		found = append(found, ranked{rank: c.FocusHistoryID, win: Window{
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
		}})
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
