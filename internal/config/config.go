// Package config loads, defaults, and validates Jarvix configuration.
//
// Configuration lives in ~/.config/jarvix/config.toml. Secrets are never
// stored in the file by default; API keys come from environment variables
// (e.g. OPENAI_API_KEY) with an explicit api_key_env override per provider.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/rpickz/jarvix/internal/hotkey"
)

// Config is the root configuration document.
type Config struct {
	Activation   Activation   `toml:"activation"`
	AI           AI           `toml:"ai"`
	STT          STT          `toml:"stt"`
	TTS          TTS          `toml:"tts"`
	Conversation Conversation `toml:"conversation"`
	Tools        Tools        `toml:"tools"`
	Audio        Audio        `toml:"audio"`
	UI           UI           `toml:"ui"`
	Log          Log          `toml:"log"`
}

// Tools configures the assistant's tool access. Tools are opt-in: enabling
// shell.run gives the assistant the same authority as the user's shell.
type Tools struct {
	Shell            bool `toml:"shell"`              // enable shell.run
	ShellTimeoutSec  int  `toml:"shell_timeout_sec"`  // per-command timeout
	ShellMaxOutputKB int  `toml:"shell_max_output_kb"` // captured output cap
}

// Activation configures how sessions are initiated.
type Activation struct {
	Mode string `toml:"mode"` // "push_to_talk"
	// PTTChord is the hold-to-talk key chord watched by the daemon (evdev
	// key names, e.g. ["leftmeta","leftalt","v"]). Requires read access to
	// input devices (jarvix doctor explains how to grant it); without
	// access, the Hyprland tap-to-toggle binding is the fallback. Empty
	// disables the daemon-side watcher.
	PTTChord []string `toml:"ptt_chord"`
}

// AI selects and configures the assistant provider.
type AI struct {
	Provider     string  `toml:"provider"` // key into Endpoints, or a preset name
	Model        string  `toml:"model"`
	SystemPrompt string  `toml:"system_prompt"`
	MaxTokens    int     `toml:"max_tokens"`
	Temperature  float64 `toml:"temperature"`

	// Endpoints maps a provider name to an OpenAI-compatible endpoint.
	// Presets exist for "openai", "ollama", "openrouter", and "lmstudio";
	// any additional table under [ai.<name>] defines a new endpoint without
	// code changes.
	Endpoints map[string]Endpoint `toml:"-"`
}

// Endpoint describes one OpenAI-compatible API endpoint.
type Endpoint struct {
	BaseURL string `toml:"base_url"`
	// APIKeyEnv names the environment variable holding the API key.
	// The key itself is never stored in configuration.
	APIKeyEnv string `toml:"api_key_env"`
	// APIKey is a developer/testing fallback only. Prefer APIKeyEnv.
	APIKey string `toml:"api_key"`
}

// Key resolves the API key for the endpoint, preferring the environment.
func (e Endpoint) Key() string {
	if e.APIKeyEnv != "" {
		if v := os.Getenv(e.APIKeyEnv); v != "" {
			return v
		}
	}
	return e.APIKey
}

// STT selects and configures speech-to-text.
type STT struct {
	Provider string  `toml:"provider"` // "whisper"
	Whisper  Whisper `toml:"whisper"`
}

// Whisper configures the whisper.cpp adapter.
type Whisper struct {
	Model    string `toml:"model"`    // model name ("base.en") or absolute path to a ggml file
	Binary   string `toml:"binary"`   // whisper-cli binary; searched on PATH when relative
	Language string `toml:"language"` // e.g. "en", or "auto"
}

// TTS selects and configures text-to-speech.
type TTS struct {
	Provider string `toml:"provider"` // "piper" or "kokoro"
	Piper    Piper  `toml:"piper"`
	Kokoro   Kokoro `toml:"kokoro"`
}

// Piper configures the Piper adapter.
type Piper struct {
	Voice  string `toml:"voice"`  // voice name ("en_US-amy-medium") or absolute path to .onnx
	Binary string `toml:"binary"` // piper binary; searched on PATH when relative
}

