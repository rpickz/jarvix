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

// startRoutineDaemon is startDaemon with one routine configured and its
// application already "running" on the fake compositor, so a run resolves by
// dedupe — no appear-wait, no clock dependency — and the whole trigger path
// (IPC → session → router → gate → runner → events) is real.
func startRoutineDaemon(t *testing.T) (*ipc.Client, *ai.Fake, *desktop.FakeCompositor) {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	cfg := testConfig()
	cfg.Audio.MinRecordingMs = 0
	cfg.Routines = []config.Routine{{
		Name:    "morning setup",
		Phrases: []string{"morning setup"},
		Steps:   []config.RoutineStep{{App: "firefox", Workspace: 2}},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	provider := &ai.Fake{Response: "should never be needed"}
	comp := desktop.NewFakeCompositor(
		desktop.Window{Address: "0xf", Class: "firefox", Workspace: 5},
	)
	d, err := New(cfg, paths, nil, Deps{
		Provider:    provider,
		Transcriber: &stt.Fake{Text: "morning setup"},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
		Compositor:  comp,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	return dialDaemon(t, paths.Socket), provider, comp
}

// TestRoutinesListOverSocket: the panel's and the CLI's listing — names,
// phrases, and step counts, never the steps themselves.
func TestRoutinesListOverSocket(t *testing.T) {
	client, _, _ := startRoutineDaemon(t)
	var out struct {
		Routines []struct {
			Name    string   `json:"name"`
			Phrases []string `json:"phrases"`
			Steps   int      `json:"steps"`
		} `json:"routines"`
	}
	if err := client.Call("routines.list", nil, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Routines) != 1 {
		t.Fatalf("routines = %+v", out.Routines)
	}
	r := out.Routines[0]
	if r.Name != "morning setup" || len(r.Phrases) != 1 || r.Steps != 1 {
		t.Errorf("routine = %+v", r)
	}
}

// TestRoutinesRunOverSocket: running from the panel or CLI replays the
// trigger phrase through the ordinary session path, so the router claims it
// (zero provider calls), the runner places the window, and the summary comes
// back on the same events a spoken run produces.
func TestRoutinesRunOverSocket(t *testing.T) {
	client, provider, comp := startRoutineDaemon(t)

	var out map[string]string
	if err := client.Call("routines.run", map[string]string{"name": "Morning Setup"}, &out); err != nil {
		t.Fatal(err)
	}
	if out["session_id"] == "" {
		t.Fatal("no session id")
	}
	fin := waitForEvent(t, client, "routine.finished")
	if fin["routine"] != "morning setup" {
		t.Errorf("routine.finished = %v", fin)
	}
	ev := waitForEvent(t, client, "intent.executed")
	if ev["routine"] != "morning setup" || ev["status"] != "ok" {
		t.Errorf("intent.executed = %v", ev)
	}
	waitForEvent(t, client, "session.finished")
	if len(provider.Requests) != 0 {
		t.Errorf("a routine run made %d provider calls", len(provider.Requests))
	}
	var moved bool
	for _, a := range comp.Actions() {
		if a.Verb == "move" && a.Address == "0xf" && a.Workspace == 2 {
			moved = true
		}
	}
	if !moved {
		t.Errorf("the window was not placed: %v", comp.Actions())
	}

	// An unknown name is a params error with a pointer to the listing.
	err := client.Call("routines.run", map[string]string{"name": "no such"}, nil)
	if err == nil {
		t.Fatal("an unknown routine ran")
	}
}
