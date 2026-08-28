package overlay

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
)

// The service under an injected clock: no test here sleeps, and every input
// — inventory, threads, nicknames, the enabled switch — is a fake the test
// mutates mid-run, which is exactly how the real desktop behaves.

// harness owns the seams and the published record.
type harness struct {
	mu        sync.Mutex
	windows   []desktop.Window
	windowErr error
	reads     int
	threads   []Thread
	nicknames bool
	enabled   bool

	published chan []Row

	tickMu sync.Mutex
	ticks  []chan time.Time
}

func newHarness() *harness {
	return &harness{enabled: true, published: make(chan []Row, 16)}
}

func (h *harness) options() Options {
	return Options{
		Windows: func(ctx context.Context) ([]desktop.Window, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.reads++
			if h.windowErr != nil {
				return nil, h.windowErr
			}
			return append([]desktop.Window(nil), h.windows...), nil
		},
		Threads: func(context.Context) []Thread {
			h.mu.Lock()
			defer h.mu.Unlock()
			return append([]Thread(nil), h.threads...)
		},
		Tags: func([]desktop.Window) map[string]string {
			return nil
		},
		NicknamesHeld: func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.nicknames
		},
		Enabled: func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.enabled
		},
		Publish: func(rows []Row) { h.published <- rows },
		Timer: func(time.Duration) (<-chan time.Time, func()) {
			c := make(chan time.Time, 1)
			h.tickMu.Lock()
			h.ticks = append(h.ticks, c)
			h.tickMu.Unlock()
			return c, func() {}
		},
	}
}

// tick fires the most recent armed timer, waiting for the loop to arm one
// first — the loop arms a timer only while enrolled, so this is also the
// assertion that it is ticking at all.
func (h *harness) tick(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	seen := 0
	h.tickMu.Lock()
	seen = len(h.ticks)
	h.tickMu.Unlock()
	for {
		h.tickMu.Lock()
		if len(h.ticks) > seen || (seen > 0 && len(h.ticks) == seen) {
			// Fire the newest armed timer.
			if len(h.ticks) == 0 {
				h.tickMu.Unlock()
				continue
			}
			c := h.ticks[len(h.ticks)-1]
			h.tickMu.Unlock()
			c <- time.Now()
			return
		}
		h.tickMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("the loop never armed a timer; it is not enrolled or not running")
		}
		time.Sleep(time.Millisecond)
	}
}

func (h *harness) set(f func(h *harness)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(h)
}

func (h *harness) readCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reads
}

// waitRows blocks for the next publication.
func (h *harness) waitRows(t *testing.T) []Row {
	t.Helper()
	select {
	case rows := <-h.published:
		return rows
	case <-time.After(5 * time.Second):
		t.Fatal("no overlays.changed publication arrived")
		return nil
	}
}

