package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
)

// tieredConfig is a validated document with an instant tier and an
// advisor-backed deep one — the mix the ticket's own example configures.
func tieredConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := testConfig()
	cfg.AI.Endpoints["lmstudio"] = config.Endpoint{BaseURL: "http://127.0.0.1:1234/v1"}
	cfg.Advisors = map[string]config.Advisor{"claude": {Binary: "claude", ReadOnly: true}}
	cfg.AI.Tiers = config.AITiers{
		Default: "medium",
		Tiers: map[string]config.AITier{
			"instant": {Provider: "lmstudio", Model: "qwen3-1.7b", HistoryTurns: 4},
			"deep":    {Advisor: "claude"},
		},
	}
	return cfg
}

// The backwards-compatibility promise, at the wiring layer: with no tiers
// nothing is bound at all, so the engine takes the path it always took.
func TestNoTiersBindsNothing(t *testing.T) {
	brain := &ai.Fake{}
	if set := tierSet(testConfig(), brain, nil); set.Enabled() {
		t.Errorf("tiering is on with no [ai.tiers] table: %+v", set)
	}
}

// Medium with no table of its own is the [ai] brain. That is what makes
// adding only an instant tier safe: ordinary turns keep answering from
// exactly the model they answered from yesterday.
func TestMediumWithNoTableIsTheExistingBrain(t *testing.T) {
	brain := &ai.Fake{}
	cfg := tieredConfig(t)
	set := tierSet(cfg, brain, nil)

	medium, ok := set.Bindings[ai.TierMedium]
	if !ok {
		t.Fatal("medium has no binding")
	}
	if medium.Provider != ai.Provider(brain) {
		t.Error("medium is not bound to the [ai] provider the daemon built")
	}
	if medium.Model != cfg.AI.Model {
		t.Errorf("medium model = %q, want %q", medium.Model, cfg.AI.Model)
	}
}

// An absent instant or deep does *not* fall back to the brain: it does not
// exist, and asking for it is answered by saying so rather than by serving the
// same model under a stronger name.
func TestAnAbsentTierIsNotBoundToTheBrain(t *testing.T) {
	cfg := testConfig()
	cfg.AI.Endpoints["lmstudio"] = config.Endpoint{BaseURL: "http://127.0.0.1:1234/v1"}
	cfg.AI.Tiers = config.AITiers{Default: "medium", Tiers: map[string]config.AITier{
		"instant": {Provider: "lmstudio", Model: "small"},
	}}
	set := tierSet(cfg, &ai.Fake{}, nil)
	if _, ok := set.Bindings[ai.TierDeep]; ok {
		t.Error("deep was bound with no [ai.tiers.deep] table")
	}
	if _, ok := set.Bindings[ai.TierInstant]; !ok {
		t.Error("instant was not bound")
	}
}

func TestAnAdvisorBackedTierBindsToTheBridge(t *testing.T) {
	set := tierSet(tieredConfig(t), &ai.Fake{}, nil)
	deep, ok := set.Bindings[ai.TierDeep]
	if !ok {
		t.Fatal("deep has no binding")
	}
	if deep.Advisor != "claude" {
		t.Errorf("advisor = %q, want claude", deep.Advisor)
	}
	if deep.Tools() {
		t.Error("an advisor-backed tier claims it can hold tools; it cannot call any")
	}
	if got := deep.Provider.Name(); got != "advisor:claude" {
		t.Errorf("provider name = %q, want it to say which bridge it is", got)
	}
}

// A default naming a tier this document does not configure falls back to
// medium rather than leaving the engine pointing at nothing.
func TestAnUnservableDefaultFallsBackToMedium(t *testing.T) {
	cfg := tieredConfig(t)
	cfg.AI.Tiers.Default = "deep"
	delete(cfg.AI.Tiers.Tiers, "deep")
	if got := tierSet(cfg, &ai.Fake{}, nil).Default; got != ai.TierMedium {
		t.Errorf("default = %q, want medium", got)
	}
}

// The model's escalation tool is registered only when there is somewhere for
// it to escalate to. A tool advertising a deeper answer this machine cannot
// give would invite the model to promise one.
func TestTheDeepToolIsRegisteredOnlyWithADeepTier(t *testing.T) {
	d := newTestDaemon(t, testConfig())
	for _, def := range d.registry.Defs() {
		if def.Name == tools.DeepToolName {
			t.Fatal("the escalation tool is registered with no deep tier configured")
		}
	}

	d = newTestDaemon(t, tieredConfig(t))
	found := false
	for _, def := range d.registry.Defs() {
		if def.Name == tools.DeepToolName {
			found = true
		}
	}
	if !found {
		t.Error("the escalation tool is missing with a deep tier configured")
	}
}

