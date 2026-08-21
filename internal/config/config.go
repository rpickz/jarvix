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
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/rpickz/jarvix/internal/hotkey"
	"github.com/rpickz/jarvix/internal/intent"
)

// Config is the root configuration document.
type Config struct {
	Activation   Activation   `toml:"activation"`
	AI           AI           `toml:"ai"`
	STT          STT          `toml:"stt"`
	TTS          TTS          `toml:"tts"`
	Conversation Conversation `toml:"conversation"`
	Intents      Intents      `toml:"intents"`
	Tools        Tools        `toml:"tools"`
	Artifacts    Artifacts    `toml:"artifacts"`
	// Context is what Jarvix may look at on the desktop before answering
	// (see context.go). Every source is opt-in; the clipboard defaults off.
	Context Context `toml:"context"`
	// Advisors are the assistant CLIs Jarvix may delegate a question to, one
	// [advisors.<name>] table each (see advisors.go). Empty disables
	// delegation entirely — the tool is not registered.
	Advisors    map[string]Advisor `toml:"advisors"`
	Audio       Audio              `toml:"audio"`
	Performance Performance        `toml:"performance"`
	UI          UI                 `toml:"ui"`
	Log         Log                `toml:"log"`
}

// Performance decides how much of the engine stack stays warm between
// interactions (ADR 0018).
//
// The whole section is a memory-for-latency trade. With warm engines off,
// Jarvix behaves exactly as it did before: whisper reloads its model per
// transcription and the TTS helper boots per response, costing hundreds of
// milliseconds on the release-to-first-audio path but leaving nothing resident
// between questions. With them on, one supervised child per engine stays
// loaded — which is the difference between "answers begin" and "answers begin
// after a pause", at the cost of a few hundred MB while the machine is idle.
type Performance struct {
	// WarmEngines keeps supervised STT and TTS workers alive between
	// sessions. Turn it off on a low-RAM machine to get the old behaviour
	// back, exactly.
	WarmEngines bool `toml:"warm_engines"`
	// WarmMemoryCapMB retires a warm worker whose resident set grows past
	// this, so a leaking engine costs one cold start instead of the machine's
	// memory. 0 disables the cap.
	WarmMemoryCapMB int `toml:"warm_memory_cap_mb"`
	// WarmIdleReapSec frees the warm workers after this long without an
	// interaction; the next question pays one cold start. 0 keeps them until
	// jarvixd exits.
	WarmIdleReapSec int `toml:"warm_idle_reap_sec"`
}

// Tools configures the assistant's tool access. shell.run is opt-in: it
// gives the assistant the same authority as the user's shell — which is why
// every call also passes the permission gate in Policy. artifact.create
// only writes into the artifact directory and opens a viewer, so it defaults
// on — with the renderer missing it degrades to a prose answer.
type Tools struct {
	Shell            bool        `toml:"shell"`               // enable shell.run
	ShellTimeoutSec  int         `toml:"shell_timeout_sec"`   // per-command timeout
	ShellMaxOutputKB int         `toml:"shell_max_output_kb"` // captured output cap
	Artifacts        bool        `toml:"artifacts"`           // enable artifact.create
	Policy           ToolsPolicy `toml:"policy"`
}

// Command is a viewer invocation: argv, not a shell line. Jarvix never runs
// a viewer through a shell, so quoting and globbing would be silently
// ignored — the argv form is what actually reaches exec.
//
// Both TOML shapes decode into it:
//
//	open_command = "xdg-open"                     # split on whitespace
//	open_command = ["/opt/my viewer/bin/view", "--new"]
//
// The string form is the original config shape and stays supported, so every
// config written before the array existed keeps working unchanged. It splits
// on whitespace, which means it cannot express a viewer whose path or
// argument *contains* whitespace — that is exactly what the array form is
// for, and the reason it was added (raised in review of #19).
type Command []string

