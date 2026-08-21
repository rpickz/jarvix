package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AI.Provider != "ollama" {
		t.Errorf("default provider = %q, want ollama", cfg.AI.Provider)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

func TestNotificationDefaultsOn(t *testing.T) {
	cfg := Default()
	if !cfg.UI.Notifications {
		t.Error("ui.notifications should default to true")
	}
	if !cfg.UI.NotificationPreview {
		t.Error("ui.notification_preview should default to true")
	}
}

func TestNotificationKeysOverride(t *testing.T) {
	cfg := writeAndLoad(t, `
[ui]
notifications = false
notification_preview = false
`)
	if cfg.UI.Notifications {
		t.Error("notifications should be off")
	}
	if cfg.UI.NotificationPreview {
		t.Error("notification_preview should be off")
	}
	// Untouched [ui] defaults survive a partial table.
	if !cfg.UI.ShowTranscript {
		t.Error("show_transcript default lost")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	cfg := writeAndLoad(t, `
[ai]
provider = "openai"
model = "gpt-4.1-mini"

[stt.whisper]
model = "small.en"

[conversation]
speak_responses = false
`)
	if cfg.AI.Provider != "openai" || cfg.AI.Model != "gpt-4.1-mini" {
		t.Errorf("ai = %+v", cfg.AI)
	}
	if cfg.STT.Whisper.Model != "small.en" {
		t.Errorf("whisper model = %q", cfg.STT.Whisper.Model)
	}
	if cfg.Conversation.SpeakResponses {
		t.Error("speak_responses should be false")
	}
	// Untouched defaults survive.
	if cfg.TTS.Piper.Voice != "en_US-amy-medium" {
		t.Errorf("piper voice default lost: %q", cfg.TTS.Piper.Voice)
	}
}

func TestCustomEndpointWithoutCodeChanges(t *testing.T) {
	cfg := writeAndLoad(t, `
[ai]
provider = "myserver"
model = "local-model"

[ai.myserver]
base_url = "http://10.0.0.5:8080/v1"
api_key_env = "MYSERVER_KEY"
`)
	ep, ok := cfg.Endpoint()
	if !ok {
		t.Fatal("custom endpoint not registered")
	}
	if ep.BaseURL != "http://10.0.0.5:8080/v1" || ep.APIKeyEnv != "MYSERVER_KEY" {
		t.Errorf("endpoint = %+v", ep)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestPresetEndpointOverride(t *testing.T) {
	cfg := writeAndLoad(t, `
[ai.openai]
base_url = "https://proxy.example.com/v1"
`)
	if got := cfg.AI.Endpoints["openai"].BaseURL; got != "https://proxy.example.com/v1" {
		t.Errorf("base_url = %q", got)
	}
	// Preset key env is preserved when only base_url is overridden.
	if got := cfg.AI.Endpoints["openai"].APIKeyEnv; got != "OPENAI_API_KEY" {
		t.Errorf("api_key_env = %q", got)
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	cfg := writeAndLoad(t, `
[activation]
mode = "telepathy"

[ai]
provider = "nonexistent"
model = ""

[log]
level = "loud"
`)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, want := range []string{"telepathy", "nonexistent", "ai.model", "log.level"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing mention of %q: %v", want, err)
		}
	}
}

func TestArtifactDefaults(t *testing.T) {
	cfg := Default()
	if !strings.HasSuffix(cfg.Artifacts.Dir, filepath.Join("Documents", "Jarvix")) {
		t.Errorf("artifacts.dir default = %q", cfg.Artifacts.Dir)
	}
	if cfg.Artifacts.OpenCommand != "xdg-open" || cfg.Artifacts.RenderTimeoutSec != 10 {
		t.Errorf("artifacts defaults = %+v", cfg.Artifacts)
	}
	if !cfg.Tools.Artifacts {
		t.Error("tools.artifacts should default on: the tool degrades safely without its renderer")
	}
}

func TestArtifactOverridesAndValidation(t *testing.T) {
	cfg := writeAndLoad(t, `
[tools]
artifacts = false

[artifacts]
dir = "/tmp/my-artifacts"
open_command = "imv"
render_timeout_sec = 30
`)
	if cfg.Tools.Artifacts {
		t.Error("tools.artifacts should be off")
	}
	if cfg.Artifacts.Dir != "/tmp/my-artifacts" || cfg.Artifacts.OpenCommand != "imv" ||
		cfg.Artifacts.RenderTimeoutSec != 30 {
		t.Errorf("artifacts = %+v", cfg.Artifacts)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestArtifactValidationRejectsBadValues(t *testing.T) {
	cfg := writeAndLoad(t, `
[artifacts]
dir = "~/Documents/Jarvix"
open_command = " "
render_timeout_sec = 0
`)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, want := range []string{"artifacts.dir", "artifacts.open_command", "artifacts.render_timeout_sec"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing mention of %q: %v", want, err)
		}
	}
}

func TestEndpointKeyPrefersEnvironment(t *testing.T) {
	t.Setenv("JARVIX_TEST_KEY", "from-env")
	ep := Endpoint{APIKeyEnv: "JARVIX_TEST_KEY", APIKey: "inline"}
	if got := ep.Key(); got != "from-env" {
		t.Errorf("Key() = %q, want from-env", got)
	}
	t.Setenv("JARVIX_TEST_KEY", "")
	if got := ep.Key(); got != "inline" {
		t.Errorf("Key() fallback = %q, want inline", got)
	}
}

func TestRedactMasksInlineKeys(t *testing.T) {
	cfg := writeAndLoad(t, `
[ai.openai]
api_key = "sk-secret"
`)
	red := cfg.Redact()
	if got := red.AI.Endpoints["openai"].APIKey; got != "[redacted]" {
		t.Errorf("redacted key = %q", got)
	}
	// Original is untouched.
	if got := cfg.AI.Endpoints["openai"].APIKey; got != "sk-secret" {
		t.Errorf("original mutated: %q", got)
	}
}

func TestParseErrorIsHelpful(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[ai\nbroken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected parse error")
	}
}

func writeAndLoad(t *testing.T, content string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}
