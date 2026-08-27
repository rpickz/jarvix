package daemon

import (
	"testing"
)

// The overlay's confirmation surface (issue #119) is display-only like the
// window card (ADR 0013): approve/decline are the same session.confirm verb,
// and its opening state comes from status.get — the one call the overlay
// already makes on connect. This test pins that source: the pending
// confirmation must ride status.get with the same facts conversation.get
// carries, so an overlay attaching mid-wait is never blind to a question that
// is already open, and must leave the reply the moment it resolves.
func TestStatusCarriesThePendingConfirmation(t *testing.T) {
	client, socket := startShellGateDaemon(t)

	// Before anything is pending, the field is null — a client must be able
	// to clear its card from the same read that would have populated it.
	var idle struct {
		Confirmation map[string]any `json:"confirmation"`
	}
	if err := client.Call("status.get", nil, &idle); err != nil {
		t.Fatal(err)
	}
	if idle.Confirmation != nil {
		t.Errorf("status.get carries a confirmation while nothing is pending: %v", idle.Confirmation)
	}

	if err := client.Call("session.text", map[string]string{"text": "clean up"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	started := waitForEvent(t, client, "tool.confirmation_deadline")

	// The freshly-attached overlay: a new connection that saw none of the
	// events, exactly as the mid-wait window test dials for conversation.get.
	late := dialDaemon(t, socket)
	var status struct {
		State        string         `json:"state"`
		Confirmation map[string]any `json:"confirmation"`
	}
	if err := late.Call("status.get", nil, &status); err != nil {
		t.Fatal(err)
	}
	if status.State != "awaiting_confirmation" {
		t.Errorf("status state = %q, want awaiting_confirmation", status.State)
	}
	if status.Confirmation == nil {
		t.Fatal("status.get carries no pending confirmation; an overlay attaching mid-wait would be blind")
	}
	if status.Confirmation["command"] != "rm -rf ./build" {
		t.Errorf("status command = %v, want it verbatim", status.Confirmation["command"])
	}
	if summary, _ := status.Confirmation["summary"].(string); summary == "" {
		t.Error("status carries no question to show")
	}
	if status.Confirmation["tool"] != "shell.run" {
		t.Errorf("status tool = %v, want shell.run", status.Confirmation["tool"])
	}
	// The same daemon-computed deadline every surface counts down from:
	// status.get and the deadline event must never disagree, or the overlay
	// and the card would show different clocks for one question.
	if status.Confirmation["deadline_ms"] != started["deadline_ms"] {
		t.Errorf("status deadline %v disagrees with the published deadline %v",
			status.Confirmation["deadline_ms"], started["deadline_ms"])
	}

	// Resolving through the shared verb — what the overlay's tick/cross call —
	// clears the field for every later reader.
	if err := late.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.declined")
	waitForEvent(t, client, "session.finished")

	var after struct {
		Confirmation map[string]any `json:"confirmation"`
	}
	if err := late.Call("status.get", nil, &after); err != nil {
		t.Fatal(err)
	}
	if after.Confirmation != nil {
		t.Errorf("resolved confirmation still in status.get: %v", after.Confirmation)
	}
}
