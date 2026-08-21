package history

import (
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
)

// Fake is an in-memory Store for tests. Each operation can be scripted to
// fail, and completed operations are announced on Ops so tests can wait for
// persistence — which the engine runs after session.finished, off its lock
// path — without sleeping.
type Fake struct {
	// LoadErr, SaveErr and ClearErr, when set, make the matching operation
	// fail. Set them before handing the Fake to an engine.
	LoadErr  error
	SaveErr  error
	ClearErr error
	// Ops receives "save" or "clear" after each successful or failed attempt
	// completes. Buffered; sends never block.
	Ops chan string

	// SaveStarted and SaveGate turn a Save into something a test can hold
	// open, which is what shutdown-drain tests need: the write the daemon must
	// wait for has to be *in flight* while shutdown runs, and no amount of
	// event-waiting can arrange that from outside.
	//
	// SaveStarted, when non-nil, receives once at the top of every Save,
	// before the gate. Receiving from it is the guarantee that a write is
	// under way and cannot finish yet.
	//
	// SaveGate, when non-nil, holds every Save there until the channel is
	// closed (or a value is received from it).
	//
	// Both must be set before the Fake is handed to an engine; they are read
	// without the lock precisely so a held-open Save does not also hold the
	// Fake's mutex, which would make the block indistinguishable from a
	// deadlock in the store itself.
	SaveStarted chan struct{}
	SaveGate    chan struct{}

	mu       sync.Mutex
	messages []ai.Message
	lastTurn time.Time
	saves    int
	clears   int
}

// NewFake returns a Fake ready for waiting on Ops.
func NewFake() *Fake { return &Fake{Ops: make(chan string, 64)} }

// Seed installs a persisted history without counting as a Save, as if a
// previous daemon run had written it.
func (f *Fake) Seed(messages []ai.Message, lastTurn time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append([]ai.Message(nil), messages...)
	f.lastTurn = lastTurn
}

// Load implements Store.
func (f *Fake) Load() ([]ai.Message, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.LoadErr != nil {
		return nil, time.Time{}, f.LoadErr
	}
	return append([]ai.Message(nil), f.messages...), f.lastTurn, nil
}

// Save implements Store.
func (f *Fake) Save(messages []ai.Message, lastTurn time.Time) error {
	if f.SaveStarted != nil {
		f.SaveStarted <- struct{}{}
	}
	if f.SaveGate != nil {
		<-f.SaveGate
	}
	f.mu.Lock()
	f.saves++
	err := f.SaveErr
	if err == nil {
		f.messages = append([]ai.Message(nil), messages...)
		f.lastTurn = lastTurn
	}
	f.mu.Unlock()
	f.notify("save")
	return err
}

// Clear implements Store.
func (f *Fake) Clear() error {
	f.mu.Lock()
	f.clears++
	err := f.ClearErr
	if err == nil {
		f.messages = nil
		f.lastTurn = time.Time{}
	}
	f.mu.Unlock()
	f.notify("clear")
	return err
}

// Saves reports how many times Save was attempted.
func (f *Fake) Saves() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.saves
}

// Clears reports how many times Clear was attempted.
func (f *Fake) Clears() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clears
}

func (f *Fake) notify(op string) {
	if f.Ops == nil {
		return
	}
	select {
	case f.Ops <- op:
	default:
	}
}
