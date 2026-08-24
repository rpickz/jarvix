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
	"github.com/rpickz/jarvix/internal/voice"
)

// Config is the root configuration document.
type Config struct {
	// Assistant is the assistant's identity (issue #103): the name it
	// answers to and the spellings transcripts mishear it as. The STT bias
	// sentence, the wake-transcript strip, the wake detector's word, and
	// the default prompt's self-reference all derive from this one table
	// (see assistant.go).
	Assistant    Assistant    `toml:"assistant"`
	Activation   Activation   `toml:"activation"`
	AI           AI           `toml:"ai"`
	STT          STT          `toml:"stt"`
	TTS          TTS          `toml:"tts"`
	Conversation Conversation `toml:"conversation"`
	Intents      Intents      `toml:"intents"`
	// Routines are the named app-placement sequences ([[routines]], ADR
	// 0026), triggered through the intent router and executed by
	// internal/routine.
	Routines []Routine `toml:"routines"`
	// Scripts are the user-authored executables behind spoken phrases
	// ([[scripts]], ADR 0030), triggered through the intent router, gated
	// under script.run, and executed by internal/script with zero arguments.
	Scripts   []Script  `toml:"scripts"`
	Tools     Tools     `toml:"tools"`
	Artifacts Artifacts `toml:"artifacts"`
	// Context is what Jarvix may look at on the desktop before answering
	// (see context.go). Every source is opt-in; the clipboard defaults off.
	Context Context `toml:"context"`
	// Memory is the knowledge base — facts the user explicitly asks Jarvix
	// to remember, consulted on every model turn (see memory.go, ADR 0025).
	Memory Memory `toml:"memory"`
	// Knowledge is the feed section — user-configured fetchers whose latest
	// value the daemon keeps warm so changing facts answer instantly (see
	// knowledge.go, ADR 0031). Empty feeds disable the feature.
	Knowledge Knowledge `toml:"knowledge"`
	// Advisors are the assistant CLIs Jarvix may delegate a question to, one
	// [advisors.<name>] table each (see advisors.go). Empty disables
	// delegation entirely — the tool is not registered.
	Advisors    map[string]Advisor `toml:"advisors"`
	Audio       Audio              `toml:"audio"`
	Performance Performance        `toml:"performance"`
	UI          UI                 `toml:"ui"`
	Log         Log                `toml:"log"`

	// Voices enumerates the voices the machine actually has installed, for
	// the configured TTS engine. It is not configuration — it is the state of
	// the machine — so it never appears in the TOML document, and it is a
	// field rather than a package-level lookup for two reasons. Validation
	// must be able to say "that voice is not installed, try these" (see
	// voice.go), which needs the real list; and no test may be made to depend
	// on a 27 MB archive being present, which needs the fake.
	//
	// Nil is the safe default and means "do not object to the voice": the
	// daemon and the CLI attach the real catalog at their entry points, tests
	// attach voice.Fake, and everything else validates the rest of the
	// document exactly as before.
	Voices voice.Catalog `toml:"-"`
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
	Shell            bool `toml:"shell"`               // enable shell.run
	ShellTimeoutSec  int  `toml:"shell_timeout_sec"`   // per-command timeout
	ShellMaxOutputKB int  `toml:"shell_max_output_kb"` // captured output cap
	Artifacts        bool `toml:"artifacts"`           // enable artifact.create
	// Desktop enables the desktop.* window tools: list, focus, move, close,
	// launch. On by default — unlike shell.run, each verb is one bounded
	// action on a window the compositor named, every one of them is visible
	// on screen and undoable by hand, and none can enter data anywhere. The
	// state-changing verbs still pass the gate, which asks.
	Desktop bool `toml:"desktop"`
	// DesktopApps restricts what desktop.launch_app may start, as bare
	// program names or absolute paths. Empty means anything installed on
	// PATH: launching an application the user already has is not an
	// escalation, and the launch verb confirms first. Set it to pin the
	// assistant to a shortlist.
	DesktopApps []string `toml:"desktop_apps"`
	// Typing enables the typing.* tools. Off by default, the same way
	// shell.run is — see Typing.
	Typing Typing      `toml:"typing"`
	Policy ToolsPolicy `toml:"policy"`
}