func startService(t *testing.T, h *harness) *Service {
	t.Helper()
	s := NewService(h.options(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	t.Cleanup(func() {
		cancel()
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer drainCancel()
		if err := s.Drain(drainCtx); err != nil {
			t.Errorf("drain: %v", err)
		}
	})
	return s
}

func anchoredDesktop() ([]desktop.Window, []Thread) {
	return []desktop.Window{
			{Address: "0xa", Workspace: 1, Width: 800, Height: 600, Focused: true},
		}, []Thread{
			{Name: "th", Active: true, Anchors: []string{"0xa"}},
		}
}

func TestServicePublishesOnceAndOnlyOnChange(t *testing.T) {
	h := newHarness()
	windows, threads := anchoredDesktop()
	h.set(func(h *harness) { h.windows, h.threads = windows, threads })
	s := startService(t, h)

	first := h.waitRows(t)
	if len(first) != 1 || first[0].Badge == nil || !first[0].Badge.Active {
		t.Fatalf("first publication = %+v, want one filled badge row", first)
	}

	// Identical inventory on the next ticks: silence, not a re-publication.
	h.tick(t)
	h.tick(t)
	select {
	case rows := <-h.published:
		t.Fatalf("unchanged desktop republished %+v; the feed publishes on change only", rows)
	case <-time.After(50 * time.Millisecond):
	}

	// The window moves: the next tick publishes the new geometry.
	h.set(func(h *harness) {
		moved := append([]desktop.Window(nil), windows...)
		moved[0].X = 400
		h.windows = moved
	})
	h.tick(t)
	moved := h.waitRows(t)
	if len(moved) != 1 || moved[0].X != 400 {
		t.Fatalf("after a move, rows = %+v, want x=400", moved)
	}
	_ = s
}

func TestServiceConvergesWhenTheWindowCloses(t *testing.T) {
	h := newHarness()
	windows, threads := anchoredDesktop()
	// A second, unenrolled window keeps focus after the enrolled one dies,
	// so the empty result comes from pruning, not from "no focus".
	other := desktop.Window{Address: "0xz", Workspace: 1, X: 800, Width: 800, Height: 600}
	h.set(func(h *harness) { h.windows, h.threads = append(windows, other), threads })
	s := startService(t, h)
	if rows := h.waitRows(t); len(rows) != 1 {
		t.Fatalf("rows = %+v, want the anchored window's", rows)
	}

	// The compositor kills the anchored window; focus lands on the survivor.
	h.set(func(h *harness) {
		survivor := other
		survivor.Focused = true
		h.windows = []desktop.Window{survivor}
	})
	h.tick(t)
	if rows := h.waitRows(t); len(rows) != 0 {
		t.Fatalf("after the kill, rows = %+v, want empty — no stale badges on closed windows", rows)
	}
	_ = s
}

func TestServiceDisabledPublishesEmptyAndStopsPolling(t *testing.T) {
	h := newHarness()
	windows, threads := anchoredDesktop()
	h.set(func(h *harness) { h.windows, h.threads = windows, threads })
	s := startService(t, h)
	h.waitRows(t)

	reads := h.readCount()
	h.set(func(h *harness) { h.enabled = false })
	s.Poke()
	if rows := h.waitRows(t); len(rows) != 0 {
		t.Fatalf("after disable, rows = %+v, want empty", rows)
	}
	// Disabled means parked: pokes recompute the gate but never reach the
	// compositor.
	s.Poke()
	time.Sleep(20 * time.Millisecond)
	if got := h.readCount(); got != reads {
		t.Errorf("inventory reads went %d → %d while disabled; the gate must stop the subprocess", reads, got)
	}

	// Re-enabled: the next poke restores the overlays.
	h.set(func(h *harness) { h.enabled = true })
	s.Poke()
	if rows := h.waitRows(t); len(rows) != 1 {
		t.Fatalf("after re-enable, rows = %+v, want the badge back", rows)
	}
}

func TestServiceParksWithNothingEnrolled(t *testing.T) {
	h := newHarness()
	// Enabled, but no anchors and no nicknames: the loop must park without
	// ever reading the inventory — a user who never enrols pays zero.
	h.set(func(h *harness) {
		h.windows = []desktop.Window{{Address: "0xa", Workspace: 1, Width: 800, Height: 600, Focused: true}}
	})
	s := startService(t, h)
	s.Poke()
	s.Poke()
	time.Sleep(20 * time.Millisecond)
	if got := h.readCount(); got != 0 {
		t.Errorf("inventory reads = %d with nothing enrolled, want 0", got)
	}
	select {
	case rows := <-h.published:
		t.Fatalf("published %+v with nothing enrolled; an idle daemon says nothing", rows)
	default:
	}

	// Enrolment arrives (a thread gains an anchor → focus.changed → Poke).
	h.set(func(h *harness) { h.threads = []Thread{{Name: "th", Active: true, Anchors: []string{"0xa"}}} })
	s.Poke()
	if rows := h.waitRows(t); len(rows) != 1 {
		t.Fatalf("after enrolment, rows = %+v, want one", rows)
	}
}

func TestServiceClearsWhenTheDesktopCannotBeRead(t *testing.T) {
	h := newHarness()
	windows, threads := anchoredDesktop()
	h.set(func(h *harness) { h.windows, h.threads = windows, threads })
	s := startService(t, h)
	h.waitRows(t)

	h.set(func(h *harness) { h.windowErr = desktop.ErrNoCompositor })
	h.tick(t)
	if rows := h.waitRows(t); len(rows) != 0 {
		t.Fatalf("unreadable desktop kept rows %+v; stale geometry must clear", rows)
	}
	// Recovery on a later tick.
	h.set(func(h *harness) { h.windowErr = nil })
	h.tick(t)
	if rows := h.waitRows(t); len(rows) != 1 {
		t.Fatalf("after recovery, rows = %+v, want the badge back", rows)
	}
	_ = s
}

func TestServiceCurrentComputesFreshWithoutPublishing(t *testing.T) {
	h := newHarness()
	windows, threads := anchoredDesktop()
	h.set(func(h *harness) { h.windows, h.threads = windows, threads })
	s := startService(t, h)
	h.waitRows(t)

	// Current is the overlays.get read: fresh rows, no bus traffic.
	rows := s.Current(context.Background())
	if len(rows) != 1 {
		t.Fatalf("Current = %+v, want one row", rows)
	}
	select {
	case extra := <-h.published:
		t.Fatalf("Current published %+v; it is a read, not a change", extra)
	default:
	}
}

// The loop, pokes, Current, and Drain under the race detector: the service
// is poked from bus watchers while sessions call overlays.get, so the
// concurrency is the production shape, not a synthetic one.
func TestServiceIsRaceCleanUnderConcurrentUse(t *testing.T) {
	h := newHarness()
	windows, threads := anchoredDesktop()
	h.set(func(h *harness) { h.windows, h.threads = windows, threads })
	s := startService(t, h)
	h.waitRows(t)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				s.Poke()
				_ = s.Current(context.Background())
				if i%10 == 0 {
					h.set(func(h *harness) { h.enabled = !h.enabled })
				}
			}
		}()
	}
	wg.Wait()
	// Leave the switch on so the deferred drain sees a live loop.
	h.set(func(h *harness) { h.enabled = true })
	for len(h.published) > 0 {
		<-h.published
	}
}
