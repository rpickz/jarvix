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
	"github.com/rpickz/jarvix/internal/desktopentry"
	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/placement"
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
	// Compositor is the seam every placement — and every bare-program launch
	// — goes through. Required.
	Compositor desktop.Compositor
	// Definitions are the validated routines this runner can execute.
	Definitions []Definition
	// Resolver decides what a step's launching half resolves to on this
	// machine (issue #175). Nil reads the real machine, lazily, on the first
	// run: PATH through exec.LookPath and the desktop entries under the XDG
	// search path. Lazily rather than at construction because a runner is
	// rebuilt on every config reload and the entry index is a directory walk.
	Resolver *Resolver
	// Launcher starts a resolved target — an argv, never a command line. Nil
	// uses ExecLauncher, which starts a detached child the way
	// desktop.launch_app does. Tests supply a recorder, so no test in this
	// package can start a process.
	Launcher Launcher
	// Publish emits progress events (routine.started / routine.step /
	// routine.finished) for the bar and the window. Nil publishes nothing.
	Publish func(event string, data map[string]any)
	// Log records the run; nil uses slog.Default().
	Log *slog.Logger
	// CallTimeout overrides DefaultCallTimeout; AppearTimeout overrides
	// DefaultAppearTimeout.
	CallTimeout   time.Duration
	AppearTimeout time.Duration
	// MonitorNicknames resolves a user-chosen screen name to a connector
	// (#180) — monitors.Store.Lookup behind a func so this package never
	// imports the store. Nil leaves the runner resolving connector names and
	// "current" only, which is what a daemon with no nicknames does.
	//
	// It is held as a func and called on every run rather than snapshotted,
	// because a nickname assigned by voice thirty seconds ago must be live on
	// the next run — a routine that needed a restart to see its own screen
	// name would be a worse indirection than the connector it replaced.
	MonitorNicknames func(name string) (connector string, known bool)
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
	// monitorNames is the nickname table every monitor reference resolves
	// through (#180). Nil is the no-nicknames daemon.
	monitorNames func(name string) (string, bool)
	// launcher starts a resolved argv.
	launcher Launcher

	// resolverMu guards the lazily built default resolver. A run reads it;
	// nothing else writes it after the first.
	resolverMu sync.Mutex
	resolver   *Resolver

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
		monitorNames:  opts.MonitorNicknames,
		launcher:      opts.Launcher,
		resolver:      opts.Resolver,
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
	if r.launcher == nil {
		r.launcher = ExecLauncher{}
	}
	return r
}

// resolve returns the launch resolver, building the real machine's on first
// use. Lazily because reading every desktop entry is a directory walk and a
// daemon that has no routines carrying one should never pay for it.
func (r *Runner) resolve() Resolver {
	r.resolverMu.Lock()
	defer r.resolverMu.Unlock()
	if r.resolver == nil {
		built := DefaultResolver()
		r.resolver = &built
	}
	return *r.resolver
}

// Definitions returns the routines this runner knows, in configured order.
func (r *Runner) Definitions() []Definition {
	return append([]Definition(nil), r.defs...)
}

// Failure classifies why a step did not produce a placed window.
//
// The classification is the reporting criterion of issue #175, and it is a
// classification rather than a message because three different things were
// being said with one sentence. "Nothing launched" covered an application
// that is not installed (no wait will help), one that started and mapped
// nothing (still starting, or it crashed), and one that mapped a window the
// step's match did not recognise (the routine is looking for the wrong
// thing) — three different fixes, reported identically, which is why the user
// saw "placed=3 failed=3" and could not tell what to do about it.
type Failure string

// The failure kinds. Empty is a step that worked.
const (
	// FailureNotInstalled: the program or desktop entry is not on this
	// machine. Nothing was started and nothing was waited for.
	FailureNotInstalled Failure = "not_installed"
	// FailureDidNotStart: the launch itself was refused.
	FailureDidNotStart Failure = "did_not_start"
	// FailureNoWindow: it was started, and no window appeared at all inside
	// the bounded wait.
	FailureNoWindow Failure = "no_window"
	// FailureNoMatch: it was started, a window appeared, and nothing matched
	// what the step is looking for.
	FailureNoMatch Failure = "no_match"
	// FailureNotPlaced: the window exists and the compositor refused some
	// part of the placement — the #177 family, unchanged by this ticket.
	FailureNotPlaced Failure = "not_placed"
)

