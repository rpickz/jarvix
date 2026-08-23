package daemon

import (
	"context"
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
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
)

// The daemon half of the feed cache (ADR 0031): the wiring — tool
// registration, the knowledge.status method, the reload path — over a fully
// wired daemon with all engines faked. The feeds here are lazy and never
// asked, so no test ever runs a feed command; the scheduler's own behaviour
// is tested in internal/knowledge through its injected seams.

func feedConfig() config.Config {
	cfg := testConfig()
	cfg.Knowledge.Feeds = []config.KnowledgeFeed{{
		Name: "amd", Description: "AMD share price",
		Command: config.Command{"/bin/echo", "187.42"},
		Mode:    config.FeedModeLazy, IntervalSec: 300, TTLSec: 600, TimeoutSec: 30,
	}}
	return cfg
}

func startFeedDaemon(t *testing.T, cfg config.Config) *ipc.Client {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
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
	return dialDaemon(t, paths.Socket)
}

// TestKnowledgeStatusOverSocket: the read-only listing every surface shares —
// doctor's window into the scheduler — plus the shutdown drain, exercised by
// serveDaemon's cancel-and-wait on the way out.
func TestKnowledgeStatusOverSocket(t *testing.T) {
	client := startFeedDaemon(t, feedConfig())
	var status map[string]any
	if err := client.Call("knowledge.status", nil, &status); err != nil {
		t.Fatal(err)
	}
	if status["enabled"] != true {
		t.Fatalf("status = %v, want enabled", status)
	}
	feeds, _ := status["feeds"].([]any)
	if len(feeds) != 1 {
		t.Fatalf("feeds = %v, want the one configured", status["feeds"])
	}
	feed, _ := feeds[0].(map[string]any)
	if feed["name"] != "amd" || feed["mode"] != "lazy" {
		t.Errorf("feed entry = %v, want name and mode", feed)
	}
	if feed["has_value"] != false {
		t.Errorf("feed entry = %v, want cold — nothing may have fetched", feed)
	}
}

func TestKnowledgeStatusDisabledWithoutFeeds(t *testing.T) {
	client := startFeedDaemon(t, testConfig())
	var status map[string]any
	if err := client.Call("knowledge.status", nil, &status); err != nil {
		t.Fatal(err)
	}
	if status["enabled"] != false {
		t.Fatalf("status = %v, want enabled=false with no feeds configured", status)
	}
}

// TestKnowledgeToolRegistration: the tool exists exactly when feeds do.
func TestKnowledgeToolRegistration(t *testing.T) {
	client := startFeedDaemon(t, feedConfig())
	var status map[string]any
	if err := client.Call("status.get", nil, &status); err != nil {
		t.Fatal(err)
	}
	policy, _ := status["policy"].(map[string]any)
	perTool, _ := policy["tools"].(map[string]any)
	// The gate resolves the tool through the knowledge.refresh identity,
	// which defaults to allow: registered and silently readable.
	if perTool[tools.KnowledgeGetToolName] != "allow" {
		t.Errorf("policy tools = %v, want %s registered at allow", perTool, tools.KnowledgeGetToolName)
	}
}

// TestFeedTablesApplyOnReload: the daemon half of "rebuilt on Reconfigure" —
// a hand-edited [[knowledge.feeds]] table lands on config.reload, with the
// same service (and its cached values file) carrying straight on.
func TestFeedTablesApplyOnReload(t *testing.T) {
	const bootTOML = `
[context]
window = false
selection = false
clipboard = false

[[knowledge.feeds]]
name = "amd"
description = "AMD share price"
command = ["/bin/echo", "187.42"]
mode = "lazy"
`
	dir := t.TempDir()
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	if err := os.WriteFile(paths.ConfigFile(), []byte(bootTOML), 0o600); err != nil {
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
	client := dialDaemon(t, paths.Socket)

	edited := strings.Replace(bootTOML, `name = "amd"`, `name = "nvda"`, 1)
	if err := os.WriteFile(paths.ConfigFile(), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.Call("config.reload", nil, nil); err != nil {
		t.Fatal(err)
	}

	var status map[string]any
	if err := client.Call("knowledge.status", nil, &status); err != nil {
		t.Fatal(err)
	}
	feeds, _ := status["feeds"].([]any)
	if len(feeds) != 1 {
		t.Fatalf("feeds after reload = %v, want the edited one", status["feeds"])
	}
	if feed, _ := feeds[0].(map[string]any); feed["name"] != "nvda" {
		t.Errorf("feed after reload = %v, want the hand-edited name", feed)
	}
}

// TestFeedSpecsConversion pins the config → spec translation, durations
// included.
func TestFeedSpecsConversion(t *testing.T) {
	specs := feedSpecs(feedConfig())
	if len(specs) != 1 {
		t.Fatalf("specs = %d, want 1", len(specs))
	}
	s := specs[0]
	if s.Name != "amd" || s.Mode != "lazy" ||
		s.Interval.Seconds() != 300 || s.TTL.Seconds() != 600 || s.Timeout.Seconds() != 30 {
		t.Errorf("spec = %+v, want the config's values as durations", s)
	}
	if len(s.Argv) != 2 || s.Argv[0] != "/bin/echo" {
		t.Errorf("argv = %v, want the fixed command", s.Argv)
	}
}
