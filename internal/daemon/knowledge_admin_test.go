package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// The feed admin surface (issue #92) over a fully wired daemon: refresh_now
// fetching through the scheduled path and announcing itself, set_enabled
// writing through the surgical entry editor with the settings discipline —
// fingerprint check, byte preservation, the standard reload — and every
// refusal shaped as a structured error a window can show. All decisions are
// pinned here, on the socket, because the window renders and calls (ADR 0013).

// adminConfigTOML is the hand-written file the set_enabled tests edit: the
// comments and the sibling entry are the byte-preservation fixture.
const adminConfigTOML = `# my config, hand-written
[context]
window = false
selection = false
clipboard = false

# watches the AMD price
[[knowledge.feeds]]
name = "amd"
description = "AMD share price"
command = ["/bin/echo", "187.42"]
mode = "lazy"

# the weather feed I keep meaning to fix
[[knowledge.feeds]]
name = "weather"
description = "Local weather"
command = ["/bin/echo", "sunny"]
mode = "lazy"
`

// startAdminDaemon boots a daemon from a real config file, which is what
// set_enabled edits, and hands back the client plus the paths.
func startAdminDaemon(t *testing.T, configTOML string) (*ipc.Client, config.Paths) {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	if err := os.WriteFile(paths.ConfigFile(), []byte(configTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(cfg, paths, nil, Deps{
		Provider:    &ai.Fake{Response: "ok"},
		Transcriber: &stt.Fake{Text: "hello"},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
		Compositor:  desktop.NewFakeCompositor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	return dialDaemon(t, paths.Socket), paths
}

// feedEntry pulls one feed's status entry by name.
func feedEntry(t *testing.T, client *ipc.Client, name string) map[string]any {
	t.Helper()
	var status map[string]any
	if err := client.Call("knowledge.status", nil, &status); err != nil {
		t.Fatal(err)
	}
	feeds, _ := status["feeds"].([]any)
	for _, f := range feeds {
		entry, _ := f.(map[string]any)
		if entry["name"] == name {
			return entry
		}
	}
	t.Fatalf("no feed %q in status %v", name, status)
	return nil
}

// TestKnowledgeRefreshNowOverSocket: the button's whole round trip — the call
// starts a real fetch (a lazy feed, so nothing else could have fetched it),
// completion arrives as the knowledge.updated event, and the status then
// carries the value with its spoken-style age.
func TestKnowledgeRefreshNowOverSocket(t *testing.T) {
	client, _ := startAdminDaemon(t, adminConfigTOML)

	entry := feedEntry(t, client, "amd")
	if entry["has_value"] != false {
		t.Fatalf("feed fetched before the button was pressed: %v", entry)
	}

	var res map[string]any
	if err := client.Call("knowledge.refresh_now", map[string]string{"name": "amd"}, &res); err != nil {
		t.Fatal(err)
	}
	updated := waitForEvent(t, client, "knowledge.updated")
	if updated["feed"] != "amd" {
		t.Errorf("knowledge.updated = %v, want the feed named", updated)
	}
	if _, hasValue := updated["value"]; hasValue {
		t.Error("the event carries a value; values travel over knowledge.status only")
	}

	entry = feedEntry(t, client, "amd")
	if entry["has_value"] != true || entry["value"] != "187.42" {
		t.Errorf("feed after refresh = %v, want the fetched value", entry)
	}
	if entry["age_spoken"] != "just now" {
		t.Errorf("age_spoken = %v, want the shared spoken wording", entry["age_spoken"])
	}
	if entry["enabled"] != true || entry["ttl_sec"] == nil || entry["interval_sec"] == nil {
		t.Errorf("feed entry = %v, want enabled and cadence fields for the card", entry)
	}
}

// TestKnowledgeRefreshNowRefusals: unknown and disabled feeds refuse with the
// reason named — the structured error the card surfaces.
func TestKnowledgeRefreshNowRefusals(t *testing.T) {
	client, _ := startAdminDaemon(t, adminConfigTOML+`
[[knowledge.feeds]]
name = "parked"
description = "switched off"
command = ["/bin/echo", "x"]
mode = "lazy"
enabled = false
`)
	err := client.Call("knowledge.refresh_now", map[string]string{"name": "nvda"}, nil)
	if err == nil || !strings.Contains(err.Error(), `"nvda"`) {
		t.Errorf("unknown feed error = %v, want it named", err)
	}
	err = client.Call("knowledge.refresh_now", map[string]string{"name": "parked"}, nil)
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("disabled feed error = %v, want the disabled reason", err)
	}
}

// TestKnowledgeSetEnabledOverSocket is the acceptance path: the switch writes
// `enabled = false` into exactly one entry with every other byte preserved,
// the running scheduler adopts it through the standard reload (the status
// reflects the *service*, not the file), and switching back restores it.
func TestKnowledgeSetEnabledOverSocket(t *testing.T) {
	client, paths := startAdminDaemon(t, adminConfigTOML)

	var status map[string]any
	if err := client.Call("knowledge.status", nil, &status); err != nil {
		t.Fatal(err)
	}
	fp, _ := status["fingerprint"].(string)
	if fp == "" {
		t.Fatal("knowledge.status carries no fingerprint; set_enabled could not detect external edits")
	}

	var res map[string]any
	if err := client.Call("knowledge.set_enabled",
		map[string]any{"name": "amd", "enabled": false, "fingerprint": fp}, &res); err != nil {
		t.Fatal(err)
	}
	if res["applied"] != true {
		t.Fatalf("set_enabled = %v, want it applied on an idle daemon", res)
	}

	raw, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(adminConfigTOML, "mode = \"lazy\"\n\n# the weather",
		"mode = \"lazy\"\nenabled = false\n\n# the weather", 1)
	if string(raw) != want {
		t.Errorf("config after set_enabled:\n%s\n--- want ---\n%s", raw, want)
	}

	// The running service — not just the file — knows: the scheduler was
	// reconfigured on the same path a hand edit plus config.reload uses.
	if entry := feedEntry(t, client, "amd"); entry["enabled"] != false {
		t.Errorf("running feed = %v, want it disabled", entry)
	}
	if entry := feedEntry(t, client, "weather"); entry["enabled"] != true {
		t.Errorf("sibling feed = %v, want it untouched", entry)
	}

	// Back on: the same line flips in place; the comments are still there.
	var again map[string]any
	if err := client.Call("knowledge.set_enabled",
		map[string]any{"name": "amd", "enabled": true, "fingerprint": res["fingerprint"]}, &again); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "enabled = true") ||
		!strings.Contains(string(raw), "# the weather feed I keep meaning to fix") {
		t.Errorf("config after re-enable lost content:\n%s", raw)
	}
	if entry := feedEntry(t, client, "amd"); entry["enabled"] != true {
		t.Errorf("running feed = %v, want it re-adopted", entry)
	}
}

// TestKnowledgeSetEnabledConflictNeverClobbers: an external edit between the
// listing and the switch is a structured conflict — the hand edit survives
// untouched, and the fresh fingerprint rides in the error's data.
func TestKnowledgeSetEnabledConflictNeverClobbers(t *testing.T) {
	client, paths := startAdminDaemon(t, adminConfigTOML)

	var status map[string]any
	if err := client.Call("knowledge.status", nil, &status); err != nil {
		t.Fatal(err)
	}
	fp, _ := status["fingerprint"].(string)

	// The user edits the file by hand while the window sits open.
	edited := adminConfigTOML + "\n# hand note added while the window was open\n"
	if err := os.WriteFile(paths.ConfigFile(), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	err := client.Call("knowledge.set_enabled",
		map[string]any{"name": "amd", "enabled": false, "fingerprint": fp}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigConflict {
		t.Fatalf("err = %v, want CodeConfigConflict", err)
	}
	data, _ := rpcErr.Data.(map[string]any)
	if fresh, _ := data["fingerprint"].(string); fresh == "" || fresh == fp {
		t.Errorf("conflict data = %v, want the fresh fingerprint to retry with", rpcErr.Data)
	}
	raw, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != edited {
		t.Errorf("the hand edit was clobbered:\n%s", raw)
	}
}

// TestKnowledgeSetEnabledUnknownFeed: -32602 with the entry named, nothing
// written.
func TestKnowledgeSetEnabledUnknownFeed(t *testing.T) {
	client, paths := startAdminDaemon(t, adminConfigTOML)
	err := client.Call("knowledge.set_enabled",
		map[string]any{"name": "nvda", "enabled": false}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeInvalidParams {
		t.Fatalf("err = %v, want CodeInvalidParams", err)
	}
	raw, readErr := os.ReadFile(paths.ConfigFile())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(raw) != adminConfigTOML {
		t.Errorf("a refused set still changed the file:\n%s", raw)
	}
}
