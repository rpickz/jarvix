package knowledge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests pin the window-admin half of the service (issue #92):
// RefreshNow runs the exact scheduled-fetch path under the single-flight
// latch, the enabled switch parks a feed without losing its value, and every
// completed fetch announces itself through Notify. All through the injected
// seams — no sleeps, no real commands.

// notifyRecorder collects Notify calls and lets a test wait for the nth.
type notifyRecorder struct {
	ch chan string
}

func newNotifyRecorder() *notifyRecorder {
	return &notifyRecorder{ch: make(chan string, 64)}
}

func (n *notifyRecorder) notify(name string) { n.ch <- name }

func (n *notifyRecorder) wait(t *testing.T) string {
	t.Helper()
	select {
	case name := <-n.ch:
		return name
	case <-time.After(5 * time.Second):
		t.Fatal("no fetch completion was announced")
		return ""
	}
}

// TestRefreshNowFetchesThroughTheScheduledPath: with a fresh value cached and
// the scheduler parked on its interval timer, RefreshNow fetches immediately —
// no timer fires — records the result, and announces completion through the
// same Notify a scheduled fetch uses.
func TestRefreshNowFetchesThroughTheScheduledPath(t *testing.T) {
	clock := newFakeClock()
	timers := newFakeTimers()
	runner := newStubRunner()
	runner.script("amd", ok("187.42"), ok("188.10"))
	notify := newNotifyRecorder()
	s := NewService(filepath.Join(t.TempDir(), "feeds.toml"), Options{
		Feeds: []Feed{eagerFeed("amd")}, RefreshAllowed: true,
		Now: clock.Now, Timer: timers.Timer, Runner: runner.run,
		Notify: notify.notify,
	}, testLogger(t))
	s.Start(context.Background())
	defer drain(t, s)

	// Boot fetch lands the first value; its completion is announced too.
	timers.waitArmed(t, 1)
	timers.fire(t, 1, clock.Now())
	if name := notify.wait(t); name != "amd" {
		t.Fatalf("scheduled fetch announced %q, want amd", name)
	}
	timers.waitArmed(t, 2) // the loop is now parked on the 5m interval

	clock.Advance(time.Minute)
	if err := s.RefreshNow("amd"); err != nil {
		t.Fatal(err)
	}
	if name := notify.wait(t); name != "amd" {
		t.Fatalf("manual fetch announced %q, want amd", name)
	}
	st := s.Status()[0]
	if st.Value != "188.10" || !st.FetchedAt.Equal(clock.Now()) {
		t.Errorf("status after RefreshNow = %+v, want the fresh fetch at the injected clock", st)
	}
	if n := runner.callCount("amd"); n != 2 {
		t.Errorf("fetches = %d, want boot + manual and nothing else", n)
	}
}

// TestRefreshNowIsSingleFlight is the mutation check on the latch: a manual
// refresh during an in-flight scheduled fetch — and a scheduled fire during a
// manual fetch — must not run the command twice.
func TestRefreshNowIsSingleFlight(t *testing.T) {
	clock := newFakeClock()
	timers := newFakeTimers()
	entered := make(chan struct{})
	release := make(chan struct{})
	calls := make(chan string, 16)
	blocking := func(ctx context.Context, feed Feed, _ []string) FetchResult {
		calls <- feed.Name
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return ok("187.42")
	}
	s := NewService(filepath.Join(t.TempDir(), "feeds.toml"), Options{
		Feeds: []Feed{eagerFeed("amd")}, RefreshAllowed: true,
		Now: clock.Now, Timer: timers.Timer, Runner: blocking,
	}, testLogger(t))
	s.Start(context.Background())
	defer drain(t, s)

	// The scheduled boot fetch enters and blocks.
	timers.waitArmed(t, 1)
	timers.fire(t, 1, clock.Now())
	<-entered

	// Manual refreshes while it is in flight are the no-op answer, not a
	// second command: the in-flight fetch's completion is the refresh.
	if err := s.RefreshNow("amd"); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshNow("amd"); err != nil {
		t.Fatal(err)
	}
	close(release)
	// The loop re-arming is the proof the blocked fetch completed and was
	// recorded; only then can the call count be final.
	timers.waitArmed(t, 2)
	if n := len(calls); n != 1 {
		t.Fatalf("the command ran %d times with refreshes stacked on one flight, want 1", n)
	}
}

// TestRefreshNowRefusals: unknown, disabled, and gate-refused feeds each get
// a reason, and nothing fetches.
func TestRefreshNowRefusals(t *testing.T) {
	clock := newFakeClock()
	timers := newFakeTimers()
	runner := newStubRunner()
	parked := eagerFeed("parked")
	parked.Enabled = false
	s := testService(t, clock, timers, runner, eagerFeed("amd"), parked)
	s.Start(context.Background())
	defer drain(t, s)

	if err := s.RefreshNow("nvda"); err == nil || !strings.Contains(err.Error(), `"nvda"`) {
		t.Errorf("unknown feed error = %v, want it named", err)
	}
	if err := s.RefreshNow("parked"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("disabled feed error = %v, want the disabled reason", err)
	}

	gated := NewService(filepath.Join(t.TempDir(), "feeds.toml"), Options{
		Feeds: []Feed{eagerFeed("amd")}, RefreshAllowed: false,
		Now: clock.Now, Timer: newFakeTimers().Timer,
		Runner: func(context.Context, Feed, []string) FetchResult {
			t.Error("a refused refresh ran the command")
			return FetchResult{}
		},
	}, testLogger(t))
	gated.Start(context.Background())
	defer drain(t, gated)
	if err := gated.RefreshNow("amd"); err == nil ||
		!strings.Contains(err.Error(), "knowledge.refresh") {
		t.Errorf("gate-refused error = %v, want the policy fix named", err)
	}
	if n := runner.callCount("parked"); n != 0 {
		t.Errorf("the parked feed fetched %d times, want none", n)
	}
}

// TestDisabledFeedIsParkedNotForgotten: a feed reconfigured to disabled stops
// fetching everywhere — scheduler, Get, injection — but its last value stays,
// served and reported with its honest age.
func TestDisabledFeedIsParkedNotForgotten(t *testing.T) {
	clock := newFakeClock()
	timers := newFakeTimers()
	runner := newStubRunner()
	runner.script("amd", ok("187.42"))
	amd := eagerFeed("amd")
	amd.Inject = true
	s := testService(t, clock, timers, runner, amd)
	s.Start(context.Background())
	defer drain(t, s)

	timers.waitArmed(t, 1)
	timers.fire(t, 1, clock.Now())
	timers.waitArmed(t, 2)

	parked := amd
	parked.Enabled = false
	s.Reconfigure([]Feed{parked})
	clock.Advance(time.Hour) // far past the ttl: stale, and it must say so

	st := s.Status()[0]
	if st.Enabled {
		t.Fatal("status still reports the feed enabled")
	}
	if !st.HasValue || st.Value != "187.42" || !st.Stale {
		t.Errorf("status = %+v, want the kept value, disclosed stale", st)
	}
	r, found := s.Get(context.Background(), "amd")
	if !found || !r.HasValue || r.Value != "187.42" {
		t.Errorf("reading = %+v, want the kept value with no fetch", r)
	}
	if inj := s.Inject(); inj.Feeds != 0 || inj.Message != "" {
		t.Errorf("a disabled feed was injected: %+v", inj)
	}
	if n := runner.callCount("amd"); n != 1 {
		t.Errorf("fetches = %d, want only the one from before it was parked", n)
	}
}

// TestDisabledEagerFeedNeverSchedules: a service whose only eager feed is
// disabled starts no loop and answers Get from what it has, without a fetch.
func TestDisabledEagerFeedNeverSchedules(t *testing.T) {
	clock := newFakeClock()
	parked := eagerFeed("amd")
	parked.Enabled = false
	s := NewService(filepath.Join(t.TempDir(), "feeds.toml"), Options{
		Feeds: []Feed{parked}, RefreshAllowed: true,
		Now: clock.Now,
		Runner: func(context.Context, Feed, []string) FetchResult {
			t.Error("a disabled feed's command ran")
			return FetchResult{}
		},
	}, testLogger(t))
	s.Start(context.Background())
	defer drain(t, s)

	r, found := s.Get(context.Background(), "amd")
	if !found || r.HasValue {
		t.Errorf("reading = %+v, want the feed known and cold", r)
	}
	if st := s.Status()[0]; st.Enabled {
		t.Error("status reports a disabled feed as enabled")
	}
}
