package knowledge

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

// testLogger keeps expected warnings (failed fetches, cold boots) out of the
// test output without ever hitting the default logger.
func testLogger(_ *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The scheduler here is exercised entirely through its seams: an injected
// clock, an injected timer, and a stubbed runner. No test sleeps and no test
// runs a real command — a feed refresh is driven by firing the exact timer
// the scheduler armed, and "the fetch finished" is observed by waiting for
// the next timer to be armed (the loop arms it only after recording the
// result).

// fakeClock is a hand-advanced clock shared by the service and the test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeTimers records every timer the scheduler arms and lets the test fire
// them. Waiting for the nth arm is the tests' only synchronisation point.
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

// waitArmed blocks until n timers have been armed in total, then returns the
// nth (1-based). The deadline is a failsafe against a hung scheduler, not a
// synchronisation mechanism.
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

// fire delivers the nth (1-based) armed timer.
func (f *fakeTimers) fire(t *testing.T, n int, at time.Time) {
	t.Helper()
	f.mu.Lock()
	timer := f.armed[n-1]
	f.mu.Unlock()
	timer.ch <- at
}

// stubRunner scripts fetch outcomes per feed and records every call.
type stubRunner struct {
	mu      sync.Mutex
	results map[string][]FetchResult // queue per feed; the last entry repeats
	calls   []string
}

func newStubRunner() *stubRunner {
	return &stubRunner{results: make(map[string][]FetchResult)}
}

func (r *stubRunner) script(feed string, results ...FetchResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results[feed] = append(r.results[feed], results...)
}

func (r *stubRunner) run(_ context.Context, feed Feed, _ []string) FetchResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, feed.Name)
	queue := r.results[feed.Name]
	if len(queue) == 0 {
		return FetchResult{Err: context.Canceled}
	}
	res := queue[0]
	if len(queue) > 1 {
		r.results[feed.Name] = queue[1:]
	}
	return res
}

func (r *stubRunner) callCount(feed string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c == feed {
			n++
		}
	}
	return n
}

func ok(value string) FetchResult { return FetchResult{Stdout: value} }
func failed(code int) FetchResult { return FetchResult{ExitCode: code, Stderr: "boom"} }
func eagerFeed(name string) Feed {
	return Feed{
		Name: name, Description: name + " test feed",
		Argv: []string{"/bin/false"}, Mode: ModeEager,
		Interval: 5 * time.Minute, TTL: 10 * time.Minute, Timeout: 30 * time.Second,
	}
}

func lazyFeed(name string) Feed {
	f := eagerFeed(name)
	f.Mode = ModeLazy
	return f
}

// testService builds a service over a temp values file with every seam
// injected.
func testService(t *testing.T, clock *fakeClock, timers *fakeTimers, runner *stubRunner, feeds ...Feed) *Service {
	t.Helper()
	opts := Options{
		Feeds:          feeds,
		RefreshAllowed: true,
		Now:            clock.Now,
		Runner:         runner.run,
	}
	if timers != nil {
		opts.Timer = timers.Timer
	}
	return NewService(filepath.Join(t.TempDir(), "feeds.toml"), opts, testLogger(t))
}

// drain stops the service and requires it to settle: the assertion every
// scheduler test ends with, because a leaked loop is exactly the bug class
// this component exists to prevent (#74).
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

