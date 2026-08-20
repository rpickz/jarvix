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
