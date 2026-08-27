package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
)

// The windows.* IPC verbs (#126): the CLI's window list with nicknames, and
// assignment from a listing — both thin clients of the window tools' one
// seam, exercised here over a real socket against a fake compositor.

func windowsDaemon(t *testing.T) (*Daemon, string) {
	t.Helper()
	dir := t.TempDir()
	socket := filepath.Join(dir, "j.sock")
	d, err := New(testConfig(), config.Paths{Config: dir, Data: dir, State: dir,
		Runtime: dir, Socket: socket}, nil, Deps{
		Provider:    &ai.Fake{},
		Transcriber: &stt.Fake{},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
		Compositor: desktop.NewFakeCompositor(
			desktop.Window{Address: "0x1", Class: "code", Title: "engine.go",
				Workspace: 1, WorkspaceName: "1", StableID: "s1", Focused: true},
			desktop.Window{Address: "0x2", Class: "Alacritty", Title: "go test",
				Workspace: 2, WorkspaceName: "2", StableID: "s2"},
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d, socket
}

func TestWindowVerbsListAndName(t *testing.T) {
	d, socket := windowsDaemon(t)
	serveDaemon(t, d)
	client := dialDaemon(t, socket)

	var named struct {
		Spoken string `json:"spoken"`
	}
	if err := client.Call("windows.name", map[string]any{"name": "Builds."}, &named); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(named.Spoken, "the code window is now called builds") {
		t.Errorf("spoken = %q, want the focused window named", named.Spoken)
	}

	var listing struct {
		Windows []tools.WindowListing `json:"windows"`
	}
	if err := client.Call("windows.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Windows) != 2 {
		t.Fatalf("windows = %+v, want both", listing.Windows)
	}
	var found bool
	for _, w := range listing.Windows {
		if w.Nickname == "builds" && w.App == "code" && w.Focused {
			found = true
		}
	}
	if !found {
		t.Errorf("windows = %+v, want the nickname on the focused code window", listing.Windows)
	}
}

// TestWindowNameRefusalTravelsAsTheError: the seam's spoken-ready refusal is
// the IPC error's message, verbatim — the CLI and any UI print it as-is.
func TestWindowNameRefusalTravelsAsTheError(t *testing.T) {
	d, socket := windowsDaemon(t)
	serveDaemon(t, d)
	client := dialDaemon(t, socket)

	err := client.Call("windows.name", map[string]any{"name": "build terminal"}, nil)
	if err == nil || !strings.Contains(err.Error(), "single word") {
		t.Errorf("err = %v, want the single-word refusal", err)
	}
	// The built-in grammar owns "mute": the refusal names it, proving the
	// daemon's router reached the registry's collision check.
	err = client.Call("windows.name", map[string]any{"name": "mute"}, nil)
	if err == nil || !strings.Contains(err.Error(), "volume.mute") {
		t.Errorf("err = %v, want the intent owner named", err)
	}
	if err := client.Call("windows.name", map[string]any{}, nil); err == nil {
		t.Error("windows.name with no name did not error")
	}
}
