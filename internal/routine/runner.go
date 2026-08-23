package routine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/tools"
)

// Timing bounds. All three exist so a routine can never wedge the session
// that runs it: every compositor call is bounded, every wait for a window is
// bounded, and everything selects on the run's context so "stop" lands
// mid-placement rather than after it.
const (
	// DefaultCallTimeout bounds one compositor call, matching the seam's own
	// budget for a local IPC round trip.
	DefaultCallTimeout = time.Second
	// DefaultAppearTimeout is how long a launched application gets to map a
	// window before the step is recorded as failed and the routine moves on.
	// Generous by intent-standards — an editor cold-starting on a slow disk
	// is the normal case, not the pathological one — but firmly bounded: a
	// binary that starts and never shows a window costs one wait, never a
	// wedged session.
	DefaultAppearTimeout = 8 * time.Second
	// appearPoll is how often the inventory is re-read while waiting. Polling
	// is the honest mechanism here: the seam reads state through short-lived
	// subprocesses (ADR 0002), so there is no event stream to subscribe to,
	// and a poll every beat of this size is invisible next to an application
	// start.
	appearPoll = 150 * time.Millisecond
)

// ErrAlreadyRunning is wrapped into the refusal when a routine's phrase
// arrives while a run is still placing windows. Refusing is the point: two
// interleaved placement sequences would fight over the same workspaces, and
// an idempotent re-run costs nothing once the first run finishes.
var ErrAlreadyRunning = errors.New("already running")

// Options configure a Runner.
type Options struct {
	// Compositor is the seam every launch and placement goes through.
	// Required.
	Compositor desktop.Compositor
	// Definitions are the validated routines this runner can execute.
	Definitions []Definition
	// Publish emits progress events (routine.started / routine.step /
	// routine.finished) for the bar and the window. Nil publishes nothing.
	Publish func(event string, data map[string]any)
	// Log records the run; nil uses slog.Default().
	Log *slog.Logger
	// CallTimeout overrides DefaultCallTimeout; AppearTimeout overrides
	// DefaultAppearTimeout.
	CallTimeout   time.Duration
	AppearTimeout time.Duration
}

// Runner executes routines, one at a time. It is stateless between runs
// apart from the mutual-exclusion flag: every run re-reads the live window
// inventory, which is what makes a re-run converge on the same layout
// instead of replaying stale decisions.
type Runner struct {
	comp          desktop.Compositor
	defs          []Definition
	publish       func(string, map[string]any)
	log           *slog.Logger
	callTimeout   time.Duration
	appearTimeout time.Duration

	// now and timer are the run's clock, injectable so the appear-wait is
	// deterministic in tests — the same shape the session engine uses, because
	// this repo forbids sleeping at a test's mercy.
	now   func() time.Time
	timer func(d time.Duration) (<-chan time.Time, func())

	mu      sync.Mutex
	running string // the routine currently placing windows, "" when idle
}

// New builds a Runner.
func New(opts Options) *Runner {
	r := &Runner{
		comp:          opts.Compositor,
		defs:          append([]Definition(nil), opts.Definitions...),
		publish:       opts.Publish,
		log:           opts.Log,
		callTimeout:   opts.CallTimeout,
		appearTimeout: opts.AppearTimeout,
		now:           time.Now,
		timer: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTimer(d)
			return t.C, func() { t.Stop() }
		},
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	if r.callTimeout <= 0 {
		r.callTimeout = DefaultCallTimeout
	}
	if r.appearTimeout <= 0 {
		r.appearTimeout = DefaultAppearTimeout
	}
	return r
}

// Definitions returns the routines this runner knows, in configured order.
func (r *Runner) Definitions() []Definition {
	return append([]Definition(nil), r.defs...)
}

// outcome is what happened to one step, kept so the summary can be composed
// once at the end instead of spoken piecemeal.
type outcome struct {
	window   desktop.Window
	resolved bool   // a window exists (found or appeared) to place
	launched bool   // the app was spawned rather than deduped
	failure  string // one spoken clause, empty for a placed step
}

