package config

import (
	"fmt"
	"strconv"
	"strings"
)

// Reload classifies when a changed setting takes effect. The daemon enforces
// these classes (docs/configuration.md documents them per option):
//
//   - ReloadLive: applied the moment the change is saved, even mid-session.
//   - ReloadIdle: applied on save when no session is in flight; with one
//     active, the file is written and the change applies on the next
//     config.reload (or daemon restart).
//   - ReloadRestart: the file is written, but the value is wired up at daemon
//     construction (activation chord watcher, tool registry, logger), so only
//     a daemon restart picks it up.
type Reload string

// Reload classes.
const (
	ReloadLive    Reload = "live"
	ReloadIdle    Reload = "idle"
	ReloadRestart Reload = "restart"
)

// SettingType is the wire type of a setting's value. Clients may send the
// native JSON type or a string; Coerce accepts both, so the CLI can pass raw
// argv strings and the settings screen can pass typed values.
type SettingType string

// Setting value types.
const (
	TypeString     SettingType = "string"
	TypeInt        SettingType = "int"
	TypeFloat      SettingType = "float"
	TypeBool       SettingType = "bool"
	TypeStringList SettingType = "string_list"
	// TypeStringMap is a table of string → string, written as its own TOML
	// table ([tts.lexicon]). Clients may send a JSON object or the CLI's
	// "key=value,key=value" form.
	TypeStringMap SettingType = "string_map"
)

// Setting describes one editable configuration option: where it lives in the
// TOML document (Key, dotted), how to read and write it on a Config, and how
// the daemon applies a change (Reload). The registry is the single source of
// truth shared by the config.get/config.set IPC methods, the CLI, and the
// TOML rewrite — anything not listed here stays hand-edit-only.
type Setting struct {
	// Key is the dotted TOML path, e.g. "tts.piper.voice".
	Key string
	// Label is the human name shown by the settings screen.
	Label string
	// Type is the value type used for coercion and client rendering.
	Type SettingType
	// Reload is when a change to this setting takes effect.
	Reload Reload
	// Enum lists the allowed values for closed sets; empty means free-form.
	// Enforcement stays in Config.Validate so its messages remain the single
	// vocabulary for validation errors.
	Enum []string

	// Get reads the current value.
	Get func(Config) any
	// set writes an already-coerced native value. Use Apply.
	set func(*Config, any)
}

// Coerce converts a client-supplied value (native JSON decoding or a raw
// string) into the setting's native type.
func (s Setting) Coerce(v any) (any, error) {
	switch s.Type {
	case TypeString:
		return coerceString(v)
	case TypeInt:
		return coerceInt(v)
	case TypeFloat:
		return coerceFloat(v)
	case TypeBool:
		return coerceBool(v)
	case TypeStringList:
		return coerceStringList(v)
	case TypeStringMap:
		return coerceStringMap(v)
	}
	return nil, fmt.Errorf("%s: unknown setting type %q", s.Key, s.Type)
}

// Apply coerces v and writes it onto cfg. Validation is not performed here:
// callers validate the whole document afterwards with Config.Validate, so
// error messages come from one place.
func (s Setting) Apply(cfg *Config, v any) error {
	nv, err := s.Coerce(v)
	if err != nil {
		return err
	}
	s.set(cfg, nv)
	return nil
}

