package automation

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/quiesce"
	"github.com/rpickz/jarvix/internal/statehold"
)

// Kind is what a schedule fires: a routine or a script.
type Kind string

// Entry kinds.
const (
	KindRoutine Kind = "routine"
	KindScript  Kind = "script"
)

// Entry is one configured schedule. Nothing here is model-controlled or even
// daemon-invented: the kind, name, schedule and announce flag all come
// verbatim from a [[routines]] or [[scripts]] table the user wrote.
type Entry struct {
	Kind Kind
	Name string
	// Schedule is the parsed firing rule.
	Schedule Spec
	// Announce opts a firing's outcome into speech. It defaults to false and
	// that default is load-bearing (ADR 0032): a 02:00 backup must land as a
	// notification and an activity row, never as a voice in a dark house.
	Announce bool
}

// Options configure a Service. The seams exist so tests control every
// timestamp and never sleep.
type Options struct {
	// Entries are the configured schedules.
	Entries []Entry
	// Fire carries out one clockfire. It blocks until the run has finished —
	// that duration is exactly what the overlap skip measures — and it is the
	// daemon's, not this package's: the policy check, the refusal
	// notification, and the session entry all live with the caller, so this
	// scheduler knows time and nothing else.
	Fire func(ctx context.Context, e Entry)
	// Publish emits the scheduler's own report events (automation.fired /
	// automation.skipped / automation.missed) for the activity feed and the
	// window. Nil publishes nothing.
	Publish func(event string, data map[string]any)
	// Now is the clock; Timer creates one shot of it.
	Now   func() time.Time
	Timer func(d time.Duration) (<-chan time.Time, func())
	// Gate is the backup write barrier (ADR 0045); nil — the CLI, tests —
	// means writes are never held. Only the daemon threads one through.
	Gate *statehold.Gate
}

// Service owns the schedules: the loops, the last-run trail, and the
// missed-while-down boot report. All methods are safe for concurrent use.
type Service struct {
	path string
	// gate is the backup write barrier (ADR 0045); nil never blocks.
	gate    *statehold.Gate
	fire    func(ctx context.Context, e Entry)
	publish func(string, map[string]any)
	now     func() time.Time
	timer   func(d time.Duration) (<-chan time.Time, func())
	log     *slog.Logger

	// group tracks every scheduler goroutine — the loops and the in-flight
	// clockfires — from the moment it exists, never a bare `go` (the #74
	// lesson). Drain waits on it.
	group quiesce.Group

	mu      sync.Mutex
	entries []Entry
	states  map[string]*entryState
	// base is the lifetime context Start was given; schedule generations
	// derive from it so Drain's cancel reaches every loop and every run.
	base context.Context
	// cancelGen stops the current generation of loops. Reconfigure cancels it
	// and starts a new generation; the old goroutines unwind into the same
	// tracked group, so a reload can never orphan one.
	cancelGen context.CancelFunc
	closed    bool
}

// Status is one schedule's snapshot for the automations.schedules IPC
// surface: the next fire is computed here, daemon-side, so the tab never
// re-derives schedule arithmetic.
type Status struct {
	Kind     Kind
	Name     string
	Schedule string
	Announce bool
	NextFire time.Time
	// LastFired is when this schedule last actually fired; zero means never.
	LastFired time.Time
	// Running reports a clockfire still in flight.
	Running bool
}

// NewService builds the scheduler over the trail file at path.
func NewService(path string, opts Options, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	s := &Service{
		path:    path,
		gate:    opts.Gate,
		fire:    opts.Fire,
		publish: opts.Publish,
		now:     opts.Now,
		timer:   opts.Timer,
		log:     log,
		entries: append([]Entry(nil), opts.Entries...),
		states:  make(map[string]*entryState),
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.timer == nil {
		s.timer = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTimer(d)
			return t.C, func() { t.Stop() }
		}
	}
	return s
}

// Entries returns the configured schedules.
func (s *Service) Entries() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Entry(nil), s.entries...)
}