// Typing configures synthetic keystrokes: the typing.type_text and
// typing.press_key tools (ADR 0023).
//
// It is off by default and stays off until the user says otherwise, because it
// is the most powerful thing Jarvix can be given. Everything else the
// assistant does either answers a question or performs one bounded, visible
// action on a named object. Keystrokes are neither bounded nor named: they go
// wherever focus is at that instant, they can be characters or a command, and
// a wrong one is not undoable by looking somewhere else.
//
// The other fields are the blast-radius controls. They are configuration
// rather than constants because the right numbers depend on what the machine
// is used for, and because a user who wants Jarvix to fill in forms all day
// should be able to say so without editing Go.
type Typing struct {
	// Enable turns the typing tools on. Default false.
	Enable bool `toml:"enable"`
	// MaxChars caps one payload. A runaway loop must not be able to fill a
	// document before anyone reaches the keyboard.
	MaxChars int `toml:"max_chars"`
	// RateLimit is how many typing actions may happen inside
	// RateWindowSec before further ones are refused with a reason.
	RateLimit int `toml:"rate_limit"`
	// RateWindowSec is the rate limiter's window, in seconds.
	RateWindowSec int `toml:"rate_window_sec"`
	// TerminalClasses are the window classes whose contents are a command
	// line. Typing into one always asks first, however the policy tier is
	// otherwise set. Empty uses the shipped list (alacritty, kitty, foot,
	// ghostty, …); set it to add a terminal Jarvix would not recognise.
	TerminalClasses []string `toml:"terminal_classes"`
	// Binary overrides the keystroke injector. Empty means "wtype" from PATH.
	Binary string `toml:"binary"`
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
	// DiagramFormat is what a Mermaid diagram renders to: "png" (the
	// default) or "svg". PNG is the default because mermaid's SVG carries
	// its labels as HTML in <foreignObject>, which image viewers do not
	// render — the diagram opens as boxes with no text (#56). "svg" is for
	// users who want markup to edit or embed; that path renders with
	// htmlLabels disabled so labels become real <text>, though some shapes
	// still emit foreignObject.
	DiagramFormat string `toml:"diagram_format"`
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
	// Mode is "push_to_talk" or "wake_word". The two are not alternatives in
	// the way the name suggests: "wake_word" *adds* background listening, and
	// the chord keeps working exactly as before (ADR 0024). A user who has
	// enabled hands-free activation has not given up their keyboard.
	Mode string `toml:"mode"`
	// PTTChord is the hold-to-talk key chord watched by the daemon (evdev
	// key names, e.g. ["leftmeta","leftalt","v"]). Requires read access to
	// input devices (jarvix doctor explains how to grant it); without
	// access, the Hyprland tap-to-toggle binding is the fallback. Empty
	// disables the daemon-side watcher.
	PTTChord []string `toml:"ptt_chord"`

	// WakeWord overrides what is handed to the wake detector — a bundled
	// model word or a path to a model you trained yourself. Empty (the
	// default) derives the assistant's name, lowercased (see
	// Config.WakeDetectorWord). It exists because the detector's vocabulary
	// is whatever models exist, not whatever names people choose: keeping
	// the chosen name everywhere while pointing the detector at the nearest
	// real model is the documented custom-name setup. Only the detector
	// reads it; the bias prompt, the transcript strip, and the system
	// prompt follow [assistant] (issue #103).
	WakeWord string `toml:"wake_word"`
	// WakeCommand is the detector helper's argv (ADR 0002: engines are
	// external processes). Missing or unrunnable degrades to push-to-talk
	// with one warning; `jarvix doctor` explains the fix.
	WakeCommand []string `toml:"wake_command"`
	// WakeSensitivity is 0..1, higher being more eager. It maps onto the
	// detector's score threshold; the default 0.5 is the threshold every
	// openWakeWord example uses, so published guidance transfers directly.
	WakeSensitivity float64 `toml:"wake_sensitivity"`
	// EndpointSilenceMs is how long a lull ends a hands-free request. With no
	// key to release, this is the only "I have finished" signal there is.
	EndpointSilenceMs int `toml:"endpoint_silence_ms"`
	// WakeRingMs is how much audio is held *before* the wake word, so the
	// first syllables of a request are not lost. It is the only ambient audio
	// that can ever reach a transcript, so it is deliberately short by
	// default and hard-capped at 3000 ms — a limit the wake listener enforces
	// as well, because a privacy boundary should not depend on validation
	// having been run.
	WakeRingMs int `toml:"wake_ring_ms"`
	// MaxUtteranceSec bounds one hands-free request.
	MaxUtteranceSec int `toml:"max_utterance_sec"`
	// WakeAliases moved to [assistant] aliases (issue #103). The field is
	// kept decode-only so a configuration still setting it is *refused with
	// directions* (wakeProblems) instead of silently reverting to the
	// shipped list — a strip that quietly stopped accepting a tuned alias
	// would look exactly like the mishearing bug it existed to fix.
	//
	// Deprecated: set [assistant] aliases instead.
	WakeAliases []string `toml:"wake_aliases"`
}

