// Package derivedgood is the other half of the fixture pair: every legitimate
// use of the same calls that exists in the real tree, collected in one file.
// The guard's own test asserts that NONE of it is reported. This is the file
// that stops the rule being tightened into something that cries wolf — and a
// rule that cries wolf gets deleted, taking the true positives with it.
//
// It lives under testdata so the go tool never builds it.
package derivedgood

import "time"

type client struct{}

func (c *client) Call(method string, args, out any) error { return nil }

type engine struct{}

func (e *engine) ActiveConversationID() string { return "" }
func (e *engine) SyncArchive()                 {}
func (e *engine) Shutdown() error              { return nil }

type harness struct{ engine *engine }

type fake struct{ Ops chan string }

func (f *fake) Appends() int { return 0 }

// The post-#170 helper takes the engine's read barrier before returning, so
// every caller gets the adoption for free. The rule resolves this by body: a
// helper that calls SyncArchive IS a barrier, wherever its callers are.
func (h *harness) awaitAppend(f *fake) {
	for op := range f.Ops {
		if op == "append" {
			h.engine.SyncArchive()
			return
		}
	}
}

func waitForEvent(c *client, want string) map[string]any    { return nil }
func waitForActivityRow(c *client, label string)            {}
func activityRowsOf(c *client) []map[string]any             { return nil }
func conversationTurns(c *client) []map[string]any          { return nil }
func entryCall(c *client, m string, a any) map[string]any   { return nil }
func drainEvents(c *client) []map[string]any                { return nil }
func waitForRunObserved(c *client, label string)            {}
func waitActivityRow(c *client, want string) map[string]any { return nil }

// The corrected #167 shape: wait for the row, which is published after it is
// appended, and only then sample the feed.
func TestFeedRowSampledAfterTheRow(t any) {
	c := &client{}
	waitForEvent(c, "tool.pre_approved")
	waitForActivityRow(c, "Ran without asking: shell.run")
	waitForEvent(c, "session.finished")
	var feed map[string]any
	_ = c.Call("activity.get", nil, &feed)
}

// The same, through the wrapper helper and the other spelling of the barrier.
func TestFeedRowsThroughTheHelper(t any) {
	c := &client{}
	waitActivityRow(c, "Failed at assistant")
	_ = activityRowsOf(c)
}

// waitForRunObserved watches for the row and the session's end at once, so it
// is a barrier too.
func TestRunObserved(t any) {
	c := &client{}
	waitForRunObserved(c, "Ran: backup notes")
	_ = activityRowsOf(c)
}

// A purely synchronous read: no event was waited for, so there is no cause to
// have mistaken for an effect and the rule must stay quiet. Most reads of
// activity.get in the tree look like this.
func TestFeedReadSynchronously(t any) {
	c := &client{}
	_ = entryCall(c, "config.get_entry", nil)
	var feed map[string]any
	_ = c.Call("activity.get", nil, &feed)
}

// conversation.get reads the engine directly and an exchange is committed
// before session.finished publishes, so this is correct and deliberately not
// a rule. If it ever became one, this function would be the first casualty.
func TestConversationReadAfterTheEvent(t any) {
	c := &client{}
	waitForEvent(c, "session.finished")
	_ = conversationTurns(c)
	var snapshot map[string]any
	_ = c.Call("conversation.get", nil, &snapshot)
}

// The corrected #170 shape, via the helper that now takes the barrier.
func TestArchivedIDAfterTheHelperBarrier(t any) {
	h := &harness{engine: &engine{}}
	f := &fake{}
	h.awaitAppend(f)
	_ = h.engine.ActiveConversationID()
}

// The barrier taken explicitly, as interrupted_test.go does.
func TestArchivedIDAfterAnExplicitBarrier(t any) {
	h := &harness{engine: &engine{}}
	f := &fake{}
	h.awaitAppend(f)
	h.engine.SyncArchive()
	_ = h.engine.ActiveConversationID()
}

// No append was observed at all — the assertion is that there is no id,
// because archiving is off. TestNilArchiveNeverWrites is exactly this, and an
// unconditional "always take the barrier" rule would have condemned it.
func TestNoArchiveMeansNoID(t any) {
	h := &harness{engine: &engine{}}
	if id := h.engine.ActiveConversationID(); id != "" {
		panic(id)
	}
}

// Shutdown drains the archive before it returns, so the id is adopted by the
// time it does.
func TestArchivedIDAfterShutdown(t any) {
	h := &harness{engine: &engine{}}
	f := &fake{}
	h.awaitAppend(f)
	_ = h.engine.Shutdown()
	_ = f.Appends()
	_ = h.engine.ActiveConversationID()
}