// Settings returns every editable setting in display order. Endpoint tables
// ([ai.<name>]) and secrets are deliberately absent: endpoints are hand-edited
// TOML, and API keys never travel through the settings surface at all.
func Settings() []Setting {
	return []Setting{
		{Key: "ai.provider", Label: "AI provider", Type: TypeString, Reload: ReloadIdle,
			Get: func(c Config) any { return c.AI.Provider },
			set: func(c *Config, v any) { c.AI.Provider = v.(string) }},
		{Key: "ai.model", Label: "AI model", Type: TypeString, Reload: ReloadIdle,
			Get: func(c Config) any { return c.AI.Model },
			set: func(c *Config, v any) { c.AI.Model = v.(string) }},
		{Key: "ai.system_prompt", Label: "System prompt", Type: TypeString, Reload: ReloadIdle,
			Get: func(c Config) any { return c.AI.SystemPrompt },
			set: func(c *Config, v any) { c.AI.SystemPrompt = v.(string) }},
		{Key: "ai.max_tokens", Label: "Max response tokens", Type: TypeInt, Reload: ReloadIdle,
			Get: func(c Config) any { return c.AI.MaxTokens },
			set: func(c *Config, v any) { c.AI.MaxTokens = v.(int) }},
		{Key: "ai.temperature", Label: "Temperature", Type: TypeFloat, Reload: ReloadIdle,
			Get: func(c Config) any { return c.AI.Temperature },
			set: func(c *Config, v any) { c.AI.Temperature = v.(float64) }},

		{Key: "tts.provider", Label: "Voice engine", Type: TypeString, Reload: ReloadIdle,
			Enum: []string{"piper", "kokoro"},
			Get:  func(c Config) any { return c.TTS.Provider },
			set:  func(c *Config, v any) { c.TTS.Provider = v.(string) }},
		{Key: "tts.piper.voice", Label: "Piper voice", Type: TypeString, Reload: ReloadIdle,
			Get: func(c Config) any { return c.TTS.Piper.Voice },
			set: func(c *Config, v any) { c.TTS.Piper.Voice = v.(string) }},
		{Key: "tts.piper.binary", Label: "Piper binary", Type: TypeString, Reload: ReloadIdle,
			Get: func(c Config) any { return c.TTS.Piper.Binary },
			set: func(c *Config, v any) { c.TTS.Piper.Binary = v.(string) }},
		{Key: "tts.kokoro.voice", Label: "Kokoro voice", Type: TypeString, Reload: ReloadIdle,
			Get: func(c Config) any { return c.TTS.Kokoro.Voice },
			set: func(c *Config, v any) { c.TTS.Kokoro.Voice = v.(string) }},
		{Key: "tts.kokoro.speed", Label: "Kokoro speed", Type: TypeFloat, Reload: ReloadIdle,
			Get: func(c Config) any { return c.TTS.Kokoro.Speed },
			set: func(c *Config, v any) { c.TTS.Kokoro.Speed = v.(float64) }},
		// Idle class, like the rest of the voice: the normalizer that holds
		// the lexicon is rebuilt with the engine's collaborators, so a fix to
		// a mispronounced word is spoken on the next answer — no restart.
		// Get copies, and never returns nil, so the daemon's "did this change"
		// comparison sees a value rather than the map the config is holding.
		{Key: "tts.lexicon", Label: "Pronunciation lexicon", Type: TypeStringMap, Reload: ReloadIdle,
			Get: func(c Config) any {
				out := make(map[string]string, len(c.TTS.Lexicon))
				for term, spoken := range c.TTS.Lexicon {
					out[term] = spoken
				}
				return out
			},
			set: func(c *Config, v any) { c.TTS.Lexicon = v.(map[string]string) }},

		{Key: "stt.whisper.model", Label: "Whisper model", Type: TypeString, Reload: ReloadIdle,
			Get: func(c Config) any { return c.STT.Whisper.Model },
			set: func(c *Config, v any) { c.STT.Whisper.Model = v.(string) }},
		{Key: "stt.whisper.binary", Label: "Whisper binary", Type: TypeString, Reload: ReloadIdle,
			Get: func(c Config) any { return c.STT.Whisper.Binary },
			set: func(c *Config, v any) { c.STT.Whisper.Binary = v.(string) }},
		{Key: "stt.whisper.language", Label: "Speech language", Type: TypeString, Reload: ReloadIdle,
			Get: func(c Config) any { return c.STT.Whisper.Language },
			set: func(c *Config, v any) { c.STT.Whisper.Language = v.(string) }},

		{Key: "activation.ptt_chord", Label: "Push-to-talk chord", Type: TypeStringList, Reload: ReloadRestart,
			Get: func(c Config) any { return append([]string(nil), c.Activation.PTTChord...) },
			set: func(c *Config, v any) { c.Activation.PTTChord = v.([]string) }},

		{Key: "conversation.speak_responses", Label: "Speak responses aloud", Type: TypeBool, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Conversation.SpeakResponses },
			set: func(c *Config, v any) { c.Conversation.SpeakResponses = v.(bool) }},
		{Key: "conversation.history_turns", Label: "Remembered turns", Type: TypeInt, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Conversation.HistoryTurns },
			set: func(c *Config, v any) { c.Conversation.HistoryTurns = v.(int) }},
		{Key: "conversation.follow_up_window_sec", Label: "Follow-up window (seconds)", Type: TypeInt, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Conversation.FollowUpWindowSec },
			set: func(c *Config, v any) { c.Conversation.FollowUpWindowSec = v.(int) }},

		// The intent table itself is rebuilt with the engine, so these are
		// idle-class. [[intents.custom]] entries stay hand-edited TOML — like
		// [ai.<name>] endpoints, they are structured tables rather than single
		// values — and land on the next idle-class reload or restart.
		{Key: "intents.enabled", Label: "Deterministic intents", Type: TypeBool, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Intents.Enabled },
			set: func(c *Config, v any) { c.Intents.Enabled = v.(bool) }},
		{Key: "intents.terminal", Label: "Terminal for \"open terminal\"", Type: TypeString, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Intents.Terminal },
			set: func(c *Config, v any) { c.Intents.Terminal = v.(string) }},

		// Desktop context belongs in the registry because these are the
		// switches a user reaches for *in the moment* — "don't look at my
		// clipboard right now" must not require editing a file and restarting
		// a daemon. They are idle-class: the collector is rebuilt with the
		// engine's options, so the change lands between sessions, never
		// underneath one that is already gathering.
		{Key: "context.window", Label: "See the active window", Type: TypeBool, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Context.Window },
			set: func(c *Config, v any) { c.Context.Window = v.(bool) }},
		{Key: "context.selection", Label: "See selected text", Type: TypeBool, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Context.Selection },
			set: func(c *Config, v any) { c.Context.Selection = v.(bool) }},
		{Key: "context.clipboard", Label: "See the clipboard", Type: TypeBool, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Context.Clipboard },
			set: func(c *Config, v any) { c.Context.Clipboard = v.(bool) }},
		{Key: "context.max_chars", Label: "Context characters per source", Type: TypeInt, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Context.MaxChars },
			set: func(c *Config, v any) { c.Context.MaxChars = v.(int) }},
		{Key: "context.timeout_ms", Label: "Context gathering budget (milliseconds)", Type: TypeInt, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Context.TimeoutMs },
			set: func(c *Config, v any) { c.Context.TimeoutMs = v.(int) }},

		{Key: "audio.input_device", Label: "Microphone device", Type: TypeString, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Audio.InputDevice },
			set: func(c *Config, v any) { c.Audio.InputDevice = v.(string) }},
		{Key: "audio.output_device", Label: "Output device", Type: TypeString, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Audio.OutputDevice },
			set: func(c *Config, v any) { c.Audio.OutputDevice = v.(string) }},
		{Key: "audio.max_recording_sec", Label: "Max recording (seconds)", Type: TypeInt, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Audio.MaxRecordingSec },
			set: func(c *Config, v any) { c.Audio.MaxRecordingSec = v.(int) }},
		{Key: "audio.min_recording_ms", Label: "Min recording (milliseconds)", Type: TypeInt, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Audio.MinRecordingMs },
			set: func(c *Config, v any) { c.Audio.MinRecordingMs = v.(int) }},

		// Idle class: the warm workers live inside the STT/TTS adapters, and
		// adapters are only ever swapped between sessions (engine.Reconfigure).
		{Key: "performance.warm_engines", Label: "Keep engines warm", Type: TypeBool, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Performance.WarmEngines },
			set: func(c *Config, v any) { c.Performance.WarmEngines = v.(bool) }},
		{Key: "performance.warm_memory_cap_mb", Label: "Warm worker memory cap (MB)", Type: TypeInt, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Performance.WarmMemoryCapMB },
			set: func(c *Config, v any) { c.Performance.WarmMemoryCapMB = v.(int) }},
		{Key: "performance.warm_idle_reap_sec", Label: "Reap warm workers after (seconds)", Type: TypeInt, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Performance.WarmIdleReapSec },
			set: func(c *Config, v any) { c.Performance.WarmIdleReapSec = v.(int) }},

		{Key: "ui.notifications", Label: "Desktop notifications", Type: TypeBool, Reload: ReloadLive,
			Get: func(c Config) any { return c.UI.Notifications },
			set: func(c *Config, v any) { c.UI.Notifications = v.(bool) }},
		{Key: "ui.notification_preview", Label: "Answer preview in notifications", Type: TypeBool, Reload: ReloadLive,
			Get: func(c Config) any { return c.UI.NotificationPreview },
			set: func(c *Config, v any) { c.UI.NotificationPreview = v.(bool) }},
		{Key: "ui.show_transcript", Label: "Show transcript in overlay", Type: TypeBool, Reload: ReloadLive,
			Get: func(c Config) any { return c.UI.ShowTranscript },
			set: func(c *Config, v any) { c.UI.ShowTranscript = v.(bool) }},
		{Key: "ui.show_response", Label: "Show response in overlay", Type: TypeBool, Reload: ReloadLive,
			Get: func(c Config) any { return c.UI.ShowResponse },
			set: func(c *Config, v any) { c.UI.ShowResponse = v.(bool) }},

		{Key: "tools.shell", Label: "Assistant may run shell commands", Type: TypeBool, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Tools.Shell },
			set: func(c *Config, v any) { c.Tools.Shell = v.(bool) }},
		{Key: "tools.shell_timeout_sec", Label: "Shell command timeout (seconds)", Type: TypeInt, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Tools.ShellTimeoutSec },
			set: func(c *Config, v any) { c.Tools.ShellTimeoutSec = v.(int) }},
		{Key: "tools.shell_max_output_kb", Label: "Shell output cap (KB)", Type: TypeInt, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Tools.ShellMaxOutputKB },
			set: func(c *Config, v any) { c.Tools.ShellMaxOutputKB = v.(int) }},
		{Key: "tools.artifacts", Label: "Assistant may create artifacts", Type: TypeBool, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Tools.Artifacts },
			set: func(c *Config, v any) { c.Tools.Artifacts = v.(bool) }},

		{Key: "artifacts.dir", Label: "Artifact directory", Type: TypeString, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Artifacts.Dir },
			set: func(c *Config, v any) { c.Artifacts.Dir = v.(string) }},
		// A list, not a string: the viewer is exec'd directly, so a path or
		// argument containing a space has to stay its own argv element. The
		// CLI's comma form ("flatpak,run,org.libreoffice.LibreOffice") is how
		// that is spelled from a shell.
		{Key: "artifacts.open_command", Label: "Artifact viewer command", Type: TypeStringList, Reload: ReloadRestart,
			Get: func(c Config) any { return append([]string(nil), c.Artifacts.OpenCommand...) },
			set: func(c *Config, v any) { c.Artifacts.OpenCommand = Command(v.([]string)) }},
		{Key: "artifacts.render_timeout_sec", Label: "Artifact render timeout (seconds)", Type: TypeInt, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Artifacts.RenderTimeoutSec },
			set: func(c *Config, v any) { c.Artifacts.RenderTimeoutSec = v.(int) }},

		{Key: "log.level", Label: "Log level", Type: TypeString, Reload: ReloadRestart,
			Enum: []string{"debug", "info", "warn", "error"},
			Get:  func(c Config) any { return c.Log.Level },
			set:  func(c *Config, v any) { c.Log.Level = v.(string) }},
	}
}

