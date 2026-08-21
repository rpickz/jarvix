package daemon

import (
	"fmt"
	"time"

	"github.com/rpickz/jarvix/internal/ai/openaicompat"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/stt/whispercpp"
	"github.com/rpickz/jarvix/internal/tts/kokoro"
	"github.com/rpickz/jarvix/internal/tts/piper"
)

// fillDeps builds every engine collaborator the caller did not inject,
// straight from configuration. It is shared by construction (New) and by
// config reloads (settings.go): a reload rebuilds exactly what New would
// have built, while injected collaborators — test fakes — survive reloads
// untouched, because a fake cannot be rebuilt from config.
func fillDeps(cfg config.Config, paths config.Paths, deps Deps) (Deps, error) {
	if deps.Provider == nil {
		ep, ok := cfg.Endpoint()
		if !ok {
			return deps, fmt.Errorf("no endpoint for ai.provider %q", cfg.AI.Provider)
		}
		deps.Provider = openaicompat.New(cfg.AI.Provider, ep.BaseURL, ep.Key())
	}
	if deps.Transcriber == nil {
		deps.Transcriber = &whispercpp.Transcriber{
			Binary:    cfg.STT.Whisper.Binary,
			ModelPath: whispercpp.ResolveModelPath(cfg.STT.Whisper.Model, paths.WhisperModelDir()),
			Language:  cfg.STT.Whisper.Language,
		}
	}
	if deps.Synthesizer == nil {
		switch cfg.TTS.Provider {
		case "kokoro":
			deps.Synthesizer = &kokoro.Synthesizer{
				Voice: cfg.TTS.Kokoro.Voice,
				Speed: cfg.TTS.Kokoro.Speed,
			}
		default:
			deps.Synthesizer = &piper.Synthesizer{
				Binary: cfg.TTS.Piper.Binary,
				Voice:  cfg.TTS.Piper.Voice,
			}
		}
	}
	if deps.Recorder == nil {
		deps.Recorder = &audio.PipeWireRecorder{
			Dir:         paths.Runtime,
			Device:      cfg.Audio.InputDevice,
			MaxDuration: time.Duration(cfg.Audio.MaxRecordingSec) * time.Second,
		}
	}
	if deps.Player == nil {
		deps.Player = &audio.PipeWirePlayer{Device: cfg.Audio.OutputDevice}
	}
	return deps, nil
}

// assistantSystemPrompt is the system prompt the engine runs with: the
// configured base plus the instructions for each enabled tool. The tool
// flags decide the suffixes because the tool registry is built from them —
// on a reload the running (booted) tool flags are what matter, not the file.
func assistantSystemPrompt(cfg config.Config) string {
	prompt := cfg.AI.SystemPrompt
	if cfg.Tools.Shell {
		prompt += config.ToolSystemPrompt
	}
	if cfg.Tools.Artifacts {
		prompt += config.ArtifactSystemPrompt
	}
	if len(cfg.Advisors) > 0 {
		prompt += config.AdvisorSystemPrompt
	}
	return prompt
}

// engineOptions maps configuration onto engine options, shared by New and by
// config reloads so both always agree on the translation.
func engineOptions(cfg config.Config) session.Options {
	return session.Options{
		Model:             cfg.AI.Model,
		SystemPrompt:      assistantSystemPrompt(cfg),
		MaxTokens:         cfg.AI.MaxTokens,
		Temperature:       cfg.AI.Temperature,
		SpeakResponses:    cfg.Conversation.SpeakResponses,
		MinRecording:      time.Duration(cfg.Audio.MinRecordingMs) * time.Millisecond,
		HistoryTurns:      cfg.Conversation.HistoryTurns,
		FollowUpWindow:    time.Duration(cfg.Conversation.FollowUpWindowSec) * time.Second,
		ConfirmTimeout:    time.Duration(cfg.Tools.Policy.ConfirmTimeoutSec) * time.Second,
		RememberApprovals: cfg.Tools.Policy.RememberForConversation,
		Intents:           intentRouter(cfg),
	}
}

// intentRouter compiles the deterministic intent table (ADR 0017). Nil means
// no routing at all — every transcript reaches the assistant. A compile error
// cannot happen for a validated configuration, because Config.Validate
// compiles this very table; treating one as "disabled" keeps a live reload
// from ever handing the engine a half-built router.
func intentRouter(cfg config.Config) *intent.Router {
	if !cfg.Intents.Enabled {
		return nil
	}
	router, err := intent.New(cfg.IntentOptions())
	if err != nil {
		return nil
	}
	return router
}
