package daemon

import (
	"context"
	"os"
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

// startScriptDaemon is startDaemon with one script configured — a stub in a
// test-owned temp dir, never a user's file — promoted to allow so the run
// test exercises the whole trigger path (IPC → session → router → gate →
// runner → child process → events) without a confirmation exchange; the
// ask-default behaviour itself is covered by the session tests. The stub
// writes a marker file, so "the script actually ran" is a filesystem fact,
// not an inference from events.
func startScriptDaemon(t *testing.T) (*ipc.Client, *ai.Fake, string) {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "backup-notes.sh")
	marker := filepath.Join(dir, "ran.marker")
	if err := os.WriteFile(scriptPath,
		[]byte("#!/bin/sh\ntouch "+marker+"\necho 'Notes backed up.'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	cfg := testConfig()
	cfg.Audio.MinRecordingMs = 0
	cfg.Scripts = []config.Script{{
		Name:    "backup notes",
		Phrases: []string{"backup my notes"},
		Path:    scriptPath,
		Report:  "stdout",
	}}
	cfg.Tools.Policy.Tool = map[string]string{"script.run": "allow"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	provider := &ai.Fake{Response: "should never be needed"}
	d, err := New(cfg, paths, nil, Deps{
		Provider:    provider,
		Transcriber: &stt.Fake{Text: "backup my notes"},
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
	return dialDaemon(t, paths.Socket), provider, marker
}

// TestScriptsListOverSocket: the panel's and the CLI's listing — names,
// phrases, the exact path, and the effective report and timeout, defaults
// applied. The path is deliberately present: it is what the gate names.
func TestScriptsListOverSocket(t *testing.T) {
	client, _, _ := startScriptDaemon(t)
	var out struct {
		Scripts []struct {
			Name       string   `json:"name"`
			Phrases    []string `json:"phrases"`
			Path       string   `json:"path"`
			Report     string   `json:"report"`
			TimeoutSec int      `json:"timeout_sec"`
		} `json:"scripts"`
	}
	if err := client.Call("scripts.list", nil, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Scripts) != 1 {
		t.Fatalf("scripts = %+v", out.Scripts)
	}
	s := out.Scripts[0]
	if s.Name != "backup notes" || len(s.Phrases) != 1 || s.Report != "stdout" {
		t.Errorf("script = %+v", s)
	}
	if !filepath.IsAbs(s.Path) || s.TimeoutSec != 60 {
		t.Errorf("script = %+v; want the absolute path and the default timeout", s)
	}
}

// TestScriptsRunOverSocket: running from the panel or CLI replays the
// trigger phrase through the ordinary session path, so the router claims it
// (zero provider calls), the real runner executes the stub (the marker file
// proves it), and the events carry the run's exit status.
func TestScriptsRunOverSocket(t *testing.T) {
	client, provider, marker := startScriptDaemon(t)

	var out map[string]string
	if err := client.Call("scripts.run", map[string]string{"name": "Backup Notes"}, &out); err != nil {
		t.Fatal(err)
	}
	if out["session_id"] == "" {
		t.Fatal("no session id")
	}
	fin := waitForEvent(t, client, "script.finished")
	if fin["script"] != "backup notes" || fin["status"] != "ok" {
		t.Errorf("script.finished = %v", fin)
	}
	ev := waitForEvent(t, client, "intent.executed")
	if ev["script"] != "backup notes" || ev["status"] != "ok" {
		t.Errorf("intent.executed = %v", ev)
	}
	if ev["acknowledgement"] != "Notes backed up." {
		t.Errorf("acknowledgement = %v; the stdout mode speaks the script's own line", ev["acknowledgement"])
	}
	waitForEvent(t, client, "session.finished")
	if len(provider.Requests) != 0 {
		t.Errorf("a script run made %d provider calls", len(provider.Requests))
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the stub script never ran: %v", err)
	}

	// An unknown name is a params error with a pointer to the listing.
	err := client.Call("scripts.run", map[string]string{"name": "no such"}, nil)
	if err == nil {
		t.Fatal("an unknown script name was accepted")
	}
}
