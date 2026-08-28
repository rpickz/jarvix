package reminders

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The clockwork: a moment arrives and is spoken through the fire seam; a
// moment behind a live session defers to the boundary and is delivered
// exactly once there (the #136 lesson, owed variant); a moment missed while
// no daemon ran fires once at boot, marked late; and a deferral survives a
// restart. Everything runs on the injected clock and timer: no test here
// sleeps.

// manualTimer is the timer seam under test control (the focus harness's).
// Every arm hands the loop the same unbuffered channel, so fire() is a
// rendezvous: it returns only once the scheduler has actually received the
// tick.
type manualTimer struct {
	ch chan time.Time
}

func newManualTimer() *manualTimer { return &manualTimer{ch: make(chan time.Time)} }

func (m *manualTimer) timer(time.Duration) (<-chan time.Time, func()) {
	return m.ch, func() {}
}

func (m *manualTimer) fire(t *testing.T) {
	t.Helper()
	select {
	case m.ch <- time.Time{}:
	case <-time.After(5 * time.Second):
		t.Fatal("the scheduler never took the tick — nothing is armed")
	}
}

// attemptOutcome scripts one delivery attempt for the harness fire seam.
type attemptOutcome struct {
	// refuse reports the floor as taken: no claim runs, false returns.
	refuse bool
}

// scheduleHarness is one running service with a scripted fire seam and
// captured events.
type scheduleHarness struct {
	s      *Service
	clock  *fixedClock
	timers *manualTimer
	// script is consumed one entry per attempt; an empty script claims.
	mu     sync.Mutex
	script []attemptOutcome
	// spoken records every claimed announcement, in delivery order.
	spoken []string
	// attempts counts fire-seam calls; delivered counts claimed reminders.
	attempts  atomic.Int64
	delivered atomic.Int64
	events    chan map[string]any
	cancel    context.CancelFunc
	drained   bool
}

func startSchedule(t *testing.T, path string) *scheduleHarness {
	t.Helper()
	h := &scheduleHarness{
		clock:  newFixedClock(),
		timers: newManualTimer(),
		events: make(chan map[string]any, 100),
	}
	h.s = NewService(path, Options{
		Now:   h.clock.now,
		Timer: h.timers.timer,
		Fire:  h.fire,
		Publish: func(_ string, data map[string]any) {
			h.events <- data
		},
	}, testLogger(t))
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	t.Cleanup(func() { h.settle(t) })
	h.s.Start(ctx)
	return h
}

// fire is the scripted delivery seam: a refusal returns false without a
// claim — exactly what the daemon does when StartScheduledSession is refused
// — and anything else claims, the way the real delivery session's phrase
// dispatch does.
func (h *scheduleHarness) fire(context.Context) bool {
	h.attempts.Add(1)
	h.mu.Lock()
	outcome := attemptOutcome{}
	if len(h.script) > 0 {
		outcome, h.script = h.script[0], h.script[1:]
	}
	h.mu.Unlock()
	if outcome.refuse {
		return false
	}
	spoken, n := h.s.ClaimDue()
	if n > 0 {
		h.mu.Lock()
		h.spoken = append(h.spoken, spoken)
		h.mu.Unlock()
		h.delivered.Add(int64(n))
	}
	return true
}

func (h *scheduleHarness) scriptNext(outcomes ...attemptOutcome) {
	h.mu.Lock()
	h.script = append(h.script, outcomes...)
	h.mu.Unlock()
}

func (h *scheduleHarness) spokenLines() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.spoken...)
}

