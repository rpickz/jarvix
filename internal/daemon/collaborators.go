package daemon

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/rpickz/jarvix/internal/ai/openaicompat"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/memory"
	"github.com/rpickz/jarvix/internal/routine"
	"github.com/rpickz/jarvix/internal/script"
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

// assistantSystemPrompt is the system prompt the engine runs with. The
// composition lives in config.AssistantSystemPrompt so doctor's context-floor
// check (issue #71) measures the same prompt the daemon sends, from one copy.
func assistantSystemPrompt(cfg config.Config) string {
	return config.AssistantSystemPrompt(cfg)
}

// routineRunner builds the routine runner (ADR 0026), or nil when nothing is
// configured. The explicit nil matters for the same reason contextCollector's
// does: a typed-nil *routine.Runner in the interface field would read as
// "routines exist" to the engine. Rebuilt on config reload alongside the
// intent router, and around the same shared compositor, so the probed
// dispatch dialect is discovered once for intents, window tools, and
// routines alike.
func routineRunner(cfg config.Config, compositor desktop.Compositor, bus *session.Bus,
	logger *slog.Logger) session.RoutineRunner {
	defs := cfg.RoutineDefinitions()
	if len(defs) == 0 {
		return nil
	}
	return routine.New(routine.Options{
		Compositor:  compositor,
		Definitions: defs,
		Log:         logger,
		// routine.started / routine.step / routine.finished go out on the bus
		// so the bar and the window can show progress; the user only ever
		// *hears* the final summary.
		Publish: func(event string, data map[string]any) {
			bus.Publish(session.Event{Type: event, Data: data})
		},
	})
}

// scriptRunner builds the script runner (ADR 0030), or nil when nothing is
// configured. The explicit nil matters for the same reason routineRunner's
// does: a typed-nil *script.Runner in the interface field would read as
// "scripts exist" to the engine. Rebuilt on config reload alongside the
// intent router, so a hand-edited [[scripts]] entry and the phrase that
// triggers it always come from the same file read.
func scriptRunner(cfg config.Config, bus *session.Bus, logger *slog.Logger) session.ScriptRunner {
	defs := cfg.ScriptDefinitions()
	if len(defs) == 0 {
		return nil
	}
	return script.New(script.Options{
		Definitions: defs,
		Log:         logger,
		// script.started / script.finished go out on the bus so the activity
		// feed records every run with its exit status and duration; the user
		// only ever *hears* the configured report.
		Publish: func(event string, data map[string]any) {
			bus.Publish(session.Event{Type: event, Data: data})
		},
	})
}

// engineOptions maps configuration onto engine options, shared by New and by
// config reloads so both always agree on the translation. The bus is here for
// the routine runner, which publishes its progress events through it. book is
// the daemon's knowledge base (ADR 0025), nil when memory is disabled — it is
// a parameter rather than rebuilt from cfg because the store is
// construction-wired (restart-class) and must stay the same instance the
// memory tools write through. bus carries the routine progress events
// (ADR 0026). archive is the durable conversation store (ADR 0027), a
// parameter for the same reason as book: one instance serves the engine's
// appends and the conversation.* IPC methods, and only the retention switch
// here decides whether the engine writes to it.
func engineOptions(cfg config.Config, compositor desktop.Compositor, bus *session.Bus,
	book *memory.Book, archive conversations.Store, logger *slog.Logger) session.Options {
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
		Routines:          routineRunner(cfg, compositor, bus, logger),
		Scripts:           scriptRunner(cfg, bus, logger),
		Compositor:        compositor,
		Context:           contextCollector(cfg, logger),
		Memory:            memoryInjector(book),
		Archive:           conversationArchive(cfg, archive),
		WakeWord:          cfg.Activation.WakeWord,
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

// memoryInjector adapts the knowledge base for the engine, or leaves the
// option nil when memory is disabled. Same typed-nil trap as
// contextCollector: assigning a nil *memory.Book into the interface field
// would leave the engine consulting "nothing" on every turn — disabled must
// mean absent.
func memoryInjector(book *memory.Book) session.MemoryInjector {
	if book == nil {
		return nil
	}
	return book
}

// conversationArchive gates the durable archive (ADR 0027) on the retention
// switch: "off" hands the engine nil, which means absent — nothing staged,
// nothing written — while the daemon keeps its own handle for listing and
// deletion. Same typed-nil discipline as the collaborators above: disabled
// must mean a nil interface, never an interface holding a nil pointer.
func conversationArchive(cfg config.Config, archive conversations.Store) conversations.Store {
	if cfg.Conversation.Retention == config.RetentionOff {
		return nil
	}
	return archive
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