// TestShutdownDrainStopsABlockedFetch is the supervised-component contract,
// written before the scheduler was (the #74 lesson): shutdown must cancel an
// in-flight fetch and wait for every scheduler goroutine — a stopping daemon
// never abandons or orphans one.
func TestShutdownDrainStopsABlockedFetch(t *testing.T) {
	clock := newFakeClock()
	timers := newFakeTimers()
	entered := make(chan struct{})
	blocking := func(ctx context.Context, _ Feed, _ []string) FetchResult {
		close(entered)
		<-ctx.Done() // a wedged feed command: only cancellation ends it
		return FetchResult{Err: ctx.Err()}
	}
	s := NewService(filepath.Join(t.TempDir(), "feeds.toml"), Options{
		Feeds:          []Feed{eagerFeed("amd")},
		RefreshAllowed: true,
		Now:            clock.Now,
		Timer:          timers.Timer,
		Runner:         blocking,
	}, testLogger(t))
	s.Start(context.Background())

	// The boot fetch: nothing is cached, so the first timer is immediate.
	if at := timers.waitArmed(t, 1); at.d != 0 {
		t.Fatalf("first timer armed for %v, want immediate", at.d)
	}
	timers.fire(t, 1, clock.Now())
	<-entered // the fetch is now genuinely in flight
	// The tracking is the contract: the goroutine holding that fetch must be
	// visible to the drain, or shutdown would return while it still ran.
	if n := s.InFlight(); n == 0 {
		t.Fatal("an in-flight fetch is not tracked; shutdown could abandon it (#74)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Drain(ctx); err != nil {
		t.Fatalf("drain did not settle with a fetch in flight: %v", err)
	}
	if n := s.InFlight(); n != 0 {
		t.Fatalf("%d scheduler goroutines survived the drain", n)
	}
}

// TestEagerFeedRefreshesOnSchedule is the headline: an eager feed fetches at
// boot, re-arms for its interval, and refreshes when the interval elapses —
// each value persisted as it lands.
func TestEagerFeedRefreshesOnSchedule(t *testing.T) {
	clock := newFakeClock()
	timers := newFakeTimers()
	runner := newStubRunner()
	runner.script("amd", ok("187.42"), ok("188.10"))
	s := testService(t, clock, timers, runner, eagerFeed("amd"))
	s.Start(context.Background())
	defer drain(t, s)

	if at := timers.waitArmed(t, 1); at.d != 0 {
		t.Fatalf("boot timer armed for %v, want immediate", at.d)
	}
	timers.fire(t, 1, clock.Now())
	// The next arm is the proof the fetch completed and was recorded.
	if at := timers.waitArmed(t, 2); at.d != 5*time.Minute {
		t.Fatalf("re-armed for %v, want the 5m interval", at.d)
	}
	status := s.Status()
	if len(status) != 1 || !status[0].HasValue || status[0].Value != "187.42" {
		t.Fatalf("status after boot fetch = %+v, want the fetched value", status)
	}
	if !status[0].FetchedAt.Equal(clock.Now()) {
		t.Errorf("fetched_at = %v, want the injected clock's %v", status[0].FetchedAt, clock.Now())
	}

	clock.Advance(5 * time.Minute)
	timers.fire(t, 2, clock.Now())
	timers.waitArmed(t, 3)
	if v := s.Status()[0].Value; v != "188.10" {
		t.Errorf("value after scheduled refresh = %q, want the second fetch", v)
	}
	if n := runner.callCount("amd"); n != 2 {
		t.Errorf("fetches = %d, want exactly one per fired timer", n)
	}
}

// TestValuesPersistPrivatelyAndBootWarm: a value fetched by one service is
// on disk 0600 and serves from a fresh service — the restart — without a
// single fetch.
func TestValuesPersistPrivatelyAndBootWarm(t *testing.T) {
	clock := newFakeClock()
	timers := newFakeTimers()
	runner := newStubRunner()
	runner.script("amd", ok("187.42"))
	dir := t.TempDir()
	path := filepath.Join(dir, "feeds.toml")
	s := NewService(path, Options{
		Feeds: []Feed{eagerFeed("amd")}, RefreshAllowed: true,
		Now: clock.Now, Timer: timers.Timer, Runner: runner.run,
	}, testLogger(t))
	s.Start(context.Background())
	timers.waitArmed(t, 1)
	timers.fire(t, 1, clock.Now())
	timers.waitArmed(t, 2)
	drain(t, s)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("values file was not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("values file mode = %o, want 0600 — feed values may be sensitive", perm)
	}

	// The restart: a fresh service over the same file, two minutes later.
	clock.Advance(2 * time.Minute)
	failIfCalled := func(context.Context, Feed, []string) FetchResult {
		t.Error("a warm boot must not fetch")
		return FetchResult{}
	}
	s2 := NewService(path, Options{
		Feeds: []Feed{eagerFeed("amd")}, RefreshAllowed: true,
		Now: clock.Now, Runner: failIfCalled,
	}, testLogger(t))
	r, found := s2.Get(context.Background(), "amd")
	if !found || !r.HasValue || r.Value != "187.42" {
		t.Fatalf("reading after restart = %+v, want the persisted value", r)
	}
	if r.Age != 2*time.Minute || r.Stale {
		t.Errorf("age = %v stale = %v, want two minutes and fresh", r.Age, r.Stale)
	}
}

// TestBootHonoursStaleness: eager schedules resume where the persisted
// timestamps left off — a value inside its interval waits out the remainder,
// one past its ttl is disclosed stale and refreshed immediately.
func TestBootHonoursStaleness(t *testing.T) {
	clock := newFakeClock()
	timers := newFakeTimers()
	runner := newStubRunner()
	runner.script("amd", ok("187.42"))
	dir := t.TempDir()
	path := filepath.Join(dir, "feeds.toml")
	s := NewService(path, Options{
		Feeds: []Feed{eagerFeed("amd")}, RefreshAllowed: true,
		Now: clock.Now, Timer: timers.Timer, Runner: runner.run,
	}, testLogger(t))
	s.Start(context.Background())
	timers.waitArmed(t, 1)
	timers.fire(t, 1, clock.Now())
	timers.waitArmed(t, 2)
	drain(t, s)

	// Restart two minutes in: the schedule resumes with the remaining three.
	clock.Advance(2 * time.Minute)
	timers2 := newFakeTimers()
	s2 := NewService(path, Options{
		Feeds: []Feed{eagerFeed("amd")}, RefreshAllowed: true,
		Now: clock.Now, Timer: timers2.Timer, Runner: runner.run,
	}, testLogger(t))
	s2.Start(context.Background())
	if at := timers2.waitArmed(t, 1); at.d != 3*time.Minute {
		t.Fatalf("resumed schedule armed for %v, want the remaining 3m of the interval", at.d)
	}
	drain(t, s2)

	// Restart past the ttl: the value is served stale and refreshed at once.
	clock.Advance(20 * time.Minute)
	timers3 := newFakeTimers()
	s3 := NewService(path, Options{
		Feeds: []Feed{eagerFeed("amd")}, RefreshAllowed: true,
		Now: clock.Now, Timer: timers3.Timer, Runner: runner.run,
	}, testLogger(t))
	s3.Start(context.Background())
	defer drain(t, s3)
	if at := timers3.waitArmed(t, 1); at.d != 0 {
		t.Fatalf("stale boot armed for %v, want an immediate refresh", at.d)
	}
	r, _ := s3.Get(context.Background(), "amd")
	if !r.Stale || r.Value != "187.42" {
		t.Errorf("reading past ttl at boot = %+v, want the old value marked stale", r)
	}
}

// TestEagerFailuresBackOffAndRecover pins the backoff sequence exactly —
// interval, doubled per consecutive failure — and its reset on success.
func TestEagerFailuresBackOffAndRecover(t *testing.T) {
	clock := newFakeClock()
	timers := newFakeTimers()
	runner := newStubRunner()
	runner.script("amd", failed(1), failed(1), failed(1), ok("187.42"))
	s := testService(t, clock, timers, runner, eagerFeed("amd"))
	s.Start(context.Background())
	defer drain(t, s)

	timers.waitArmed(t, 1)
	firstFailure := clock.Now()
	timers.fire(t, 1, clock.Now())
	// One failure: retry after the plain interval — no sooner than a normal
	// refresh, no later either.
	if at := timers.waitArmed(t, 2); at.d != 5*time.Minute {
		t.Fatalf("delay after 1 failure = %v, want 5m", at.d)
	}
	clock.Advance(5 * time.Minute)
	timers.fire(t, 2, clock.Now())
	if at := timers.waitArmed(t, 3); at.d != 10*time.Minute {
		t.Fatalf("delay after 2 failures = %v, want 10m (doubled)", at.d)
	}
	clock.Advance(10 * time.Minute)
	timers.fire(t, 3, clock.Now())
	if at := timers.waitArmed(t, 4); at.d != 20*time.Minute {
		t.Fatalf("delay after 3 failures = %v, want 20m (doubled again)", at.d)
	}

	st := s.Status()[0]
	if !st.Failing || st.Attempts != 3 || !st.FailingSince.Equal(firstFailure) {
		t.Fatalf("status mid-streak = %+v, want failing since the first failure with 3 attempts", st)
	}

	clock.Advance(20 * time.Minute)
	timers.fire(t, 4, clock.Now())
	if at := timers.waitArmed(t, 5); at.d != 5*time.Minute {
		t.Fatalf("delay after recovery = %v, want the plain interval again", at.d)
	}
	st = s.Status()[0]
	if st.Failing || st.Value != "187.42" {
		t.Errorf("status after recovery = %+v, want healthy with the fetched value", st)
	}
}

// TestBackoffDelayValues pins the arithmetic, boundaries included, so a
// mutation to the doubling or the cap fails a test and not a user.
func TestBackoffDelayValues(t *testing.T) {
	interval := 5 * time.Minute
	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{
		{1, 5 * time.Minute},
		{2, 10 * time.Minute},
		{3, 20 * time.Minute},
		{4, 40 * time.Minute},
		{5, time.Hour},  // 80m capped
		{50, time.Hour}, // stays capped, no overflow
	} {
		if got := backoffDelay(interval, tc.failures); got != tc.want {
			t.Errorf("backoffDelay(5m, %d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
	// A cadence above the cap is its own ceiling: retries never come faster
	// than the user asked for.
	if got := backoffDelay(2*time.Hour, 3); got != 2*time.Hour {
		t.Errorf("backoffDelay(2h, 3) = %v, want the cadence itself", got)
	}
}

// TestLazyFeedFetchesOnFirstUseThenCachesToTTL: no fetch until the first
// ask, then the cached value serves until the ttl lapses — boundary
// included: a value aged exactly ttl is still fresh.
func TestLazyFeedFetchesOnFirstUseThenCachesToTTL(t *testing.T) {
	clock := newFakeClock()
	runner := newStubRunner()
	runner.script("weather", ok("light rain"), ok("clearing up"))
	s := testService(t, clock, nil, runner, lazyFeed("weather"))
	defer drain(t, s)

	if n := runner.callCount("weather"); n != 0 {
		t.Fatalf("%d fetches before first use, want none — lazy means lazy", n)
	}
	r, found := s.Get(context.Background(), "weather")
	if !found || r.Value != "light rain" || runner.callCount("weather") != 1 {
		t.Fatalf("first use: reading %+v after %d fetches, want one fetch", r, runner.callCount("weather"))
	}

	// Exactly at the ttl: still fresh, still cached — the boundary a
	// mutated comparison would move.
	clock.Advance(10 * time.Minute)
	r, _ = s.Get(context.Background(), "weather")
	if runner.callCount("weather") != 1 || r.Stale || r.Value != "light rain" {
		t.Fatalf("at ttl: %d fetches, reading %+v; want the cached value, fresh", runner.callCount("weather"), r)
	}

	// One second past: the next ask refetches.
	clock.Advance(time.Second)
	r, _ = s.Get(context.Background(), "weather")
	if runner.callCount("weather") != 2 || r.Value != "clearing up" {
		t.Fatalf("past ttl: %d fetches, reading %+v; want a refetch", runner.callCount("weather"), r)
	}
	if r.Age != 0 || r.Stale {
		t.Errorf("refetched reading age = %v stale = %v, want fresh as of now", r.Age, r.Stale)
	}
}

// TestLazyFailureServesLastGoodAndBacksOff: the acceptance criterion for a
// failing feed — last good value, age disclosed, failure visible, and no
// hammering while the backoff stands.
func TestLazyFailureServesLastGoodAndBacksOff(t *testing.T) {
	clock := newFakeClock()
	runner := newStubRunner()
	runner.script("weather", ok("light rain"), failed(7), ok("sunny"))
	s := testService(t, clock, nil, runner, lazyFeed("weather"))
	defer drain(t, s)

	if _, found := s.Get(context.Background(), "weather"); !found {
		t.Fatal("feed not found")
	}
	fetchedAt := clock.Now()

	clock.Advance(11 * time.Minute) // past the 10m ttl
	r, _ := s.Get(context.Background(), "weather")
	if runner.callCount("weather") != 2 {
		t.Fatalf("fetches = %d, want the failed refetch attempt", runner.callCount("weather"))
	}
	if !r.HasValue || r.Value != "light rain" || !r.FetchedAt.Equal(fetchedAt) {
		t.Fatalf("reading after failure = %+v, want the last good value with its old timestamp", r)
	}
	if !r.Stale || !r.Failing || !r.FailingSince.Equal(clock.Now()) {
		t.Fatalf("reading after failure = %+v, want stale and failing since now", r)
	}

	// Asking again inside the backoff must not run the command again.
	clock.Advance(time.Minute)
	if _, _ = s.Get(context.Background(), "weather"); runner.callCount("weather") != 2 {
		t.Fatalf("fetches = %d after ask inside backoff, want no retry", runner.callCount("weather"))
	}
	// Past the backoff (one ttl after the failed attempt), the ask retries.
	clock.Advance(10 * time.Minute)
	r, _ = s.Get(context.Background(), "weather")
	if runner.callCount("weather") != 3 || r.Value != "sunny" || r.Failing {
		t.Fatalf("after backoff: %d fetches, reading %+v; want a recovered fetch", runner.callCount("weather"), r)
	}
}

// TestReconfigureRebuildsSchedules: a reload swaps the feed set — the old
// feed's loop dies without ever fetching again, the new feed's schedule
// starts, and the whole thing still drains clean.
func TestReconfigureRebuildsSchedules(t *testing.T) {
	clock := newFakeClock()
	timers := newFakeTimers()
	runner := newStubRunner()
	runner.script("amd", ok("187.42"))
	runner.script("nvda", ok("903.11"))
	s := testService(t, clock, timers, runner, eagerFeed("amd"))
	s.Start(context.Background())

	timers.waitArmed(t, 1) // amd's boot timer, never fired

	s.Reconfigure([]Feed{eagerFeed("nvda")})
	timers.waitArmed(t, 2) // nvda's boot timer

	// Firing the orphaned timer must do nothing: its loop's generation was
	// cancelled by the reconfigure.
	timers.fire(t, 1, clock.Now())
	timers.fire(t, 2, clock.Now())
	timers.waitArmed(t, 3) // nvda re-armed: its fetch completed
	drain(t, s)

	if n := runner.callCount("amd"); n != 0 {
		t.Errorf("the removed feed fetched %d times after reconfigure, want zero", n)
	}
	if n := runner.callCount("nvda"); n != 1 {
		t.Errorf("the new feed fetched %d times, want once", n)
	}
}

// TestRemovedFeedIsPrunedFromDisk: dropping a feed's table drops its cached
// value on the next write — the cache never outlives the configuration that
// explains it.
func TestRemovedFeedIsPrunedFromDisk(t *testing.T) {
	clock := newFakeClock()
	runner := newStubRunner()
	runner.script("amd", ok("187.42"))
	runner.script("weather", ok("light rain"), ok("clearing up"))
	path := filepath.Join(t.TempDir(), "feeds.toml")
	s := NewService(path, Options{
		Feeds:          []Feed{lazyFeed("amd"), lazyFeed("weather")},
		RefreshAllowed: true,
		Now:            clock.Now, Runner: runner.run,
	}, testLogger(t))
	s.Get(context.Background(), "amd")
	s.Get(context.Background(), "weather")

	s.Reconfigure([]Feed{lazyFeed("weather")})
	clock.Advance(11 * time.Minute)
	s.Get(context.Background(), "weather") // triggers the pruning write
	drain(t, s)

	states, err := readValues(path)
	if err != nil {
		t.Fatalf("re-reading values: %v", err)
	}
	if _, kept := states["amd"]; kept {
		t.Error("the removed feed's value survived on disk")
	}
	if _, kept := states["weather"]; !kept {
		t.Error("the surviving feed's value was lost")
	}
}

// TestUnknownFeedIsReportedNotInvented: Get on a name nobody configured says
// so, so the tool can answer with the configured list.
func TestUnknownFeedIsReportedNotInvented(t *testing.T) {
	s := testService(t, newFakeClock(), nil, newStubRunner(), lazyFeed("amd"))
	defer drain(t, s)
	if _, found := s.Get(context.Background(), "tesla"); found {
		t.Fatal("an unconfigured feed produced a reading")
	}
}

// TestEagerWithoutRefreshAllowedFetchesOnAskOnly: with the gate anything
// short of allow, no schedule runs — but a gate-approved tool ask still
// fetches, including a stale refetch, so the feature degrades to lazy rather
// than to silently stale.
func TestEagerWithoutRefreshAllowedFetchesOnAskOnly(t *testing.T) {
	clock := newFakeClock()
	timers := newFakeTimers()
	runner := newStubRunner()
	runner.script("amd", ok("187.42"), ok("188.10"))
	s := NewService(filepath.Join(t.TempDir(), "feeds.toml"), Options{
		Feeds:          []Feed{eagerFeed("amd")},
		RefreshAllowed: false,
		Now:            clock.Now, Timer: timers.Timer, Runner: runner.run,
	}, testLogger(t))
	s.Start(context.Background())
	defer drain(t, s)

	if s.InFlight() != 0 {
		t.Fatal("scheduler goroutines started despite refresh not being allowed")
	}
	r, _ := s.Get(context.Background(), "amd")
	if r.Value != "187.42" || runner.callCount("amd") != 1 {
		t.Fatalf("ask did not fetch: reading %+v after %d fetches", r, runner.callCount("amd"))
	}
	clock.Advance(11 * time.Minute) // past ttl; no scheduler to refresh it
	r, _ = s.Get(context.Background(), "amd")
	if r.Value != "188.10" || runner.callCount("amd") != 2 {
		t.Fatalf("stale ask did not refetch: reading %+v after %d fetches", r, runner.callCount("amd"))
	}
}