// SettingFor looks a setting up by its dotted key.
func SettingFor(key string) (Setting, bool) {
	for _, s := range Settings() {
		if s.Key == key {
			return s, true
		}
	}
	return Setting{}, false
}

// ---------------------------------------------------------------- coercion

func coerceString(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	return "", fmt.Errorf("expected a string, got %T", v)
}

func coerceInt(v any) (int, error) {
	switch t := v.(type) {
	case int:
		return t, nil
	case float64: // JSON numbers decode as float64
		if t != float64(int(t)) {
			return 0, fmt.Errorf("expected a whole number, got %v", t)
		}
		return int(t), nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, fmt.Errorf("expected a whole number, got %q", t)
		}
		return n, nil
	}
	return 0, fmt.Errorf("expected a whole number, got %T", v)
}

func coerceFloat(v any) (float64, error) {
	switch t := v.(type) {
	case float64:
		return t, nil
	case int:
		return float64(t), nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, fmt.Errorf("expected a number, got %q", t)
		}
		return f, nil
	}
	return 0, fmt.Errorf("expected a number, got %T", v)
}

func coerceBool(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "on", "yes":
			return true, nil
		case "false", "off", "no":
			return false, nil
		}
		return false, fmt.Errorf("expected true or false, got %q", t)
	}
	return false, fmt.Errorf("expected true or false, got %T", v)
}