// Kokoro configures the Kokoro adapter (more natural voice, heavier setup).
type Kokoro struct {
	Voice string  `toml:"voice"` // Kokoro voice id, e.g. "af_heart"
	Speed float64 `toml:"speed"` // speech rate multiplier, default 1.0
}

// Conversation configures assistant behaviour.
type Conversation struct {
	SpeakResponses bool `toml:"speak_responses"`
}

// Audio configures capture and playback.
type Audio struct {
	InputDevice     string `toml:"input_device"`     // PipeWire target, empty = default
	OutputDevice    string `toml:"output_device"`    // PipeWire target, empty = default
	MaxRecordingSec int    `toml:"max_recording_sec"` // safety cap on recording length
	// MinRecordingMs discards recordings shorter than this as accidental
	// activations (a stray tap) instead of transcribing them.
	MinRecordingMs int `toml:"min_recording_ms"`
}

// UI configures what the overlay is told to display.
type UI struct {
	ShowTranscript bool `toml:"show_transcript"`
	ShowResponse   bool `toml:"show_response"`
}

// Log configures daemon logging.
type Log struct {
	Level string `toml:"level"` // debug, info, warn, error
}

// Default returns the configuration used when no file exists. Jarvix must
// work with an empty config file on a machine with Ollama and Piper present.
func Default() Config {
	return Config{
		Activation: Activation{
			Mode:     "push_to_talk",
			PTTChord: []string{"leftmeta", "leftalt", "v"},
		},
		AI: AI{
			Provider:     "ollama",
			Model:        "llama3.2:3b",
			SystemPrompt: defaultSystemPrompt,
			MaxTokens:    1024,
			Temperature:  0.7,
			Endpoints: map[string]Endpoint{
				"openai":     {BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
				"openrouter": {BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY"},
				"ollama":     {BaseURL: "http://127.0.0.1:11434/v1"},
				"lmstudio":   {BaseURL: "http://127.0.0.1:1234/v1"},
			},
		},
		STT: STT{
			Provider: "whisper",
			Whisper:  Whisper{Model: "base.en", Binary: "whisper-cli", Language: "en"},
		},
		TTS: TTS{
			Provider: "piper",
			Piper:    Piper{Voice: "en_US-amy-medium", Binary: "piper-tts"},
			Kokoro:   Kokoro{Voice: "af_heart", Speed: 1.0},
		},
		Conversation: Conversation{SpeakResponses: true},
		Tools:        Tools{Shell: false, ShellTimeoutSec: 30, ShellMaxOutputKB: 16},
		Audio:        Audio{MaxRecordingSec: 60, MinRecordingMs: 300},
		UI:           UI{ShowTranscript: true, ShowResponse: true},
		Log:          Log{Level: "info"},
	}
}

const defaultSystemPrompt = "You are Jarvix, a voice assistant built into the user's Linux computer. " +
	"Your responses are spoken aloud, so answer concisely in plain prose: no markdown, " +
	"no lists, no code blocks, no preamble. Get straight to the point."

// ToolSystemPrompt is appended to the system prompt when tools are enabled.
// It tells the model to act on its own rather than instruct the user, and to
// keep spoken answers about command output brief.
const ToolSystemPrompt = " You can run shell commands yourself with the shell.run tool to answer " +
	"questions about the computer's live state and to carry out tasks. When the user asks what is " +
	"happening with something (Docker, git, processes, disk, services), run the appropriate command " +
	"and summarise the result — do not tell the user which command to run, run it. Prefer read-only " +
	"commands; before anything destructive or irreversible, ask for confirmation first. Summarise " +
	"command output for speech: report what matters, not raw tables."

// Load reads the config file at path, applying defaults for anything unset.
// A missing file is not an error; defaults are returned.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	return parse(data, cfg)
}

func parse(data []byte, cfg Config) (Config, error) {
	// Custom endpoints live as arbitrary tables under [ai.<name>], which TOML
	// struct decoding cannot express directly. Decode the document twice: once
	// into the typed struct, once loosely to harvest endpoint tables.
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	_ = md

	var loose struct {
		AI map[string]toml.Primitive `toml:"ai"`
	}
	looseMD, err := toml.Decode(string(data), &loose)
	if err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	reserved := map[string]bool{
		"provider": true, "model": true, "system_prompt": true,
		"max_tokens": true, "temperature": true,
	}
	for name, prim := range loose.AI {
		if reserved[name] {
			continue
		}
		var ep Endpoint
		if err := looseMD.PrimitiveDecode(prim, &ep); err != nil {
			return cfg, fmt.Errorf("parse config: [ai.%s]: %w", name, err)
		}
		base := cfg.AI.Endpoints[name]
		if ep.BaseURL != "" {
			base.BaseURL = ep.BaseURL
		}
		if ep.APIKeyEnv != "" {
			base.APIKeyEnv = ep.APIKeyEnv
		}
		if ep.APIKey != "" {
			base.APIKey = ep.APIKey
		}
		cfg.AI.Endpoints[name] = base
	}
	return cfg, nil
}

// Validate checks the configuration for problems a user must fix, returning
// actionable messages.
func (c Config) Validate() error {
	var problems []string

	if c.Activation.Mode != "push_to_talk" {
		problems = append(problems, fmt.Sprintf(
			"activation.mode %q is not supported; use \"push_to_talk\"", c.Activation.Mode))
	}
	if len(c.Activation.PTTChord) > 0 {
		if _, err := hotkey.ResolveChord(c.Activation.PTTChord); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if c.AI.Provider == "" {
		problems = append(problems, "ai.provider is empty; set it to a provider such as \"ollama\" or \"openai\"")
	} else if _, ok := c.AI.Endpoints[c.AI.Provider]; !ok {
		problems = append(problems, fmt.Sprintf(
			"ai.provider %q has no endpoint; add an [ai.%s] table with a base_url, or use one of: %s",
			c.AI.Provider, c.AI.Provider, strings.Join(c.endpointNames(), ", ")))
	}
	if c.AI.Model == "" {
		problems = append(problems, "ai.model is empty; set the model name your provider should use")
	}
	if c.STT.Provider != "whisper" {
		problems = append(problems, fmt.Sprintf(
			"stt.provider %q is not supported; use \"whisper\"", c.STT.Provider))
	}
	if c.TTS.Provider != "piper" && c.TTS.Provider != "kokoro" {
		problems = append(problems, fmt.Sprintf(
			"tts.provider %q is not supported; use \"piper\" or \"kokoro\"", c.TTS.Provider))
	}
	if c.Audio.MaxRecordingSec <= 0 {
		problems = append(problems, "audio.max_recording_sec must be positive")
	}
	if c.Audio.MinRecordingMs < 0 {
		problems = append(problems, "audio.min_recording_ms must not be negative")
	} else if c.Audio.MinRecordingMs >= c.Audio.MaxRecordingSec*1000 {
		problems = append(problems, "audio.min_recording_ms must be smaller than audio.max_recording_sec")
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, fmt.Sprintf(
			"log.level %q is invalid; use debug, info, warn, or error", c.Log.Level))
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

func (c Config) endpointNames() []string {
	names := make([]string, 0, len(c.AI.Endpoints))
	for name := range c.AI.Endpoints {
		names = append(names, name)
	}
	// Deterministic order for error messages.
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names
}

// Endpoint returns the endpoint for the configured AI provider.
func (c Config) Endpoint() (Endpoint, bool) {
	ep, ok := c.AI.Endpoints[c.AI.Provider]
	return ep, ok
}

// Redact returns a copy safe for logging or display: any inline API keys are
// masked.
func (c Config) Redact() Config {
	out := c
	out.AI.Endpoints = make(map[string]Endpoint, len(c.AI.Endpoints))
	for name, ep := range c.AI.Endpoints {
		if ep.APIKey != "" {
			ep.APIKey = "[redacted]"
		}
		out.AI.Endpoints[name] = ep
	}
	return out
}
