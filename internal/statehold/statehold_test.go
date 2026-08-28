package statehold

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// A nil gate is open forever: the CLI and tests construct stores without one,
// and their writes must cost nothing and block never.
func TestNilGateNeverBlocks(t *testing.T) {
	var g *Gate
	release := g.Enter()
	release()
	if g.Held() {
		t.Fatal("nil gate reports held")
	}
}

func TestHoldBlocksNewWritesUntilRelease(t *testing.T) {
	g := &Gate{}
	release, err := g.Hold(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Held() {
		t.Fatal("gate not held after Hold")
	}

	entered := make(chan struct{})
	go func() {
		defer close(entered)
		g.Enter()()
	}()
	select {
	case <-entered:
		t.Fatal("Enter proceeded while the gate was held")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Enter still blocked after release")
	}
	if g.Held() {
		t.Fatal("gate still held after release")
	}
}

// Hold waits for mutations already inside the gate: coherence is promised
// only once the write that was mid-flight has settled.
func TestHoldDrainsInFlightWrites(t *testing.T) {
	g := &Gate{}
	release := g.Enter()

	held := make(chan error, 1)
	go func() {
		r, err := g.Hold(context.Background(), time.Minute)
		if err == nil {
			defer r()
		}
		held <- err
	}()
	select {
	case <-held:
		t.Fatal("Hold returned while a write was in flight")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case err := <-held:
		if err != nil {
			t.Fatalf("Hold failed after the write settled: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Hold never returned after the write settled")
	}
}

// A write that will not settle must fail the hold, not wedge it — and a
// failed Hold reopens the gate so writes are never left blocked.
func TestHoldGivesUpOnAWedgedWriteAndReopens(t *testing.T) {
	g := &Gate{}
	release := g.Enter() // never released before the deadline

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := g.Hold(ctx, time.Minute); err == nil {
		t.Fatal("Hold succeeded over an unsettled write")
	}
	if g.Held() {
		t.Fatal("failed Hold left the gate closed")
	}
	release()
}

func TestSecondHoldIsRefused(t *testing.T) {
	g := &Gate{}
	release, err := g.Hold(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := g.Hold(context.Background(), time.Minute); !errors.Is(err, ErrHeld) {
		t.Fatalf("second Hold: got %v, want ErrHeld", err)
	}
}

// The TTL is the daemon's protection against a backup process that dies
// mid-copy: the gate reopens on its own, and the late explicit release is a
// harmless no-op.
func TestTTLReopensTheGate(t *testing.T) {
	g := &Gate{}
	release, err := g.Hold(context.Background(), 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for g.Held() {
		if time.Now().After(deadline) {
			t.Fatal("TTL never reopened the gate")
		}
		time.Sleep(5 * time.Millisecond)
	}
	release() // after the TTL fired: must not panic, must change nothing
	if g.Held() {
		t.Fatal("late release re-closed the gate")
	}
}

// Concurrent writers against hold/release cycles: the race detector's test.
func TestConcurrentWritersAndHolds(t *testing.T) {
	g := &Gate{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				g.Enter()()
			}
		}()
	}
	for range 10 {
		release, err := g.Hold(context.Background(), time.Second)
		if err != nil {
			t.Errorf("Hold: %v", err)
			break
		}
		release()
	}
	wg.Wait()
}
