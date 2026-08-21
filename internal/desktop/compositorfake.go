package desktop

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// FakeCompositor is a scripted compositor: canned inventories in, recorded
// dispatches out. It exists so that no test anywhere in the tree needs a
// running Hyprland — the window tools, the matcher, and the argv guarantees
// are all exercised against this.
//
// The one piece of machinery beyond "return what I was given" is BeforeAction,
// which lets a test change the inventory *between* a resolution and the
// dispatch it produced. That is the race the whole design exists to survive,
// so it has to be reproducible on demand.
type FakeCompositor struct {
	// Name is what Describe reports. Empty means a plausible default.
	Name string
	// Err, when set, fails every call — the "no compositor here" case.
	Err error
	// FailAction, when set, fails the window verbs only (focus, close, move,
	// float, resize, position, master): the inventory reads fine and the
	// compositor refuses to act, which is what a window closing underneath
	// the user looks like.
	FailAction error
	// FailSpawn fails Spawn for exactly the named programs — the routine
	// tests' "one dead app must not strand the other six" case. The attempt
	// is still recorded, because "it was asked for and refused" and "it was
	// never asked for" are different assertions.
	FailSpawn map[string]error

	// BeforeAction runs at the start of every action, before the recorded
	// call, with the address that was asked for. A test uses it to swap the
	// inventory out from under the caller.
	BeforeAction func(address string)

	mu sync.Mutex
	// windows is the inventory Windows returns.
	windows []Window
	// actions records every dispatch, in order.
	actions []FakeAction
	// reads counts Windows calls — the assertion behind "one inventory per
	// resolution, and the cache means one per turn".
	reads int
}

// FakeAction is one recorded dispatch.
type FakeAction struct {
	// Verb is one of "focus", "close", "move", "workspace", "spawn",
	// "float", "resize", "position", "master".
	Verb      string
	Address   string
	Workspace int
	// Program is the executable a "spawn" was asked to start, empty for the
	// window verbs.
	Program string
	// Floating is what a "float" set the state to.
	Floating bool
	// Width and Height are a "resize"'s pixels; X and Y a "position"'s.
	Width, Height int
	X, Y          int
}

// NewFakeCompositor builds a fake holding the given inventory.
func NewFakeCompositor(windows ...Window) *FakeCompositor {
	return &FakeCompositor{windows: windows}
}

// SetWindows replaces the inventory. Safe to call from BeforeAction.
func (f *FakeCompositor) SetWindows(windows ...Window) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.windows = windows
}

// Describe implements Compositor.
func (f *FakeCompositor) Describe(context.Context) (string, error) {
	if f.Err != nil {
		return "", f.Err
	}
	if f.Name != "" {
		return f.Name, nil
	}
	return "FakeCompositor 1.0 (test dispatch)", nil
}

// Windows implements Compositor.
func (f *FakeCompositor) Windows(ctx context.Context) ([]Window, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	f.reads++
	return append([]Window(nil), f.windows...), nil
}

// Focus implements Compositor.
func (f *FakeCompositor) Focus(ctx context.Context, address string) error {
	return f.act(ctx, "focus", address, 0)
}

// Close implements Compositor.
func (f *FakeCompositor) Close(ctx context.Context, address string) error {
	return f.act(ctx, "close", address, 0)
}

// MoveToWorkspace implements Compositor.
func (f *FakeCompositor) MoveToWorkspace(ctx context.Context, address string, workspace int) error {
	return f.act(ctx, "move", address, workspace)
}

// SwitchWorkspace implements Compositor.
func (f *FakeCompositor) SwitchWorkspace(ctx context.Context, workspace int) error {
	if workspace < minWorkspace || workspace > maxWorkspace {
		return fmt.Errorf("workspace %d does not exist", workspace)
	}
	return f.record(ctx, FakeAction{Verb: "workspace", Workspace: workspace})
}

// Spawn implements Compositor.
func (f *FakeCompositor) Spawn(ctx context.Context, program string) error {
	if !spawnPattern.MatchString(program) {
		return fmt.Errorf("refusing to start %q", program)
	}
	if err := f.record(ctx, FakeAction{Verb: "spawn", Program: program}); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.FailSpawn[program]
}

// SetFloating implements Compositor.
func (f *FakeCompositor) SetFloating(ctx context.Context, address string, floating bool) error {
	return f.actOn(ctx, FakeAction{Verb: "float", Address: address, Floating: floating})
}

// ResizeWindow implements Compositor.
func (f *FakeCompositor) ResizeWindow(ctx context.Context, address string, width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("refusing to resize a window to %d by %d pixels", width, height)
	}
	return f.actOn(ctx, FakeAction{Verb: "resize", Address: address, Width: width, Height: height})
}

// PositionWindow implements Compositor.
func (f *FakeCompositor) PositionWindow(ctx context.Context, address string, x, y int) error {
	return f.actOn(ctx, FakeAction{Verb: "position", Address: address, X: x, Y: y})
}

// PromoteMaster implements Compositor.
func (f *FakeCompositor) PromoteMaster(ctx context.Context, address string) error {
	return f.actOn(ctx, FakeAction{Verb: "master", Address: address})
}

// record notes a dispatch that names no window. The window verbs cannot use
// it because they must also check the address against the inventory; these
// have nothing to check against, which is precisely why they need no
// BeforeAction hook either — there is no resolution to race.
func (f *FakeCompositor) record(ctx context.Context, action FakeAction) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.actions = append(f.actions, action)
	return f.FailAction
}

func (f *FakeCompositor) act(ctx context.Context, verb, address string, workspace int) error {
	return f.actOn(ctx, FakeAction{Verb: verb, Address: address, Workspace: workspace})
}

// actOn records one window-addressed dispatch, whatever its payload. The
// original three verbs and the placement verbs (ADR 0026) share it so all of
// them get the same discipline: the BeforeAction hook, and the refusal to
// "succeed" against an address the inventory never reported.
func (f *FakeCompositor) actOn(ctx context.Context, action FakeAction) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.BeforeAction != nil {
		f.BeforeAction(action.Address)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.actions = append(f.actions, action)
	if f.FailAction != nil {
		return f.FailAction
	}
	// A fake that dispatched to an address it has never reported would let a
	// test pass while the real compositor did nothing, so it says so.
	for _, w := range f.windows {
		if w.Address == action.Address {
			return nil
		}
	}
	return fmt.Errorf("no window at address %s", action.Address)
}

// Actions returns the recorded dispatches, in order.
func (f *FakeCompositor) Actions() []FakeAction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeAction(nil), f.actions...)
}

// LastAction returns the most recent dispatch, or false when nothing was
// dispatched — the assertion behind "it refused rather than guessed".
func (f *FakeCompositor) LastAction() (FakeAction, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.actions) == 0 {
		return FakeAction{}, false
	}
	return f.actions[len(f.actions)-1], true
}

// Reads reports how many inventories were fetched.
func (f *FakeCompositor) Reads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// ErrNoCompositor is the canned unavailability the fake reports when a test
// wants the "Hyprland is not running" path.
var ErrNoCompositor = errors.New("hyprctl: no compositor running")
