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
	// Dangerous marks a setting whose change widens what the assistant may do
	// on this machine or re-points something the daemon executes. The
	// assistant's own settings tool (issue #105, ADR 0036) always confirms a
	// dangerous write out loud, whatever the [tools] policy says — no
	// configuration can make these land silently. Set from
	// dangerousSettingKey in Settings(), never per-row, so the rule that
	// decides is written once and pinned by one test.
	Dangerous bool

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
	list := settingRows()
	for i := range list {
		list[i].Dangerous = dangerousSettingKey(list[i].Key)
	}
	return list
}

func settingRows() []Setting {
	return []Setting{
		// The assistant's identity (issue #103) leads the list: the name is
		// the most personal knob there is, and the STT bias, the transcript
		// strip, the detector's word, and the default prompt all follow it.
		// Restart class because the wake detector is construction-wired and
		// its word derives from the name: everything else about a rename
		// (bias, strip, prompt) reloads idle, and applying those while the
		// detector still listened under the old word would be a half-rename
		// that answers to neither name reliably.
		{Key: "assistant.name", Label: "Assistant name", Type: TypeString, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Assistant.Name },
			set: func(c *Config, v any) { c.Assistant.Name = v.(string) }},
		// Idle class, unlike the name, and deliberately so: aliases live only
		// in the engine's transcript strip, which is rebuilt with the
		// engine's options — the wake listener and its supervised children
		// never see them, so nothing needs a restart. The settings screen's
		// generic string_list widget renders this as a comma-separated field
		// and the daemon parses that form (Setting.Coerce); Get resolves the
		// *effective* list so the shipped mishearings are visible rather
		// than an implicit blank.
		{Key: "assistant.aliases", Label: "Name aliases (spellings transcripts mishear it as)", Type: TypeStringList, Reload: ReloadIdle,
			Get: func(c Config) any { return append([]string(nil), c.Assistant.EffectiveAliases()...) },
			set: func(c *Config, v any) { c.Assistant.Aliases = v.([]string) }},

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
		// Idle class like the rest of [stt]: the transcriber that carries the
		// bias prompt is rebuilt with the engine's collaborators, so a new term
		// is heard on the next question — no restart.
		{Key: "stt.vocabulary", Label: "Recognition vocabulary", Type: TypeStringList, Reload: ReloadIdle,
			Get: func(c Config) any { return append([]string(nil), c.STT.Vocabulary...) },
			set: func(c *Config, v any) { c.STT.Vocabulary = v.([]string) }},

		{Key: "activation.ptt_chord", Label: "Push-to-talk chord", Type: TypeStringList, Reload: ReloadRestart,
			Get: func(c Config) any { return append([]string(nil), c.Activation.PTTChord...) },
			set: func(c *Config, v any) { c.Activation.PTTChord = v.([]string) }},

		// Background listening (ADR 0024). Restart class, all of it, for the
		// same reason the chord is: the wake listener and its two supervised
		// children are wired at daemon construction, and a microphone is not
		// something to hot-swap underneath itself. The knob that *is* live is
		// `jarvix mute`, which is the one a user reaches for in the moment.
		{Key: "activation.mode", Label: "Activation", Type: TypeString, Reload: ReloadRestart,
			Enum: []string{ModePushToTalk, ModeWakeWord},
			Get:  func(c Config) any { return c.Activation.Mode },
			set:  func(c *Config, v any) { c.Activation.Mode = v.(string) }},
		// Empty (the default) derives the detector's word from the
		// assistant's name; set it to a bundled model word or a model path
		// when no acoustic model exists for the chosen name (issue #103).
		{Key: "activation.wake_word", Label: "Wake detector word or model (empty: the assistant's name)", Type: TypeString, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Activation.WakeWord },
			set: func(c *Config, v any) { c.Activation.WakeWord = v.(string) }},
		{Key: "activation.wake_command", Label: "Wake detector command", Type: TypeStringList, Reload: ReloadRestart,
			Get: func(c Config) any { return append([]string(nil), c.Activation.WakeCommand...) },
			set: func(c *Config, v any) { c.Activation.WakeCommand = v.([]string) }},
		{Key: "activation.wake_sensitivity", Label: "Wake sensitivity", Type: TypeFloat, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Activation.WakeSensitivity },
			set: func(c *Config, v any) { c.Activation.WakeSensitivity = v.(float64) }},
		{Key: "activation.endpoint_silence_ms", Label: "Submit after silence (milliseconds)", Type: TypeInt, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Activation.EndpointSilenceMs },
			set: func(c *Config, v any) { c.Activation.EndpointSilenceMs = v.(int) }},
		{Key: "activation.wake_ring_ms", Label: "Audio kept before the wake word (milliseconds)", Type: TypeInt, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Activation.WakeRingMs },
			set: func(c *Config, v any) { c.Activation.WakeRingMs = v.(int) }},
		{Key: "activation.max_utterance_sec", Label: "Longest hands-free request (seconds)", Type: TypeInt, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Activation.MaxUtteranceSec },
			set: func(c *Config, v any) { c.Activation.MaxUtteranceSec = v.(int) }},
		{Key: "conversation.speak_responses", Label: "Speak responses aloud", Type: TypeBool, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Conversation.SpeakResponses },
			set: func(c *Config, v any) { c.Conversation.SpeakResponses = v.(bool) }},
		{Key: "conversation.history_turns", Label: "Remembered turns", Type: TypeInt, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Conversation.HistoryTurns },
			set: func(c *Config, v any) { c.Conversation.HistoryTurns = v.(int) }},
		{Key: "conversation.follow_up_window_sec", Label: "Follow-up window (seconds)", Type: TypeInt, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Conversation.FollowUpWindowSec },
			set: func(c *Config, v any) { c.Conversation.FollowUpWindowSec = v.(int) }},
		// Retention is the archive's off switch (ADR 0027): a privacy control
		// the user reaches for in the moment, so it belongs in the registry
		// beside the context switches, not behind a hand edit and a restart.
		// Idle-class: the engine's archive option is rebuilt with its options,
		// and turning it off stops writing without touching what is kept.
		{Key: "conversation.retention", Label: "Keep conversations until deleted", Type: TypeString, Reload: ReloadIdle,
			Enum: []string{RetentionOn, RetentionOff},
			Get:  func(c Config) any { return c.Conversation.Retention },
			set:  func(c *Config, v any) { c.Conversation.Retention = v.(string) }},
		// The permission gate's audio knob (issue #119): off speaks the short
		// prompt ("May I run a shell command? The details are on screen."), on
		// restores the full read-out that quotes the command. In the registry —
		// not hand-edit-only like [tools.policy] — because it changes what is
		// said, never what is allowed, so the settings screen and the
		// assistant's own settings tool (#109) may both flip it. Idle-class:
		// the engine's options are rebuilt between sessions, and swapping the
		// prompt's wording underneath a question already being asked would be
		// worse than answering the next one in the new style.
		{Key: "confirmations.speak_details", Label: "Read the full command aloud when asking permission", Type: TypeBool, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Confirmations.SpeakDetails },
			set: func(c *Config, v any) { c.Confirmations.SpeakDetails = v.(bool) }},

		// The intent table itself is rebuilt with the engine, so these are
		// idle-class. [[intents.custom]], [[routines]], [[scripts]] and
		// [[knowledge.feeds]] entries stay hand-edited TOML — like
		// [ai.<name>] endpoints, they are structured tables rather than
		// single values — and land on the next idle-class reload or restart.
		// Routines, scripts and feeds are listed read-only through the
		// `routines.list` / `scripts.list` / `knowledge.status` IPC methods
		// (v1 lists, never edits — for scripts that is also a control: no IPC
		// client can repoint a phrase at a different file).
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

		// The knowledge base (ADR 0025) is restart-class like the rest of
		// the tool registry: the store and the memory tools are wired at
		// daemon construction, and a half-applied toggle — injection off but
		// tools still registered — would be worse than an honest "restart to
		// finish".
		{Key: "memory.enabled", Label: "Remember facts the user asks Jarvix to keep", Type: TypeBool, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Memory.Enabled },
			set: func(c *Config, v any) { c.Memory.Enabled = v.(bool) }},
		{Key: "memory.max_facts", Label: "Remembered facts the store may hold", Type: TypeInt, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Memory.MaxFacts },
			set: func(c *Config, v any) { c.Memory.MaxFacts = v.(int) }},
		{Key: "memory.max_injected_tokens", Label: "Token budget for remembered facts per turn", Type: TypeInt, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Memory.MaxInjectedTokens },
			set: func(c *Config, v any) { c.Memory.MaxInjectedTokens = v.(int) }},

		// Live class: the focus service reads the switch at fire time
		// (ADR 0041), so a save lands on the very next timebox moment.
		{Key: "focus.midpoint_checkin", Label: "Speak a halfway check-in during focus sessions", Type: TypeBool, Reload: ReloadLive,
			Get: func(c Config) any { return c.Focus.MidpointCheckin },
			set: func(c *Config, v any) { c.Focus.MidpointCheckin = v.(bool) }},

		// The taught vocabulary (issue #129) is restart-class like memory and
		// for the same reason: the store and the vocabulary tools are wired at
		// daemon construction. speak_back is the exception — it only selects
		// the injection block's stance sentence, which is composed through the
		// engine's collaborators, so it lands on an idle reload like the other
		// prompt-shaping settings. Its default is false on purpose: the
		// vocabulary exists so Jarvix *understands* the user, and mirrored
		// slang from a machine reads as mockery more often than rapport
		// (recorded in the ADR); using the user's words back is theirs to
		// invite, not Jarvix's to assume.
		{Key: "vocabulary.enabled", Label: "Learn words and phrases the user teaches", Type: TypeBool, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Vocabulary.Enabled },
			set: func(c *Config, v any) { c.Vocabulary.Enabled = v.(bool) }},
		{Key: "vocabulary.max_entries", Label: "Taught phrases the store may hold", Type: TypeInt, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Vocabulary.MaxEntries },
			set: func(c *Config, v any) { c.Vocabulary.MaxEntries = v.(int) }},
		{Key: "vocabulary.max_injected_tokens", Label: "Token budget for taught words per turn", Type: TypeInt, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Vocabulary.MaxInjectedTokens },
			set: func(c *Config, v any) { c.Vocabulary.MaxInjectedTokens = v.(int) }},
		{Key: "vocabulary.speak_back", Label: "Use taught words in replies", Type: TypeBool, Reload: ReloadIdle,
			Get: func(c Config) any { return c.Vocabulary.SpeakBack },
			set: func(c *Config, v any) { c.Vocabulary.SpeakBack = v.(bool) }},

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
		{Key: "ui.activity_rows", Label: "Activity rows kept in memory", Type: TypeInt, Reload: ReloadLive,
			Get: func(c Config) any { return c.UI.ActivityRows },
			set: func(c *Config, v any) { c.UI.ActivityRows = v.(int) }},
		{Key: "ui.activity_clear_on_new", Label: "Clear activity on a new conversation", Type: TypeBool, Reload: ReloadLive,
			Get: func(c Config) any { return c.UI.ActivityClearOnNew },
			set: func(c *Config, v any) { c.UI.ActivityClearOnNew = v.(bool) }},
		// Reading comfort (issue #121): the transcript's typography is
		// personal — for a dyslexic or ADHD reader, line spacing and text
		// density decide whether an answer is readable at a glance or a wall
		// to bounce off. Three knobs and no more (a wall of typography
		// toggles is its own accessibility failure), all relative units so
		// they ride the shell's font scale, all scoped to transcript messages
		// — window chrome, tabs, and cards keep the design system's scale.
		// Live class like the rest of [ui]: these are display-only values the
		// conversation window reads (nothing in the engine consumes them),
		// and "increase the line spacing a bit" spoken through the settings
		// tool mid-session must land on the transcript being looked at, not
		// after the session ends. Bounds live in Config.Validate beside the
		// other [ui] checks, so an out-of-range value is refused with the
		// standard field problem.
		{Key: "ui.line_spacing", Label: "Line spacing (× line height; extra room helps dyslexic readers keep their place)", Type: TypeFloat, Reload: ReloadLive,
			Get: func(c Config) any { return c.UI.LineSpacing },
			set: func(c *Config, v any) { c.UI.LineSpacing = v.(float64) }},
		{Key: "ui.text_size", Label: "Message text size (× the design size; larger turns a wall of text back into lines)", Type: TypeFloat, Reload: ReloadLive,
			Get: func(c Config) any { return c.UI.TextSize },
			set: func(c *Config, v any) { c.UI.TextSize = v.(float64) }},
		{Key: "ui.letter_spacing", Label: "Letter spacing (ems between letters; a little air stops letters crowding)", Type: TypeFloat, Reload: ReloadLive,
			Get: func(c Config) any { return c.UI.LetterSpacing },
			set: func(c *Config, v any) { c.UI.LetterSpacing = v.(float64) }},

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
		{Key: "tools.desktop", Label: "Assistant may move windows", Type: TypeBool, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Tools.Desktop },
			set: func(c *Config, v any) { c.Tools.Desktop = v.(bool) }},
		// A list, not a string: each entry is one program, launched directly
		// rather than through a shell. The CLI's comma form
		// ("firefox,alacritty") is how that is spelled from a shell.
		{Key: "tools.desktop_apps", Label: "Applications the assistant may start", Type: TypeStringList, Reload: ReloadRestart,
			Get: func(c Config) any { return append([]string(nil), c.Tools.DesktopApps...) },
			set: func(c *Config, v any) { c.Tools.DesktopApps = v.([]string) }},

		// Typing (ADR 0023). Editable from the settings screen so the switch is
		// where a user would look for it — and so turning it *off* is one
		// click, which is the direction that has to be easy.
		{Key: "tools.typing.enable", Label: "Assistant may type on your keyboard", Type: TypeBool, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Tools.Typing.Enable },
			set: func(c *Config, v any) { c.Tools.Typing.Enable = v.(bool) }},
		{Key: "tools.typing.max_chars", Label: "Longest text the assistant may type", Type: TypeInt, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Tools.Typing.MaxChars },
			set: func(c *Config, v any) { c.Tools.Typing.MaxChars = v.(int) }},
		{Key: "tools.typing.rate_limit", Label: "Typing actions allowed per window", Type: TypeInt, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Tools.Typing.RateLimit },
			set: func(c *Config, v any) { c.Tools.Typing.RateLimit = v.(int) }},
		{Key: "tools.typing.rate_window_sec", Label: "Typing rate-limit window (seconds)", Type: TypeInt, Reload: ReloadRestart,
			Get: func(c Config) any { return c.Tools.Typing.RateWindowSec },
			set: func(c *Config, v any) { c.Tools.Typing.RateWindowSec = v.(int) }},
		// A list, not a string: each entry is one window class. The CLI's comma
		// form ("alacritty,kitty") is how that is spelled from a shell.
		{Key: "tools.typing.terminal_classes", Label: "Window classes treated as terminals", Type: TypeStringList, Reload: ReloadRestart,
			Get: func(c Config) any { return append([]string(nil), c.Tools.Typing.TerminalClasses...) },
			set: func(c *Config, v any) { c.Tools.Typing.TerminalClasses = v.([]string) }},

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
		{Key: "artifacts.diagram_format", Label: "Diagram output format", Type: TypeString, Reload: ReloadRestart,
			Enum: []string{"png", "svg"},
			Get:  func(c Config) any { return c.Artifacts.DiagramFormat },
			set:  func(c *Config, v any) { c.Artifacts.DiagramFormat = v.(string) }},

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