// WakeWordEnabled reports whether background listening is configured.
func (a Activation) WakeWordEnabled() bool { return a.Mode == ModeWakeWord }

// Activation modes.
const (
	// ModePushToTalk is the keyboard-only default.
	ModePushToTalk = "push_to_talk"
	// ModeWakeWord adds always-on background listening for the wake word.
	ModeWakeWord = "wake_word"
)

// MaxWakeRingMs is the hard ceiling on pre-wake retention, in milliseconds.
// It is a privacy limit rather than a performance one, and it is stated here
// as well as in internal/wake so a user reading the configuration reference
// sees the same number the code enforces.
const MaxWakeRingMs = 3000

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
	// Vocabulary lists extra terms the recogniser is biased toward — project
	// names, jargon, anything whisper keeps rounding to a nearby real word.
	// They join the assistant's name in the bias prompt both transcription
	// paths carry (issue #83). Input-side only: this is what the *user* says, where
	// tts.lexicon respells what *Jarvix* says — different vocabularies on
	// purpose.
	Vocabulary []string `toml:"vocabulary"`
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
	// Lexicon respells words the voice mispronounces: term → spoken form,
	// written as a [tts.lexicon] table. These are the user's entries only;
	// Jarvix merges them over its own shipped defaults (Golang, Kubernetes,
	// nginx, …), so an entry here either adds a word or overrides a default.
	// Terms match case-insensitively on word boundaries, and the change is
	// spoken-only — the overlay always shows the original text.
	Lexicon map[string]string `toml:"lexicon"`
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
	// Retention archives every conversation to disk until the user deletes it
	// (ADR 0027): "on" (the default) keeps whole conversations — untouched by
	// history_turns, which only governs what the model is sent — and "off"
	// stops all archive writing without removing anything already kept.
	Retention string `toml:"retention"`
}