// outcome is what happened to one step, kept so the summary can be composed
// once at the end instead of spoken piecemeal.
type outcome struct {
	window   desktop.Window
	resolved bool   // a window exists (found or appeared) to place
	launched bool   // the app was started rather than adopted
	failure  string // one spoken clause, empty for a placed step
	kind     Failure
	// excluded is the set of windows this step may not take: empty for a step
	// that adopts, and everything already open for one that insists on a
	// fresh window. Without it, `launch = "always"` would start a new window
	// and then place the old one, which is the instruction inverted.
	excluded map[string]bool
}

// fail records one step's failure: the clause the user hears and the kind the
// feed and the log carry.
func (o *outcome) fail(kind Failure, clause string) {
	o.kind, o.failure = kind, clause
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

	// The monitor inventory, read once for the same reason the window
	// inventory is: percentages resolve against it, and a screen that
	// vanished mid-run would otherwise give two steps two different answers.
	// A compositor that will not report its outputs is not a failed run — the
	// steps that named no monitor and no percentage are unaffected — so the
	// error is carried into the steps that need it rather than raised here.
	monitors, monitorErr := r.monitors(ctx)

	// Phase one: put each named workspace on the monitor its steps asked for,
	// BEFORE anything launches. A workspace that arrives on the right screen
	// after its windows have opened has already shown the user the wrong
	// screen, and on a tiling layout it re-tiles them on the way.
	targets := r.targetMonitors(ctx, def, monitors, monitorErr)

	// Phase two: claim existing windows, and — unless the routine arranges
	// its windows — launch everything missing up front so the applications
	// cold-start in parallel. An arranging routine cannot do that: the layout
	// decides where a window lands the moment it maps, from the preselection
	// standing at that moment, so its launches are serialised into phase
	// three, one window at a time. The cost is the routine's slowest path,
	// and it is the price of being able to say what the desktop looks like.
	arranged := def.arranges()
	place := r.placer()
	resolver := r.resolve()
	outcomes := make([]outcome, len(def.Steps))
	claimed := make(map[string]bool)
	present := addressSet(inventory)
	for i, step := range def.Steps {
		// Whether an already-open window may be adopted is the step's own
		// decision now (#175). A step that says `launch = "always"` skips the
		// search entirely: it is describing a window it wants started, and
		// finding an existing one would be finding the wrong thing.
		if step.Launch.Adopts() {
			if w, found := findUnclaimed(step, inventory, claimed); found {
				claimed[w.Address] = true
				outcomes[i].window, outcomes[i].resolved = w, true
				continue
			}
		}
		outcomes[i].launched = true
		if !step.Launch.Adopts() {
			outcomes[i].excluded = present
		}
		if arranged {
			continue
		}
		if err := r.startStep(ctx, resolver, step); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			r.log.Warn("routine step could not launch", "component", "routine",
				"routine", def.Name, "step", i+1, "app", step.Launches(), "error", err.Error())
			outcomes[i].fail(launchFailure(err), launchClause(step, err))
		}
	}

	// Phase three: launch (when arranging), wait for the window, place it,
	// and set the preselection the next step's window will land against.
	for i, step := range def.Steps {
		if outcomes[i].failure == "" && arranged && outcomes[i].launched {
			// The new window lands on whatever workspace is in view, so the
			// view has to be the step's workspace before the launch. It is a
			// visible side effect and an unavoidable one: no compositor this
			// seam models lets a window be mapped onto a workspace nobody is
			// looking at and tiled against a chosen sibling.
			if err := r.call(ctx, func(c context.Context) error {
				return r.comp.SwitchWorkspace(c, step.Workspace)
			}); err != nil {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				outcomes[i].fail(FailureNotPlaced,
					fmt.Sprintf("workspace %d could not be shown", step.Workspace))
			} else {
				// Retaken here, not reused from phase two: an arranging
				// routine launches one window at a time, so a step insisting
				// on a fresh window must exclude everything open at THIS
				// moment, including windows earlier steps of this same run
				// opened.
				if !step.Launch.Adopts() {
					outcomes[i].excluded = r.addresses(ctx)
				}
				if err := r.startStep(ctx, resolver, step); err != nil {
					if ctx.Err() != nil {
						return "", ctx.Err()
					}
					r.log.Warn("routine step could not launch", "component", "routine",
						"routine", def.Name, "step", i+1, "app", step.Launches(), "error", err.Error())
					outcomes[i].fail(launchFailure(err), launchClause(step, err))
				}
			}
		}
		if outcomes[i].failure == "" && !outcomes[i].resolved {
			w, appeared, err := r.awaitWindow(ctx, step, claimed, outcomes[i].excluded)
			if err != nil {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				r.log.Warn("routine step window never appeared", "component", "routine",
					"routine", def.Name, "step", i+1, "app", step.Launches(),
					"match", step.matchQuery(), "new_windows", appeared, "error", err.Error())
				if appeared {
					outcomes[i].fail(FailureNoMatch, fmt.Sprintf("%s opened a window, but nothing matched %q",
						step.Launches(), step.matchQuery()))
				} else {
					outcomes[i].fail(FailureNoWindow, fmt.Sprintf("%s opened no window within %s",
						step.Launches(), spokenSeconds(r.appearTimeout)))
				}
			} else {
				claimed[w.Address] = true
				outcomes[i].window, outcomes[i].resolved = w, true
			}
		}
		if outcomes[i].failure == "" {
			if err := targets[i].err; err != nil {
				// The screen the step named is not there. Say which one and
				// why, and carry on with the remaining steps — the #180
				// contract, and the ordinary "failure continues" rule.
				outcomes[i].fail(FailureNotPlaced, failureClause(step, "placed", err))
			} else if err := place.Apply(ctx, outcomes[i].window, step.Placement, targets[i].monitor); err != nil {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				r.log.Warn("routine step could not be placed", "component", "routine",
					"routine", def.Name, "step", i+1, "app", step.Launches(), "error", err.Error())
				outcomes[i].fail(FailureNotPlaced, failureClause(step, "placed", err))
			} else if err := place.Preselect(ctx, outcomes[i].window, step.PlaceNext); err != nil {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				r.log.Warn("routine step could not arrange the next window", "component", "routine",
					"routine", def.Name, "step", i+1, "app", step.Launches(), "error", err.Error())
				outcomes[i].fail(FailureNotPlaced, failureClause(step, "arranged", err))
			}
		}
		r.emitStep(def.Name, i, step, outcomes[i])
	}

	// Phase four: the tiled proportions, once every window that shares a
	// split exists. Sizing earlier would move a split with nothing on the
	// other side of it, which the compositor answers by doing nothing —
	// silently, which is exactly the failure mode this ticket exists to end.
	for i, step := range def.Steps {
		if outcomes[i].failure != "" || !step.Tiles() || !step.Sized() {
			continue
		}
		if err := place.Proportion(ctx, outcomes[i].window, step.Placement, targets[i].monitor); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			r.log.Warn("routine step could not be sized", "component", "routine",
				"routine", def.Name, "step", i+1, "app", step.Launches(), "error", err.Error())
			outcomes[i].fail(FailureNotPlaced, failureClause(step, "sized", err))
			r.emitStep(def.Name, i, step, outcomes[i])
		}
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

