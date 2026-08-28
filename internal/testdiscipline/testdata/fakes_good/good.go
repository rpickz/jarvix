// Package fakesgood collects every legitimate exported field a fake in this
// repo carries, so the rule cannot be tightened into something that condemns
// them all. The guard's own test asserts that none of it is reported.
//
// It lives under testdata so the go tool never builds it.
package fakesgood

import (
	"context"
	"sync"
)

// Fake is tts.Fake as it stands after #149.
type Fake struct {
	// Scripting: written by the test at construction, only ever read by the
	// fake. This is the bulk of every fake's surface and none of it is a race.
	Chunks [][]byte
	Fail   error

	// A func field is scripting too — the fake calls it, and calling is not
	// writing. ai.Fake.BeforeToolCalls is exactly this.
	BeforeToolCalls func(ctx context.Context)

	// Channels stay exported on purpose: the notifying-fake pattern needs the
	// test to receive on them, and a send is not a data race. Assigning one
	// inside a method (SetHold's shape, but on an exported field) would still
	// be excluded, which is the one thing this rule knowingly gives up in
	// exchange for not breaking conversations.Fake and history.Fake.
	Ops         chan string
	AppendGate  chan struct{}
	SaveStarted chan struct{}

	// The recording field, unexported, behind an accessor that takes the same
	// mutex the write does. This is the fix #149 landed.
	lastRequest string
	speaks      int
	mu          sync.Mutex
}

func (f *Fake) Last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRequest
}

func (f *Fake) Speaks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.speaks
}

func (f *Fake) Speak(req string) [][]byte {
	f.mu.Lock()
	f.lastRequest = req
	f.speaks++
	f.mu.Unlock()
	if f.BeforeToolCalls != nil {
		f.BeforeToolCalls(context.Background())
	}
	select {
	case f.Ops <- "speak":
	default:
	}
	return f.Chunks
}

// A seeder writes the fake's state from the TEST's goroutine, before the fake
// is handed over. conversations.Fake.Seed is this. It writes only unexported
// fields, so nothing here is reported either.
func (f *Fake) Seed(req string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRequest = req
}

// A type whose name does not mark it as scaffolding is out of scope, however
// it behaves. Widening past the naming convention would mean deciding which
// production types are only used by tests, which a source scan cannot do.
type recorder struct {
	Calls int
}

func (r *recorder) Record() { r.Calls++ }

// A receiver with no name cannot write to a field, and must not confuse the
// scan into thinking it did.
type FakeNamer struct{ Name string }

func (*FakeNamer) Kind() string { return "fake" }
