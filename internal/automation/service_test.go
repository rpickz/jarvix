package automation

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The scheduler is exercised entirely through its seams: an injected clock,
// an injected timer, and a stubbed fire callback. No test sleeps and no test
// runs a routine or script — a clockfire is driven by firing the exact timer
// the scheduler armed, and every test ends by draining the tracked group,
// because a leaked loop is the bug class this component exists to prevent
// (#74, the ADR 0031 discipline).

func testLogger(_ *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeClock is a hand-advanced clock shared by the service and the test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	// A Thursday noon, in a fixed non-UTC zone: schedules are wall-clock.
	return &fakeClock{t: time.Date(2026, 8, 20, 12, 0, 0, 0, time.FixedZone("test", 3600))}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// fakeTimers records every timer the scheduler arms and lets the test fire
// them; waiting for the nth arm is the tests' only synchronisation point.
type fakeTimers struct {
	mu     sync.Mutex
	armed  []*armedTimer
	notify chan struct{}
}

type armedTimer struct {
	d  time.Duration
	ch chan time.Time
}

func newFakeTimers() *fakeTimers {
	return &fakeTimers{notify: make(chan struct{}, 1024)}
}

func (f *fakeTimers) Timer(d time.Duration) (<-chan time.Time, func()) {
	f.mu.Lock()
	at := &armedTimer{d: d, ch: make(chan time.Time, 1)}
	f.armed = append(f.armed, at)
	f.mu.Unlock()
	f.notify <- struct{}{}
	return at.ch, func() {}
}

func (f *fakeTimers) waitArmed(t *testing.T, n int) *armedTimer {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		f.mu.Lock()
		if len(f.armed) >= n {
			at := f.armed[n-1]
			f.mu.Unlock()
			return at
		}
		f.mu.Unlock()
		select {
		case <-f.notify:
		case <-deadline:
			t.Fatalf("timer %d was never armed", n)
		}
	}
}

func (f *fakeTimers) fire(t *testing.T, n int, at time.Time) {
	t.Helper()
	f.mu.Lock()
	timer := f.armed[n-1]
	f.mu.Unlock()
	timer.ch <- at
}

// stubFire records every clockfire and can park until cancelled — the
// deterministic stand-in for a run still going at the next fire.
type stubFire struct {
	mu      sync.Mutex
	calls   []Entry
	block   bool
	release chan struct{}
	started chan struct{} // closed on first call
}

func newStubFire() *stubFire {
	return &stubFire{release: make(chan struct{}), started: make(chan struct{})}
}

func (f *stubFire) fire(ctx context.Context, e Entry) {
	f.mu.Lock()
	f.calls = append(f.calls, e)
	first := len(f.calls) == 1
	block := f.block
	f.mu.Unlock()
	if first {
		close(f.started)
	}
	if block {
		select {
		case <-f.release:
		case <-ctx.Done():
		}
	}
}

func (f *stubFire) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// eventLog records everything the scheduler publishes.
type eventLog struct {
	mu     sync.Mutex
	events []publishedEvent
}

type publishedEvent struct {
	event string
	data  map[string]any
}

func (l *eventLog) publish(event string, data map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, publishedEvent{event, data})
}

func (l *eventLog) ofType(event string) []publishedEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []publishedEvent
	for _, e := range l.events {
		if e.event == event {
			out = append(out, e)
		}
	}
	return out
}

func nightly(t *testing.T, name string) Entry {
	t.Helper()
	return Entry{Kind: KindScript, Name: name, Schedule: mustSpec(t, "02:00")}
}

// testService builds a service over a temp trail file with every seam
// injected.
func testService(t *testing.T, clock *fakeClock, timers *fakeTimers, fire *stubFire, log *eventLog, entries ...Entry) *Service {
	t.Helper()
	return NewService(filepath.Join(t.TempDir(), "automations.toml"), Options{
		Entries: entries,
		Fire:    fire.fire,
		Publish: log.publish,
		Now:     clock.Now,
		Timer:   timers.Timer,
	}, testLogger(t))
}

// drain stops the service and requires it to settle: the assertion every
// scheduler test ends with.
func drain(t *testing.T, s *Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Drain(ctx); err != nil {
		t.Fatalf("scheduler had not quiesced: %v (in flight: %d)", err, s.InFlight())
	}
	if n := s.InFlight(); n != 0 {
		t.Fatalf("drain returned with %d goroutines in flight", n)
	}
}

