package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// The conversation window's confirmation card (issue #76) is display-only
// (ADR 0013): everything it shows — the question, the verbatim command, the
// countdown — must come off this socket. These tests pin the two sources the
// card renders from: the deadline event published when the countdown starts,
// and the conversation.get snapshot a window opened *during* the wait uses,
// so that missing the events never means missing the question.

// startShellGateDaemon runs a wired daemon whose shell tool is behind the
// real permission gate, scripted to request one risky command. It also
// returns the socket path, because the snapshot test below needs what
// startTypingDaemon cannot give it: a *second* connection, dialled mid-wait,
// standing in for a window opened after the events went out.
func startShellGateDaemon(t *testing.T) (*ipc.Client, string) {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	cfg := testConfig()
	cfg.Audio.MinRecordingMs = 0
	cfg.Tools.Shell = true // the real gate; every test here declines the call
	provider := &ai.Fake{Response: "Understood, nothing was deleted."}
	provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "shell.run", Arguments: `{"command":"rm -rf ./build"}`}},
	}
	d, err := New(cfg, paths, nil, Deps{
		Provider:    provider,
		Transcriber: &stt.Fake{Text: "unused"},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	return dialDaemon(t, paths.Socket), paths.Socket
}

// TestConfirmationDeadlineIsPublishedOverTheSocket: the deadline follows the
// question onto the bus, carrying an absolute deadline for clients to count
// down from — the daemon's clock, not a client-side assumption of 30.
func TestConfirmationDeadlineIsPublishedOverTheSocket(t *testing.T) {
	client, _ := startShellGateDaemon(t)

	if err := client.Call("session.text", map[string]string{"text": "clean up"}, nil); err != nil {
		t.Fatal(err)
	}
	required := waitForEvent(t, client, "tool.confirmation_required")
	if required["command"] != "rm -rf ./build" {
		t.Errorf("published command = %v, want it verbatim", required["command"])
	}
	started := waitForEvent(t, client, "tool.confirmation_deadline")
	if started["command"] != "rm -rf ./build" {
		t.Errorf("deadline event command = %v, want it verbatim", started["command"])
	}
	deadline, ok := started["deadline_ms"].(float64)
	if !ok || deadline <= 0 {
		t.Fatalf("deadline_ms = %v, want a positive Unix-millisecond deadline", started["deadline_ms"])
	}
	if started["timeout_sec"] != float64(30) {
		t.Errorf("timeout_sec = %v, want the configured 30", started["timeout_sec"])
	}

	if err := client.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.declined")
	waitForEvent(t, client, "session.finished")
}

// TestSnapshotCarriesThePendingConfirmation: a window opened mid-wait renders
// from conversation.get, so the snapshot must carry the whole card — question,
// verbatim command, and the deadline the events announced — and must stop
// carrying it the moment the confirmation resolves. The snapshot is read on a
// second connection, exactly as a freshly-opened window would.
func TestSnapshotCarriesThePendingConfirmation(t *testing.T) {
	client, socket := startShellGateDaemon(t)

	if err := client.Call("session.text", map[string]string{"text": "clean up"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	started := waitForEvent(t, client, "tool.confirmation_deadline")

	// The freshly-opened window: a new connection that saw none of the above.
	late := dialDaemon(t, socket)

	var snapshot struct {
		State        string         `json:"state"`
		Confirmation map[string]any `json:"confirmation"`
	}
	if err := late.Call("conversation.get", nil, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.State != "awaiting_confirmation" {
		t.Errorf("snapshot state = %q, want awaiting_confirmation", snapshot.State)
	}
	if snapshot.Confirmation == nil {
		t.Fatal("the snapshot carries no pending confirmation; a window opened mid-wait would be blind")
	}
	if snapshot.Confirmation["command"] != "rm -rf ./build" {
		t.Errorf("snapshot command = %v, want it verbatim", snapshot.Confirmation["command"])
	}
	if summary, _ := snapshot.Confirmation["summary"].(string); summary == "" {
		t.Error("snapshot carries no question to show")
	}
	if snapshot.Confirmation["timeout_sec"] != float64(30) {
		t.Errorf("snapshot timeout_sec = %v, want the configured 30", snapshot.Confirmation["timeout_sec"])
	}
	if snapshot.Confirmation["deadline_ms"] != started["deadline_ms"] {
		t.Errorf("snapshot deadline %v disagrees with the published deadline %v",
			snapshot.Confirmation["deadline_ms"], started["deadline_ms"])
	}

	// The late window answers through the same session.confirm the buttons
	// call; afterwards the snapshot must not offer a stale card to anyone.
	if err := late.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.declined")
	waitForEvent(t, client, "session.finished")

	var after struct {
		Confirmation map[string]any `json:"confirmation"`
	}
	if err := late.Call("conversation.get", nil, &after); err != nil {
		t.Fatal(err)
	}
	if after.Confirmation != nil {
		t.Errorf("resolved confirmation still in the snapshot: %v", after.Confirmation)
	}
}