// monitors reads the output inventory under the per-call bound.
func (r *Runner) monitors(ctx context.Context) ([]placement.Monitor, error) {
	callCtx, cancel := context.WithTimeout(ctx, r.callTimeout)
	defer cancel()
	return r.comp.Monitors(callCtx)
}

// arranges reports whether any step asks for an arrangement, which is what
// forces the run to launch its windows one at a time (see Run's phase two).
func (d Definition) arranges() bool {
	for _, s := range d.Steps {
		if s.PlaceNext != placement.PlaceNextNone {
			return true
		}
	}
	return false
}

// stepTarget is one step's resolved screen: the monitor its percentages
// resolve against, or the reason there isn't one.
type stepTarget struct {
	monitor placement.Monitor
	err     error
}

// targetMonitors resolves every step's monitor and moves the workspaces that
// named one, returning per-step targets in step order.
//
// Two rules are worth stating. A workspace is moved once however many steps
// name it, because moving it twice is two visible jumps for one intention;
// and a step that named NO monitor still gets one — whichever holds its
// workspace — because a percentage has to resolve against something real, and
// "the monitor this workspace is on" is what a person means when they write
// `width = "66%"` and nothing else.
func (r *Runner) targetMonitors(ctx context.Context, def Definition, monitors []placement.Monitor,
	invErr error) []stepTarget {
	targets := make([]stepTarget, len(def.Steps))
	// One field, and every consumer of the vocabulary gained nicknames at
	// once (#180): the runner, the window tools and the placer all resolve a
	// monitor through this one seam, so "top" means the same screen in a
	// routine step, a spoken request and a tool call.
	resolver := placement.Resolver{Nicknames: r.monitorNames}
	moved := make(map[int]bool, len(def.Steps))
	for i, step := range def.Steps {
		if invErr != nil {
			if step.Monitor != "" || step.Sized() {
				targets[i].err = fmt.Errorf("I cannot see which screens are attached: %w", invErr)
			}
			continue
		}
		if step.Monitor == "" {
			targets[i].monitor = placement.ForWorkspace(step.Workspace, monitors)
			continue
		}
		mon, err := resolver.Resolve(step.Monitor, monitors)
		if err != nil {
			targets[i].err = err
			continue
		}
		targets[i].monitor = mon
		if moved[step.Workspace] {
			continue
		}
		moved[step.Workspace] = true
		if mon.ActiveWorkspace == step.Workspace {
			// Already there. Dispatching anyway would be harmless and is
			// skipped for the reason every convergent operation skips a
			// no-op: the fewer things a re-run moves, the less it looks like
			// it is fighting the user.
			continue
		}
		if err := r.call(ctx, func(c context.Context) error {
			return r.comp.MoveWorkspaceToMonitor(c, step.Workspace, mon.Name)
		}); err != nil {
			targets[i].err = err
		}
	}
	return targets
}