// Retention values. Strings rather than a bool so the file reads as the
// decision it is ("retention = \"off\"" is unambiguous where "retention =
// false" invites guessing what exactly stops), and so a later mode — such as
// time-bounded retention — is a new value, not a new key.
const (
	RetentionOn  = "on"
	RetentionOff = "off"
)

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
	// ActivityRows bounds the daemon-side activity ring (issue #70): how many
	// rendered rows of recent activity `activity.get` can return. In-memory
	// only — activity never touches disk; conversations are the durable
	// record.
	ActivityRows int `toml:"activity_rows"`
	// ActivityClearOnNew empties the activity feed when the conversation is
	// reset (`jarvix new`). Off by default: activity is operational history,
	// and "what did it just do?" is usually asked *after* starting fresh.
	ActivityClearOnNew bool `toml:"activity_clear_on_new"`
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
		// The identity ships with no aliases *stored*: the tuned mishearing
		// list is derived while the name is the default one (see
		// Assistant.EffectiveAliases), so choosing a new name never inherits
		// another name's mishearings.
		Assistant: Assistant{Name: defaultAssistantName},
		Activation: Activation{
			// Push-to-talk stays the default. Background listening is a
			// microphone that is open when nobody asked it to be, and that is
			// a decision for the user to make deliberately, not one to
			// inherit from a default (ADR 0024).
			Mode:     ModePushToTalk,
			PTTChord: []string{"leftmeta", "leftalt", "v"},
			// The wake settings carry working values regardless, so switching
			// the mode on is one edit rather than six. The detector's word is
			// not among them: it derives from assistant.name unless
			// wake_word overrides it.
			WakeCommand:       []string{"jarvix-wake"},
			WakeSensitivity:   0.5,
			EndpointSilenceMs: 800,
			WakeRingMs:        1200,
			MaxUtteranceSec:   15,
		},
		AI: AI{
			Provider:     "ollama",
			Model:        "llama3.2:3b",
			SystemPrompt: DefaultSystemPrompt(defaultAssistantName),
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
		Conversation: Conversation{SpeakResponses: true, HistoryTurns: 16, FollowUpWindowSec: 900,
			Retention: RetentionOn},
		Intents: Intents{Enabled: true, Terminal: intent.DefaultTerminal},
		Tools: Tools{
			Shell: false, ShellTimeoutSec: 30, ShellMaxOutputKB: 16, Artifacts: true,
			Desktop: true,
			// Off, like shell.run: a capability this powerful is opted into,
			// never inherited from a default (ADR 0023).
			Typing: Typing{Enable: false, MaxChars: 500, RateLimit: 6, RateWindowSec: 60},
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
			DiagramFormat:    "png",
		},
		// Window and selection are on: a title bar is already on screen, and a
		// selection is what the user is pointing at as they speak. The
		// clipboard is off — it holds whatever was last copied for any
		// purpose, and turning that on is the user's decision to make.
		Context: Context{
			Window: true, Selection: true, Clipboard: false,
			MaxChars: 2000, TimeoutMs: MaxContextTimeoutMs,
		},
		// On by default: nothing enters the store without the user saying
		// "remember ..." explicitly, so the trust decision is made per fact,
		// not per install (ADR 0025).
		Memory: Memory{Enabled: true, MaxFacts: 200, MaxInjectedTokens: 500},
		// No feeds by default: a feed runs a command on a schedule, and that
		// is a decision the user makes by writing the command (ADR 0031).
		Knowledge: Knowledge{MaxInjectedTokens: DefaultKnowledgeInjectedTokens},
		Audio:     Audio{MaxRecordingSec: 60, MinRecordingMs: 300},
		// Warm by default: presence is the product, and the memory is
		// reclaimed after ten idle minutes. The cap is a leak detector, not a
		// working budget — whisper base.en plus Kokoro sit well under it.
		Performance: Performance{WarmEngines: true, WarmMemoryCapMB: 2048, WarmIdleReapSec: 600},
		UI: UI{ShowTranscript: true, ShowResponse: true, Notifications: true, NotificationPreview: true,
			// 400 rows is several sessions of tool-heavy work at a few
			// kilobytes total: enough to answer "what happened earlier?",
			// small enough to never matter.
			ActivityRows: 400},
		Log: Log{Level: "info"},
	}
}

// The assistant's shipped identity. These are deliberately the ONLY places
// the default name and its tuned mishearing list are written down — every
// call-site that hears, strips, or speaks the name derives from the
// [assistant] table (assistant.go), and the grep-guard test in this package
// fails if a copy creeps back into one of them.
const defaultAssistantName = "Jarvix"

// defaultAssistantAliases returns the mishearings whisper's English models
// actually produce for the default name (observed with base.en, issue #83):
// the transcript strip accepts any of them as the summons. A fresh slice per
// call, so no caller can mutate the shipped tuning for the rest of a process.
func defaultAssistantAliases() []string {
	return []string{"jarvis", "javax", "jarvic", "jarvicks", "jarvex"}
}