// Start loads the trail, reports anything missed while no daemon ran, and
// begins the loops. ctx is the service's lifetime: its cancellation reaches
// every loop and every in-flight run, which is what makes Drain's deadline
// effective.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	if s.closed || s.base != nil {
		s.mu.Unlock()
		return
	}
	s.base = ctx
	s.loadLocked()
	missed := s.bootScanLocked()
	s.startLocked()
	s.mu.Unlock()
	// Published outside the lock, like every event here: Publish reaches the
	// bus, and a report must never hold the scheduler's lock while it waits.
	for _, ev := range missed {
		s.emit("automation.missed", ev)
	}
}

// Reconfigure swaps in a new entry set — the reload path. The trail survives
// by identity (a renamed schedule starts cold), the previous generation of
// loops is cancelled into the same tracked group, and the new schedules start
// immediately.
func (s *Service) Reconfigure(entries []Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.entries = append([]Entry(nil), entries...)
	if s.base == nil {
		return // not started yet; Start will schedule the new set
	}
	if s.cancelGen != nil {
		s.cancelGen()
		s.cancelGen = nil
	}
	s.startLocked()
	s.log.Info("automation schedules reconfigured", "component", "automation",
		"schedules", len(entries))
}

// Drain stops the schedules and waits — bounded by ctx — for every tracked
// goroutine to finish. After it returns nothing of the scheduler survives,
// which is what lets daemon shutdown treat it as one more drained stage.
func (s *Service) Drain(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	if s.cancelGen != nil {
		s.cancelGen()
		s.cancelGen = nil
	}
	s.mu.Unlock()
	return s.group.Wait(ctx)
}

// InFlight reports how many scheduler goroutines are still running, for the
// shutdown log when a drain gives up.
func (s *Service) InFlight() int { return s.group.InFlight() }

// Status reports every schedule in declaration order, next-fire times
// computed against the injected clock.
func (s *Service) Status() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make([]Status, 0, len(s.entries))
	for _, e := range s.entries {
		st := Status{
			Kind: e.Kind, Name: e.Name,
			Schedule: e.Schedule.String(), Announce: e.Announce,
			NextFire: e.Schedule.Next(now),
		}
		if state := s.states[entryKey(e.Kind, e.Name)]; state != nil {
			st.LastFired = state.lastFired
			st.Running = state.running
		}
		out = append(out, st)
	}
	return out
}

// bootScanLocked reconciles the persisted trail with the clock: a schedule
// whose most recent occurrence fell after the last moment any daemon dealt
// with it was missed while down — reported once, never re-fired (ADR 0032).
// A schedule with no trail at all adopts silently: it is new, and "you
// missed what you only just configured" would be a false alarm. Returns the
// report payloads for the caller to publish off the lock. Callers hold s.mu.
func (s *Service) bootScanLocked() []map[string]any {
	if len(s.entries) == 0 {
		return nil
	}
	now := s.now()
	var missed []map[string]any
	for _, e := range s.entries {
		prev := e.Schedule.Prev(now)
		st := s.stateLocked(e)
		if !st.lastDue.IsZero() && st.lastDue.Before(prev) {
			missed = append(missed, map[string]any{
				"kind": string(e.Kind), "name": e.Name,
				"schedule": e.Schedule.String(),
				"due":      prev.Format(time.RFC3339),
			})
			s.log.Info("scheduled firing was missed while the daemon was off; reporting, not re-firing",
				"component", "automation", "kind", string(e.Kind), "name", e.Name,
				"due", prev.Format(time.RFC3339))
		}
		// Adopt the occurrence either way, and persist it now: the report (or
		// the silent adoption of a new schedule) must not repeat on the next
		// boot.
		st.lastDue = prev
	}
	s.saveLocked()
	return missed
}