// dangerousSettingKey is the rule behind Setting.Dangerous (issue #105, ADR
// 0036): which registry keys the assistant must always confirm before
// writing, however permissive the [tools] policy is. Three groups, each for a
// stated reason:
//
//   - Everything under [tools] (tools.*): these switches enable, widen, or
//     re-parameterise what the assistant itself may do — shell, typing,
//     window control, launchable apps, output caps. A prefix rather than a
//     key list on purpose: a future tools.* key that forgot to classify
//     itself must land on the always-confirm side, never the silent one.
//     (tools.policy.* is not here because it is not a registry key at all —
//     the whole [tools.policy] table is structurally unreachable, see
//     AssistantExcludedSettingReason.)
//   - activation.mode: flips the microphone model between push-to-talk and
//     always-listening — the most consequential privacy switch there is.
//   - The command- and binary-bearing keys: activation.wake_command,
//     artifacts.open_command, tts.piper.binary, stt.whisper.binary, and
//     intents.terminal each name a program the daemon will execute, so a
//     silent write here is a silent change to what runs on this machine —
//     the shell.run discipline (the exact value on the card, spoken consent)
//     applies at authoring time too.
//
// TestDangerousSettingsAreExactlyTheEnumeratedSet pins the resulting set, so
// widening or narrowing it is a reviewed decision, not a drive-by.
func dangerousSettingKey(key string) bool {
	if strings.HasPrefix(key, "tools.") {
		return true
	}
	switch key {
	case "activation.mode",
		"activation.wake_command",
		"artifacts.open_command",
		"tts.piper.binary",
		"stt.whisper.binary",
		"intents.terminal":
		return true
	}
	return false
}