// ToolSystemPrompt is appended to the system prompt when tools are enabled.
// It tells the model to act on its own rather than instruct the user, and to
// keep spoken answers about command output brief. It also states the cardinal
// rule of the tool loop — an action exists only as a tool call — because the
// failure it forbids (narrating "opening it now" while calling nothing) is a
// choice the model makes before any tool description is read (issue #71).
// The rule's wording is pinned by TestToolSystemPromptPinsTheHonestyRule.
const ToolSystemPrompt = " You can run shell commands yourself with the shell.run tool to answer " +
	"questions about the computer's live state and to carry out tasks. When the user asks what is " +
	"happening with something (Docker, git, processes, disk, services), run the appropriate command " +
	"and summarise the result — do not tell the user which command to run, run it. Prefer read-only " +
	"commands. Commands that change the system trigger a built-in spoken confirmation from the user " +
	"before they run; if a tool result says the command was declined or not permitted, do not retry " +
	"it — acknowledge and move on. Summarise command output for speech: report what matters, not " +
	"raw tables. The cardinal rule: an action only happens when you make the tool call that " +
	"performs it, in this turn. Never describe an action as done or underway unless you are " +
	"making that call; if you did not call the tool, the action did not happen — say plainly " +
	"that you have not done it."

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

// DesktopSystemPrompt is appended to the system prompt when the window tools
// are enabled. Two behaviours have to be stated here rather than left to the
// tool descriptions, because both are judgements the model makes before it
// calls anything: that "put me back in the browser" is a tool call rather than
// a sentence about keyboard shortcuts, and that an ambiguous reference is a
// question to the user rather than a guess.
const DesktopSystemPrompt = " You can act on the user's desktop: list their open windows, focus " +
	"one, move one to another workspace, close one, and start an application. When they ask you " +
	"to go somewhere (\"put me back in my browser\", \"switch to the terminal\") or to open, move " +
	"or close something, do it with those tools instead of telling them how. Describe the window " +
	"the way they did and let the tool find it; if it reports several matches, ask which one they " +
	"meant rather than choosing. Never read window identifiers, workspace internals, or raw tool " +
	"output aloud — say what happened in one short sentence."

// MemorySystemPrompt is appended to the system prompt when the knowledge
// base is enabled (ADR 0025). The trust boundary is stated here, once, as
// model behaviour: memory is written only on the user's explicit word.
// Everything mechanical — supersede candidates, ids, cap warnings — is
// carried by the tool descriptions and results instead.
const MemorySystemPrompt = " You have a long-term memory of facts, injected above as remembered " +
	"facts when any exist. Store a fact with memory.remember only when the user explicitly asks " +
	"you to remember something — never decide on your own that something is worth keeping. When " +
	"the user corrects a remembered fact, update the existing fact rather than adding a new one. " +
	"When they ask what you know or remember, answer from the remembered facts or memory.recall, " +
	"in plain words. When they ask you to forget something, use memory.forget. After remembering " +
	"or forgetting, confirm what changed in one short sentence."

// ConfigSystemPrompt is appended to the system prompt for the always-present
// self-configuration tools (issue #105, ADR 0036). Deliberately terse — the
// prompt pays for every turn (the context-floor check measures it) — so it
// carries only the judgements the model makes *before* any tool description
// is read: that "remind yourself…"/"talk faster" are tool calls, that an edit
// starts with a read, that validation problems come back to be fixed, and
// that the confirmation states what actually changed. The off-limits sentence
// is here, not just in the wall that enforces it, so the model refuses in
// words instead of discovering the refusal by calling.
// TestConfigSystemPromptPinsTheContract pins the wording.
const ConfigSystemPrompt = " You can change your own configuration when the user asks: create, " +
	"edit, or remove routines, scripts, and knowledge feeds with the config entry tools, and " +
	"change settings with config.write_setting. Before editing an entry, read it with " +
	"config.get_entry and send back the whole edited entry. The daemon validates every draft " +
	"and returns field-keyed problems: fix exactly what they name and retry, or tell the user " +
	"the real problem — never claim a change you did not make. The tool permission policy, " +
	"advisors, and AI provider settings are off limits to you; say so if asked. After a change, " +
	"confirm in one short sentence what actually changed, using the values the tool result " +
	"reports — never a paraphrase of the request."