// settle stops the loop and drains every in-flight attempt, so an absence
// assertion afterwards is a fact, not a race.
func (h *scheduleHarness) settle(t *testing.T) {
	t.Helper()
	if h.drained {
		return
	}
	h.drained = true
	h.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.s.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

// awaitEvent waits for one reminders.changed payload with the wanted reason.
func (h *scheduleHarness) awaitEvent(t *testing.T, reason string) map[string]any {
	t.Helper()
	for {
		select {
		case data := <-h.events:
			if data["reason"] == reason {
				return data
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no %q event arrived", reason)
		}
	}
}

func TestMomentArrivesAndIsSpokenOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	h := startSchedule(t, path)
	if _, err := h.s.Create("in ten minutes", "stretch"); err != nil {
		t.Fatal(err)
	}
	// Not due yet: a tick before the moment attempts nothing.
	h.timers.fire(t)
	// Due: the moment arrives, the attempt claims, the line is spoken.
	h.clock.advance(11 * time.Minute)
	h.timers.fire(t)
	h.awaitEvent(t, "fired")
	h.settle(t)
	if got := h.spokenLines(); len(got) != 1 || got[0] != "Reminder: stretch." {
		t.Fatalf("spoken = %q", got)
	}
	if h.attempts.Load() != 1 || h.delivered.Load() != 1 {
		t.Errorf("attempts = %d, delivered = %d; want 1 and 1",
			h.attempts.Load(), h.delivered.Load())
	}
	if owed := h.s.Owed(); owed != 0 {
		t.Errorf("owed after delivery = %d", owed)
	}
}

// TestDueBehindALiveSessionDefersToTheBoundary is the #136 lesson, owed
// variant: a moment that arrives while a session holds the floor is parked —
// one refused attempt, no retry loop — and the boundary (FlushOwed, the
// daemon's session-end watcher) delivers it exactly ONCE, marked late in the
// spoken line once past the grace. Never lost, never doubled.
func TestDueBehindALiveSessionDefersToTheBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	h := startSchedule(t, path)
	if _, err := h.s.Create("in ten minutes", "call the pharmacy"); err != nil {
		t.Fatal(err)
	}
	// A pre-due tick is the rendezvous that proves the loop is parked on
	// the timer before the clock moves (the focus harness's discipline).
	h.timers.fire(t)
	// The moment arrives behind a live session: the one attempt is refused.
	h.scriptNext(attemptOutcome{refuse: true})
	h.clock.advance(11 * time.Minute)
	h.timers.fire(t)
	h.awaitEvent(t, "deferred")
	if owed := h.s.Owed(); owed != 1 {
		t.Fatalf("owed after deferral = %d; the reminder was lost", owed)
	}
	// A stray wake is not a boundary: the parked reminder must not be
	// re-attempted by it (the total-attempts assertion below is the proof).
	h.s.Rearm()

	// The session runs long: the boundary lands six minutes past the moment.
	h.clock.advance(5 * time.Minute)
	h.s.FlushOwed()
	h.awaitEvent(t, "fired")
	h.settle(t)

	got := h.spokenLines()
	if len(got) != 1 || got[0] != "Reminder, six minutes late: call the pharmacy." {
		t.Fatalf("spoken = %q; want the one late-marked line", got)
	}
	if h.attempts.Load() != 2 {
		t.Errorf("attempts = %d; want exactly the refused one and the boundary one", h.attempts.Load())
	}
	if h.delivered.Load() != 1 {
		t.Errorf("delivered = %d; a deferred reminder poured out more than once", h.delivered.Load())
	}
	if owed := h.s.Owed(); owed != 0 {
		t.Errorf("owed after the boundary = %d", owed)
	}
}

// TestBoundaryInsideTheGraceSpeaksPlainly: a deferral released within two
// minutes of the moment is not branded late — the do-not-nag discipline
// should not turn a thirty-second wait into an apology.
func TestBoundaryInsideTheGraceSpeaksPlainly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	h := startSchedule(t, path)
	if _, err := h.s.Create("in ten minutes", "stretch"); err != nil {
		t.Fatal(err)
	}
	h.timers.fire(t) // rendezvous: armed before the clock moves
	h.scriptNext(attemptOutcome{refuse: true})
	h.clock.advance(10*time.Minute + 30*time.Second)
	h.timers.fire(t)
	h.awaitEvent(t, "deferred")
	h.clock.advance(time.Minute)
	h.s.FlushOwed()
	h.awaitEvent(t, "fired")
	h.settle(t)
	if got := h.spokenLines(); len(got) != 1 || got[0] != "Reminder: stretch." {
		t.Fatalf("spoken = %q", got)
	}
}