// call runs one compositor dispatch under the per-call bound, refusing to
// start once the run's context is done so "stop" lands mid-placement.
func (r *Runner) call(ctx context.Context, f func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, r.callTimeout)
	defer cancel()
	return f(callCtx)
}

// placer applies the vocabulary. It is desktop.Placer rather than logic of
// this package's own precisely because the window tools place windows too:
// "what does mode = pinned dispatch?" has one answer, and a routine and a
// spoken request must not be able to disagree about it (ADR 0056).
func (r *Runner) placer() desktop.Placer {
	return desktop.Placer{Comp: r.comp, Timeout: r.callTimeout}
}

// startStep resolves one step's launching half and starts it.
//
// Resolution comes first even for the plainest step, and that is the whole of
// the "not installed" report: a routine that names an application this
// machine does not have used to spend eight seconds waiting for its window
// and then say the window did not appear. Asking PATH — the same question
// desktop.launch_app asks — turns that into one sentence with a name in it,
// before anything is started or waited for.
//
// Then two paths, and the split is deliberate rather than tidy:
//
//   - A bare program with no arguments still goes through the compositor's
//     spawn dispatcher, exactly as it has since ADR 0026. That path is the
//     only one that starts the application as a child of the compositor, with
//     the graphical session's environment, which matters for a daemon started
//     outside that session — and it has worked for every routine in the
//     field. Changing it for steps that did not ask for anything new would be
//     spending a working feature on symmetry.
//   - Anything carrying arguments — an argv, a desktop entry's Exec, an
//     identity flag — cannot go that way at all, because the dispatcher takes
//     a COMMAND LINE and hands it to a shell. Quoting a value for a shell is
//     a much weaker promise than not having one, so those steps are started
//     directly through the Launcher, with the argv reaching execve as a list.
func (r *Runner) startStep(ctx context.Context, resolver Resolver, step Step) error {
	target, err := resolver.Resolve(step)
	if err != nil {
		return err
	}
	if len(target.Argv) == 1 && strings.TrimSpace(step.App) != "" {
		return r.call(ctx, func(c context.Context) error { return r.comp.Spawn(c, step.App) })
	}
	if err := r.call(ctx, func(c context.Context) error {
		return r.launcher.Launch(c, target)
	}); err != nil {
		// The argv joins the ERROR rather than a second log line, so the one
		// warning the caller writes carries what was actually attempted. It
		// is operator material and never reaches the spoken clause, which
		// launchClause words from the step's own name.
		return fmt.Errorf("%s: %w", target, err)
	}
	return nil
}

// launchFailure classifies a launch error for the feed and the log.
func launchFailure(err error) Failure {
	var missing *desktopentry.NotFoundError
	if errors.Is(err, ErrNotInstalled) || errors.As(err, &missing) {
		return FailureNotInstalled
	}
	return FailureDidNotStart
}

// launchClause words a launch failure for the spoken summary.
//
// A resolver refusal is already a sentence written to be spoken ("chromium is
// not installed", "there is no Signal desktop entry on this computer"), so it
// is used as it stands. Anything else is the operating system's own error,
// which belongs in the log line beside it and not in the user's ear.
func launchClause(step Step, err error) string {
	var missing *desktopentry.NotFoundError
	if errors.Is(err, ErrNotInstalled) || errors.As(err, &missing) {
		return err.Error()
	}
	return step.Launches() + " did not start"
}

// addressSet is the set of window addresses in an inventory.
func addressSet(windows []desktop.Window) map[string]bool {
	set := make(map[string]bool, len(windows))
	for _, w := range windows {
		set[w.Address] = true
	}
	return set
}

