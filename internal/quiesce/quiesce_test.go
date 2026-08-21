package quiesce

import (
	"context"
	"testing"
)

func TestZeroGroupIsAlreadyQuiescent(t *testing.T) {
	var g Group
	if err := g.Wait(context.Background()); err != nil {
		t.Fatalf("Wait on an idle Group: %v", err)
	}
	if n := g.InFlight(); n != 0 {
		t.Errorf("InFlight = %d, want 0", n)
	}
}

// An expired context must not turn an idle Group into a timeout. This is the
// property that lets a test assert "everything has finished" without sleeping,
// so it is asserted rather than assumed — a plain two-way select would get it
// right only half the time.
func TestIdleGroupIgnoresAnExpiredContext(t *testing.T) {
	var g Group
	g.Go(func() {})
	if err := g.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 100 {
		if err := g.Wait(ctx); err != nil {
			t.Fatalf("Wait %d on an idle Group with an expired context: %v", i, err)
		}
	}
}

func TestWaitBlocksUntilTheLastGoroutineFinishes(t *testing.T) {
	var g Group
	release := make(chan struct{})
	started := make(chan struct{})
	g.Go(func() {
		close(started)
		<-release
	})
	<-started
	if n := g.InFlight(); n != 1 {
		t.Fatalf("InFlight = %d, want 1", n)
	}

	// The goroutine cannot finish while release is open, so a Wait bounded by
	// an already-expired context must report the timeout — deterministically,
	// with no timer involved.
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Wait(expired); err == nil {
		t.Fatal("Wait returned nil while a tracked goroutine was still running")
	}

	close(release)
	if err := g.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if n := g.InFlight(); n != 0 {
		t.Errorf("InFlight = %d, want 0", n)
	}
}

// Work that starts after a waiter has been handed its channel does not extend
// that waiter: a Group promises "what was running when you asked has
// finished". Shutdown callers close their door first, so the distinction never
// bites them, but it must be the documented behaviour rather than an accident.
func TestWaitDoesNotChaseWorkStartedLater(t *testing.T) {
	var g Group
	if err := g.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	release := make(chan struct{})
	g.Go(func() { <-release })
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Wait(expired); err == nil {
		t.Error("a later Wait ignored newly started work")
	}
	close(release)
	if err := g.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestDoneWithoutAddPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Done past zero did not panic")
		}
	}()
	var g Group
	g.Done()
}
