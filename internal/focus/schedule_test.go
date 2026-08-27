package focus

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The clockwork: check-in reminders fire on their interval and are skipped —
// never queued — while a timebox holds the floor; the timebox's midpoint and
// close latch state before they speak; and a restart resumes a live session
// or closes a blown one quietly. Everything runs on the injected clock and
// timer: no test here sleeps.

// manualTimer is the timer seam under test control. Every arm hands the loop
// the same unbuffered channel, so fire() is a rendezvous: it returns only
// once the scheduler has actually received the tick — the deterministic
// handshake that makes every assertion after a fire() a statement about a
// dispatch that has happened, not one that might.
type manualTimer struct {
	ch chan time.Time
}

func newManualTimer() *manualTimer {
	return &manualTimer{ch: make(chan time.Time)}
}

func (m *manualTimer) timer(time.Duration) (<-chan time.Time, func()) {
	return m.ch, func() {}
}

// fire delivers one tick and blocks until the loop takes it.
func (m *manualTimer) fire(t *testing.T) {
	t.Helper()
	select {
	case m.ch <- time.Time{}:
	case <-time.After(5 * time.Second):
		t.Fatal("the scheduler never took the tick — nothing is armed")
	}
}

// lockedClock is a hand-advanced clock safe to read from the scheduler
// goroutine.
type lockedClock struct {
	mu sync.Mutex
	t  time.Time
}

func newLockedClock() *lockedClock {
	return &lockedClock{t: time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)}
}

func (c *lockedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *lockedClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// scheduleHarness is one running service with captured firings and events.
type scheduleHarness struct {
	s       *Service
	clock   *lockedClock
	timers  *manualTimer
	firings chan Firing
	events  chan map[string]any
	cancel  context.CancelFunc
	drained bool
}

func startSchedule(t *testing.T, path string, midpoint bool) *scheduleHarness {
	t.Helper()
	h := &scheduleHarness{
		clock:   newLockedClock(),
		timers:  newManualTimer(),
		firings: make(chan Firing, 100),
		events:  make(chan map[string]any, 100),
	}
	h.s = NewService(path, Options{
		Now:   h.clock.now,
		Timer: h.timers.timer,
		Fire: func(_ context.Context, f Firing) {
			h.firings <- f
		},
		Publish: func(_ string, data map[string]any) {
			h.events <- data
		},
		Midpoint: func() bool { return midpoint },
	}, testLogger(t))
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	t.Cleanup(func() { h.settle(t) })
	h.s.Start(ctx)
	return h
}

// settle stops the loop and drains every in-flight firing goroutine, so an
// absence assertion afterwards is a fact, not a race.
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

// awaitFiring waits for one firing of the wanted kind.
func (h *scheduleHarness) awaitFiring(t *testing.T, kind FiringKind) Firing {
	t.Helper()
	for {
		select {
		case f := <-h.firings:
			if f.Kind == kind {
				return f
			}
			t.Fatalf("unexpected %s firing while waiting for %s", f.Kind, kind)
		case <-time.After(5 * time.Second):
			t.Fatalf("no %s firing arrived", kind)
		}
	}
}

// awaitEvent waits for one focus.changed payload with the wanted reason.
func (h *scheduleHarness) awaitEvent(t *testing.T, reason string) {
	t.Helper()
	for {
		select {
		case data := <-h.events:
			if data["reason"] == reason {
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no %q event arrived", reason)
		}
	}
}

func TestReminderFiresOnItsCadenceAndNeverBanks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "focus.toml")
	h := startSchedule(t, path, false)
	ctx := context.Background()
	if _, _, err := h.s.Create(ctx, "deploy", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Remind(45); err != nil {
		t.Fatal(err)
	}
	// Not due yet: a tick before the interval fires nothing.
	h.timers.fire(t)
	// Due: one interval passes, one firing arrives.
	h.clock.advance(46 * time.Minute)
	h.timers.fire(t)
	f := h.awaitFiring(t, FiringReminder)
	if f.Thread.Name != "deploy" {
		t.Errorf("reminder fired for %+v", f.Thread)
	}
	// The next tick is one whole interval from the firing — a cadence, not a
	// queue: an immediate tick fires nothing again.
	h.timers.fire(t)
	h.settle(t)
	select {
	case f := <-h.firings:
		t.Fatalf("a reminder banked and poured out: %+v", f)
	default:
	}
}

// TestReminderIsSkippedWhileATimeboxRuns is the monotask half of do-not-nag:
// a live focus session silences every check-in, and a skipped tick is
// rescheduled — never queued into a backlog that pours out later. Ending the
// session proves the machinery both ways: the next tick after the session is
// the first one heard.
func TestReminderIsSkippedWhileATimeboxRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "focus.toml")
	h := startSchedule(t, path, false)
	ctx := context.Background()
	if _, _, err := h.s.Create(ctx, "deploy", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.s.Create(ctx, "reviews", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.RemindThread("deploy", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.StartSession(ctx, "reviews", 60); err != nil {
		t.Fatal(err)
	}
	// Three reminder intervals pass inside the timebox: every tick must be
	// skipped, not banked.
	for range 3 {
		h.clock.advance(11 * time.Minute)
		h.timers.fire(t)
	}
	if _, err := h.s.EndSession(); err != nil {
		t.Fatal(err)
	}
	// The first interval after the session ends is the first one heard.
	h.clock.advance(11 * time.Minute)
	h.timers.fire(t)
	h.awaitFiring(t, FiringReminder)
	h.settle(t)
	select {
	case f := <-h.firings:
		t.Fatalf("a skipped check-in poured out later: %+v", f)
	default:
	}
}

func TestTimeboxMidpointAndCloseLatchBeforeTheySpeak(t *testing.T) {
	path := filepath.Join(t.TempDir(), "focus.toml")
	h := startSchedule(t, path, true)
	ctx := context.Background()
	if _, _, err := h.s.Create(ctx, "deploy", 0); err != nil {
		t.Fatal(err)
	}
	ack, err := h.s.StartSession(ctx, "deploy", 24)
	if err != nil {
		t.Fatal(err)
	}
	if ack != "Focusing on deploy for twenty-four minutes." {
		t.Errorf("start ack = %q", ack)
	}

	// Midpoint: the firing latches the due flag; the tick phrase speaks it
	// exactly once, then reports remaining time.
	h.clock.advance(13 * time.Minute)
	h.timers.fire(t)
	h.awaitFiring(t, FiringMidpoint)
	line, err := h.s.Tick()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "Halfway — ") || !strings.Contains(line, "on deploy") {
		t.Errorf("midpoint line = %q", line)
	}
	line, err = h.s.Tick()
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(line, "Halfway") {
		t.Errorf("the midpoint line repeated: %q", line)
	}

	// Close: time runs out, Closing latches at dispatch — so a refused
	// spoken attempt cannot re-fire — and the prompt repeats from Tick until
	// answered.
	h.clock.advance(12 * time.Minute)
	h.timers.fire(t)
	h.awaitFiring(t, FiringClose)
	for range 2 {
		line, err = h.s.Tick()
		if err != nil {
			t.Fatal(err)
		}
		if line != "Time — twenty-four minutes on deploy. Keep focusing, or take a break?" {
			t.Fatalf("close prompt = %q", line)
		}
	}

	// "Keep focusing" starts another round of the same shape.
	ack, err = h.s.Continue()
	if err != nil {
		t.Fatal(err)
	}
	if ack != "Another twenty-four minutes on deploy." {
		t.Errorf("continue ack = %q", ack)
	}
	if v := h.s.Snapshot(ctx); v.Session == nil || v.Session.Phase != "running" {
		t.Errorf("session after continue = %+v", v.Session)
	}
}

func TestUnansweredCloseExpiresQuietly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "focus.toml")
	h := startSchedule(t, path, false)
	ctx := context.Background()
	if _, _, err := h.s.Create(ctx, "deploy", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.StartSession(ctx, "deploy", 10); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(11 * time.Minute)
	h.timers.fire(t)
	h.awaitFiring(t, FiringClose)
	// The continue-or-break question goes unanswered for the whole window:
	// the record follows the user, quietly — an event for the tab, no voice.
	h.clock.advance(closingAnswerWindow + time.Minute)
	h.timers.fire(t)
	h.awaitEvent(t, "session_ended")
	if v := h.s.Snapshot(ctx); v.Session != nil {
		t.Errorf("the expired session survived: %+v", v.Session)
	}
	h.settle(t)
	select {
	case f := <-h.firings:
		t.Fatalf("an expired close still spoke: %+v", f)
	default:
	}
}