// addresses reads the live inventory as a set, or an empty one if the
// compositor will not answer. An empty set makes the wait's diagnosis say
// "opened nothing", which is the conservative reading when we could not look.
func (r *Runner) addresses(ctx context.Context) map[string]bool {
	windows, err := r.windows(ctx)
	if err != nil {
		return map[string]bool{}
	}
	return addressSet(windows)
}

// spokenSeconds renders a wait for a sentence: "8 seconds", not "8s".
func spokenSeconds(d time.Duration) string {
	seconds := int(d.Round(time.Second) / time.Second)
	if seconds == 1 {
		return "1 second"
	}
	return fmt.Sprintf("%d seconds", seconds)
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
//
// appeared reports whether ANY window that was not on screen when this wait
// began turned up while it ran. It is the difference between the two honest
// diagnoses this ticket asks for: nothing appeared at all (the application is
// still starting, or it died), or something appeared and the step's match did
// not recognise it (the routine is looking for the wrong thing — the reported
// case was `app = "chromium", match = "facebook"`, which launched a browser
// and then waited for a window nothing had told it to open).
//
// The baseline is taken when the WAIT starts rather than when the launch was
// dispatched, and that is deliberate. A routine that does not arrange its
// windows launches everything at once and then waits in step order, so a
// launch-time baseline would let step one's window count as step three's
// evidence. Waits run one at a time, so "what turned up while I was waiting"
// attributes each window to a single step — the best attribution an observer
// of a window list can make, and the same one the user's own script made.
//
// excluded are windows this step may not take. It is empty for a step that
// adopts (the ordinary case: any matching window will do) and the
// pre-existing inventory for one that insists on a fresh window, which is
// what makes `launch = "always"` mean what it says rather than picking up the
// window it was told to ignore.
func (r *Runner) awaitWindow(ctx context.Context, step Step, claimed map[string]bool,
	excluded map[string]bool) (window desktop.Window, appeared bool, err error) {
	deadline := r.now().Add(r.appearTimeout)
	var atStart map[string]bool
	for {
		if windows, readErr := r.windows(ctx); readErr == nil {
			if atStart == nil {
				atStart = addressSet(windows)
			}
			eligible := make([]desktop.Window, 0, len(windows))
			for _, w := range windows {
				if claimed[w.Address] || excluded[w.Address] {
					continue
				}
				if !atStart[w.Address] {
					appeared = true
				}
				eligible = append(eligible, w)
			}
			if w, found := tools.FindWindow(step.matchQuery(), eligible); found {
				return w, appeared, nil
			}
		}
		if err := ctx.Err(); err != nil {
			return desktop.Window{}, appeared, err
		}
		if !r.now().Before(deadline) {
			return desktop.Window{}, appeared,
				fmt.Errorf("no window matching %q appeared within %s", step.matchQuery(), r.appearTimeout)
		}
		fire, stop := r.timer(appearPoll)
		select {
		case <-ctx.Done():
			stop()
			return desktop.Window{}, appeared, ctx.Err()
		case <-fire:
			stop()
		}
	}
}

// failureClause words one step's placement failure for the spoken summary.
//
// The clause matters more than it looks: "could not be placed" was the whole
// vocabulary of failure before this change, so a resize the compositor
// refused and a workspace that does not exist sounded identical, and a step
// whose resize was silently rejected was reported as placed (#177). A run now
// says which part of the placement it could not do.
//
// The compositor's own diagnostics stay out of the sentence — they are the
// operator's material and live in the log line beside it — except for the two
// the seam has already rewritten into user-facing sentences (a layout that
// has no master pane, a monitor that is not attached), which are recognised
// by carrying no compositor jargon because the seam built them from the
// vocabulary's own words.
func failureClause(step Step, clause string, err error) string {
	if msg := desktop.PlacementSentence(err); msg != "" {
		return step.Launches() + ": " + msg
	}
	return step.Launches() + " could not be " + clause
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
		"app": step.Launches(), "workspace": step.Workspace,
		"launched": out.launched,
	}
	if out.failure != "" {
		status = "failed"
		data["detail"] = out.failure
		// The kind rides beside the sentence so the feed can say what KIND of
		// failure this was without parsing English out of the detail line —
		// "not installed" is a different row from "opened nothing", and a
		// surface that wants to offer a fix needs to know which.
		data["failure"] = string(out.kind)
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
		fmt.Fprintf(&b, "%s placed", def.Steps[0].Launches())
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