// minWarmMemoryCapMB is the smallest cap that can hold any engine Jarvix keeps
// warm (whisper base.en alone is ~165 MB resident). A cap below it would
// retire the worker the moment it loaded its model, turning warm mode into a
// restart loop — so it is rejected rather than silently obeyed.
const minWarmMemoryCapMB = 256

// maxActivityRows caps ui.activity_rows. The feed is bounded in rows *and*
// per row (internal/desktop caps label and detail), so this ceiling is what
// keeps the worst-case ring at a few megabytes rather than a memory setting
// the user has to reason about.
const maxActivityRows = 10000

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
	// [[knowledge.feeds]] decodes straight into the slice; each table is
	// filled from the feed defaults (mode, interval, ttl, timeout).
	applyKnowledgeDefaults(&cfg)
	return cfg, nil
}

// Validate checks the configuration for problems a user must fix, returning
// actionable messages.
func (c Config) Validate() error {
	var problems []string

	if c.Activation.Mode != ModePushToTalk && c.Activation.Mode != ModeWakeWord {
		problems = append(problems, fmt.Sprintf(
			"activation.mode %q is not supported; use %q or %q",
			c.Activation.Mode, ModePushToTalk, ModeWakeWord))
	}
	if len(c.Activation.PTTChord) > 0 {
		if _, err := hotkey.ResolveChord(c.Activation.PTTChord); err != nil {
			problems = append(problems, err.Error())
		}
	}
	problems = append(problems, c.assistantProblems()...)
	problems = append(problems, c.wakeProblems()...)
	problems = append(problems, c.sttProblems()...)
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
	if c.Conversation.Retention != RetentionOn && c.Conversation.Retention != RetentionOff {
		problems = append(problems, fmt.Sprintf(
			"conversation.retention %q is invalid; use %q (archive conversations until deleted) or %q",
			c.Conversation.Retention, RetentionOn, RetentionOff))
	}
	if c.UI.ActivityRows <= 0 {
		problems = append(problems,
			"ui.activity_rows must be positive (rows of recent activity the daemon keeps in memory)")
	} else if c.UI.ActivityRows > maxActivityRows {
		problems = append(problems, fmt.Sprintf(
			"ui.activity_rows is %d; the activity feed is a glanceable ring, not a log — use at most %d",
			c.UI.ActivityRows, maxActivityRows))
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
	if c.Artifacts.DiagramFormat != "png" && c.Artifacts.DiagramFormat != "svg" {
		problems = append(problems, fmt.Sprintf(
			"artifacts.diagram_format %q is invalid; use \"png\" (opens correctly in image viewers) or \"svg\"",
			c.Artifacts.DiagramFormat))
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
	// An unusable launch allow list is worth naming now rather than as a
	// refusal the user only hears when they ask for an application: an empty
	// entry allows nothing, and a relative path is neither a program name nor
	// a location.
	for _, app := range c.Tools.DesktopApps {
		switch {
		case strings.TrimSpace(app) == "":
			problems = append(problems,
				"tools.desktop_apps contains an empty entry; each one must be a program name (\"firefox\") or an absolute path")
		case strings.ContainsAny(app, " \t"):
			problems = append(problems, fmt.Sprintf(
				"tools.desktop_apps entry %q contains whitespace; applications are launched directly, not through a shell, so each entry is one program", app))
		case strings.Contains(app, "/") && !filepath.IsAbs(app):
			problems = append(problems, fmt.Sprintf(
				"tools.desktop_apps entry %q must be a program name or an absolute path (\"~\" is not expanded)", app))
		}
	}
	problems = append(problems, c.typingProblems()...)
	problems = append(problems, c.validateAdvisors()...)
	problems = append(problems, c.intentProblems()...)
	problems = append(problems, c.routineProblems()...)
	problems = append(problems, c.scriptProblems()...)
	problems = append(problems, c.contextProblems()...)
	problems = append(problems, c.memoryProblems()...)
	problems = append(problems, c.knowledgeProblems()...)
	problems = append(problems, c.voiceProblems()...)
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
