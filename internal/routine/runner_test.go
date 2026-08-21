package routine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
)

// The runner tests are hermetic and clock-injected: the compositor is the
// FakeCompositor, launches are recorded spawns, and every wait is driven by a
// fake timer that advances a fake clock — nothing here sleeps, and nothing
// here can touch a real desktop.

// fakeClock is a deterministic time source: now() reads it, and the fake
// timer advances it by the waited duration, so a bounded wait "elapses"
// exactly as fast as the loop polls.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// eventLog collects published bus events for assertions.
type eventLog struct {
	mu     sync.Mutex
	events []string
	data   []map[string]any
}

func (l *eventLog) publish(event string, data map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
	l.data = append(l.data, data)
}

func (l *eventLog) names() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func (l *eventLog) last(event string) (map[string]any, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.events) - 1; i >= 0; i-- {
		if l.events[i] == event {
			return l.data[i], true
		}
	}
	return nil, false
}

// newTestRunner wires a runner to the fake compositor with a deterministic
// clock. onPoll, when set, runs on every timer wait — tests use it to make a
// window "appear" after a chosen number of polls.
func newTestRunner(comp *desktop.FakeCompositor, defs []Definition, log *eventLog,
	onPoll func(poll int)) (*Runner, *fakeClock) {
	var publish func(string, map[string]any)
	if log != nil {
		publish = log.publish
	}
	r := New(Options{Compositor: comp, Definitions: defs, Publish: publish})
	clk := &fakeClock{t: time.Unix(1000, 0)}
	r.now = clk.now
	polls := 0
	r.timer = func(d time.Duration) (<-chan time.Time, func()) {
		clk.advance(d)
		polls++
		if onPoll != nil {
			onPoll(polls)
		}
		ch := make(chan time.Time, 1)
		ch <- clk.now()
		return ch, func() {}
	}
	return r, clk
}

func verbs(actions []desktop.FakeAction) []string {
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, a.Verb)
	}
	return out
}

// TestRoutineLaunchesWaitsAndPlaces is the happy path of the headline
// scenario: nothing is running, every app is launched through the compositor
// seam, each window is awaited, placed on its workspace, and the run ends on
// the first step's workspace with one all-good summary.
func TestRoutineLaunchesWaitsAndPlaces(t *testing.T) {
	comp := desktop.NewFakeCompositor()
	log := &eventLog{}
	defs := []Definition{{
		Name:    "morning setup",
		Phrases: []string{"morning setup"},
		Steps: []Step{
			{App: "alacritty", Workspace: 1},
			{App: "firefox", Workspace: 2, Tile: TileMaster},
		},
	}}
	r, _ := newTestRunner(comp, defs, log, func(poll int) {
		// Both apps' windows appear after the first poll, the way real
		// launches land while the routine is already waiting.
		if poll == 1 {
			comp.SetWindows(
				desktop.Window{Address: "0xa", Class: "Alacritty", Workspace: 7},
				desktop.Window{Address: "0xf", Class: "firefox", Workspace: 7},
			)
		}
	})

	summary, err := r.Run(context.Background(), "morning setup")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "Morning setup: all two apps placed." {
		t.Errorf("summary = %q", summary)
	}

	actions := comp.Actions()
	got := verbs(actions)
	// Launches first (parallel cold starts), then per-step placement, then
	// the final switch to the first step's workspace. firefox's master
	// arrangement tiles it and promotes it.
	want := []string{"spawn", "spawn", "move", "move", "float", "master", "workspace"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dispatches = %v, want %v", got, want)
	}
	if actions[0].Program != "alacritty" || actions[1].Program != "firefox" {
		t.Errorf("launch order = %q, %q", actions[0].Program, actions[1].Program)
	}
	if actions[2].Address != "0xa" || actions[2].Workspace != 1 {
		t.Errorf("alacritty placement = %+v", actions[2])
	}
	if actions[3].Address != "0xf" || actions[3].Workspace != 2 {
		t.Errorf("firefox placement = %+v", actions[3])
	}
	if actions[4].Floating {
		t.Errorf("master arrangement floated the window: %+v", actions[4])
	}
	if actions[6].Workspace != 1 {
		t.Errorf("finished on workspace %d, want the first step's (1)", actions[6].Workspace)
	}

	events := log.names()
	if events[0] != "routine.started" || events[len(events)-1] != "routine.finished" {
		t.Errorf("events = %v", events)
	}
	if fin, ok := log.last("routine.finished"); !ok || fin["placed"] != 2 || fin["failed"] != 0 {
		t.Errorf("routine.finished = %v", fin)
	}
}

