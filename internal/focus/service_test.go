package focus

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeDesktop is a mutable window inventory.
type fakeDesktop struct{ windows []desktop.Window }

func (f *fakeDesktop) list(context.Context) ([]desktop.Window, error) {
	return append([]desktop.Window(nil), f.windows...), nil
}

func newServiceWithDesktop(t *testing.T, clock *testClock, d *fakeDesktop) *Service {
	t.Helper()
	path := filepath.Join(t.TempDir(), "focus.toml")
	return NewService(path, Options{Now: clock.now, Windows: d.list}, testLogger(t))
}

// The switch recap: at most two sentences, every clause from the record —
// last time here, parked count and newest, the anchor and its liveness — and
// an honest "fresh thread" when there is no history to speak of.

func TestSwitchRecapIsBuiltFromTheRecord(t *testing.T) {
	clock := newTestClock()
	d := &fakeDesktop{windows: []desktop.Window{
		{Address: "0xa", Class: "Alacritty", Title: "make test", Focused: true},
	}}
	s := newServiceWithDesktop(t, clock, d)
	ctx := context.Background()

	if _, _, err := s.Create(ctx, "the ci refactor", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(ctx, "deploy", 0); err != nil {
		t.Fatal(err)
	}
	// Back on the first thread: park twice, wait, switch away and back.
	if _, _, err := s.Switch(ctx, "ci refactor"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Park("reply to dan"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Park("check the flaky test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Switch(ctx, "deploy"); err != nil {
		t.Fatal(err)
	}
	clock.advance(10 * time.Minute)

	_, recap, err := s.Switch(ctx, "ci refactor")
	if err != nil {
		t.Fatal(err)
	}
	want := "Back on the ci refactor — last here ten minutes ago, anchored to Alacritty. " +
		"Two parked; newest: check the flaky test."
	if recap != want {
		t.Errorf("recap = %q\nwant    %q", recap, want)
	}
	if n := strings.Count(recap, "."); n > 2 {
		t.Errorf("recap is more than two sentences: %q", recap)
	}
}

func TestSwitchRecapForFreshThreadIsHonest(t *testing.T) {
	clock := newTestClock()
	s := newStoreService(t, clock)
	ctx := context.Background()
	if _, _, err := s.Create(ctx, "deploy", 0); err != nil {
		t.Fatal(err)
	}
	// A hand-added thread nobody ever switched into.
	s.mu.Lock()
	next := clone(s.st)
	next.threads = append(next.threads, Thread{
		ID: "t9", Name: "the audit", Created: clock.now(), LastActivity: clock.now(),
	})
	if err := s.saveLocked(next); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.mu.Unlock()

	_, recap, err := s.Switch(ctx, "audit")
	if err != nil {
		t.Fatal(err)
	}
	if recap != "The the audit thread — fresh thread, nothing parked yet." {
		t.Errorf("fresh recap = %q", recap)
	}
}

// TestAnchorGoneDegradesGracefully is the anchor-gone acceptance criterion:
// the window vanishes, the thread survives, and both the recap and the
// snapshot say gone in words — never an error, never a fabricated anchor.
func TestAnchorGoneDegradesGracefully(t *testing.T) {
	clock := newTestClock()
	d := &fakeDesktop{windows: []desktop.Window{
		{Address: "0xa", Class: "Alacritty", Title: "make test", Focused: true},
	}}
	s := newServiceWithDesktop(t, clock, d)
	ctx := context.Background()
	if _, _, err := s.Create(ctx, "the ci refactor", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(ctx, "deploy", 0); err != nil {
		t.Fatal(err)
	}
	d.windows = nil // the anchored window closes
	clock.advance(time.Minute)

	_, recap, err := s.Switch(ctx, "ci refactor")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recap, "its Alacritty window is gone") {
		t.Errorf("recap does not disclose the gone anchor: %q", recap)
	}
	v := s.Snapshot(ctx)
	var th ThreadView
	for _, cand := range v.Threads {
		if cand.Name == "the ci refactor" {
			th = cand
		}
	}
	if len(th.AnchorsGone) != 1 || !th.AnchorsGone[0] {
		t.Errorf("snapshot does not mark the anchor gone: %+v", th)
	}
}

func TestParkAndParkedList(t *testing.T) {
	clock := newTestClock()
	s := newStoreService(t, clock)
	ctx := context.Background()
	if _, err := s.Park("anything"); !errors.Is(err, ErrNoActive) {
		t.Errorf("park with no active thread err = %v", err)
	}
	if _, _, err := s.Create(ctx, "deploy", 0); err != nil {
		t.Fatal(err)
	}
	ack, err := s.Park("reply to dan")
	if err != nil {
		t.Fatal(err)
	}
	// The soft confirm: no read-back of the thought.
	if ack != "Parked." {
		t.Errorf("park ack = %q", ack)
	}
	if _, err := s.Park("book the dentist"); err != nil {
		t.Fatal(err)
	}
	spoken, err := s.ParkedSpoken()
	if err != nil {
		t.Fatal(err)
	}
	want := "Two parked on deploy: book the dentist; reply to dan."
	if spoken != want {
		t.Errorf("parked = %q\nwant   %q", spoken, want)
	}
}

func TestStatusSpeaksActiveFirstAndStaysBounded(t *testing.T) {
	clock := newTestClock()
	s := newStoreService(t, clock)
	ctx := context.Background()
	names := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}
	for _, n := range names {
		if _, _, err := s.Create(ctx, n, 0); err != nil {
			t.Fatal(err)
		}
		clock.advance(time.Minute)
	}
	if _, _, err := s.Switch(ctx, "bravo"); err != nil {
		t.Fatal(err)
	}
	status := s.Status()
	if !strings.HasPrefix(status, "You're on bravo") {
		t.Errorf("the active thread is not first: %q", status)
	}
	if lines := strings.Count(status, "."); lines > maxStatusThreads+1 {
		t.Errorf("status is unbounded: %q", status)
	}
	if !strings.Contains(status, "And two more threads.") {
		t.Errorf("the overflow is not counted: %q", status)
	}
}

func TestResolveIsHonestAboutAmbiguity(t *testing.T) {
	clock := newTestClock()
	s := newStoreService(t, clock)
	ctx := context.Background()
	if _, _, err := s.Create(ctx, "the ci refactor", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(ctx, "the ci pipeline", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Switch(ctx, "ci"); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("ambiguous reference err = %v", err)
	}
	if _, _, err := s.Switch(ctx, "pipeline"); err != nil {
		t.Errorf("unique word did not resolve: %v", err)
	}
	if _, _, err := s.Switch(ctx, "the launch"); !errors.Is(err, ErrUnknownThread) {
		t.Errorf("unknown reference err = %v", err)
	}
}

func TestEndSaysWhatWentWithIt(t *testing.T) {
	clock := newTestClock()
	s := newStoreService(t, clock)
	ctx := context.Background()
	if _, _, err := s.Create(ctx, "deploy", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Park("one"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Park("two"); err != nil {
		t.Fatal(err)
	}
	ack, err := s.End("")
	if err != nil {
		t.Fatal(err)
	}
	if ack != "Ended deploy. Its two parked thoughts went with it." {
		t.Errorf("end ack = %q", ack)
	}
	if v := s.Snapshot(ctx); len(v.Threads) != 0 || v.Active != "" {
		t.Errorf("the thread survived its end: %+v", v)
	}
}
