package daemon

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/rpickz/jarvix/internal/ai/openaicompat"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/stt/whispercpp"
	"github.com/rpickz/jarvix/internal/tts/kokoro"
	"github.com/rpickz/jarvix/internal/tts/piper"
	"github.com/rpickz/jarvix/internal/warm"
)

// warmWorkers are the supervised engine processes one configuration built.
//
// They are tracked separately from the adapters that own them because their
// lifetime is the daemon's, not a session's: something has to kill them when
// jarvixd exits and when a config reload replaces the adapters, or the first
// reload of the day leaves a whisper-server and a Python interpreter running
// with nobody holding their handles (ADR 0018).
type warmWorkers struct {
	// engines are the adapters that keep a warm child; each can report its
	// status and shut its child down.
	engines []warmEngine
}

// warmEngine is the surface a warm adapter exposes to the daemon: report, and
// shut down.
type warmEngine interface {
	WarmStatus() warm.Status
	Close() error
}

// Close kills every warm child this configuration started. Safe on the zero
// value, which is what a fully injected (test) daemon has.
func (w warmWorkers) Close() {
	for _, e := range w.engines {
		_ = e.Close()
	}
}

// Status reports every warm worker, in build order, for doctor and status.get.
func (w warmWorkers) Status() []warm.Status {
	out := make([]warm.Status, 0, len(w.engines))
	for _, e := range w.engines {
		out = append(out, e.WarmStatus())
	}
	return out
}

// fillDeps builds every engine collaborator the caller did not inject,
// straight from configuration. It is shared by construction (New) and by
// config reloads (settings.go): a reload rebuilds exactly what New would
// have built, while injected collaborators — test fakes — survive reloads
// untouched, because a fake cannot be rebuilt from config.
//
// It also returns the warm workers it created, so the caller can shut them
// down when this configuration stops being the running one. Nothing is spawned
// here: a supervisor starts its child on first use, so building deps stays
// free and a daemon that is never spoken to never loads a model.
func fillDeps(cfg config.Config, paths config.Paths, deps Deps, log *slog.Logger) (Deps, warmWorkers, error) {
	var workers warmWorkers
	if log == nil {
		log = slog.Default()
	}
	memCap := uint64(cfg.Performance.WarmMemoryCapMB) << 20
	idle := time.Duration(cfg.Performance.WarmIdleReapSec) * time.Second

	if deps.Provider == nil {
		ep, ok := cfg.Endpoint()
		if !ok {
			return deps, workers, fmt.Errorf("no endpoint for ai.provider %q", cfg.AI.Provider)
		}
		deps.Provider = openaicompat.New(cfg.AI.Provider, ep.BaseURL, ep.Key())
	}
	if deps.Transcriber == nil {
		cold := &whispercpp.Transcriber{
			Binary:    cfg.STT.Whisper.Binary,
			ModelPath: whispercpp.ResolveModelPath(cfg.STT.Whisper.Model, paths.WhisperModelDir()),
			Language:  cfg.STT.Whisper.Language,
		}
		if cfg.Performance.WarmEngines {
			server := &whispercpp.ServerTranscriber{
				// whisper.cpp ships the server next to the CLI; the config
				// names the CLI, so the server is derived from it rather than
				// adding a second binary path nobody would set.
				Binary:    whispercpp.ServerBinaryFor(cfg.STT.Whisper.Binary),
				ModelPath: cold.ModelPath,
				Language:  cold.Language,
				Cold:      cold,
				MemoryCap: memCap,
				IdleAfter: idle,
				Log:       log,
			}
			deps.Transcriber = server
			workers.engines = append(workers.engines, server)
		} else {
			deps.Transcriber = cold
		}
	}
	if deps.Synthesizer == nil {
		switch cfg.TTS.Provider {
		case "kokoro":
			cold := &kokoro.Synthesizer{
				Voice: cfg.TTS.Kokoro.Voice,
				Speed: cfg.TTS.Kokoro.Speed,
			}
			if cfg.Performance.WarmEngines {
				w := &kokoro.WarmSynthesizer{
					Cold: cold, MemoryCap: memCap, IdleAfter: idle, Log: log,
				}
				deps.Synthesizer = w
				workers.engines = append(workers.engines, w)
			} else {
				deps.Synthesizer = cold
			}
		default:
			cold := &piper.Synthesizer{
				Binary: cfg.TTS.Piper.Binary,
				Voice:  cfg.TTS.Piper.Voice,
			}
			if cfg.Performance.WarmEngines {
				w := &piper.WarmSynthesizer{
					Cold: cold, Dir: paths.Runtime,
					MemoryCap: memCap, IdleAfter: idle, Log: log,
				}
				deps.Synthesizer = w
				workers.engines = append(workers.engines, w)
			} else {
				deps.Synthesizer = cold
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
	return deps, workers, nil
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
	if cfg.Tools.Desktop {
		prompt += config.DesktopSystemPrompt
	}
	if len(cfg.Advisors) > 0 {
		prompt += config.AdvisorSystemPrompt
	}
	return prompt
}

// engineOptions maps configuration onto engine options, shared by New and by
// config reloads so both always agree on the translation.
func engineOptions(cfg config.Config, logger *slog.Logger) session.Options {
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
		Context:           contextCollector(cfg, logger),
		Lexicon:           cfg.TTS.Lexicon,
	}
}

// contextCollector builds the desktop-context collector (ADR 0019), or leaves
// the option nil when every source is switched off.
//
// The explicit nil return matters more than it looks: assigning a typed-nil
// *desktop.Collector into the interface field would leave the engine holding
// a non-nil interface and gathering "nothing" on every turn. Disabled must
// mean absent.
func contextCollector(cfg config.Config, logger *slog.Logger) session.ContextCollector {
	c := desktop.NewCollector(desktop.Options{
		Window:    cfg.Context.Window,
		Selection: cfg.Context.Selection,
		Clipboard: cfg.Context.Clipboard,
		MaxChars:  cfg.Context.MaxChars,
		Timeout:   time.Duration(cfg.Context.TimeoutMs) * time.Millisecond,
	}, logger)
	if c == nil {
		return nil
	}
	return c
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