// Run executes the named routine under ctx and returns the one-sentence
// summary the engine speaks. Per-step failure is not an error — the summary
// names it; err is reserved for a run that could not happen at all (unknown
// name, already running, no reachable compositor) or was cancelled, in which
// case the caller owns what, if anything, is said.
func (r *Runner) Run(ctx context.Context, name string) (string, error) {
	def, ok := r.definition(name)
	if !ok {
		// Unreachable from the router, which only matches configured names —
		// but the IPC surface takes names too, and a bug must be a sentence.
		return "", fmt.Errorf("no routine is called %q", name)
	}
	if !r.begin(def.Name) {
		return "", fmt.Errorf("%s is %w", def.Name, ErrAlreadyRunning)
	}
	defer r.end()

	r.emit("routine.started", map[string]any{"routine": def.Name, "steps": len(def.Steps)})
	started := r.now()

	// One inventory read decides every dedupe. Reading per step instead
	// would let step three match the window step one just launched.
	inventory, err := r.windows(ctx)
	if err != nil {
		r.emit("routine.finished", map[string]any{
			"routine": def.Name, "placed": 0, "failed": len(def.Steps), "error": err.Error(),
			"duration_ms": r.now().Sub(started).Milliseconds()})
		return "", fmt.Errorf("I cannot reach the window manager")
	}

	// Phase one: claim existing windows, launch what is missing. All the
	// launches are dispatched before any window is waited for, so the
	// applications cold-start in parallel while the placements proceed in
	// order — the only parallelism the ordering allows.
	outcomes := make([]outcome, len(def.Steps))
	claimed := make(map[string]bool)
	for i, step := range def.Steps {
		if w, found := findUnclaimed(step, inventory, claimed); found {
			claimed[w.Address] = true
			outcomes[i].window, outcomes[i].resolved = w, true
			continue
		}
		outcomes[i].launched = true
		if err := r.spawn(ctx, step.App); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			r.log.Warn("routine step could not launch", "component", "routine",
				"routine", def.Name, "step", i+1, "app", step.App, "error", err.Error())
			outcomes[i].failure = step.App + " did not start"
		}
	}

	// Phase two: wait for each launched window, then place it.
	for i, step := range def.Steps {
		if outcomes[i].failure == "" && !outcomes[i].resolved {
			w, err := r.awaitWindow(ctx, step, claimed)
			if err != nil {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				r.log.Warn("routine step window never appeared", "component", "routine",
					"routine", def.Name, "step", i+1, "app", step.App, "error", err.Error())
				outcomes[i].failure = step.App + "'s window did not appear"
			} else {
				claimed[w.Address] = true
				outcomes[i].window, outcomes[i].resolved = w, true
			}
		}
		if outcomes[i].failure == "" {
			if err := r.place(ctx, step, outcomes[i].window); err != nil {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				r.log.Warn("routine step could not be placed", "component", "routine",
					"routine", def.Name, "step", i+1, "app", step.App, "error", err.Error())
				outcomes[i].failure = step.App + " could not be placed"
			}
		}
		r.emitStep(def.Name, i, step, outcomes[i])
	}

	// End somewhere predictable: the first step's workspace, which for the
	// morning-setup shape is where the user starts their day. Best-effort —
	// the windows are already placed, and failing the run over the final
	// view switch would report a lie.
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	callCtx, cancel := context.WithTimeout(ctx, r.callTimeout)
	_ = r.comp.SwitchWorkspace(callCtx, def.Steps[0].Workspace)
	cancel()

	summary, placed, failed := summarise(def, outcomes)
	// duration_ms rides the event (#93) so the Automations tab's last-run
	// line carries the same number the log line below already reports.
	r.emit("routine.finished", map[string]any{
		"routine": def.Name, "placed": placed, "failed": len(failed),
		"failures": failed, "summary": summary,
		"duration_ms": r.now().Sub(started).Milliseconds(),
	})
	r.log.Info("routine finished", "component", "routine", "routine", def.Name,
		"placed", placed, "failed", len(failed),
		"duration_ms", r.now().Sub(started).Milliseconds())
	return summary, nil
}

// definition finds a routine by name, case-insensitively — the IPC surface
// and the capture tooling should not have to reproduce exact casing.
func (r *Runner) definition(name string) (Definition, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	for _, def := range r.defs {
		if strings.ToLower(strings.TrimSpace(def.Name)) == want {
			return def, true
		}
	}
	return Definition{}, false
}

// begin claims the runner for one routine; false means a run is in flight.
func (r *Runner) begin(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running != "" {
		return false
	}
	r.running = name
	return true
}

func (r *Runner) end() {
	r.mu.Lock()
	r.running = ""
	r.mu.Unlock()
}

// windows reads the inventory under the per-call bound.
func (r *Runner) windows(ctx context.Context) ([]desktop.Window, error) {
	callCtx, cancel := context.WithTimeout(ctx, r.callTimeout)
	defer cancel()
	return r.comp.Windows(callCtx)
}

// spawn launches one step's application through the compositor, the same
// validated path the terminal intent uses (ADR 0022's exec_cmd exception):
// the name was bounded to one bare token at config load and the seam checks
// it again before rendering, so a shell — where one is involved at all —
// receives a string with nothing to chew on. Spawning through the compositor
// rather than from the daemon is also what makes the window land with the
// graphical session's environment and survive a daemon restart.
func (r *Runner) spawn(ctx context.Context, app string) error {
	callCtx, cancel := context.WithTimeout(ctx, r.callTimeout)
	defer cancel()
	return r.comp.Spawn(callCtx, app)
}