// TestCancelClearsADeferredDebt: cancelling a parked reminder is honoured —
// the next boundary delivers nothing.
func TestCancelClearsADeferredDebt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	h := startSchedule(t, path)
	if _, err := h.s.Create("in ten minutes", "call the pharmacy"); err != nil {
		t.Fatal(err)
	}
	h.timers.fire(t) // rendezvous: armed before the clock moves
	h.scriptNext(attemptOutcome{refuse: true})
	h.clock.advance(11 * time.Minute)
	h.timers.fire(t)
	h.awaitEvent(t, "deferred")
	if _, err := h.s.Cancel("pharmacy"); err != nil {
		t.Fatal(err)
	}
	h.s.FlushOwed()
	h.settle(t)
	if h.delivered.Load() != 0 {
		t.Errorf("a cancelled reminder was still delivered")
	}
}

// TestBootFiresAMissedReminderOnceMarkedLate: the daemon was down at the
// moment. One late fire at boot — "While I was off: …" — never silently
// dropped, never a backlog storm.
func TestBootFiresAMissedReminderOnceMarkedLate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	first, clock := newTestService(t, path)
	if _, err := first.Create("in five minutes", "call the pharmacy"); err != nil {
		t.Fatal(err)
	}
	// No scheduler was started on `first`: the moment simply passes with no
	// daemon awake, three hours of downtime.
	clock.advance(3 * time.Hour)

	h := &scheduleHarness{clock: clock, timers: newManualTimer(),
		events: make(chan map[string]any, 100)}
	h.s = NewService(path, Options{
		Now: clock.now, Timer: h.timers.timer, Fire: h.fire,
		Publish: func(_ string, data map[string]any) { h.events <- data },
	}, testLogger(t))
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	t.Cleanup(func() { h.settle(t) })
	h.s.Start(ctx)

	h.awaitEvent(t, "fired")
	h.settle(t)
	got := h.spokenLines()
	want := "While I was off: you asked me to remind you to call the pharmacy at one oh five."
	if len(got) != 1 || got[0] != want {
		t.Fatalf("boot fire = %q\nwant       %q", got, want)
	}
	if h.attempts.Load() != 1 || h.delivered.Load() != 1 {
		t.Errorf("attempts = %d, delivered = %d; want one late fire",
			h.attempts.Load(), h.delivered.Load())
	}
	if owed := h.s.Owed(); owed != 0 {
		t.Errorf("owed after the boot fire = %d", owed)
	}
}

// TestSeveralMissedAtBootArriveAsOneAnnouncement: however many moments
// passed while the daemon was off, boot speaks one catch-up, not a storm.
func TestSeveralMissedAtBootArriveAsOneAnnouncement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	first, clock := newTestService(t, path)
	if _, err := first.Create("in five minutes", "call the pharmacy"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Create("in ten minutes", "stretch"); err != nil {
		t.Fatal(err)
	}
	clock.advance(2 * time.Hour)

	h := &scheduleHarness{clock: clock, timers: newManualTimer(),
		events: make(chan map[string]any, 100)}
	h.s = NewService(path, Options{
		Now: clock.now, Timer: h.timers.timer, Fire: h.fire,
		Publish: func(_ string, data map[string]any) { h.events <- data },
	}, testLogger(t))
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	t.Cleanup(func() { h.settle(t) })
	h.s.Start(ctx)

	h.awaitEvent(t, "fired")
	h.settle(t)
	got := h.spokenLines()
	if len(got) != 1 || !strings.HasPrefix(got[0], "While I was off: you asked me to remind you to ") ||
		!strings.Contains(got[0], ", and to ") {
		t.Fatalf("boot catch-up = %q; want one combined announcement", got)
	}
	if h.attempts.Load() != 1 {
		t.Errorf("attempts = %d; the missed moments arrived as a storm", h.attempts.Load())
	}
}

