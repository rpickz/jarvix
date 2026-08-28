package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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

// The assistant's self-configuration surface (issue #105, ADR 0036) over a
// fully wired daemon: the shared write pipeline end to end — draft, gate,
// verbatim card, validation feedback, write, events, post-session reload —
// with the mutation checks the acceptance criteria demand: a declined,
// refused, or invalid attempt leaves config.toml byte-identical.

// selfConfigDaemon runs a wired daemon whose provider is scripted with the
// given tool rounds, returning the client, the provider (for asserting the
// tool results the model was fed), and the config file path.
func selfConfigDaemon(t *testing.T, cfg config.Config, rounds [][]ai.ToolCall) (*ipc.Client, *ai.Fake, string) {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	provider := &ai.Fake{Response: "Done."}
	provider.ToolCallsByRound = rounds
	d, err := New(cfg, paths, nil, Deps{
		Provider:    provider,
		Transcriber: &stt.Fake{Text: "unused"},
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
	return dialDaemon(t, paths.Socket), provider, paths.ConfigFile()
}

// scriptFile creates an executable the validator's path check accepts.
func scriptFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// lastToolResult returns the most recent RoleTool message the provider was
// fed — what the model actually read back from the last executed call.
func lastToolResult(t *testing.T, provider *ai.Fake) string {
	t.Helper()
	for i := len(provider.Requests) - 1; i >= 0; i-- {
		msgs := provider.Requests[i].Messages
		for j := len(msgs) - 1; j >= 0; j-- {
			if msgs[j].Role == ai.RoleTool {
				return msgs[j].Content
			}
		}
	}
	t.Fatal("the provider never saw a tool result")
	return ""
}

// TestConfigToolFamiliesMirrorTheDaemonRegistry is the drift guard the tools'
// family list promises: the closed set the tools refuse outside of is exactly
// the set of the daemon's registry the ASSISTANT may administer — the registry
// minus the families behind the exclusion wall (#109, #163). A family added to
// the registry without a decision about the model is caught here.
func TestConfigToolFamiliesMirrorTheDaemonRegistry(t *testing.T) {
	daemonFamilies := assistantEntryFamilies()
	if got := tools.ConfigEntryFamilies(); !reflect.DeepEqual(got, daemonFamilies) {
		t.Errorf("tool families %v drifted from the daemon registry %v", got, daemonFamilies)
	}
	// And the wall itself is not empty: the families it holds back are named,
	// so a future edit that quietly opened one would fail here rather than
	// ship a model that can rewrite its own brain.
	for _, family := range []string{"ai", "advisors"} {
		spec, ok := entryAdminFamilies[family]
		if !ok {
			t.Fatalf("the registry lost the %q family", family)
		}
		if spec.assistantReason == "" {
			t.Errorf("family %q is reachable by the assistant; it must not be", family)
		}
		if _, err := assistantEntryFamily(family); err == nil {
			t.Errorf("assistantEntryFamily(%q) resolved; the exclusion wall is open", family)
		}
	}
}

// TestAssistantScriptWriteDeclinedWritesNothing: the ask-tier card carries
// the draft verbatim — name, phrase, command-bearing path — and a decline is
// a mutation no-op: the config file never comes into being.
func TestAssistantScriptWriteDeclinedWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := scriptFile(t, dir, "deploy.sh")
	client, _, configFile := selfConfigDaemon(t, testConfig(), [][]ai.ToolCall{{{
		ID: "c1", Name: "config.write_entry",
		Arguments: `{"family":"scripts","entry":{"name":"deploy","phrases":["ship it"],"path":"` + path + `"}}`,
	}}})

	if err := client.Call("session.text", map[string]string{"text": "make a deploy script"}, nil); err != nil {
		t.Fatal(err)
	}
	required := waitForEvent(t, client, "tool.confirmation_required")
	card, _ := required["command"].(string)
	for _, want := range []string{
		`create script "deploy"`,
		`phrases: "ship it"`,
		"runs file (verbatim): " + path,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("card %q missing %q", card, want)
		}
	}
	if err := client.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.declined")
	waitForEvent(t, client, "session.finished")
	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		t.Errorf("a declined write touched the config file: %v", err)
	}
}