// startLocked spawns one loop per entry under a fresh generation context.
// Callers hold s.mu and have set s.base.
func (s *Service) startLocked() {
	if len(s.entries) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(s.base)
	s.cancelGen = cancel
	for _, e := range s.entries {
		entry := e
		// Add before go, never inside the goroutine: a drain that started
		// between the two would otherwise return while a loop was starting.
		s.group.Go(func() { s.runSchedule(ctx, entry) })
	}
}

// runSchedule is one schedule's loop: arm a timer for the next occurrence,
// fire, repeat. Every wait goes through the timer seam and every run through
// the generation context, so tests drive it deterministically and shutdown
// ends it promptly.
func (s *Service) runSchedule(ctx context.Context, e Entry) {
	for {
		now := s.now()
		next := e.Schedule.Next(now)
		fire, stop := s.timer(next.Sub(now))
		select {
		case <-ctx.Done():
			stop()
			return
		case <-fire:
		}
		if ctx.Err() != nil {
			return
		}
		s.clockfire(ctx, e, next)
	}
}

// clockfire handles one scheduled moment: skip it with a report when the
// previous run is still going — a schedule is not a queue, and two
// overlapping runs of "backup my notes" racing over the same files is worse
// than one late one — or record last-run and dispatch the fire on its own
// tracked goroutine, so this loop keeps timing the next occurrence while the
// run goes (which is exactly what makes the overlap observable).
func (s *Service) clockfire(ctx context.Context, e Entry, due time.Time) {
	s.mu.Lock()
	st := s.stateLocked(e)
	st.lastDue = due
	if st.running {
		s.saveLocked()
		s.mu.Unlock()
		s.log.Info("scheduled firing skipped; the previous run is still going",
			"component", "automation", "kind", string(e.Kind), "name", e.Name)
		s.emit("automation.skipped", map[string]any{
			"kind": string(e.Kind), "name": e.Name, "schedule": e.Schedule.String(),
			"reason": "the last run is still going",
		})
		return
	}
	st.running = true
	st.lastFired = s.now()
	s.saveLocked()
	s.mu.Unlock()

	s.log.Info("schedule fired", "component", "automation",
		"kind", string(e.Kind), "name", e.Name, "schedule", e.Schedule.String())
	s.emit("automation.fired", map[string]any{
		"kind": string(e.Kind), "name": e.Name, "schedule": e.Schedule.String(),
		"announce": e.Announce,
	})
	s.group.Go(func() {
		defer func() {
			s.mu.Lock()
			st.running = false
			s.mu.Unlock()
		}()
		if s.fire != nil {
			s.fire(ctx, e)
		}
	})
}

// emit publishes one report event, if anyone is listening. Never called with
// s.mu held.
func (s *Service) emit(event string, data map[string]any) {
	if s.publish != nil {
		s.publish(event, data)
	}
}

// stateLocked returns the state for an entry, creating it cold. Callers hold
// s.mu.
func (s *Service) stateLocked(e Entry) *entryState {
	key := entryKey(e.Kind, e.Name)
	st := s.states[key]
	if st == nil {
		st = &entryState{}
		s.states[key] = st
	}
	return st
}

// loadLocked reads the persisted trail once, at Start. Failures degrade to a
// cold trail — one boot's missed report is all it costs. Callers hold s.mu.
func (s *Service) loadLocked() {
	states, err := readTrail(s.path)
	if err != nil {
		s.log.Warn("schedule trail could not be loaded; starting cold",
			"component", "automation", "error", err.Error())
		return
	}
	for key, st := range states {
		s.states[key] = st
	}
}

// saveLocked persists the trail for the configured entries — records for
// schedules no longer configured are dropped by the write. A failed write is
// a warning, not an error: the trail only feeds the boot report. Callers
// hold s.mu.
func (s *Service) saveLocked() {
	// Entered before the first byte moves, released once the trail is
	// settled: `jarvix backup` holds this gate for its coherent cut.
	defer s.gate.Enter()()
	if err := writeTrail(s.path, s.entries, s.states); err != nil {
		s.log.Warn("schedule trail could not be persisted",
			"component", "automation", "error", err.Error())
	}
}