// TestDeferredSurvivesARestart: a reminder parked behind a session when the
// daemon dies is still owed on disk, so the next boot's late fire delivers
// it — deferral can delay a reminder, never lose one.
func TestDeferredSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	h := startSchedule(t, path)
	if _, err := h.s.Create("in ten minutes", "call the pharmacy"); err != nil {
		t.Fatal(err)
	}
	h.timers.fire(t) // rendezvous: armed before the clock moves
	h.scriptNext(attemptOutcome{refuse: true})
	h.clock.advance(11 * time.Minute)
	h.timers.fire(t)
	h.awaitEvent(t, "deferred")
	h.settle(t) // the daemon dies with the reminder still parked

	h2 := &scheduleHarness{clock: h.clock, timers: newManualTimer(),
		events: make(chan map[string]any, 100)}
	h2.s = NewService(path, Options{
		Now: h.clock.now, Timer: h2.timers.timer, Fire: h2.fire,
		Publish: func(_ string, data map[string]any) { h2.events <- data },
	}, testLogger(t))
	ctx, cancel := context.WithCancel(context.Background())
	h2.cancel = cancel
	t.Cleanup(func() { h2.settle(t) })
	h2.s.Start(ctx)

	h2.awaitEvent(t, "fired")
	h2.settle(t)
	got := h2.spokenLines()
	if len(got) != 1 || !strings.HasPrefix(got[0], "While I was off: ") {
		t.Fatalf("post-restart delivery = %q", got)
	}
	if h2.delivered.Load() != 1 {
		t.Errorf("delivered = %d", h2.delivered.Load())
	}
}

// TestBoundaryStormNeverDoublesADelivery hammers the boundary from many
// goroutines while attempts alternate between refusal and delivery — the
// GOMAXPROCS=2 -race stress target. Whatever the interleaving, every
// reminder is delivered exactly once: the claim under the store's lock is
// the only owed→delivered transition there is.
func TestBoundaryStormNeverDoublesADelivery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	h := &scheduleHarness{clock: newFixedClock(), events: make(chan map[string]any, 1000)}
	// A permissive timer: always ready, so the loop never parks on a moment
	// — the storm of Rearms below drives it instead.
	readyTimer := func(time.Duration) (<-chan time.Time, func()) {
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch, func() {}
	}
	h.s = NewService(path, Options{
		Now: h.clock.now, Timer: readyTimer, Fire: h.fire,
		Publish: func(_ string, data map[string]any) {
			select {
			case h.events <- data:
			default:
			}
		},
	}, testLogger(t))

	const total = 6
	for range total {
		if _, err := h.s.Create("in one minute", "blink"); err != nil {
			t.Fatal(err)
		}
	}
	h.clock.advance(2 * time.Minute)
	// Refuse the first few attempts so deferrals and boundary releases
	// interleave with fresh dispatches.
	h.scriptNext(attemptOutcome{refuse: true}, attemptOutcome{refuse: true},
		attemptOutcome{refuse: true})

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	t.Cleanup(func() { h.settle(t) })
	h.s.Start(ctx)

	// The storm: boundaries and wakes from every direction.
	var storm sync.WaitGroup
	for range 8 {
		storm.Add(1)
		go func() {
			defer storm.Done()
			for range 200 {
				h.s.FlushOwed()
				h.s.Rearm()
			}
		}()
	}
	storm.Wait()

	// Drive boundaries until nothing is owed; every step is event-bounded,
	// so a lost reminder fails loudly rather than hanging.
	for range 100 {
		if h.s.Owed() == 0 {
			break
		}
		h.s.FlushOwed()
		select {
		case <-h.events:
		case <-time.After(5 * time.Second):
			t.Fatalf("owed reminders stopped making progress; owed = %d", h.s.Owed())
		}
	}
	h.settle(t)

	if h.delivered.Load() != total {
		t.Fatalf("delivered = %d, want exactly %d — %s", h.delivered.Load(), total,
			"a boundary lost or doubled a reminder")
	}
	if owed := h.s.Owed(); owed != 0 {
		t.Errorf("owed after the storm = %d", owed)
	}
	v := h.s.Snapshot()
	seen := map[string]bool{}
	for _, f := range v.History {
		if f.Outcome != OutcomeFired {
			t.Errorf("history outcome = %+v", f)
		}
		if seen[f.ID] {
			t.Errorf("reminder %s appears twice in history", f.ID)
		}
		seen[f.ID] = true
	}
	if len(v.History) != total {
		t.Errorf("history = %d entries, want %d", len(v.History), total)
	}
}