// UnmarshalTOML implements toml.Unmarshaler for both accepted shapes.
func (c *Command) UnmarshalTOML(v any) error {
	switch t := v.(type) {
	case string:
		// Legacy shorthand: whitespace-separated argv. Empty (or the
		// literal "none") stays empty, which callers read as "no viewer".
		*c = strings.Fields(t)
		return nil
	case []any:
		argv := make(Command, 0, len(t))
		for i, raw := range t {
			s, ok := raw.(string)
			if !ok {
				return fmt.Errorf("element %d is %T; a command array holds strings only", i, raw)
			}
			argv = append(argv, s)
		}
		*c = argv
		return nil
	default:
		return fmt.Errorf("must be a string (\"xdg-open\") or an array of strings ([\"/opt/my viewer\", \"--new\"]), got %T", v)
	}
}

// Artifacts configures where rendered artifacts (diagrams, later documents)
// land and how they are shown.
type Artifacts struct {
	// Dir is where artifacts are saved. Created 0700 on first use; must be
	// absolute ("~" is not expanded — write the real path).
	Dir string `toml:"dir"`
	// OpenCommand launches a rendered artifact in a viewer. Either a string
	// ("xdg-open", split on whitespace) or an argv array — see Command.
	OpenCommand Command `toml:"open_command"`
	// OpenCommands overrides OpenCommand per artifact format, e.g. under
	// [artifacts.open_commands]: document = "obsidian". An entry set to "",
	// [], or "none" declares the format has no viewer — the artifact is
	// saved and announced by name, nothing is launched. Formats without an
	// entry fall back to OpenCommand.
	OpenCommands map[string]Command `toml:"open_commands"`
	// RenderTimeoutSec bounds one render; the renderer is killed past it.
	RenderTimeoutSec int `toml:"render_timeout_sec"`
}

// ToolsPolicy is the tool permission gate (ADR 0014). Every tool call is
// classified allow / ask / deny before it executes: allow runs silently, ask
// makes Jarvix speak a one-sentence summary and wait for confirmation, deny
// never runs. Classification happens daemon-side on the parsed command — the
// model's own description of what it is doing is never trusted.
type ToolsPolicy struct {
	// Default is the decision for tools with no [tools.policy.tool] entry.
	// Unknown tools must never run silently, so the default is "ask".
	Default string `toml:"default"`
	// ConfirmTimeoutSec is how long a spoken confirmation waits before
	// declining the command.
	ConfirmTimeoutSec int `toml:"confirm_timeout_sec"`
	// RememberForConversation re-runs an approved command without asking
	// again for the rest of the conversation. Approvals never persist
	// across conversations (`jarvix new` and the follow-up window clear
	// them) or across daemon restarts.
	RememberForConversation bool `toml:"remember_for_conversation"`
	// Tool maps a tool name to "allow", "ask", or "deny". For shell.run the
	// entry is the fallback for commands no pattern classifies: the default
	// "ask" keeps read-only commands silent (the shipped allow list) and
	// confirms everything else; "allow" restores the pre-gate trust-all
	// behaviour (deny patterns still win); "deny" disables the tool.
	Tool map[string]string `toml:"tool"`
	// ShellAllow adds command word-prefix patterns (e.g. "docker compose ps")
	// that run without confirmation.
	ShellAllow []string `toml:"shell_allow"`
	// ShellDeny adds command word-prefix patterns that never run, regardless
	// of any confirmation. Deny beats everything, including ShellAllow.
	ShellDeny []string `toml:"shell_deny"`
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
	// HistoryTurns is how many prior exchanges to remember as context for
	// follow-up questions. 0 makes every turn standalone.
	HistoryTurns int `toml:"history_turns"`
	// FollowUpWindowSec resets the conversation after this many seconds of
	// inactivity, so a new question does not inherit a stale thread. 0 keeps
	// context until Jarvix restarts or the conversation is reset explicitly.
	FollowUpWindowSec int `toml:"follow_up_window_sec"`
}

