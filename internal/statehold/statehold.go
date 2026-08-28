// Package statehold is the write barrier behind `jarvix backup` (ADR 0045).
//
// Every store already writes atomically — temp file, fsync, rename — so no
// single file is ever torn on disk. What atomic renames cannot promise is
// coherence *across* files: history.json naming a conversation whose
// transcript is still being appended, a metadata file one turn ahead of its
// transcript. A backup that walks the state dir mid-interaction could catch
// exactly that skew.
//
// The Gate closes that window. Every daemon-owned store enters the gate for
// the duration of one disk mutation; the backup CLI asks the daemon (the
// state.hold verb) to hold the gate, which waits for in-flight mutations to
// drain and then blocks new ones until release — or until a TTL expires, so
// a backup process that dies mid-copy can never wedge the daemon. While the
// gate is held, the state root is a coherent point in time.
//
// A nil *Gate is valid and never blocks: the CLI and tests construct stores
// without one, and only the daemon threads a real gate through. Holding is
// deliberately not a flush — work that has not started writing yet simply
// lands after release; the archive is a coherent cut, not a final one.
package statehold

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/quiesce"
)

// ErrHeld reports a Hold attempted while another hold is active. One backup
// at a time: the second caller retries or reports, it never queues.
var ErrHeld = errors.New("state writes are already held")

// Gate is a write barrier stores enter around each disk mutation. The zero
// value is open and ready to use; a nil Gate is open forever.
type Gate struct {
	mu sync.Mutex
	// reopen is non-nil exactly while the gate is held, and is closed (then
	// dropped) on release — the channel handoff that lets Enter block
	// without polling, the quiesce package's idiom.
	reopen chan struct{}
	// writes counts mutations currently inside the gate, so Hold can wait
	// for the ones already running before promising coherence.
	writes quiesce.Group
	// releaseTimer enforces the TTL of the current hold.
	releaseTimer *time.Timer
}

// Enter blocks while the gate is held, then registers one in-flight mutation
// and returns the func that ends it. Callers write `defer g.Enter()()` (or
// hold the release across a multi-file mutation): the gate is entered before
// the first byte moves and released only when the files are settled.
func (g *Gate) Enter() (release func()) {
	if g == nil {
		return func() {}
	}
	for {
		g.mu.Lock()
		if g.reopen == nil {
			// Registered under the same lock that Hold sets reopen under, so
			// a mutation can never slip in between "not held" and "counted".
			g.writes.Add(1)
			g.mu.Unlock()
			return g.writes.Done
		}
		reopen := g.reopen
		g.mu.Unlock()
		<-reopen
	}
}

// Hold closes the gate: it waits (bounded by ctx) for in-flight mutations to
// drain, then keeps new ones blocked until the returned release runs or ttl
// expires — whichever comes first. It returns ErrHeld when a hold is already
// active, and reopens the gate before returning any error, so a failed Hold
// never leaves writes blocked.
func (g *Gate) Hold(ctx context.Context, ttl time.Duration) (release func(), err error) {
	g.mu.Lock()
	if g.reopen != nil {
		g.mu.Unlock()
		return nil, ErrHeld
	}
	g.reopen = make(chan struct{})
	g.mu.Unlock()

	if err := g.writes.Wait(ctx); err != nil {
		g.release()
		return nil, errors.New("state writes did not settle in time")
	}
	g.mu.Lock()
	g.releaseTimer = time.AfterFunc(ttl, g.release)
	g.mu.Unlock()
	return g.release, nil
}

// Held reports whether the gate is currently held, for the status surface
// and tests.
func (g *Gate) Held() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reopen != nil
}

// release reopens the gate. Idempotent: the TTL firing after an explicit
// release (or vice versa) is a no-op, never a double close.
func (g *Gate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reopen == nil {
		return
	}
	if g.releaseTimer != nil {
		g.releaseTimer.Stop()
		g.releaseTimer = nil
	}
	close(g.reopen)
	g.reopen = nil
}