// TestAssistantScriptWriteApprovedLandsAndReloads: on approval the entry
// lands through config.upsert_entry (the event says so, with the assistant as
// source), the tool result words the WRITTEN entry with the honest "not live
// until this exchange ends", and the post-session reload then applies it —
// the same mechanism a layout capture uses — so the phrase is runnable by the
// time it can next be spoken.
func TestAssistantScriptWriteApprovedLandsAndReloads(t *testing.T) {
	dir := t.TempDir()
	path := scriptFile(t, dir, "deploy.sh")
	client, provider, configFile := selfConfigDaemon(t, testConfig(), [][]ai.ToolCall{{{
		ID: "c1", Name: "config.write_entry",
		Arguments: `{"family":"scripts","entry":{"name":"deploy","phrases":["ship it"],"path":"` + path + `"}}`,
	}}})

	if err := client.Call("session.text", map[string]string{"text": "make a deploy script"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	if err := client.Call("session.confirm", map[string]bool{"approved": true}, nil); err != nil {
		t.Fatal(err)
	}
	changed := waitForEvent(t, client, "config.entry_changed")
	if changed["action"] != "created" || changed["name"] != "deploy" || changed["source"] != "assistant" {
		t.Errorf("entry_changed = %v, want a created deploy sourced to the assistant", changed)
	}
	waitForEvent(t, client, "session.finished")

	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "[[scripts]]") || !strings.Contains(string(raw), path) {
		t.Errorf("config.toml does not carry the written entry:\n%s", raw)
	}
	result := lastToolResult(t, provider)
	for _, want := range []string{"Saved", path, "takes effect the moment this exchange ends"} {
		if !strings.Contains(result, want) {
			t.Errorf("tool result %q missing %q", result, want)
		}
	}

	// The post-session reload (the captureReload mechanism): config.changed
	// fires again once the engine's collaborators pick the entry up, after
	// which the daemon's own listing serves it — the write is live, not just
	// on disk.
	waitForEvent(t, client, "config.changed")
	var listing struct {
		Scripts []map[string]any `json:"scripts"`
	}
	if err := client.Call("scripts.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range listing.Scripts {
		if s["name"] == "deploy" {
			found = true
		}
	}
	if !found {
		t.Errorf("scripts.list does not serve the reloaded entry: %v", listing.Scripts)
	}
}

// TestAssistantValidationProblemsFeedTheRetryLoop: a colliding draft comes
// back as field-keyed problems with nothing written, the model's corrected
// second draft lands — the loop the acceptance criteria describe, run
// end-to-end. The tool is explicitly allowed so the loop is the only thing
// under test (and explicit naming reaching the floor is itself an assertion).
func TestAssistantValidationProblemsFeedTheRetryLoop(t *testing.T) {
	dir := t.TempDir()
	path := scriptFile(t, dir, "deploy.sh")
	cfg := testConfig()
	cfg.Tools.Policy.Tool = map[string]string{"config.write_entry": "allow"}
	// The file already holds a script answering to "ship it"; validation
	// judges the whole rewritten document, so the colliding draft must fail.
	shipper := scriptFile(t, dir, "shipper.sh")
	client, provider, configFile := selfConfigDaemon(t, cfg, [][]ai.ToolCall{
		{{ID: "c1", Name: "config.write_entry",
			Arguments: `{"family":"scripts","entry":{"name":"deploy","phrases":["ship it"],"path":"` + path + `"}}`}},
		{{ID: "c2", Name: "config.write_entry",
			Arguments: `{"family":"scripts","entry":{"name":"deploy","phrases":["deploy it"],"path":"` + path + `"}}`}},
	})
	seed := "[[scripts]]\nname = \"shipper\"\nphrases = [\"ship it\"]\npath = \"" + shipper + "\"\n"
	if err := os.WriteFile(configFile, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := client.Call("session.text", map[string]string{"text": "make a deploy script"}, nil); err != nil {
		t.Fatal(err)
	}
	changed := waitForEvent(t, client, "config.entry_changed")
	if changed["name"] != "deploy" {
		t.Errorf("entry_changed = %v", changed)
	}
	waitForEvent(t, client, "session.finished")

	// Round 2's request carries the first attempt's refusal: field-keyed,
	// nothing-written, and the two legal continuations.
	if len(provider.Requests) < 3 {
		t.Fatalf("rounds = %d, want the retry to have happened", len(provider.Requests))
	}
	var firstResult string
	for _, m := range provider.Requests[1].Messages {
		if m.Role == ai.RoleTool {
			firstResult = m.Content
		}
	}
	for _, want := range []string{"NOTHING was written", `"ship it"`, "Fix exactly what each problem names"} {
		if !strings.Contains(firstResult, want) {
			t.Errorf("validation feedback %q missing %q", firstResult, want)
		}
	}
	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"deploy it"`) {
		t.Errorf("the corrected draft did not land:\n%s", raw)
	}
	if strings.Count(string(raw), "[[scripts]]") != 2 {
		t.Errorf("expected the seeded entry plus one new entry:\n%s", raw)
	}
}

// TestAssistantExclusionWallOverSocket is the NFR test as stated: a policy of
// `default = "allow"` plus a direct attempt at the [ai] space still refuses —
// before the gate, with a spoken-ready rule on the tool.denied event — and
// the config file is untouched.
func TestAssistantExclusionWallOverSocket(t *testing.T) {
	cfg := testConfig()
	cfg.Tools.Policy.Default = "allow"
	client, provider, configFile := selfConfigDaemon(t, cfg, [][]ai.ToolCall{{{
		ID: "c1", Name: "config.write_setting",
		Arguments: `{"key":"ai.model","value":"other-model"}`,
	}}})

	if err := client.Call("session.text", map[string]string{"text": "switch your model"}, nil); err != nil {
		t.Fatal(err)
	}
	denied := waitForEvent(t, client, "tool.denied")
	rule, _ := denied["rule"].(string)
	if !strings.Contains(rule, "may not change its own AI provider") {
		t.Errorf("denied rule %q is not the wall's spoken-ready reason", rule)
	}
	waitForEvent(t, client, "session.finished")
	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		t.Errorf("a refused write touched the config file: %v", err)
	}
	if !strings.Contains(lastToolResult(t, provider), "not permitted") {
		t.Error("the model was not told the call is not permitted")
	}
}

// TestAssistantSettingWriteConfirmsAndLands: "talk faster" end to end — the
// card shows the exact key and value, the write lands through the settings
// screen's own path (config.setting_changed says the assistant did it), and
// the result reports the saved value with the deferred-apply honesty.
func TestAssistantSettingWriteConfirmsAndLands(t *testing.T) {
	client, provider, configFile := selfConfigDaemon(t, testConfig(), [][]ai.ToolCall{{{
		ID: "c1", Name: "config.write_setting",
		Arguments: `{"key":"tts.kokoro.speed","value":1.3}`,
	}}})

	if err := client.Call("session.text", map[string]string{"text": "talk faster"}, nil); err != nil {
		t.Fatal(err)
	}
	required := waitForEvent(t, client, "tool.confirmation_required")
	if required["command"] != "set tts.kokoro.speed = 1.3" {
		t.Errorf("card = %v, want the exact key and value", required["command"])
	}
	if err := client.Call("session.confirm", map[string]bool{"approved": true}, nil); err != nil {
		t.Fatal(err)
	}
	changed := waitForEvent(t, client, "config.setting_changed")
	if changed["key"] != "tts.kokoro.speed" || changed["source"] != "assistant" {
		t.Errorf("setting_changed = %v", changed)
	}
	waitForEvent(t, client, "session.finished")

	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "speed = 1.3") {
		t.Errorf("config.toml does not carry the setting:\n%s", raw)
	}
	result := lastToolResult(t, provider)
	if !strings.Contains(result, "tts.kokoro.speed is now 1.3") {
		t.Errorf("tool result %q does not state the saved value", result)
	}
}

// TestAssistantAttemptsLandInTheActivityRing: the observability criterion —
// approved, declined, and refused attempts each leave a row.
func TestAssistantAttemptsLandInTheActivityRing(t *testing.T) {
	dir := t.TempDir()
	path := scriptFile(t, dir, "deploy.sh")
	client, _, _ := selfConfigDaemon(t, testConfig(), [][]ai.ToolCall{{{
		ID: "c1", Name: "config.write_entry",
		Arguments: `{"family":"scripts","entry":{"name":"deploy","phrases":["ship it"],"path":"` + path + `"}}`,
	}}})

	if err := client.Call("session.text", map[string]string{"text": "make a deploy script"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	if err := client.Call("session.confirm", map[string]bool{"approved": true}, nil); err != nil {
		t.Fatal(err)
	}
	waitForActivityRow(t, client, "Approved: config.write_entry")
	waitForActivityRow(t, client, "Script created: deploy")
	waitForEvent(t, client, "session.finished")
}

// --------------------------------------------------------- the bridge alone

// bridgeDaemon builds a wired (unserved) daemon for direct bridge tests.
func bridgeDaemon(t *testing.T) (*assistantConfigAdmin, string) {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	d, err := New(testConfig(), paths, nil, Deps{
		Provider:    &ai.Fake{},
		Transcriber: &stt.Fake{},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &assistantConfigAdmin{d: d}, paths.ConfigFile()
}

// TestBridgeSettingsViewPrunesTheExcludedSpace: what the tool can see is
// already the pruned registry — the structural half of the wall — and the
// dangerous flags travel with it.
func TestBridgeSettingsViewPrunesTheExcludedSpace(t *testing.T) {
	bridge, _ := bridgeDaemon(t)
	view := bridge.Settings()
	if len(view) == 0 {
		t.Fatal("empty settings view")
	}
	dangerous := false
	for _, s := range view {
		if strings.HasPrefix(s.Key, "ai.") {
			t.Errorf("excluded key %q reached the tool's view", s.Key)
		}
		if s.Key == "tools.typing.enable" && s.Dangerous {
			dangerous = true
		}
	}
	if !dangerous {
		t.Error("tools.typing.enable missing or not flagged dangerous in the view")
	}
}

// TestBridgeWriteSettingRefusesExcludedKeysWithoutWriting: even a tool bug
// that skipped every earlier check dies here, the last code before the
// shared write path — and the file proves nothing happened.
func TestBridgeWriteSettingRefusesExcludedKeysWithoutWriting(t *testing.T) {
	bridge, configFile := bridgeDaemon(t)
	if _, err := bridge.WriteSetting("ai.model", "other"); err == nil ||
		!strings.Contains(err.Error(), "may not change") {
		t.Errorf("excluded write error = %v", err)
	}
	if _, err := bridge.WriteSetting("tools.policy.default", "allow"); err == nil {
		t.Error("the policy default was writable through the bridge")
	}
	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		t.Errorf("a refused setting write touched the config file: %v", err)
	}
}

// TestBridgeWriteSettingReadsTheSavedValueBack: the receipt's value comes
// from the file after the write — the number a spoken confirmation may state.
func TestBridgeWriteSettingReadsTheSavedValueBack(t *testing.T) {
	bridge, configFile := bridgeDaemon(t)
	receipt, err := bridge.WriteSetting("tts.kokoro.speed", 1.3)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Value != 1.3 {
		t.Errorf("receipt value = %v, want the saved 1.3", receipt.Value)
	}
	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "speed = 1.3") {
		t.Errorf("file does not carry the write:\n%s", raw)
	}
}
