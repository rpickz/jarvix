package config

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The assistant's view of the settings registry (issue #105, ADR 0036): the
// dangerous set is pinned key by key, and the exclusion wall's membership is
// asserted from both sides — what is out is out for a stated reason, and
// what is in is genuinely writable.

// TestDangerousSettingsAreExactlyTheEnumeratedSet is the guard the
// dangerousSettingKey comment promises: widening or narrowing the
// always-confirm set is a reviewed decision, not a drive-by.
func TestDangerousSettingsAreExactlyTheEnumeratedSet(t *testing.T) {
	want := []string{
		"activation.mode",
		"activation.wake_command",
		"artifacts.open_command",
		"intents.terminal",
		"stt.whisper.binary",
		"tools.artifacts",
		"tools.desktop",
		"tools.desktop_apps",
		// The launch overrides (#194) are dangerous by the tools. prefix, and
		// deserve it: they decide whether a program is started bare or handed
		// to a terminal, which is a decision about what actually runs.
		"tools.launch.graphical_programs",
		"tools.launch.terminal_programs",
		"tools.shell",
		"tools.shell_max_output_kb",
		"tools.shell_timeout_sec",
		"tools.typing.enable",
		"tools.typing.max_chars",
		"tools.typing.rate_limit",
		"tools.typing.rate_window_sec",
		"tools.typing.terminal_classes",
		"tts.piper.binary",
	}
	var got []string
	for _, s := range Settings() {
		if s.Dangerous {
			got = append(got, s.Key)
		}
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dangerous settings drifted:\n got %v\nwant %v", got, want)
	}
}

// TestAssistantSettingsExcludeTheAISpace: the [ai] table never enters the
// assistant's view — not one key, whatever the registry grows.
func TestAssistantSettingsExcludeTheAISpace(t *testing.T) {
	view := AssistantSettings()
	if len(view) == 0 {
		t.Fatal("the assistant's settings view is empty")
	}
	for _, s := range view {
		if strings.HasPrefix(s.Key, "ai.") {
			t.Errorf("excluded key %q reached the assistant's view", s.Key)
		}
	}
	// The registry genuinely holds ai.* keys — the exclusion is doing work,
	// not passing vacuously.
	if _, ok := SettingFor("ai.model"); !ok {
		t.Fatal("the registry lost ai.model; this test no longer proves the pruning")
	}
	// And the writable-but-dangerous keys stay IN the view: dangerous is a
	// confirmation floor, not an exclusion.
	found := false
	for _, s := range view {
		if s.Key == "tools.typing.enable" {
			found = s.Dangerous
		}
	}
	if !found {
		t.Error("tools.typing.enable missing from the view (or not flagged dangerous); " +
			"dangerous settings are writable-with-confirmation, not excluded")
	}
}

// TestAssistantExcludedSettingReasons: every excluded space refuses with a
// spoken-ready reason, prefix and bare-table spellings alike, while ordinary
// and merely-unknown keys do not touch the wall.
func TestAssistantExcludedSettingReasons(t *testing.T) {
	excluded := []string{
		"ai", "ai.model", "ai.system_prompt", "ai.anthropic.api_key",
		"tools.policy", "tools.policy.default", "tools.policy.shell_allow",
		"advisors", "advisors.claude.command",
		"intents.custom", "intents.custom.0.command",
	}
	for _, key := range excluded {
		reason, ok := AssistantExcludedSettingReason(key)
		if !ok {
			t.Errorf("%q is not excluded", key)
			continue
		}
		if !strings.Contains(reason, "may not change") {
			t.Errorf("%q reason %q is not spoken-ready", key, reason)
		}
	}
	for _, key := range []string{
		"tts.kokoro.speed", "assistant.name", "tools.typing.enable",
		"intents.enabled", "intents.terminal", "no.such.key",
	} {
		if reason, ok := AssistantExcludedSettingReason(key); ok {
			t.Errorf("%q wrongly excluded: %q", key, reason)
		}
	}
}
