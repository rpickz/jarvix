// Package derivedbad is a fixture, not a test. It reproduces the two shapes
// the derived-state rule exists to catch, as close to the originals as a
// self-contained file can get, and the guard's own test asserts that every one
// of them is reported. If a change to the rule stops it firing here, the rule
// has stopped watching.
//
// It lives under testdata so the go tool never builds it: none of the helpers
// below do anything, and several of the calls would not type-check.
package derivedbad

import "time"

type client struct{}

func (c *client) Call(method string, args, out any) error { return nil }

type engine struct{}

func (e *engine) ActiveConversationID() string { return "" }

type harness struct{ engine *engine }

type fake struct{ Ops chan string }

// The pre-#170 helper: it watches the store's op channel and returns. Seeing
// the op proves the turns are stored; it does not prove the engine adopted the
// id, because that happens after Append returns. Note the absence of any
// SyncArchive call — that absence is what makes this a cause and not a
// barrier.
func (h *harness) awaitAppend(f *fake) {
	for op := range f.Ops {
		if op == "append" {
			return
		}
	}
}

func waitForEvent(c *client, want string) map[string]any { return nil }

// #167: the row is derived from the event by the daemon's own subscriber, so
// waiting for the event and then sampling activity.get races the watcher.
func TestFeedRowSampledAfterOnlyItsEvent(t any) {
	c := &client{}
	waitForEvent(c, "tool.pre_approved")
	var feed map[string]any
	_ = c.Call("activity.get", nil, &feed)
}

// #170: the id is adopted after Append returns, so awaiting the append and
// then reading the id races the flush's tail.
func TestArchivedIDReadAfterOnlyTheAppend(t any) {
	h := &harness{engine: &engine{}}
	f := &fake{}
	h.awaitAppend(f)
	_ = h.engine.ActiveConversationID()
}

// An opt-out with no reason is itself a finding: the exceptions have to be
// argued, or the marker becomes a way to silence the rule quietly.
//
// testdiscipline:allow
func TestOptOutWithoutAReason(t any) {
	c := &client{}
	waitForEvent(c, "tool.pre_approved")
	var feed map[string]any
	_ = c.Call("activity.get", nil, &feed)
}

// The #215 half of the fixture: an *action* taken on the belief that a turn is
// still in flight, ordered by an event published before the provider request is
// even opened. The engine methods below stand in for the real ones; what
// matters is the shape.

type provider struct{ Delay time.Duration }

func (h *harness) waitFor(t any, event string) map[string]any { return nil }

func (e *engine) StartWake() (string, error)     { return "", nil }
func (e *engine) StartSession() (string, error)  { return "", nil }
func (e *engine) Submit(text string) error       { return nil }
func (e *engine) Conversation() []map[string]any { return nil }

// #215, sighting A: with nothing holding the turn open it can be over before
// the wake word arrives, and then there is no session to cancel and the
// session.cancelled this would wait for is never owed.
func TestWakeActedOnAssistantStarted(t any) {
	h := &harness{engine: &engine{}}
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("tell me a story")
	h.waitFor(t, "assistant.started")
	_, _ = h.engine.StartWake()
}

// #215, sighting C: the same belief, this time that the provider call the
// fake parks is the first session's. assistant.started is published before
// that call is made.
func TestSupersessionActedOnAssistantStarted(t any) {
	h := &harness{engine: &engine{}}
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("what's the weather like?")
	h.waitFor(t, "assistant.started")
	_, _ = h.engine.StartSession()
}

// The same shape with a read rather than an action: the mid-turn conversation
// holds one turn only while the turn is still running. A bounded delay is not
// a barrier — it is the window the whole family is made of — so the rule must
// still fire here.
func TestInFlightConversationReadAfterTheStart(t any) {
	h := &harness{engine: &engine{}}
	p := &provider{}
	p.Delay = 30 * time.Millisecond
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("what is streaming?")
	h.waitFor(t, "assistant.started")
	_ = h.engine.Conversation()
}