// coerceStringList accepts a JSON array of strings or a single
// comma-separated string ("leftmeta,leftalt,v" — the CLI form). An empty
// string means an empty list, so a chord can be cleared from the CLI.
func coerceStringList(v any) ([]string, error) {
	switch t := v.(type) {
	case []string:
		return t, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("expected a list of strings, got a %T element", e)
			}
			out = append(out, s)
		}
		return out, nil
	case string:
		if strings.TrimSpace(t) == "" {
			return []string{}, nil
		}
		parts := strings.Split(t, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			out = append(out, strings.TrimSpace(p))
		}
		return out, nil
	}
	return nil, fmt.Errorf("expected a list of strings, got %T", v)
}

// coerceStringMap accepts a JSON object of strings or the CLI's comma form
// ("Golang=go lang,nginx=engine ex"). An empty string means an empty table,
// so a lexicon can be cleared from the CLI. The returned map is never nil:
// "no entries" is a value the rewrite and the change-detection can compare.
func coerceStringMap(v any) (map[string]string, error) {
	switch t := v.(type) {
	case map[string]string:
		if t == nil {
			return map[string]string{}, nil
		}
		return t, nil
	case map[string]any:
		out := make(map[string]string, len(t))
		for key, raw := range t {
			s, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("expected string values, got a %T for %q", raw, key)
			}
			out[key] = s
		}
		return out, nil
	case string:
		out := map[string]string{}
		if strings.TrimSpace(t) == "" {
			return out, nil
		}
		for _, pair := range strings.Split(t, ",") {
			key, value, ok := strings.Cut(pair, "=")
			if !ok {
				return nil, fmt.Errorf("expected key=value pairs, got %q", pair)
			}
			key = strings.TrimSpace(key)
			if key == "" {
				return nil, fmt.Errorf("expected key=value pairs, got %q", pair)
			}
			out[key] = strings.TrimSpace(value)
		}
		return out, nil
	}
	return nil, fmt.Errorf("expected a table of strings, got %T", v)
}