// TestRoutineDedupesAlreadyRunningApps is the classic annoyance inverted: an
// application that is already open is moved into place, never launched again
// — and a re-run therefore converges on the same layout instead of breeding
// browsers.
func TestRoutineDedupesAlreadyRunningApps(t *testing.T) {
	comp := desktop.NewFakeCompositor(
		desktop.Window{Address: "0xf", Class: "firefox", Title: "GitHub", Workspace: 5},
	)
	defs := []Definition{{
		Name: "morning setup", Phrases: []string{"morning setup"},
		Steps: []Step{{App: "firefox", Workspace: 2}},
	}}
	r, _ := newTestRunner(comp, defs, nil, nil)

	for run := 0; run < 2; run++ {
		if _, err := r.Run(context.Background(), "morning setup"); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range comp.Actions() {
		if a.Verb == "spawn" {
			t.Fatalf("a running app was launched again: %+v", a)
		}
	}
	got := verbs(comp.Actions())
	want := []string{"move", "workspace", "move", "workspace"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("dispatches across two runs = %v, want %v (idempotent convergence)", got, want)
	}
}

// TestTwoStepsOfTheSameAppClaimDistinctWindows: dedupe must not resolve two
// terminal steps onto one terminal window.
func TestTwoStepsOfTheSameAppClaimDistinctWindows(t *testing.T) {
	comp := desktop.NewFakeCompositor(
		desktop.Window{Address: "0x1", Class: "Alacritty", Workspace: 1},
		desktop.Window{Address: "0x2", Class: "Alacritty", Workspace: 1},
	)
	defs := []Definition{{
		Name: "terminals", Phrases: []string{"terminals"},
		Steps: []Step{
			{App: "alacritty", Workspace: 3},
			{App: "alacritty", Workspace: 4},
		},
	}}
	r, _ := newTestRunner(comp, defs, nil, nil)
	if _, err := r.Run(context.Background(), "terminals"); err != nil {
		t.Fatal(err)
	}
	actions := comp.Actions()
	if actions[0].Address == actions[1].Address {
		t.Errorf("both steps claimed %s; each step must claim its own window", actions[0].Address)
	}
}

// TestRoutineContinuesPastAFailedLaunch: one dead app must not strand the
// rest, and the single summary names the casualty.
func TestRoutineContinuesPastAFailedLaunch(t *testing.T) {
	comp := desktop.NewFakeCompositor(
		desktop.Window{Address: "0xf", Class: "firefox", Workspace: 5},
	)
	comp.FailSpawn = map[string]error{"slack": errors.New("exec: not found")}
	log := &eventLog{}
	defs := []Definition{{
		Name: "morning setup", Phrases: []string{"morning setup"},
		Steps: []Step{
			{App: "slack", Workspace: 9},
			{App: "firefox", Workspace: 2},
		},
	}}
	r, _ := newTestRunner(comp, defs, log, nil)

	summary, err := r.Run(context.Background(), "morning setup")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "Morning setup: one app placed; slack did not start." {
		t.Errorf("summary = %q", summary)
	}
	// firefox was still placed after slack died.
	var moved bool
	for _, a := range comp.Actions() {
		if a.Verb == "move" && a.Address == "0xf" && a.Workspace == 2 {
			moved = true
		}
	}
	if !moved {
		t.Error("the step after the failure was not placed")
	}
	if fin, ok := log.last("routine.finished"); !ok || fin["placed"] != 1 || fin["failed"] != 1 {
		t.Errorf("routine.finished = %v", fin)
	}
}

// TestRoutineBoundsTheWaitForAWindow: an app that starts but never maps a
// window costs one bounded wait — driven entirely by the injected clock — and
// is named in the summary while later steps still run.
func TestRoutineBoundsTheWaitForAWindow(t *testing.T) {
	comp := desktop.NewFakeCompositor(
		desktop.Window{Address: "0xf", Class: "firefox", Workspace: 5},
	)
	defs := []Definition{{
		Name: "morning setup", Phrases: []string{"morning setup"},
		Steps: []Step{
			{App: "ghostwindow", Workspace: 1},
			{App: "firefox", Workspace: 2},
		},
	}}
	r, _ := newTestRunner(comp, defs, nil, nil)

	summary, err := r.Run(context.Background(), "morning setup")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "Morning setup: one app placed; ghostwindow's window did not appear." {
		t.Errorf("summary = %q", summary)
	}
}

// TestRoutinePlacementDirectives pins the dispatch sequence each directive
// family produces: floating with size and position, and the split
// arrangement.
func TestRoutinePlacementDirectives(t *testing.T) {
	comp := desktop.NewFakeCompositor(
		desktop.Window{Address: "0xs", Class: "signal", Workspace: 1},
		desktop.Window{Address: "0xc", Class: "code", Workspace: 1},
	)
	defs := []Definition{{
		Name: "layout", Phrases: []string{"layout"},
		Steps: []Step{
			{App: "signal", Workspace: 9, Float: true, Width: 1200, Height: 800, X: 100, Y: 50, HasPosition: true},
			{App: "code", Workspace: 2, Tile: TileSplit},
		},
	}}
	r, _ := newTestRunner(comp, defs, nil, nil)
	if _, err := r.Run(context.Background(), "layout"); err != nil {
		t.Fatal(err)
	}
	actions := comp.Actions()
	got := verbs(actions)
	want := []string{"move", "float", "resize", "position", "move", "float", "workspace"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dispatches = %v, want %v", got, want)
	}
	if !actions[1].Floating {
		t.Error("the float directive did not float")
	}
	if actions[2].Width != 1200 || actions[2].Height != 800 {
		t.Errorf("resize = %+v", actions[2])
	}
	if actions[3].X != 100 || actions[3].Y != 50 {
		t.Errorf("position = %+v", actions[3])
	}
	if actions[5].Floating {
		t.Error("the split arrangement floated instead of tiling")
	}
}

// TestRoutineRefusesASecondRunWhileRunning: the AC's interleaving guard. The
// first run is parked deterministically inside its appear-wait; the second
// invocation is refused with ErrAlreadyRunning rather than dispatching a
// single thing.
func TestRoutineRefusesASecondRunWhileRunning(t *testing.T) {
	comp := desktop.NewFakeCompositor()
	defs := []Definition{{
		Name: "morning setup", Phrases: []string{"morning setup"},
		Steps: []Step{{App: "firefox", Workspace: 2}},
	}}
	parked := make(chan struct{})
	release := make(chan struct{})
	r, _ := newTestRunner(comp, defs, nil, nil)
	first := true
	clk := &fakeClock{t: time.Unix(1000, 0)}
	r.now = clk.now
	r.timer = func(d time.Duration) (<-chan time.Time, func()) {
		if first {
			first = false
			close(parked)
			<-release // hold the first run mid-wait, deterministically
			comp.SetWindows(desktop.Window{Address: "0xf", Class: "firefox", Workspace: 7})
		}
		clk.advance(d)
		ch := make(chan time.Time, 1)
		ch <- clk.now()
		return ch, func() {}
	}

	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), "morning setup")
		done <- err
	}()
	<-parked

	before := len(comp.Actions())
	_, err := r.Run(context.Background(), "morning setup")
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second invocation returned %v, want ErrAlreadyRunning", err)
	}
	if err.Error() != "morning setup is already running" {
		t.Errorf("refusal reads %q", err.Error())
	}
	if len(comp.Actions()) != before {
		t.Error("the refused run still dispatched something")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first run: %v", err)
	}
	// The flag is released with the run: a third invocation is allowed again.
	if _, err := r.Run(context.Background(), "morning setup"); err != nil {
		t.Errorf("run after completion refused: %v", err)
	}
}

