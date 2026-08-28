// Package derivedbad is a fixture, not a test. It reproduces the two shapes
// the derived-state rule exists to catch, as close to the originals as a
// self-contained file can get, and the guard's own test asserts that every one
// of them is reported. If a change to the rule stops it firing here, the rule
// has stopped watching.
//
// It lives under testdata so the go tool never builds it: none of the helpers
// below do anything, and several of the calls would not type-check.
package derivedbad

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