// An argued opt-out. The reason is the point: it is what a reviewer reads.
//
// testdiscipline:allow the sweep asserts a negative, so a row that has not
// landed cannot fail it.
func TestOptOutWithAReason(t any) {
	c := &client{}
	waitForEvent(c, "config.entry_changed")
	var feed map[string]any
	_ = c.Call("activity.get", nil, &feed)
	_ = drainEvents(c)
}

// The #215 half: every legitimate way the tree orders an action against a turn
// that is genuinely still in flight. None of these may be reported.

type tts struct{}

func (s *tts) SetHold(hold chan struct{}) {}

type provider struct {
	Delay  time.Duration
	parked chan struct{}
}

func (h *harness) waitFor(t any, event string) map[string]any             { return nil }
func (h *harness) collectUntil(t any, event string) map[string]any        { return nil }
func (e *engine) StartWake() (string, error)                              { return "", nil }
func (e *engine) StartSession() (string, error)                           { return "", nil }
func (e *engine) Submit(text string) error                                { return nil }
func (e *engine) Cancel() error                                           { return nil }
func (e *engine) CancelSpeech() bool                                      { return false }
func (e *engine) ReplaySpeech(n int, role string) (string, string, error) { return "", "", nil }
func (e *engine) Conversation() []map[string]any                          { return nil }

// The corrected sighting-A shape: the synthesizer is parked, so the turn
// cannot end however the runner schedules it, and the wait is for the event
// that says speech has actually begun.
func TestWakeInterruptsHeldSpeech(t any) {
	h := &harness{engine: &engine{}}
	s := &tts{}
	hold := make(chan struct{})
	s.SetHold(hold)
	defer close(hold)
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("tell me a story")
	h.waitFor(t, "tts.started")
	_, _ = h.engine.StartWake()
}

// The corrected sighting-C shape: the fake says when it has parked, and that
// channel — not an event — is the barrier. There is no cause here at all, so
// the rule has nothing to fire on, which is the point.
func TestSupersessionOrderedByTheParkedCall(t any) {
	h := &harness{engine: &engine{}}
	p := &provider{parked: make(chan struct{})}
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("what's the weather like?")
	<-p.parked
	_, _ = h.engine.StartSession()
	h.waitFor(t, "session.cancelled")
}

// A provider stalled for an hour cannot produce a chunk, so the turn cannot
// end: the assignment is the barrier, and the wait after it is free to be the
// cheap one. interrupted_test.go takes this route twice.
func TestCancelAfterStallingTheProvider(t any) {
	h := &harness{engine: &engine{}}
	p := &provider{}
	p.Delay = time.Hour
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("summarise my inbox")
	h.waitFor(t, "assistant.started")
	_ = h.engine.Cancel()
	h.waitFor(t, "session.cancelled")
}

// The same barrier, guarding a mid-turn read rather than an action.
func TestInFlightConversationBehindAStall(t any) {
	h := &harness{engine: &engine{}}
	p := &provider{}
	p.Delay = time.Hour
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("what is streaming?")
	h.waitFor(t, "assistant.started")
	_ = h.engine.Conversation()
	_ = h.engine.Cancel()
}

// A delta is proof the provider call is open and streaming, which is what the
// tree already writes down at session/text_test.go:88. It is weaker than a
// park and stronger than the event it replaces, and it is accepted.
func TestInterruptOrderedByTheFirstDelta(t any) {
	h := &harness{engine: &engine{}}
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("first question")
	h.waitFor(t, "assistant.delta")
	_, _ = h.engine.StartSession()
}

// No turn was ever observed to start: the actions are ordered by nothing
// asynchronous, so there is no cause to have mistaken for an effect. Most
// StartSession calls in the tree look like this, and a rule that fired on them
// would be deleted the same week.
func TestPlainSessionNeedsNoBarrier(t any) {
	h := &harness{engine: &engine{}}
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("explain recursion")
	h.collectUntil(t, "session.finished")
	_ = h.engine.Conversation()
}

// Waiting for assistant.started to assert something *about that event* is not
// this shape: nothing is acted on, and the read is of the event itself.
func TestAssistantStartedCarriesItsTier(t any) {
	h := &harness{engine: &engine{}}
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("what is streaming?")
	started := h.waitFor(t, "assistant.started")
	if started["tier"] != "instant" {
		panic(started)
	}
	h.collectUntil(t, "session.finished")
}