// The thinking surface over the real socket: what the window reads to draw its
// control, and what a click sends.
func TestThinkingOverTheSocket(t *testing.T) {
	client, _ := startDaemonWith(t, tieredConfig(t))

	var report map[string]any
	if err := client.Call("thinking.get", nil, &report); err != nil {
		t.Fatal(err)
	}
	if report["thinking"] != "medium" || report["thinking_label"] != "Balanced" {
		t.Errorf("thinking.get = %v, want the configured default", report)
	}
	levels, _ := report["levels"].([]any)
	if len(levels) != 3 {
		t.Fatalf("levels = %v, want all three — a level this machine lacks is shown, not hidden", levels)
	}
	byTier := map[string]map[string]any{}
	for _, l := range levels {
		row, _ := l.(map[string]any)
		byTier[row["tier"].(string)] = row
	}
	if byTier["instant"]["available"] != true || byTier["deep"]["available"] != true {
		t.Errorf("configured levels are not marked available: %v", byTier)
	}

	// Setting it moves the level and answers with the same shape.
	var after map[string]any
	if err := client.Call("thinking.set", map[string]any{"thinking": "deep"}, &after); err != nil {
		t.Fatal(err)
	}
	if after["thinking"] != "deep" || after["thinking_label"] != "Deep" {
		t.Errorf("thinking.set = %v", after)
	}

	// And it is refused, in words, for a level this machine cannot serve —
	// where the control stands, rather than at answer time.
	client2, _ := startDaemonWith(t, testConfig())
	err := client2.Call("thinking.set", map[string]any{"thinking": "deep"}, nil)
	if err == nil {
		t.Fatal("setting a level with no tiers configured was accepted")
	}
	if !strings.Contains(err.Error(), "tiers") {
		t.Errorf("error = %q, want it to say what is missing", err)
	}

	// Nonsense is refused as invalid params rather than silently ignored.
	if err := client.Call("thinking.set", map[string]any{"thinking": "turbo"}, nil); err == nil {
		t.Error("an unknown level was accepted")
	}
}

// The conversation snapshot carries the level, so a window opened mid-thought
// draws the control right without a second request.
func TestConversationSnapshotCarriesTheThinkingLevel(t *testing.T) {
	client, _ := startDaemonWith(t, tieredConfig(t))
	var snapshot map[string]any
	if err := client.Call("conversation.get", nil, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot["thinking"] != "medium" || snapshot["thinking_label"] != "Balanced" {
		t.Errorf("snapshot = %v, want the level", snapshot)
	}
	if levels, _ := snapshot["thinking_levels"].([]any); len(levels) != 3 {
		t.Errorf("snapshot levels = %v", snapshot["thinking_levels"])
	}

	// And says nothing about tiers on a machine with none, so a client can
	// tell "no control to draw" from "the control is on Balanced".
	plain, _ := startDaemonWith(t, testConfig())
	var bare map[string]any
	if err := plain.Call("conversation.get", nil, &bare); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"thinking", "thinking_label", "thinking_levels", "tier"} {
		if _, ok := bare[key]; ok {
			t.Errorf("snapshot carries %q with no tiers configured", key)
		}
	}
}

// startDaemonWith runs a fully wired daemon over a real socket for a given
// configuration. It is startDaemon with the config as a parameter — the tier
// surface is entirely a function of the document, so a test that could not
// vary it could not test anything here.
func startDaemonWith(t *testing.T, cfg config.Config) (*ipc.Client, *ai.Fake) {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock")}
	cfg.Audio.MinRecordingMs = 0
	provider := &ai.Fake{Response: "Streaming works."}
	d, err := New(cfg, paths, nil, Deps{
		Provider:    provider,
		Transcriber: &stt.Fake{Text: "hello computer"},
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
	return dialDaemon(t, paths.Socket), provider
}

// The host's grace reaches the engine through the one translation both boot
// and reload share (#161, ADR 0064). It matters that it goes through
// engineOptions rather than being read anywhere else: a grace applied at boot
// and not on reload would be a setting that appears to do nothing.
func TestTheHostGraceReachesTheEngineOptions(t *testing.T) {
	cfg := tieredConfig(t)
	cfg.AI.Tiers.HostGraceMs = 400
	opts := engineOptions(cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if opts.HostGrace != 400*time.Millisecond {
		t.Errorf("HostGrace = %v, want 400ms", opts.HostGrace)
	}

	cfg.AI.Tiers.HostGraceMs = 0
	off := engineOptions(cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if off.HostGrace != 0 {
		t.Errorf("HostGrace = %v with the host switched off, want zero", off.HostGrace)
	}
}