// TestShutdownDrainStopsABlockedFire is the supervised-component contract,
// written before the scheduler was (the #74/#84 discipline): shutdown must
// cancel an in-flight clockfire and wait for every scheduler goroutine — a
// stopping daemon never abandons or orphans one.
func TestShutdownDrainStopsABlockedFire(t *testing.T) {
	clock := newFakeClock()
	timers := newFakeTimers()
	fire := newStubFire()
	fire.block = true
	log := &eventLog{}
	s := testService(t, clock, timers, fire, log, nightly(t, "backup notes"))
	s.Start(context.Background())

	at := timers.waitArmed(t, 1)
	if at.d != 14*time.Hour { // noon → 02:00 tomorrow
		t.Fatalf("first timer armed for %v, want 14h", at.d)
	}
	clock.Set(clock.Now().Add(at.d))
	timers.fire(t, 1, clock.Now())
	<-fire.started // the clockfire is genuinely in flight, parked on its ctx
	// The tracking is the contract: the goroutine holding that run must be
	// visible to the drain, or shutdown would return while it still ran.
	if n := s.InFlight(); n == 0 {
		t.Fatal("an in-flight clockfire is not tracked; shutdown could abandon it (#74)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Drain(ctx); err != nil {
		t.Fatalf("drain did not settle with a clockfire in flight: %v", err)
	}
	if n := s.InFlight(); n != 0 {
		t.Fatalf("%d scheduler goroutines survived the drain", n)
	}
}

// TestScheduleFiresAtItsMoment is the headline: the loop arms a timer for
// exactly the next occurrence, fires through the callback once, records
// last-run, and re-arms for the following day.
func TestScheduleFiresAtItsMoment(t *testing.T) {
	clock := newFakeClock()
	timers := newFakeTimers()
	fire := newStubFire()
	log := &eventLog{}
	s := testService(t, clock, timers, fire, log, nightly(t, "backup notes"))
	s.Start(context.Background())
	defer drain(t, s)

	if at := timers.waitArmed(t, 1); at.d != 14*time.Hour {
		t.Fatalf("armed for %v, want 14h to 02:00", at.d)
	}
	firedAt := clock.Now().Add(14 * time.Hour)
	clock.Set(firedAt)
	timers.fire(t, 1, clock.Now())
	// The next arm proves the fire was dispatched and the loop lives on.
	if at := timers.waitArmed(t, 2); at.d != 24*time.Hour {
		t.Fatalf("re-armed for %v, want the next day's 24h", at.d)
	}
	<-fire.started // the dispatched clockfire has reached the callback
	if n := fire.count(); n != 1 {
		t.Fatalf("fires = %d, want exactly one per fired timer", n)
	}
	if events := log.ofType("automation.fired"); len(events) != 1 ||
		events[0].data["name"] != "backup notes" || events[0].data["kind"] != "script" {
		t.Fatalf("automation.fired events = %+v", events)
	}

	st := s.Status()
	if len(st) != 1 || !st[0].LastFired.Equal(firedAt) {
		t.Fatalf("status = %+v, want last-run recorded at the firing", st)
	}
	if want := firedAt.Add(24 * time.Hour); !st[0].NextFire.Equal(want) {
		t.Errorf("next fire = %v, want %v", st[0].NextFire, want)
	}
}

// TestOverlapIsSkippedNeverQueued: a firing that arrives while the previous
// run is still going is skipped with a report row — exactly one run, exactly
// one skip, and no queued second run when the first finally ends.
func TestOverlapIsSkippedNeverQueued(t *testing.T) {
	clock := newFakeClock()
	timers := newFakeTimers()
	fire := newStubFire()
	fire.block = true
	log := &eventLog{}
	s := testService(t, clock, timers, fire, log, nightly(t, "backup notes"))
	s.Start(context.Background())
	defer drain(t, s)

	timers.waitArmed(t, 1)
	clock.Set(clock.Now().Add(14 * time.Hour))
	timers.fire(t, 1, clock.Now())
	<-fire.started
	timers.waitArmed(t, 2) // the loop is already timing the next day

	// The next day arrives with the first run still parked.
	clock.Set(clock.Now().Add(24 * time.Hour))
	timers.fire(t, 2, clock.Now())
	timers.waitArmed(t, 3) // the skip re-armed the loop

	if n := fire.count(); n != 1 {
		t.Fatalf("fires = %d, want the overlap skipped, never a second run", n)
	}
	skips := log.ofType("automation.skipped")
	if len(skips) != 1 || skips[0].data["name"] != "backup notes" {
		t.Fatalf("automation.skipped events = %+v, want exactly one", skips)
	}
	if reason, _ := skips[0].data["reason"].(string); reason == "" {
		t.Error("the skip row carries no reason")
	}

	// Releasing the first run must not replay the skipped firing.
	close(fire.release)
	drain(t, s)
	if n := fire.count(); n != 1 {
		t.Errorf("fires = %d after release, want the skipped firing never queued", n)
	}
}

// TestMissedWhileDownIsReportedOnceAtBoot: a firing that fell while no
// daemon was running produces one boot-time report row — and is never
// re-fired, and never re-reported by the next boot.
func TestMissedWhileDownIsReportedOnceAtBoot(t *testing.T) {
	clock := newFakeClock()
	timers := newFakeTimers()
	fire := newStubFire()
	log := &eventLog{}
	path := filepath.Join(t.TempDir(), "automations.toml")
	entry := nightly(t, "backup notes")
	s := NewService(path, Options{
		Entries: []Entry{entry}, Fire: fire.fire, Publish: log.publish,
		Now: clock.Now, Timer: timers.Timer,
	}, testLogger(t))
	s.Start(context.Background())
	// A brand new schedule adopts silently: there is no trail to have missed.
	if missed := log.ofType("automation.missed"); len(missed) != 0 {
		t.Fatalf("a first boot reported missed firings: %+v", missed)
	}
	timers.waitArmed(t, 1)
	drain(t, s)

	// The daemon is down over 02:00, and boots the next morning.
	clock.Set(clock.Now().Add(20 * time.Hour)) // 08:00 next day
	log2 := &eventLog{}
	timers2 := newFakeTimers()
	s2 := NewService(path, Options{
		Entries: []Entry{entry}, Fire: fire.fire, Publish: log2.publish,
		Now: clock.Now, Timer: timers2.Timer,
	}, testLogger(t))
	s2.Start(context.Background())
	missed := log2.ofType("automation.missed")
	if len(missed) != 1 || missed[0].data["name"] != "backup notes" {
		t.Fatalf("automation.missed events = %+v, want exactly one", missed)
	}
	if due, _ := missed[0].data["due"].(string); due == "" {
		t.Error("the missed row does not say when the firing was due")
	}
	// Never re-fired: the only timer armed is for the *next* occurrence.
	if at := timers2.waitArmed(t, 1); at.d != 18*time.Hour {
		t.Errorf("boot armed for %v, want 18h to the next 02:00 — a missed firing is reported, not replayed", at.d)
	}
	if n := fire.count(); n != 0 {
		t.Errorf("fires = %d, want a missed firing never re-fired", n)
	}
	drain(t, s2)

	// A restart five minutes later must not repeat the report.
	clock.Set(clock.Now().Add(5 * time.Minute))
	log3 := &eventLog{}
	timers3 := newFakeTimers()
	s3 := NewService(path, Options{
		Entries: []Entry{entry}, Fire: fire.fire, Publish: log3.publish,
		Now: clock.Now, Timer: timers3.Timer,
	}, testLogger(t))
	s3.Start(context.Background())
	defer drain(t, s3)
	if missed := log3.ofType("automation.missed"); len(missed) != 0 {
		t.Errorf("the second boot repeated the missed report: %+v", missed)
	}
}

// TestReconfigureRebuildsSchedules: a reload swaps the entry set — the old
// entry's loop dies without ever firing again, the new entry's schedule
// starts, and the whole thing still drains clean.
func TestReconfigureRebuildsSchedules(t *testing.T) {
	clock := newFakeClock()
	timers := newFakeTimers()
	fire := newStubFire()
	log := &eventLog{}
	s := testService(t, clock, timers, fire, log, nightly(t, "backup notes"))
	s.Start(context.Background())

	timers.waitArmed(t, 1) // the old entry's timer, never fired

	s.Reconfigure([]Entry{{Kind: KindRoutine, Name: "morning setup", Schedule: mustSpec(t, "08:30")}})
	timers.waitArmed(t, 2) // the new entry's timer

	// Firing the orphaned timer must do nothing: its loop's generation was
	// cancelled by the reconfigure.
	clock.Set(clock.Now().Add(20*time.Hour + 30*time.Minute)) // 08:30 next day
	timers.fire(t, 1, clock.Now())
	timers.fire(t, 2, clock.Now())
	timers.waitArmed(t, 3) // the new loop re-armed: its fire completed
	drain(t, s)

	fired := log.ofType("automation.fired")
	if len(fired) != 1 || fired[0].data["name"] != "morning setup" {
		t.Fatalf("automation.fired events = %+v, want only the new entry's", fired)
	}
	if n := fire.count(); n != 1 {
		t.Errorf("fires = %d, want the removed entry silent after reconfigure", n)
	}
}

// TestTrailPersistsPrivately: the last-run trail lands 0600, and a removed
// entry's record is pruned on the next write.
func TestTrailPersistsPrivately(t *testing.T) {
	clock := newFakeClock()
	timers := newFakeTimers()
	fire := newStubFire()
	log := &eventLog{}
	path := filepath.Join(t.TempDir(), "automations.toml")
	s := NewService(path, Options{
		Entries: []Entry{nightly(t, "backup notes")}, Fire: fire.fire, Publish: log.publish,
		Now: clock.Now, Timer: timers.Timer,
	}, testLogger(t))
	s.Start(context.Background())
	timers.waitArmed(t, 1)
	clock.Set(clock.Now().Add(14 * time.Hour))
	timers.fire(t, 1, clock.Now())
	timers.waitArmed(t, 2)
	drain(t, s)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("trail file was not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("trail file mode = %o, want 0600", perm)
	}
	states, err := readTrail(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("trail = %+v, want the fired entry", states)
	}
}
