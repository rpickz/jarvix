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
	"github.com/rpickz/jarvix/internal/knowledge"
	"github.com/rpickz/jarvix/internal/memory"
	"github.com/rpickz/jarvix/internal/monitors"
	"github.com/rpickz/jarvix/internal/routine"
	"github.com/rpickz/jarvix/internal/script"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/stt/whispercpp"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts/kokoro"
	"github.com/rpickz/jarvix/internal/tts/piper"
	"github.com/rpickz/jarvix/internal/vocabulary"
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
func fillDeps(cfg config.Config, paths config.Paths, deps Deps, vocab *vocabulary.Store,
	log *slog.Logger) (Deps, warmWorkers, error) {
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
		// One prompt composition, shared by both paths: the warm request and
		// the cold fallback must bias identically or a fallback would change
		// what Jarvix's own name transcribes as (issue #83). Read through a
		// function rather than composed once, because the taught hard-to-hear
		// phrases (issue #129) live in a store, not in config: "listen for
		// the word X" must bias the very next utterance, and the store read
		// behind this closure is one stat(2) of a file in the page cache.
		biasPrompt := func() string { return cfg.STTBiasPrompt() }
		if vocab != nil {
			biasPrompt = func() string { return cfg.STTBiasPromptWith(vocab.HardToHear()) }
		}
		cold := &whispercpp.Transcriber{
			Binary:     cfg.STT.Whisper.Binary,
			ModelPath:  whispercpp.ResolveModelPath(cfg.STT.Whisper.Model, paths.WhisperModelDir()),
			Language:   cfg.STT.Whisper.Language,
			PromptFunc: biasPrompt,
		}
		if cfg.Performance.WarmEngines {
			server := &whispercpp.ServerTranscriber{
				// whisper.cpp ships the server next to the CLI; the config
				// names the CLI, so the server is derived from it rather than
				// adding a second binary path nobody would set.
				Binary:     whispercpp.ServerBinaryFor(cfg.STT.Whisper.Binary),
				ModelPath:  cold.ModelPath,
				Language:   cold.Language,
				PromptFunc: biasPrompt,
				Cold:       cold,
				MemoryCap:  memCap,
				IdleAfter:  idle,
				Log:        log,
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
		// Log wired so a playback restart — pw-play dying under a live answer
		// and being respawned on the current default sink (issue #142) — is a
		// journal line, not a silence the user has to diagnose.
		deps.Player = &audio.PipeWirePlayer{Device: cfg.Audio.OutputDevice, Log: log}
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
	screens *monitors.Store, logger *slog.Logger) session.RoutineRunner {
	defs := cfg.RoutineDefinitions()
	if len(defs) == 0 {
		return nil
	}
	return routine.New(routine.Options{
		Compositor:  compositor,
		Definitions: defs,
		Log:         logger,
		// The terminal a `Terminal=true` desktop entry opens inside (#194):
		// the same one "open a terminal" and desktop.launch_app use, because
		// the user named their terminal once.
		Terminal: cfg.Intents.Terminal,
		// The screen names a step may use (#180). Handed over as the store's
		// live lookup rather than a snapshot, so a nickname assigned by voice
		// is in force on the very next run — and so a runner rebuilt by a
		// config reload and the one it replaced can never disagree about what
		// "top" means.
		MonitorNicknames: screens.Lookup,
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
// memory tools write through. feeds is the feed cache (ADR 0031) on the same
// terms: one instance serves the scheduler, the knowledge.get tool, and the
// injection, and a reload swaps its feed set through Reconfigure rather than
// ever rebuilding the service. bus carries the routine progress events
// (ADR 0026). archive is the durable conversation store (ADR 0027), a
// parameter for the same reason as book: one instance serves the engine's
// appends and the conversation.* IPC methods, and only the retention switch
// here decides whether the engine writes to it.
func engineOptions(cfg config.Config, compositor desktop.Compositor, bus *session.Bus,
	book *memory.Book, vocab *vocabulary.Store, feeds *knowledge.Service,
	archive conversations.Store, windows *tools.Desktop, screens *monitors.Store,
	logger *slog.Logger) session.Options {
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
		// The audio half of a permission ask (issue #119): whether the spoken
		// question quotes the command or just names the action class. Display
		// is not configurable — the card and overlay always get the verbatim
		// command (ADR 0014).
		SpeakConfirmationDetails: cfg.Confirmations.SpeakDetails,
		Intents:                  intentRouter(cfg),
		Routines:                 routineRunner(cfg, compositor, bus, screens, logger),
		Scripts:                  scriptRunner(cfg, bus, logger),
		WindowNames:              windowNamer(windows),
		MonitorNames:             monitorNamer(windows),
		Compositor:               compositor,
		Context:                  contextCollector(cfg, logger),
		Memory:                   memoryInjector(book),
		Vocabulary:               vocabularyInjector(vocab, cfg.Vocabulary.SpeakBack),
		VocabularyTeacher:        vocabularyTeacher(vocab, bus),
		Knowledge:                knowledgeInjector(feeds),
		Archive:                  conversationArchive(cfg, archive),
		// The transcript strip follows the assistant's identity (issue
		// #103): the configured name and its mishearing aliases, not the
		// detector's word — the detector may be pointed at a different
		// acoustic model entirely (activation.wake_word).
		WakeWord:    cfg.Assistant.Name,
		WakeAliases: cfg.Assistant.EffectiveAliases(),
		Lexicon:     cfg.TTS.Lexicon,
	}
}

// windowNamer adapts the window tools' shared state for the engine's
// nickname intents (#126), or leaves the option nil when no window tools are
// wired. Same typed-nil trap as contextCollector: a nil *tools.Desktop in
// the interface field would read as "nicknames exist" to the engine —
// disabled must mean absent, so the intents refuse honestly.
func windowNamer(windows *tools.Desktop) session.WindowNamer {
	if windows == nil {
		return nil
	}
	return windows
}

// monitorNamer adapts the same shared state for the engine's screen-name
// intents (#180). It rides the window tools rather than the store directly
// because naming "this monitor" needs the live output inventory, which the
// tools have and the store deliberately does not — and it carries the same
// typed-nil guard windowNamer does, so a daemon with no window tools refuses
// the phrase honestly instead of appearing to support it.
func monitorNamer(windows *tools.Desktop) session.MonitorNamer {
	if windows == nil {
		return nil
	}
	return windows
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

// storeInjector binds the vocabulary store to the speak_back stance for one
// engine build. A named type rather than an inline closure so the nil rules
// below stay visible at the wiring site.
type storeInjector struct {
	store     *vocabulary.Store
	speakBack bool
}

// Inject implements session.VocabularyInjector.
func (i storeInjector) Inject() vocabulary.Injection {
	return i.store.Inject(i.speakBack)
}

// vocabularyInjector adapts the taught vocabulary for the engine, or leaves
// the option nil when the feature is disabled — the memoryInjector rules.
// speakBack rides the adapter because it shapes the block's wording and the
// store deliberately holds no configuration; idle-class reloads rebuild this
// adapter, which is what makes the setting land without a restart.
func vocabularyInjector(vocab *vocabulary.Store, speakBack bool) session.VocabularyInjector {
	if vocab == nil {
		return nil
	}
	return storeInjector{store: vocab, speakBack: speakBack}
}

// vocabularyTeacher adapts the store for the deterministic teach phrases
// (issue #129), or leaves the option nil when the feature is disabled so a
// matched phrase refuses honestly. The bus closure publishes the same
// vocabulary.entry_changed the form verbs publish — a voice teach changes
// the same listing an open window is showing.
func vocabularyTeacher(vocab *vocabulary.Store, bus *session.Bus) session.VocabularyTeacher {
	if vocab == nil {
		return nil
	}
	return &vocabularyVoice{store: vocab, publish: func(action string, e vocabulary.Entry) {
		bus.Publish(session.Event{Type: "vocabulary.entry_changed", Data: map[string]any{
			"action": action, "id": e.ID,
			"chars": len([]rune(e.Phrase)) + len([]rune(e.Meaning)),
		}})
	}}
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