// Audio configures capture and playback.
type Audio struct {
	InputDevice     string `toml:"input_device"`      // PipeWire target, empty = default
	OutputDevice    string `toml:"output_device"`     // PipeWire target, empty = default
	MaxRecordingSec int    `toml:"max_recording_sec"` // safety cap on recording length
	// MinRecordingMs discards recordings shorter than this as accidental
	// activations (a stray tap) instead of transcribing them.
	MinRecordingMs int `toml:"min_recording_ms"`
}

// UI configures the desktop surfaces: what the overlay displays, and how a
// finished session is announced.
type UI struct {
	ShowTranscript bool `toml:"show_transcript"`
	ShowResponse   bool `toml:"show_response"`
	// Notifications sends a desktop notification when a session finishes;
	// clicking it opens the conversation window. false disables notifications
	// entirely — the window stays reachable via `jarvix window`.
	Notifications bool `toml:"notifications"`
	// NotificationPreview puts the start of the assistant's answer (or the
	// error detail) in the notification body. false shows a generic
	// "Jarvix answered" instead, keeping answer content away from the
	// notification daemon and its logs.
	NotificationPreview bool `toml:"notification_preview"`
}

// Log configures daemon logging.
type Log struct {
	Level string `toml:"level"` // debug, info, warn, error
}