// TestRoutineCancelledMidWaitStopsCleanly: "stop" (or any interruption)
// cancels the session context, and the routine must abandon the wait, stop
// dispatching, release the runner, and report the cancellation to its caller
// — who owns whatever is said about it.
func TestRoutineCancelledMidWaitStopsCleanly(t *testing.T) {
	comp := desktop.NewFakeCompositor()
	defs := []Definition{{
		Name: "morning setup", Phrases: []string{"morning setup"},
		Steps: []Step{{App: "firefox", Workspace: 2}},
	}}
	log := &eventLog{}
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := newTestRunner(comp, defs, log, func(poll int) {
		if poll == 1 {
			cancel() // the user says "stop" while the wait is parked
		}
	})

	_, err := r.Run(ctx, "morning setup")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	for _, ev := range log.names() {
		if ev == "routine.finished" {
			t.Error("a cancelled run published routine.finished; the cancel path owns the ending")
		}
	}
	got := verbs(comp.Actions())
	if strings.Join(got, ",") != "spawn" {
		t.Errorf("dispatches = %v; nothing may be placed after the cancel", got)
	}
	// The runner is released: the next morning still works.
	comp.SetWindows(desktop.Window{Address: "0xf", Class: "firefox", Workspace: 7})
	if _, err := r.Run(context.Background(), "morning setup"); err != nil {
		t.Errorf("run after a cancelled run refused: %v", err)
	}
}

