// Package derivedgood is the other half of the fixture pair: every legitimate
// use of the same calls that exists in the real tree, collected in one file.
// The guard's own test asserts that NONE of it is reported. This is the file
// that stops the rule being tightened into something that cries wolf — and a
// rule that cries wolf gets deleted, taking the true positives with it.
//
// It lives under testdata so the go tool never builds it.
package derivedgood

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
