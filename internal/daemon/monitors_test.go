package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/placement"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
)

// The monitors.* IPC verbs (#180): the picker's data source and the three
// writes, exercised over a real socket against a fake compositor holding the
// user's own two screens.

func monitorsDaemon(t *testing.T) (*Daemon, string) {
	t.Helper()
	dir := t.TempDir()
	socket := filepath.Join(dir, "j.sock")
	comp := desktop.NewFakeCompositor(desktop.Window{Address: "0x1", Class: "code",
		Workspace: 1, WorkspaceName: "1", StableID: "s1", Focused: true})
	comp.Outputs = []placement.Monitor{
		{Name: "HDMI-A-1", Width: 3440, Height: 1440, Scale: 1, Focused: true, ActiveWorkspace: 1},
		{Name: "DP-2", Y: 1440, Width: 5120, Height: 1440, Scale: 1, ActiveWorkspace: 2},
	}
	d, err := New(testConfig(), config.Paths{Config: dir, Data: dir, State: dir,
		Runtime: dir, Socket: socket}, nil, Deps{
		Provider:    &ai.Fake{},
		Transcriber: &stt.Fake{},
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
	return d, socket
}

// monitorReply is the monitors.list shape the window and the CLI both decode.
type monitorReply struct {
	Monitors  []tools.MonitorListing       `json:"monitors"`
	Nicknames []tools.NicknameListingEntry `json:"nicknames"`
	Path      string                       `json:"path"`
	Count     int                          `json:"count"`
	Max       int                          `json:"max"`
	Reserved  []string                     `json:"reserved"`
	Current   string                       `json:"current"`
}

// TestMonitorVerbsNameListAndForget walks the whole surface the window drives.
func TestMonitorVerbsNameListAndForget(t *testing.T) {
	d, socket := monitorsDaemon(t)
	serveDaemon(t, d)
	client := dialDaemon(t, socket)

	var named struct {
		Spoken string `json:"spoken"`
	}
	// No connector: the screen holding focus, which is what "call this
	// monitor top" means.
	if err := client.Call("monitors.name", map[string]any{"name": "Top."}, &named); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(named.Spoken, "HDMI-A-1 (3440 by 1440) is now called top") {
		t.Errorf("spoken = %q", named.Spoken)
	}
	if err := client.Call("monitors.name",
		map[string]any{"name": "bottom", "connector": "DP-2"}, &named); err != nil {
		t.Fatal(err)
	}

	var listing monitorReply
	if err := client.Call("monitors.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Monitors) != 2 || listing.Monitors[0].Connector != "DP-2" ||
		listing.Monitors[0].Nickname != "bottom" || listing.Monitors[1].Nickname != "top" {
		t.Fatalf("monitors = %+v", listing.Monitors)
	}
	if listing.Count != 2 || listing.Max <= 0 || listing.Path == "" {
		t.Errorf("listing = %+v", listing)
	}
	// The picker's own vocabulary comes from the daemon, not from QML.
	if listing.Current != string(placement.MonitorCurrent) {
		t.Errorf("current = %q", listing.Current)
	}
	if strings.Join(listing.Reserved, ",") != strings.Join(placement.ReservedMonitorWords(), ",") {
		t.Errorf("reserved = %v, want the vocabulary's %v", listing.Reserved,
			placement.ReservedMonitorWords())
	}

	// Re-pointing is its own verb, and it says what the name used to mean.
	if err := client.Call("monitors.repoint",
		map[string]any{"name": "top", "connector": "DP-2"}, &named); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(named.Spoken, "HDMI-A-1 no longer is") {
		t.Errorf("repoint spoke %q", named.Spoken)
	}

	if err := client.Call("monitors.forget", map[string]any{"name": "top"}, &named); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(named.Spoken, "no longer called top") {
		t.Errorf("forget spoke %q", named.Spoken)
	}
	if err := client.Call("monitors.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Nicknames) != 1 || listing.Nicknames[0].Name != "bottom" {
		t.Errorf("after forgetting: %+v", listing.Nicknames)
	}
}

// TestAScreenNameCollisionArrivesFieldKeyed: the refusal reaches the window's
// form in the shape every other form on this surface uses, so it lands on the
// control the user has to change.
func TestAScreenNameCollisionArrivesFieldKeyed(t *testing.T) {
	d, socket := monitorsDaemon(t)
	serveDaemon(t, d)
	client := dialDaemon(t, socket)

	var reply struct {
		Spoken string `json:"spoken"`
	}
	for _, tc := range []struct {
		name  string
		field string
		want  string
	}{
		{"DP-2", "name", "already the name of DP-2 (5120 by 1440)"},
		{"current", "name", "it is the screen you are on"},
		{"top left", "name", `try just "top"`},
	} {
		err := client.Call("monitors.name", map[string]any{"name": tc.name}, &reply)
		var rpcErr *ipc.Error
		if !errors.As(err, &rpcErr) {
			t.Fatalf("naming %q = %v, want an rpc error", tc.name, err)
		}
		if rpcErr.Code != ipc.CodeConfigInvalid {
			t.Errorf("naming %q gave code %d, want %d", tc.name, rpcErr.Code, ipc.CodeConfigInvalid)
		}
		container, _ := rpcErr.Data.(map[string]any)
		message, ok := problemOn(entryProblemList(t, container), tc.field)
		if !ok || !strings.Contains(message, tc.want) {
			t.Errorf("naming %q gave %v, want a %q problem containing %q",
				tc.name, rpcErr.Data, tc.field, tc.want)
		}
	}

	// A name with no name at all is a parameter error, not a form problem:
	// there is no field to pin it to.
	if err := client.Call("monitors.name", map[string]any{}, &reply); err == nil {
		t.Error("an empty name was accepted")
	}
}