// TestRoutineRunMiscellany: unknown names are sentences, lookup is
// case-insensitive, and an unreachable compositor fails the run whole.
func TestRoutineRunMiscellany(t *testing.T) {
	comp := desktop.NewFakeCompositor(
		desktop.Window{Address: "0xf", Class: "firefox", Workspace: 5},
	)
	defs := []Definition{{
		Name: "Morning Setup", Phrases: []string{"morning setup"},
		Steps: []Step{{App: "firefox", Workspace: 2}},
	}}
	r, _ := newTestRunner(comp, defs, nil, nil)

	if _, err := r.Run(context.Background(), "no such thing"); err == nil ||
		!strings.Contains(err.Error(), `"no such thing"`) {
		t.Errorf("unknown routine: %v", err)
	}
	if _, err := r.Run(context.Background(), "MORNING SETUP"); err != nil {
		t.Errorf("case-insensitive lookup failed: %v", err)
	}

	dead := desktop.NewFakeCompositor()
	dead.Err = desktop.ErrNoCompositor
	r2, _ := newTestRunner(dead, defs, nil, nil)
	if _, err := r2.Run(context.Background(), "morning setup"); err == nil ||
		!strings.Contains(err.Error(), "window manager") {
		t.Errorf("unreachable compositor: %v", err)
	}
}

// TestSummaries pins the sentence shapes the user hears.
func TestSummaries(t *testing.T) {
	def := Definition{Name: "morning setup", Steps: []Step{{App: "firefox"}}}
	tests := []struct {
		name     string
		outcomes []outcome
		want     string
	}{
		{"one placed", []outcome{{}}, "Morning setup: firefox placed."},
		{"all placed", []outcome{{}, {}, {}, {}, {}}, "Morning setup: all five apps placed."},
		{"one failed", []outcome{{}, {failure: "slack did not start"}},
			"Morning setup: one app placed; slack did not start."},
		{"all failed", []outcome{{failure: "firefox did not start"}},
			"Morning setup: nothing could be placed — firefox did not start."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, _ := summarise(def, tt.outcomes)
			if got != tt.want {
				t.Errorf("summary = %q, want %q", got, tt.want)
			}
		})
	}
}
