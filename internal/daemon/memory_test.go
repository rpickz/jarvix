package daemon

import (
	"context"
	"encoding/json"
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

// The knowledge base through the daemon's own surfaces (ADR 0025): the wiring
// from [memory] to registered tools and injection, and the IPC methods the
// CLI is a thin client of. The store and tool behaviour have their own tests
// in internal/memory and internal/tools; what is pinned here is that one
// configured daemon holds them together.

// startMemoryDaemon is startDaemon with the configuration shapeable and the
// state dir returned, so a test can pre-seed or inspect the memory file.
func startMemoryDaemon(t *testing.T, shape func(*config.Config)) (*ipc.Client, *ai.Fake, string) {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	cfg := testConfig()
	cfg.Audio.MinRecordingMs = 0
	if shape != nil {
		shape(&cfg)
	}
	provider := &ai.Fake{Response: "Understood."}
	d, err := New(cfg, paths, nil, Deps{
		Provider:    provider,
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
	return dialDaemon(t, paths.Socket), provider, dir
}

// seedMemoryFile writes a hand-made store, as a user (or a previous daemon
// life) would have left it — the daemon must pick it up cold.
func seedMemoryFile(t *testing.T, dir string) {
	t.Helper()
	doc := `version = 1
next_id = 3

[[fact]]
id = "m1"
content = "the staging server is called atlas"
stored = 2026-08-01T10:00:00Z
updated = 2026-08-01T10:00:00Z

[[fact]]
id = "m2"
content = "the user's editor is neovim"
stored = 2026-08-02T10:00:00Z
updated = 2026-08-02T10:00:00Z
`
	if err := os.WriteFile(filepath.Join(dir, "memory.toml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryToolsAreRegisteredWithTheirTiers(t *testing.T) {
	client, _, _ := startMemoryDaemon(t, nil)
	var status map[string]any
	if err := client.Call("status.get", nil, &status); err != nil {
		t.Fatal(err)
	}
	tools := status["policy"].(map[string]any)["tools"].(map[string]any)
	for name, tier := range map[string]string{
		"memory.remember": "allow",
		"memory.search":   "allow",
		"memory.forget":   "ask",
	} {
		if got := tools[name]; got != tier {
			t.Errorf("%s tier = %v, want %s", name, got, tier)
		}
	}
}

// TestMemoryDisabledMeansNoToolsAndNoStore is the disabled-mode acceptance
// criterion at the daemon level: the tools are not registered, nothing is
// injected, the IPC methods answer honestly — and a pre-existing store file
// is left untouched, because disabling is not deleting.
func TestMemoryDisabledMeansNoToolsAndNoStore(t *testing.T) {
	client, provider, dir := startMemoryDaemon(t, func(cfg *config.Config) {
		cfg.Memory.Enabled = false
	})
	seedMemoryFile(t, dir)

	var status map[string]any
	if err := client.Call("status.get", nil, &status); err != nil {
		t.Fatal(err)
	}
	tools := status["policy"].(map[string]any)["tools"].(map[string]any)
	for name := range tools {
		if strings.HasPrefix(name, "memory.") {
			t.Errorf("tool %s registered with memory disabled", name)
		}
	}

	var listing map[string]any
	if err := client.Call("memory.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if listing["enabled"] != false {
		t.Errorf("memory.list = %v, want enabled false", listing)
	}
	var last map[string]any
	if err := client.Call("memory.last", nil, &last); err != nil {
		t.Fatal(err)
	}
	if last["enabled"] != false {
		t.Errorf("memory.last = %v, want enabled false", last)
	}
	if err := client.Call("memory.forget", map[string]any{"id": "m1"}, nil); err == nil {
		t.Error("memory.forget succeeded with memory disabled")
	}

	// A turn injects nothing and mentions no memory anywhere.
	if err := client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call("session.submit", map[string]any{"text": "hello"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "session.finished")
	for _, m := range provider.LastRequest.Messages {
		if strings.Contains(m.Content, "Remembered facts") || strings.Contains(m.Content, "memory.remember") {
			t.Errorf("disabled memory reached the model: %q", m.Content)
		}
	}

	// The user's facts are still on disk, byte for byte.
	data, err := os.ReadFile(filepath.Join(dir, "memory.toml"))
	if err != nil || !strings.Contains(string(data), "atlas") {
		t.Errorf("disabling memory disturbed the store: %v, %q", err, data)
	}
}

// TestSeededStoreIsListedAndInjected: a store left by a previous daemon life
// (or written by hand) is served cold — the restart-survival criterion seen
// from the daemon's side.
func TestSeededStoreIsListedAndInjected(t *testing.T) {
	client, provider, dir := startMemoryDaemon(t, nil)
	seedMemoryFile(t, dir)

	var listing struct {
		Enabled bool `json:"enabled"`
		Count   int  `json:"count"`
		Facts   []struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		} `json:"facts"`
	}
	if err := client.Call("memory.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if !listing.Enabled || listing.Count != 2 {
		t.Fatalf("memory.list = %+v, want both seeded facts", listing)
	}

	// A turn carries the facts to the model…
	if err := client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call("session.submit", map[string]any{"text": "how do I deploy?"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "session.finished")
	injected := false
	for _, m := range provider.LastRequest.Messages {
		if m.Role == ai.RoleSystem && strings.Contains(m.Content, "atlas") &&
			strings.Contains(m.Content, "things the user asked you to remember") {
			injected = true
		}
	}
	if !injected {
		t.Error("seeded facts never reached the model")
	}

	// …and the audit says so, content included, over the 0600 socket.
	var last struct {
		Injected bool `json:"injected"`
		Total    int  `json:"total"`
		Facts    []struct {
			Content string `json:"content"`
		} `json:"facts"`
	}
	if err := client.Call("memory.last", nil, &last); err != nil {
		t.Fatal(err)
	}
	if !last.Injected || last.Total != 2 || len(last.Facts) != 2 {
		t.Fatalf("memory.last = %+v, want the injection audited", last)
	}
}

// TestMemoryForgetOverIPC covers the CLI's deletion path: by unique query, by
// id, and the ambiguous case that must refuse to guess.
func TestMemoryForgetOverIPC(t *testing.T) {
	client, _, dir := startMemoryDaemon(t, nil)
	seedMemoryFile(t, dir)

	var result struct {
		Forgotten bool `json:"forgotten"`
		Fact      struct {
			Content string `json:"content"`
		} `json:"fact"`
	}
	if err := client.Call("memory.forget", map[string]any{"query": "editor"}, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Forgotten || !strings.Contains(result.Fact.Content, "neovim") {
		t.Fatalf("forget by query = %+v", result)
	}

	if err := client.Call("memory.forget", map[string]any{"id": "m1"}, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Forgotten {
		t.Fatalf("forget by id = %+v", result)
	}

	// Deletion is deletion: the file no longer holds either fact.
	data, err := os.ReadFile(filepath.Join(dir, "memory.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"atlas", "neovim"} {
		if strings.Contains(string(data), gone) {
			t.Errorf("forgotten content %q still on disk", gone)
		}
	}

	// Nothing left: a forget with no match reports so without erroring.
	var miss struct {
		Forgotten bool             `json:"forgotten"`
		Matches   []map[string]any `json:"matches"`
	}
	if err := client.Call("memory.forget", map[string]any{"query": "editor"}, &miss); err != nil {
		t.Fatal(err)
	}
	if miss.Forgotten || len(miss.Matches) != 0 {
		t.Errorf("no-match forget = %+v", miss)
	}
}

// TestMemoryForgetAmbiguousOverIPC: two facts about the same subject, one
// vague query — nothing may be deleted, and the candidates come back.
func TestMemoryForgetAmbiguousOverIPC(t *testing.T) {
	client, _, dir := startMemoryDaemon(t, nil)
	doc := `version = 1
next_id = 3

[[fact]]
id = "m1"
content = "the staging server is called atlas"
stored = 2026-08-01T10:00:00Z
updated = 2026-08-01T10:00:00Z

[[fact]]
id = "m2"
content = "the staging server certificate renews in march"
stored = 2026-08-02T10:00:00Z
updated = 2026-08-02T10:00:00Z
`
	if err := os.WriteFile(filepath.Join(dir, "memory.toml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	var result struct {
		Forgotten bool `json:"forgotten"`
		Matches   []struct {
			ID string `json:"id"`
		} `json:"matches"`
	}
	if err := client.Call("memory.forget", map[string]any{"query": "staging"}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Forgotten || len(result.Matches) != 2 {
		t.Fatalf("ambiguous forget = %+v, want a refusal with both candidates", result)
	}
	var listing struct {
		Count int `json:"count"`
	}
	if err := client.Call("memory.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Count != 2 {
		t.Errorf("ambiguous forget deleted: %d facts left, want 2", listing.Count)
	}
}

// TestMemoryLastBeforeAnyTurn: the audit method answers before the first
// consultation without inventing one.
func TestMemoryLastBeforeAnyTurn(t *testing.T) {
	client, _, _ := startMemoryDaemon(t, nil)
	var last map[string]any
	if err := client.Call("memory.last", nil, &last); err != nil {
		t.Fatal(err)
	}
	if last["enabled"] != true || last["injected"] != false {
		t.Errorf("memory.last = %v, want enabled true, injected false", last)
	}
}

// TestRememberThroughTheDaemonThenInjectedNextTurn is the full loop over one
// daemon: the model remembers on turn one, and turn two's request carries the
// fact — with the memory.injected event announcing counts on the bus.
func TestRememberThroughTheDaemonThenInjectedNextTurn(t *testing.T) {
	client, provider, _ := startMemoryDaemon(t, nil)
	provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "memory.remember",
			Arguments: `{"content":"the staging server is called atlas"}`}},
	}

	if err := client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call("session.submit",
		map[string]any{"text": "remember the staging server is atlas"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "session.finished")

	if err := client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call("session.submit", map[string]any{"text": "how do I deploy?"}, nil); err != nil {
		t.Fatal(err)
	}
	data := waitForEvent(t, client, "memory.injected")
	if data["facts"] != json.Number("1") && data["facts"] != float64(1) {
		t.Errorf("memory.injected facts = %v (%T), want 1", data["facts"], data["facts"])
	}
	waitForEvent(t, client, "session.finished")

	injected := false
	for _, m := range provider.LastRequest.Messages {
		if m.Role == ai.RoleSystem && strings.Contains(m.Content, "atlas") {
			injected = true
		}
	}
	if !injected {
		t.Error("the remembered fact did not reach the next turn")
	}
}
