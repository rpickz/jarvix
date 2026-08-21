// Package quiesce tracks in-flight goroutines so a caller can wait for them
// to finish — with a deadline.
//
// Jarvix deliberately does work *after* the user-visible part of an
// interaction is over: conversation history is persisted once the session has
// finished, off the engine's lock, so disk I/O adds no latency to the spoken
// exchange (ADR 0011); desktop notifications are dispatched from their own
// goroutine so a back-to-back session never queues behind an unclicked one.
// That tail work is real work — losing it loses the last exchange from the
// persisted conversation — so shutdown has to wait for it.
//
// sync.WaitGroup is the obvious tool and the wrong one on its own: Wait has no
// deadline, and a wedged disk or a hung subprocess would turn "stop the
// daemon" into "hang forever". A Group is a WaitGroup whose Wait takes a
// context, so every drain in the codebase is bounded by construction, and can
// report how much work was still outstanding when it gave up.
package quiesce

import (
	"context"
	"sync"
)

// Group counts in-flight goroutines. The zero value is ready to use and is
// already quiescent: Wait on it returns immediately, even with an expired
// context. A Group must not be copied after first use.
type Group struct {
	mu sync.Mutex
	n  int
	// idle is non-nil exactly while n > 0, and is closed (then dropped) on the
	// drop back to zero. Handing waiters a channel rather than a
	// sync.Cond/WaitGroup is what lets Wait select against a context — and
	// what lets "already idle" be observed without racing the scheduler, which
	// a `go wg.Wait()` helper goroutine cannot promise.
	idle chan struct{}
}

// Add adds delta to the counter. Add(1) must happen before the goroutine it
// accounts for is started, exactly as with sync.WaitGroup: adding from inside
// the goroutine races a waiter that is already draining.
//
// It panics if the counter goes negative, which can only mean a Done without
// a matching Add — a bug that would otherwise show up much later as a drain
// that returns while work is still running.
func (g *Group) Add(delta int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n += delta
	switch {
	case g.n < 0:
		panic("quiesce: negative Group counter")
	case g.n > 0 && g.idle == nil:
		g.idle = make(chan struct{})
	case g.n == 0 && g.idle != nil:
		close(g.idle)
		g.idle = nil
	}
}

// Done records one tracked goroutine as finished.
func (g *Group) Done() { g.Add(-1) }

// Go runs f in a tracked goroutine. It is the whole Add/defer Done dance in
// one call, for the common case where the goroutine is started and forgotten.
func (g *Group) Go(f func()) {
	g.Add(1)
	go func() {
		defer g.Done()
		f()
	}()
}

// InFlight reports how many tracked goroutines are still running. It exists
// for the shutdown log: when a drain gives up, the number of goroutines still
// outstanding is the difference between "one stuck write" and "the whole
// stage never started".
func (g *Group) InFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}

// Wait blocks until every tracked goroutine has finished, or until ctx is
// done — returning ctx.Err() in that case, so the caller can log which stage
// failed to settle and carry on stopping.
//
// An already-quiescent Group returns nil even when ctx is already expired.
// That is not an accident of scheduling but a promise: it makes Wait usable
// as an assertion ("is everything finished?") with an expired context, which
// is how tests observe quiescence without sleeping.
func (g *Group) Wait(ctx context.Context) error {
	idle := g.idleChan()
	// Checked before the two-way select because select picks uniformly at
	// random among ready cases: with both an idle Group and an expired
	// context, the combined select would report a timeout half the time.
	select {
	case <-idle:
		return nil
	default:
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// idleChan returns a channel that is closed once the Group is quiescent, or
// an already-closed one when it is quiescent now.
//
// A waiter holds the channel it was handed. If work starts again afterwards,
// that channel stays closed and the waiter still returns: a Group reports
// "everything running when you asked has finished", not "nothing is running
// now" — the only honest contract for a counter that anyone may add to.
// Shutdown callers close the door first (the engine refuses new sessions)
// so the two readings coincide.
func (g *Group) idleChan() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.idle != nil {
		return g.idle
	}
	closed := make(chan struct{})
	close(closed)
	return closed
}