// Default returns the configuration used when no file exists. Jarvix must
// work with an empty config file on a machine with Ollama and Piper present.
func Default() Config {
	home, _ := os.UserHomeDir()
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
		Conversation: Conversation{SpeakResponses: true, HistoryTurns: 16, FollowUpWindowSec: 900},
		Intents:      Intents{Enabled: true, Terminal: intent.DefaultTerminal},
		Tools: Tools{
			Shell: false, ShellTimeoutSec: 30, ShellMaxOutputKB: 16, Artifacts: true,
			Policy: ToolsPolicy{
				Default:                 "ask",
				ConfirmTimeoutSec:       30,
				RememberForConversation: false,
			},
		},
		Artifacts: Artifacts{
			Dir:              filepath.Join(home, "Documents", "Jarvix"),
			OpenCommand:      Command{"xdg-open"},
			RenderTimeoutSec: 10,
		},
		// Window and selection are on: a title bar is already on screen, and a
		// selection is what the user is pointing at as they speak. The
		// clipboard is off — it holds whatever was last copied for any
		// purpose, and turning that on is the user's decision to make.
		Context: Context{
			Window: true, Selection: true, Clipboard: false,
			MaxChars: 2000, TimeoutMs: MaxContextTimeoutMs,
		},
		Audio: Audio{MaxRecordingSec: 60, MinRecordingMs: 300},
		// Warm by default: presence is the product, and the memory is
		// reclaimed after ten idle minutes. The cap is a leak detector, not a
		// working budget — whisper base.en plus Kokoro sit well under it.
		Performance: Performance{WarmEngines: true, WarmMemoryCapMB: 2048, WarmIdleReapSec: 600},
		UI:          UI{ShowTranscript: true, ShowResponse: true, Notifications: true, NotificationPreview: true},
		Log:         Log{Level: "info"},
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
	"commands. Commands that change the system trigger a built-in spoken confirmation from the user " +
	"before they run; if a tool result says the command was declined or not permitted, do not retry " +
	"it — acknowledge and move on. Summarise command output for speech: report what matters, not " +
	"raw tables."

// ArtifactSystemPrompt is appended to the system prompt when the artifact
// tool is enabled. The spoken-summary rules live here because they are model
// behaviour: the tool result repeats them, and speech normalisation cannot
// save an answer that reads a path aloud on purpose.
const ArtifactSystemPrompt = " When the user asks for output better seen than heard, use the " +
	"artifact.create tool instead of describing the content in speech: Mermaid source (format " +
	"\"mermaid\") to diagram or chart how something works or connects, Markdown (format \"document\") " +
	"to draft a document or brief, CSV (format \"spreadsheet\") to put data in a table, and an " +
	"Excalidraw scene (format \"excalidraw\") to sketch on a free-form canvas. After the tool " +
	"succeeds, answer with at most two sentences about what the artifact shows — never recite its " +
	"source, file names, or paths, because your answer is read aloud. If the tool rejects your " +
	"source, fix exactly what the error names and retry once; if it fails again, or rendering is " +
	"unavailable, apologise briefly and answer in prose."

// minWarmMemoryCapMB is the smallest cap that can hold any engine Jarvix keeps
// warm (whisper base.en alone is ~165 MB resident). A cap below it would
// retire the worker the moment it loaded its model, turning warm mode into a
// restart loop — so it is rejected rather than silently obeyed.
const minWarmMemoryCapMB = 256

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
	// [advisors.<name>] decodes straight into the map (no scalars share the
	// table, unlike [ai]), so it only needs its presets applying.
	applyAdvisorDefaults(&cfg)
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
	if c.Performance.WarmMemoryCapMB < 0 {
		problems = append(problems,
			"performance.warm_memory_cap_mb must not be negative (0 disables the cap)")
	} else if c.Performance.WarmEngines && c.Performance.WarmMemoryCapMB > 0 &&
		c.Performance.WarmMemoryCapMB < minWarmMemoryCapMB {
		problems = append(problems, fmt.Sprintf(
			"performance.warm_memory_cap_mb is %d; a warm engine needs at least %d MB, so this would retire it on every use",
			c.Performance.WarmMemoryCapMB, minWarmMemoryCapMB))
	}
	if c.Performance.WarmIdleReapSec < 0 {
		problems = append(problems,
			"performance.warm_idle_reap_sec must not be negative (0 keeps warm workers until jarvixd exits)")
	}
	if c.Artifacts.Dir == "" {
		problems = append(problems, "artifacts.dir is empty; set the directory rendered artifacts are saved in")
	} else if !filepath.IsAbs(c.Artifacts.Dir) {
		problems = append(problems, fmt.Sprintf(
			"artifacts.dir %q must be an absolute path (\"~\" is not expanded)", c.Artifacts.Dir))
	}
	if len(c.Artifacts.OpenCommand) == 0 || strings.TrimSpace(c.Artifacts.OpenCommand[0]) == "" {
		problems = append(problems, "artifacts.open_command is empty; \"xdg-open\" opens the default viewer")
	}
	if c.Artifacts.RenderTimeoutSec <= 0 {
		problems = append(problems, "artifacts.render_timeout_sec must be positive")
	}
	validDecision := func(s string) bool { return s == "allow" || s == "ask" || s == "deny" }
	if !validDecision(c.Tools.Policy.Default) {
		problems = append(problems, fmt.Sprintf(
			"tools.policy.default %q is invalid; use \"allow\", \"ask\", or \"deny\"", c.Tools.Policy.Default))
	}
	if c.Tools.Policy.ConfirmTimeoutSec <= 0 {
		problems = append(problems,
			"tools.policy.confirm_timeout_sec must be positive (seconds to wait for a confirmation)")
	}
	toolNames := make([]string, 0, len(c.Tools.Policy.Tool))
	for name := range c.Tools.Policy.Tool {
		toolNames = append(toolNames, name)
	}
	sort.Strings(toolNames) // deterministic error order
	for _, name := range toolNames {
		if !validDecision(c.Tools.Policy.Tool[name]) {
			problems = append(problems, fmt.Sprintf(
				"tools.policy.tool.%q is %q; use \"allow\", \"ask\", or \"deny\"",
				name, c.Tools.Policy.Tool[name]))
		}
	}
	for _, entry := range []struct {
		key      string
		patterns []string
	}{
		{"tools.policy.shell_allow", c.Tools.Policy.ShellAllow},
		{"tools.policy.shell_deny", c.Tools.Policy.ShellDeny},
	} {
		key, patterns := entry.key, entry.patterns
		for _, p := range patterns {
			if strings.TrimSpace(p) == "" {
				problems = append(problems, fmt.Sprintf(
					"%s contains an empty pattern; each entry must be a command prefix such as \"docker ps\"", key))
			}
		}
	}
	problems = append(problems, c.validateAdvisors()...)
	problems = append(problems, c.intentProblems()...)
	problems = append(problems, c.contextProblems()...)
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
