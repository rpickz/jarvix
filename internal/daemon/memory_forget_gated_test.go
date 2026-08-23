package daemon

import (
	"errors"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ipc"
)

// The window's per-fact Forget (issue #92) over the socket: the button calls
// memory.forget_gated, the standard confirmation card's events follow with
// the exact fact named, and only an approval deletes — every decision
// daemon-side, the window rendering and calling (ADR 0013).

// memoryFacts lists the store over the socket.
func memoryFacts(t *testing.T, client *ipc.Client) []any {
	t.Helper()
	var listing map[string]any
	if err := client.Call("memory.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	facts, _ := listing["facts"].([]any)
	return facts
}

// TestMemoryForgetGatedApproveDeletesOverSocket: the full approve round trip.
func TestMemoryForgetGatedApproveDeletesOverSocket(t *testing.T) {
	client, provider, dir := startMemoryDaemon(t, nil)
	seedMemoryFile(t, dir)

	var res map[string]any
	if err := client.Call("memory.forget_gated", map[string]string{"id": "m1"}, &res); err != nil {
		t.Fatal(err)
	}
	if res["session_id"] == "" {
		t.Fatalf("reply = %v, want the session id the card belongs to", res)
	}

	required := waitForEvent(t, client, "tool.confirmation_required")
	if required["tool"] != "memory.forget" {
		t.Errorf("tool = %v, want the memory.forget identity", required["tool"])
	}
	command, _ := required["command"].(string)
	if !strings.Contains(command, "m1") ||
		!strings.Contains(command, "the staging server is called atlas") {
		t.Errorf("command = %q, want the exact fact named — resolved from the store", command)
	}

	if err := client.Call("session.confirm", map[string]bool{"approved": true}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.finished")
	waitForEvent(t, client, "session.finished")

	facts := memoryFacts(t, client)
	if len(facts) != 1 {
		t.Fatalf("facts after approved forget = %v, want only m2 left", facts)
	}
	if fact, _ := facts[0].(map[string]any); fact["id"] != "m2" {
		t.Errorf("surviving fact = %v, want m2", fact)
	}
	if len(provider.Requests) != 0 {
		t.Errorf("the provider was called %d times; the button is not a model turn", len(provider.Requests))
	}
}

// TestMemoryForgetGatedDeclineKeepsTheFact: a decline through the card's own
// path deletes nothing.
func TestMemoryForgetGatedDeclineKeepsTheFact(t *testing.T) {
	client, _, dir := startMemoryDaemon(t, nil)
	seedMemoryFile(t, dir)

	if err := client.Call("memory.forget_gated", map[string]string{"id": "m2"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	if err := client.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.declined")
	waitForEvent(t, client, "session.finished")

	if facts := memoryFacts(t, client); len(facts) != 2 {
		t.Errorf("facts after decline = %v, want both kept", facts)
	}
}

// TestMemoryForgetGatedRefusals: an unknown id and a missing id are crisp
// errors — no session starts just to apologise.
func TestMemoryForgetGatedRefusals(t *testing.T) {
	client, _, dir := startMemoryDaemon(t, nil)
	seedMemoryFile(t, dir)

	err := client.Call("memory.forget_gated", map[string]string{"id": "m99"}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeInvalidParams ||
		!strings.Contains(rpcErr.Message, `"m99"`) {
		t.Errorf("unknown id error = %v, want -32602 naming it", err)
	}
	if err := client.Call("memory.forget_gated", nil, nil); err == nil {
		t.Error("a forget with no id was accepted")
	}
	if facts := memoryFacts(t, client); len(facts) != 2 {
		t.Errorf("facts after refusals = %v, want both kept", facts)
	}
}
