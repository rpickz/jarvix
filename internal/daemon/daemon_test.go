package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// startDaemon runs a fully wired daemon over a real Unix socket, with all
// engines faked: the complete integration surface minus hardware.
func startDaemon(t *testing.T) (*ipc.Client, *ai.Fake) {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config:  dir,
		Data:    dir,
		State:   dir,
		Runtime: dir,
		Socket:  filepath.Join(dir, "j.sock"),
	}
	cfg := config.Default()
	// Fake voice flows start and stop capture instantly; the accidental-tap
	// guard has its own tests in internal/session.
	cfg.Audio.MinRecordingMs = 0
	provider := &ai.Fake{Response: "Streaming works."}
	d, err := New(cfg, paths, nil, Deps{
		Provider:    provider,
		Transcriber: &stt.Fake{Text: "hello computer"},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx)

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
	t.Cleanup(func() { client.Close() })
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
	client.Call("session.start", nil, nil)
	client.Call("session.submit", map[string]string{"text": "hi"}, nil)
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