// AssistantExcludedSettingReason is the settings half of the assistant's
// exclusion wall (issue #105, ADR 0036): the configuration the assistant's
// tools must not be able to address AT ALL — not deny-by-default but
// structurally, whatever [tools] says, because each of these is part of the
// gate (or the mind) that judges the assistant's own actions, and a gate
// must not be able to loosen itself:
//
//   - [ai] — provider, model, system prompt, token/temperature knobs, and
//     every [ai.<endpoint>] table: the assistant steering its own brain, or
//     its credentials, on request is the one write no confirmation can make
//     safe, because the thing being confirmed is the judge of later asks.
//   - [tools.policy] — the permission gate's own tables.
//   - [advisors] — advisor argvs are commands the daemon executes with the
//     user's credentials, and their tiers feed the gate (ADR 0016).
//   - [[intents.custom]] — user-authored shell commands behind spoken
//     phrases (ADR 0017); the intent.run identity's whole premise is that
//     the user wrote them by hand.
//
// The reason returned is spoken-ready: it becomes the refusal the user hears
// and the model reads. ok false means the key is merely unknown or ordinary —
// unknown keys come back as correctable errors, never as this wall.
func AssistantExcludedSettingReason(key string) (string, bool) {
	k := strings.TrimSpace(key)
	switch {
	case k == "ai" || strings.HasPrefix(k, "ai."):
		return "the assistant may not change its own AI provider, model, or system prompt; " +
			"[ai] settings are changed in the settings screen or config.toml", true
	case k == "tools.policy" || strings.HasPrefix(k, "tools.policy."):
		return "the assistant may not change the tool permission policy that governs it; " +
			"[tools.policy] is edited by hand in config.toml", true
	case k == "advisors" || strings.HasPrefix(k, "advisors."):
		return "the assistant may not change advisor configuration; " +
			"[advisors] is edited by hand in config.toml", true
	case k == "intents.custom" || strings.HasPrefix(k, "intents.custom."):
		return "the assistant may not change custom voice intents; " +
			"[[intents.custom]] is edited by hand in config.toml", true
	}
	return "", false
}

// AssistantSettings is the registry as the assistant's tools see it: every
// editable setting minus the excluded space above. This is the *structural*
// half of the wall — the settings tool resolves keys against this view and
// nothing else, so an excluded key is not a denied write, it is a key that
// does not exist on the surface the tool operates on.
func AssistantSettings() []Setting {
	all := Settings()
	out := make([]Setting, 0, len(all))
	for _, s := range all {
		if _, excluded := AssistantExcludedSettingReason(s.Key); excluded {
			continue
		}
		out = append(out, s)
	}
	return out
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
