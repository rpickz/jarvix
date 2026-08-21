package desktop

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Fakes for context gathering. They exist so that no test anywhere in the
// tree needs a compositor, a Wayland session, or anything on screen: the
// engine tests drive a FakeCollector, and the collector's own tests drive
// FakeGatherers (or stub binaries on PATH, for the argv guarantees).

// FakeGatherer is a scripted context source. Blocked, when set, makes Gather
// wait for the context to be cancelled and then report its error — which is
// how a hung source is tested without a sleep anywhere.
type FakeGatherer struct {
	Src     Source
	Text    string
	Err     error
	Blocked bool
	// Started is closed by Gather as soon as it is entered, so a test can
	// prove sources really are gathered in parallel by waiting for all of
	// them before letting any finish. Nil means no signal.
	Started chan struct{}
	// Release, when non-nil, holds Gather until it is closed. Paired with
	// Started, it pins every source open at once.
	Release chan struct{}

	calls atomic.Int64
}

// Source implements Gatherer.
func (f *FakeGatherer) Source() Source { return f.Src }

// Gather implements Gatherer.
func (f *FakeGatherer) Gather(ctx context.Context) (string, error) {
	f.calls.Add(1)
	if f.Started != nil {
		close(f.Started)
	}
	if f.Release != nil {
		select {
		case <-f.Release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if f.Blocked {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if f.Err != nil {
		return "", f.Err
	}
	return f.Text, nil
}

// Calls reports how many times this source was read — the assertion behind
// "a disabled source is never gathered".
func (f *FakeGatherer) Calls() int { return int(f.calls.Load()) }

// FakeCollector hands back a scripted snapshot and counts collections, so an
// engine test can assert both what the model was told and — for a matched
// intent — that nothing was gathered at all.
type FakeCollector struct {
	// Snapshot is returned from every Collect. Items get At/Elapsed filled in
	// if they are unset, so callers only script what they care about.
	Snapshot Snapshot

	mu     sync.Mutex
	calls  int
	lastAt time.Time
}

// Collect implements the collector contract the session engine depends on.
func (f *FakeCollector) Collect(context.Context) Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastAt = time.Now()
	snap := f.Snapshot
	if snap.At.IsZero() {
		snap.At = f.lastAt
	}
	return snap
}

// Calls reports how many captures were requested.
func (f *FakeCollector) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}
