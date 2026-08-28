package daemon

import (
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
)

// A window opened mid-thought must show what a window that was already open
// shows (issue #158). It has none of the events, so everything its pending
// turn is built from has to ride the conversation.get snapshot: the state, and
// when that state began on the daemon's own clock.
//
// The wait exercised here is the confirmation gate's, because it is the one a
// test can hold open indefinitely without racing anything — the daemon sits in
// awaiting_confirmation until somebody answers.
func TestSnapshotCarriesThePhaseStartForALateWindow(t *testing.T) {
	client, socket := startShellGateDaemon(t)
	before := time.Now()

	if err := client.Call("session.text", map[string]string{"text": "clean up"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")

	// The freshly-opened window: a connection that saw none of the above.
	late := dialDaemon(t, socket)

	var snapshot struct {
		State        string  `json:"state"`
		StateSinceMs float64 `json:"state_since_ms"`
		Tool         string  `json:"tool"`
		ToolDetail   string  `json:"tool_detail"`
	}
	if err := late.Call("conversation.get", nil, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.State != "awaiting_confirmation" {
		t.Fatalf("snapshot state = %q, want awaiting_confirmation", snapshot.State)
	}
	if snapshot.StateSinceMs <= 0 {
		t.Fatal("the snapshot carries no phase start; a late window would count this wait from zero")
	}
	since := int64(snapshot.StateSinceMs)
	if since < before.UnixMilli() || since > time.Now().UnixMilli() {
		t.Errorf("phase start %d is outside the test's own window [%d, %d]",
			since, before.UnixMilli(), time.Now().UnixMilli())
	}
	// Nothing is executing while the gate waits — the whole point of the gate
	// is that the call has not run yet — so the snapshot must not name one.
	if snapshot.Tool != "" || snapshot.ToolDetail != "" {
		t.Errorf("snapshot names tool %q / %q while the gate is still asking",
			snapshot.Tool, snapshot.ToolDetail)
	}
	// End to end: the same facts, through the same Go the window's generated
	// library mirrors, produce the pending turn the window will draw. While a
	// question is open it says exactly that and nothing that could compete
	// with the confirmation card sitting under it.
	line := desktop.PendingTurnLine(snapshot.State, snapshot.Tool, snapshot.ToolDetail, 4)
	if line != "Waiting for your answer · 4s" {
		t.Errorf("a late window would show %q", line)
	}

	if err := late.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.declined")
	waitForEvent(t, client, "session.finished")

	// And once the turn is over the snapshot says so: idle words no pending
	// turn at all, which is how the window knows to stop showing one rather
	// than leave it counting up forever.
	var after struct {
		State        string  `json:"state"`
		StateSinceMs float64 `json:"state_since_ms"`
	}
	if err := late.Call("conversation.get", nil, &after); err != nil {
		t.Fatal(err)
	}
	if after.State != "idle" {
		t.Errorf("snapshot state after the turn = %q", after.State)
	}
	if after.StateSinceMs < snapshot.StateSinceMs {
		t.Errorf("the idle phase began at %v, before the wait it replaced (%v)",
			after.StateSinceMs, snapshot.StateSinceMs)
	}
	if line := desktop.PendingTurnLine(after.State, "", "", 30); line != "" {
		t.Errorf("an ended turn still words a pending line: %q", line)
	}
}

// state.changed carries the phase start over the wire too, so a window that
// *was* open counts from the same instant the late one does. Without it the
// two would disagree by however long the window took to notice.
func TestStateChangedCarriesThePhaseStartOverTheSocket(t *testing.T) {
	client, _ := startShellGateDaemon(t)
	before := time.Now().UnixMilli()

	if err := client.Call("session.text", map[string]string{"text": "clean up"}, nil); err != nil {
		t.Fatal(err)
	}
	changed := waitForEvent(t, client, "state.changed")
	since, ok := changed["since_ms"].(float64)
	if !ok || since <= 0 {
		t.Fatalf("state.changed since_ms = %#v, want a positive Unix-millisecond start", changed["since_ms"])
	}
	if int64(since) < before || int64(since) > time.Now().UnixMilli() {
		t.Errorf("phase start %d is outside the test's own window", int64(since))
	}

	// Decline the gate the scripted turn opens, so the session ends rather
	// than sitting out its timeout.
	waitForEvent(t, client, "tool.confirmation_required")
	if err := client.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "session.finished")
}