// findUnclaimed matches a step against the windows no earlier step has
// claimed. Claiming is what lets two steps of the same application resolve
// to two different windows instead of both grabbing the first.
func findUnclaimed(step Step, inventory []desktop.Window, claimed map[string]bool) (desktop.Window, bool) {
	free := make([]desktop.Window, 0, len(inventory))
	for _, w := range inventory {
		if !claimed[w.Address] {
			free = append(free, w)
		}
	}
	return tools.FindWindow(step.matchQuery(), free)
}

// awaitWindow polls the inventory until a window matching the step appears,
// the bounded wait expires, or ctx is cancelled. The clock and the timer are
// injected, so tests drive this loop step by step instead of sleeping at it.
func (r *Runner) awaitWindow(ctx context.Context, step Step, claimed map[string]bool) (desktop.Window, error) {
	deadline := r.now().Add(r.appearTimeout)
	for {
		if windows, err := r.windows(ctx); err == nil {
			if w, found := findUnclaimed(step, windows, claimed); found {
				return w, nil
			}
		}
		if err := ctx.Err(); err != nil {
			return desktop.Window{}, err
		}
		if !r.now().Before(deadline) {
			return desktop.Window{}, fmt.Errorf("no window appeared within %s", r.appearTimeout)
		}
		fire, stop := r.timer(appearPoll)
		select {
		case <-ctx.Done():
			stop()
			return desktop.Window{}, ctx.Err()
		case <-fire:
			stop()
		}
	}
}

// place applies one step's directives to a resolved window, every dispatch
// bounded and every directive a set rather than a toggle, so re-running the
// routine converges on the same layout.
func (r *Runner) place(ctx context.Context, step Step, w desktop.Window) error {
	call := func(f func(context.Context) error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		callCtx, cancel := context.WithTimeout(ctx, r.callTimeout)
		defer cancel()
		return f(callCtx)
	}
	if err := call(func(c context.Context) error {
		return r.comp.MoveToWorkspace(c, w.Address, step.Workspace)
	}); err != nil {
		return err
	}
	if step.Float {
		if err := call(func(c context.Context) error {
			return r.comp.SetFloating(c, w.Address, true)
		}); err != nil {
			return err
		}
		if step.Width > 0 && step.Height > 0 {
			if err := call(func(c context.Context) error {
				return r.comp.ResizeWindow(c, w.Address, step.Width, step.Height)
			}); err != nil {
				return err
			}
		}
		if step.HasPosition {
			if err := call(func(c context.Context) error {
				return r.comp.PositionWindow(c, w.Address, step.X, step.Y)
			}); err != nil {
				return err
			}
		}
	}
	switch step.Tile {
	case TileSplit, TileMaster:
		if err := call(func(c context.Context) error {
			return r.comp.SetFloating(c, w.Address, false)
		}); err != nil {
			return err
		}
		if step.Tile == TileMaster {
			if err := call(func(c context.Context) error {
				return r.comp.PromoteMaster(c, w.Address)
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// emit publishes one bus event, if anyone is listening.
func (r *Runner) emit(event string, data map[string]any) {
	if r.publish != nil {
		r.publish(event, data)
	}
}

// emitStep publishes one step's outcome. Progress is events, never speech:
// the bar can show "3 of 5", but the user hears exactly one summary.
func (r *Runner) emitStep(routineName string, i int, step Step, out outcome) {
	status := "placed"
	data := map[string]any{
		"routine": routineName, "step": i + 1,
		"app": step.App, "workspace": step.Workspace,
		"launched": out.launched,
	}
	if out.failure != "" {
		status = "failed"
		data["detail"] = out.failure
	}
	data["status"] = status
	r.emit("routine.step", data)
}

// summarise composes the one sentence the user hears: what landed, and every
// failure by name. Counts are spoken as words the way intent acknowledgements
// speak them.
func summarise(def Definition, outcomes []outcome) (summary string, placed int, failed []string) {
	for _, out := range outcomes {
		if out.failure == "" {
			placed++
		} else {
			failed = append(failed, out.failure)
		}
	}
	var b strings.Builder
	b.WriteString(capitalise(def.Name))
	b.WriteString(": ")
	switch {
	case len(failed) == 0 && placed == 1:
		// One step, and it landed: name the app rather than counting to one.
		fmt.Fprintf(&b, "%s placed", def.Steps[0].App)
	case len(failed) == 0:
		fmt.Fprintf(&b, "all %s apps placed", intent.SpokenNumber(placed))
	case placed == 0:
		b.WriteString("nothing could be placed — ")
		b.WriteString(strings.Join(failed, ", "))
	default:
		apps := "apps"
		if placed == 1 {
			apps = "app"
		}
		fmt.Fprintf(&b, "%s %s placed; %s", intent.SpokenNumber(placed), apps, strings.Join(failed, ", "))
	}
	b.WriteString(".")
	return b.String(), placed, failed
}

// capitalise upper-cases the first letter so a routine named in lower case
// reads as a sentence opening.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
