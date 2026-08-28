package overlay

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/quiesce"
)

// The feed's clockwork. Geometry has no event seam in this repository — the
// compositor is driven exclusively through short-lived hyprctl subprocesses
// (ADR 0002/0022), and nothing anywhere consumes Hyprland's event socket —
// so window movement is observed by polling the inventory. The cadence is
// deliberately gentle and deliberately conditional:
//
//   - While anything is enrolled (a thread with an anchor, or a nickname),
//     the loop ticks every DefaultPollInterval and republishes only when the
//     composed rows actually changed. A moved, resized, killed, or
//     workspace-switched window therefore converges within one interval —
//     the acceptance criteria's "tracks it" and "no stale badges", bought
//     for two brief subprocesses a tick (the inventory, plus the focus
//     snapshot's own anchor-liveness read).
//   - With nothing enrolled the loop parks: no timer, no subprocess, nothing
//     — a user who never touches threads or nicknames pays zero. It wakes on
//     Poke, which the daemon wires to the bus events that can change
//     enrolment (focus.changed, a window being named, a settings change).
//
// An event-driven geometry seam would tighten the latency; the ADR records
// why polling was chosen and what would justify revisiting it.

// DefaultPollInterval is the enrolled-state cadence. Two seconds keeps a
// moved window's chip honest to within a glance while costing a couple of
// millisecond-scale hyprctl calls — far gentler than anything resembling
// animation, which these overlays must never have anyway.
const DefaultPollInterval = 2 * time.Second

// Options are the service's seams. Everything is injected so the tests own
// every input and never touch a compositor, a store, or a clock.
type Options struct {
	// Windows reads the live inventory — the daemon passes the shared
	// compositor seam, bounded by its own timeout.
	Windows func(ctx context.Context) ([]desktop.Window, error)
	// Threads reads the focus threads in display order (active first). The
	// daemon adapts focus.Snapshot; tests hand back literals.
	Threads func(ctx context.Context) []Thread
	// Tags maps window addresses to nicknames, judged against the inventory
	// just read (the registry prunes against it). Nil means no nicknames.
	Tags func(windows []desktop.Window) map[string]string
	// NicknamesHeld is the cheap half of the enrolment gate: whether any
	// nickname exists, answered without a compositor call. Nil means none.
	NicknamesHeld func() bool
	// Enabled is the live overlays.enabled switch, read per computation so a
	// settings change lands on the next tick or poke, no restart.
	Enabled func() bool
	// Publish emits one overlays.changed payload. Called only when the rows
	// differ from the last published set; an empty slice means "clear
	// everything". Nil publishes nothing.
	Publish func(rows []Row)
	// Interval overrides DefaultPollInterval (tests shrink it; zero keeps
	// the default). Timer creates one shot of the clock, the focus
	// scheduler's seam shape, so no test ever sleeps.
	Interval time.Duration
	Timer    func(d time.Duration) (<-chan time.Time, func())
}

// Service runs the overlay feed: one tracked loop, a poke channel, and the
// last published rows for change detection. All methods are safe for
// concurrent use.
type Service struct {
	opts Options
	log  *slog.Logger

	// group tracks the loop from the moment it exists, never a bare go (the
	// #74 lesson); Drain waits on it with the shutdown stage's deadline.
	group quiesce.Group

	mu     sync.Mutex
	base   context.Context
	cancel context.CancelFunc
	closed bool
	last   []Row
	// poke wakes a parked or waiting loop; buffered so no caller ever
	// blocks on the loop being mid-computation.
	poke chan struct{}
}

// NewService builds the feed over its seams. Nothing runs until Start.
func NewService(opts Options, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultPollInterval
	}
	if opts.Timer == nil {
		opts.Timer = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTimer(d)
			return t.C, func() { t.Stop() }
		}
	}
	return &Service{opts: opts, log: log, poke: make(chan struct{}, 1)}
}

// Start begins the loop. ctx is the service's lifetime: its cancellation
// reaches the loop, which is what makes Drain's deadline effective.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.base != nil {
		return
	}
	s.base = ctx
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.group.Go(func() { s.run(loopCtx) })
}

// Poke wakes the loop to recompute now rather than at the next tick — the
// daemon calls it on the bus events that change what the overlays say
// (focus.changed, a nickname assignment, a settings change), so a badge's
// fill follows a thread switch immediately instead of up to an interval
// late. A wake already pending is enough; this never blocks.
func (s *Service) Poke() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// Current computes the rows fresh, for overlays.get: a client attaching
// mid-life must not wait for the next change to learn the present. It reads
// the same seams as the loop and publishes nothing.
func (s *Service) Current(ctx context.Context) []Row {
	rows, _ := s.compute(ctx)
	return rows
}

// Drain stops the loop and waits — bounded by ctx — for it to finish, so
// daemon shutdown treats the feed as one more drained stage.
func (s *Service) Drain(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.mu.Unlock()
	return s.group.Wait(ctx)
}

// InFlight reports how many service goroutines are still running, for the
// shutdown log when a drain gives up.
func (s *Service) InFlight() int { return s.group.InFlight() }

// run is the loop: compute, publish on change, then wait — on the timer
// while enrolled, on a poke alone while parked.
func (s *Service) run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		rows, enrolled := s.compute(ctx)
		s.publishIfChanged(rows)
		if !enrolled {
			select {
			case <-ctx.Done():
				return
			case <-s.poke:
			}
			continue
		}
		tick, stop := s.opts.Timer(s.opts.Interval)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-tick:
			stop()
		case <-s.poke:
			stop()
		}
	}
}

// compute reads the seams and composes the rows. The second result reports
// enrolment — whether the loop should keep ticking. The order of the reads
// is the gate's whole design: the switch and the thread store are cheap and
// come first; the compositor subprocess runs only when something could
// possibly be overlaid.
func (s *Service) compute(ctx context.Context) ([]Row, bool) {
	if s.opts.Enabled != nil && !s.opts.Enabled() {
		return nil, false
	}
	var threads []Thread
	if s.opts.Threads != nil {
		threads = s.opts.Threads(ctx)
	}
	anchored := false
	for _, th := range threads {
		if len(th.Anchors) > 0 {
			anchored = true
			break
		}
	}
	if !anchored && (s.opts.NicknamesHeld == nil || !s.opts.NicknamesHeld()) {
		return nil, false
	}
	if s.opts.Windows == nil {
		return nil, false
	}
	windows, err := s.opts.Windows(ctx)
	if err != nil {
		// An unreadable desktop must clear the overlays, not freeze them:
		// stale geometry pinned over a desktop we cannot see is exactly the
		// lie the feed exists to avoid. Still enrolled — the next tick
		// retries, and overlays converge when the compositor answers again.
		if ctx.Err() == nil {
			s.log.Debug("overlay inventory read failed; clearing overlays until it recovers",
				"component", "overlay", "error", err.Error())
		}
		return nil, true
	}
	var tags map[string]string
	if s.opts.Tags != nil {
		tags = s.opts.Tags(windows)
	}
	return Compose(true, windows, threads, tags), true
}

// publishIfChanged emits the rows when they differ from the last published
// set. nil and empty are the same statement — "nothing overlaid" — so the
// comparison normalises before deciding, and a daemon that boots with
// nothing enrolled publishes nothing at all.
func (s *Service) publishIfChanged(rows []Row) {
	if len(rows) == 0 {
		rows = nil
	}
	s.mu.Lock()
	changed := !reflect.DeepEqual(rows, s.last)
	if changed {
		s.last = rows
	}
	s.mu.Unlock()
	if changed && s.opts.Publish != nil {
		s.opts.Publish(rows)
	}
}
