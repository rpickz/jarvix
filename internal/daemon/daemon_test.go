package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// startDaemon runs a fully wired daemon over a real Unix socket, with all
// engines faked: the complete integration surface minus hardware.
func startDaemon(t *testing.T) (*ipc.Client, *ai.Fake) {
	t.Helper()
	dir := daemonTempDir(t)
	paths := config.Paths{
		Config:  dir,
		Data:    dir,
		State:   dir,
		Runtime: dir,
		Socket:  filepath.Join(dir, "j.sock"),
	}
	cfg := testConfig()
	// Fake voice flows start and stop capture instantly; the accidental-tap
	// guard has its own tests in internal/session.
	cfg.Audio.MinRecordingMs = 0
	// Persistence runs async after session.finished and would race t.TempDir
	// cleanup ("directory not empty"); it has its own tests in
	// internal/session, so daemon tests run memory-only.
	cfg.Conversation.HistoryTurns = 0
	provider := &ai.Fake{Response: "Streaming works."}
	d, err := New(cfg, paths, nil, Deps{
		Provider:    provider,
		Transcriber: &stt.Fake{Text: "hello computer"},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		// Notifications default on, so keep the tests hermetic: no real
		// notify-send, no window opening on the machine running the suite.
		Notifier:   &desktop.FakeNotifier{},
		OpenWindow: func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = d.Run(ctx) }()

	// Wait for the socket to come up.
	var client *ipc.Client
	deadline := time.Now().Add(5 * time.Second)
	for {
		client, err = ipc.Dial(paths.Socket)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon socket never came up: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, provider
}

func TestStatusOverSocket(t *testing.T) {
	client, _ := startDaemon(t)
	var status map[string]any
	if err := client.Call("status.get", nil, &status); err != nil {
		t.Fatal(err)
	}
	if status["state"] != "idle" {
		t.Errorf("state = %v", status["state"])
	}
	if status["protocol"] != float64(ipc.ProtocolVersion) {
		t.Errorf("protocol = %v", status["protocol"])
	}
}

func TestAskFlowOverSocket(t *testing.T) {
	client, _ := startDaemon(t)

	var started map[string]string
	if err := client.Call("session.start", nil, &started); err != nil {
		t.Fatal(err)
	}
	if started["session_id"] == "" {
		t.Fatal("no session id")
	}
	if err := client.Call("session.submit", map[string]string{"text": "hi"}, nil); err != nil {
		t.Fatal(err)
	}

	var response string
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-client.Events():
			switch ev.Type {
			case "assistant.delta":
				response += ev.Data["content"].(string)
			case "session.finished":
				if response != "Streaming works." {
					t.Errorf("streamed response = %q", response)
				}
				return
			case "error":
				t.Fatalf("error event: %v", ev.Data)
			}
		case <-deadline:
			t.Fatal("session never finished")
		}
	}
}

func TestVoiceFlowOverSocket(t *testing.T) {
	client, _ := startDaemon(t)
	if err := client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call("voice.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call("voice.stop", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call("session.submit", nil, nil); err != nil {
		t.Fatal(err)
	}
	var transcript string
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-client.Events():
			switch ev.Type {
			case "transcript.final":
				transcript = ev.Data["text"].(string)
			case "session.finished":
				if transcript != "hello computer" {
					t.Errorf("transcript = %q", transcript)
				}
				return
			case "error":
				t.Fatalf("error event: %v", ev.Data)
			}
		case <-deadline:
			t.Fatal("session never finished")
		}
	}
}

func TestCancelOverSocket(t *testing.T) {
	client, provider := startDaemon(t)
	provider.Delay = 20 * time.Millisecond
	_ = client.Call("session.start", nil, nil)
	_ = client.Call("session.submit", map[string]string{"text": "hi"}, nil)
	if err := client.Call("session.cancel", nil, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-client.Events():
			if ev.Type == "session.cancelled" {
				return
			}
		case <-deadline:
			t.Fatal("no session.cancelled event")
		}
	}
}

func TestSubmitWithoutSessionIsSessionError(t *testing.T) {
	client, _ := startDaemon(t)
	err := client.Call("session.submit", map[string]string{"text": "hi"}, nil)
	rpcErr, ok := err.(*ipc.Error)
	if !ok || rpcErr.Code != ipc.CodeSessionError {
		t.Errorf("err = %v", err)
	}
}

func TestStatusReportsEffectivePolicy(t *testing.T) {
	client, _ := startDaemon(t)
	var status map[string]any
	if err := client.Call("status.get", nil, &status); err != nil {
		t.Fatal(err)
	}
	pol, ok := status["policy"].(map[string]any)
	if !ok {
		t.Fatalf("status.get has no policy: %v", status)
	}
	if pol["default"] != "ask" {
		t.Errorf("policy default = %v, want ask", pol["default"])
	}
	if pol["confirm_timeout_sec"] != float64(30) {
		t.Errorf("confirm_timeout_sec = %v", pol["confirm_timeout_sec"])
	}
	if pol["remember_for_conversation"] != false {
		t.Errorf("remember_for_conversation = %v", pol["remember_for_conversation"])
	}
}

// TestConfirmOverSocket drives the ask tier end to end through IPC: a risky
// shell command pauses the session, `session.confirm {approved:false}`
// declines it, and the daemon never runs anything — the fake provider then
// answers from the declined result.
func TestConfirmOverSocket(t *testing.T) {
	dir := daemonTempDir(t)
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	cfg := testConfig()
	cfg.Audio.MinRecordingMs = 0
	// Persistence runs async after session.finished and would race t.TempDir
	// cleanup ("directory not empty"); it has its own tests in
	// internal/session, so daemon tests run memory-only.
	cfg.Conversation.HistoryTurns = 0
	cfg.Tools.Shell = true // real shell.run behind the gate; nothing may run
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
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = d.Run(ctx) }()
	var client *ipc.Client
	deadline := time.Now().Add(5 * time.Second)
	for {
		client, err = ipc.Dial(paths.Socket)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon socket never came up: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call("session.submit", map[string]string{"text": "clean up"}, nil); err != nil {
		t.Fatal(err)
	}
	waitEvent := func(want string) map[string]any {
		timeout := time.After(5 * time.Second)
		for {
			select {
			case ev := <-client.Events():
				if ev.Type == want {
					return ev.Data
				}
				if ev.Type == "error" {
					t.Fatalf("error event: %v", ev.Data)
				}
			case <-timeout:
				t.Fatalf("no %s event", want)
			}
		}
	}
	data := waitEvent("tool.confirmation_required")
	if data["command"] != "rm -rf ./build" {
		t.Errorf("published command = %v", data["command"])
	}
	if err := client.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitEvent("tool.declined")
	// The session still finishes with the model's graceful answer. That the
	// command never ran is proven with a recording fake in the session
	// tests; this test guards the IPC wiring around the same gate.
	waitEvent("session.finished")
}

func TestConfirmWithNothingPendingIsSessionError(t *testing.T) {
	client, _ := startDaemon(t)
	err := client.Call("session.confirm", nil, nil)
	rpcErr, ok := err.(*ipc.Error)
	if !ok || rpcErr.Code != ipc.CodeSessionError {
		t.Errorf("err = %v", err)
	}
}
