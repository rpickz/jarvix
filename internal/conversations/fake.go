package conversations

import (
	"fmt"
	"sync"
)

// Fake is an in-memory Store for tests. Each operation can be scripted to
// fail, and completed operations are announced on Ops so tests can wait for
// archive writes — which the engine runs after session.finished, off its lock
// path — without sleeping (the history.Fake pattern).
type Fake struct {
	// AppendErr, ReadErr, DeleteErr and ListErr, when set, make the matching
	// operation fail. Set them before handing the Fake to an engine.
	AppendErr error
	ReadErr   error
	DeleteErr error
	ListErr   error
	// Ops receives "append", "set_active", "delete" or "delete_all" after
	// each attempt completes, successful or not. Buffered; sends never block.
	Ops chan string

	// AppendStarted and AppendGate turn an Append into something a test can
	// hold open — what a shutdown-drain test needs: the write the daemon must
	// wait for has to be *in flight* while shutdown runs. AppendStarted, when
	// non-nil, receives once at the top of every Append, before the gate;
	// AppendGate, when non-nil, holds every Append there until the channel is
	// closed. Both must be set before the Fake is handed to an engine; they
	// are read without the lock so a held-open Append does not also hold the
	// Fake's mutex (see history.Fake for the full reasoning).
	AppendStarted chan struct{}
	AppendGate    chan struct{}

	mu      sync.Mutex
	records map[string]*Conversation
	order   []string // creation order, for deterministic listing ties
	active  string
	counter int
	appends int
	deletes int
}

// NewFake returns a Fake ready for waiting on Ops.
func NewFake() *Fake {
	return &Fake{Ops: make(chan string, 64), records: map[string]*Conversation{}}
}

// Seed installs an archived conversation without counting as an Append, as if
// a previous daemon run had written it.
func (f *Fake) Seed(meta Meta, turns []Turn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	meta.Schema = SchemaVersion
	meta.TurnCount = len(turns)
	f.records[meta.ID] = &Conversation{Meta: meta, Turns: append([]Turn(nil), turns...)}
	f.order = append(f.order, meta.ID)
}

// SeedActive marks id as the conversation the live head belongs to.
func (f *Fake) SeedActive(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = id
}

// Append implements Store.
func (f *Fake) Append(id string, turns []Turn) (string, error) {
	if len(turns) == 0 {
		return id, nil
	}
	if f.AppendStarted != nil {
		f.AppendStarted <- struct{}{}
	}
	if f.AppendGate != nil {
		<-f.AppendGate
	}
	f.mu.Lock()
	f.appends++
	err := f.AppendErr
	if err == nil {
		rec, ok := f.records[id]
		if id == "" || !ok {
			f.counter++
			id = fmt.Sprintf("conv%d", f.counter)
			rec = &Conversation{Meta: Meta{Schema: SchemaVersion, ID: id, Started: turns[0].Time}}
			f.records[id] = rec
			f.order = append(f.order, id)
		}
		rec.Turns = append(rec.Turns, turns...)
		rec.Meta.LastActive = turns[len(turns)-1].Time
		rec.Meta.TurnCount = len(rec.Turns)
		if rec.Meta.Preview == "" {
			rec.Meta.Preview = preview(turns)
		}
		f.active = id
	}
	f.mu.Unlock()
	f.notify("append")
	return id, err
}

// Active implements Store.
func (f *Fake) Active() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.records[f.active]; !ok {
		return ""
	}
	return f.active
}

// SetActive implements Store.
func (f *Fake) SetActive(id string) error {
	f.mu.Lock()
	f.active = id
	f.mu.Unlock()
	f.notify("set_active")
	return nil
}

// List implements Store: newest first by LastActive, creation order breaking
// ties so tests with a frozen clock stay deterministic.
func (f *Fake) List() ([]Meta, []Unreadable, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ListErr != nil {
		return nil, nil, f.ListErr
	}
	metas := []Meta{}
	for i := len(f.order) - 1; i >= 0; i-- {
		if rec, ok := f.records[f.order[i]]; ok {
			metas = append(metas, rec.Meta)
		}
	}
	for i := 1; i < len(metas); i++ {
		for j := i; j > 0 && metas[j].LastActive.After(metas[j-1].LastActive); j-- {
			metas[j], metas[j-1] = metas[j-1], metas[j]
		}
	}
	return metas, []Unreadable{}, nil
}

// Read implements Store.
func (f *Fake) Read(id string) (Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ReadErr != nil {
		return Conversation{}, f.ReadErr
	}
	rec, ok := f.records[id]
	if !ok {
		return Conversation{}, fmt.Errorf("no conversation has id %q", id)
	}
	return Conversation{Meta: rec.Meta, Turns: append([]Turn(nil), rec.Turns...)}, nil
}

// Delete implements Store.
func (f *Fake) Delete(id string) error {
	f.mu.Lock()
	f.deletes++
	err := f.DeleteErr
	if err == nil {
		if _, ok := f.records[id]; !ok {
			err = fmt.Errorf("no conversation has id %q", id)
		} else {
			delete(f.records, id)
			if f.active == id {
				f.active = ""
			}
		}
	}
	f.mu.Unlock()
	f.notify("delete")
	return err
}

// DeleteAll implements Store.
func (f *Fake) DeleteAll() (int, error) {
	f.mu.Lock()
	n := len(f.records)
	err := f.DeleteErr
	if err == nil {
		f.records = map[string]*Conversation{}
		f.order = nil
		f.active = ""
	}
	f.mu.Unlock()
	f.notify("delete_all")
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Appends reports how many times Append was attempted.
func (f *Fake) Appends() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.appends
}

// Turns returns a copy of one conversation's turns, for assertions.
func (f *Fake) Turns(id string) []Turn {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[id]
	if !ok {
		return nil
	}
	return append([]Turn(nil), rec.Turns...)
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