func TestRestartResumesALiveTimeboxAndClosesABlownOneQuietly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "focus.toml")
	h := startSchedule(t, path, false)
	ctx := context.Background()
	if _, _, err := h.s.Create(ctx, "deploy", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.StartSession(ctx, "deploy", 25); err != nil {
		t.Fatal(err)
	}
	h.settle(t)

	// Restart inside the window: the countdown resumes.
	h.clock.advance(10 * time.Minute)
	firings := make(chan Firing, 100)
	resumed := NewService(path, Options{
		Now: h.clock.now, Timer: newManualTimer().timer,
		Fire: func(_ context.Context, f Firing) { firings <- f },
	}, testLogger(t))
	ctx2, cancel2 := context.WithCancel(context.Background())
	resumed.Start(ctx2)
	if v := resumed.Snapshot(ctx); v.Session == nil || v.Session.Phase != "running" {
		t.Fatalf("a live timebox did not survive the restart: %+v", v.Session)
	}
	cancel2()
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := resumed.Drain(drainCtx); err != nil {
		t.Fatal(err)
	}
	drainCancel()

	// Restart long after the window blew: closed quietly, never re-announced
	// (the ADR 0032 missed-while-down stance).
	h.clock.advance(2 * time.Hour)
	blown := NewService(path, Options{
		Now: h.clock.now, Timer: newManualTimer().timer,
		Fire: func(_ context.Context, f Firing) { firings <- f },
	}, testLogger(t))
	ctx3, cancel3 := context.WithCancel(context.Background())
	blown.Start(ctx3)
	if v := blown.Snapshot(ctx); v.Session != nil {
		t.Errorf("a blown timebox survived the restart: %+v", v.Session)
	}
	cancel3()
	drainCtx3, drainCancel3 := context.WithTimeout(context.Background(), 5*time.Second)
	if err := blown.Drain(drainCtx3); err != nil {
		t.Fatal(err)
	}
	drainCancel3()
	select {
	case f := <-firings:
		t.Errorf("a restart re-announced a missed moment: %+v", f)
	default:
	}
}
