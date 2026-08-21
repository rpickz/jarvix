// Package daemon wires configuration into a running jarvixd: engines built
// from config, the session engine, and the IPC surface.
package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/build"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/history"
	"github.com/rpickz/jarvix/internal/hotkey"
	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/memory"
	"github.com/rpickz/jarvix/internal/quiesce"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
	"github.com/rpickz/jarvix/internal/wake"
	"github.com/rpickz/jarvix/internal/warm"
)

// DefaultShutdownGrace bounds the whole shutdown drain: how long Run will
// wait, in total, for the work a stopping daemon still owes — the pending
// conversation-history write above all (ADR 0011).
//
// Five seconds is chosen from both ends. It is far more than the drain
// actually needs: the outstanding work is one small atomic file write plus a
// few goroutines unwinding a cancelled context, which is milliseconds on any
// working system. And it is comfortably inside systemd's default
// TimeoutStopSec of 90s, so a daemon that does hit the bound still exits on
// its own terms — logging which stage was stuck — rather than being SIGKILLed
// with no explanation in the journal.
//
// It is a constant rather than a setting deliberately: a user has no way to
// know a good value, and the only two outcomes it selects between are "wait a
// few milliseconds" and "something is broken, say so and go".
const DefaultShutdownGrace = 5 * time.Second

// Daemon is a fully wired jarvixd instance.
type Daemon struct {
	engine   *session.Engine
	server   *ipc.Server
	bus      *session.Bus
	log      *slog.Logger
	registry *tools.Registry
	policy   config.ToolsPolicy // effective gate config, reported by status.get
	pttChord []uint16           // daemon-side hold-to-talk chord; empty = disabled
	pttLive  bool               // watcher running (chord set + input devices readable)

	// memory is the knowledge base (ADR 0025), nil when memory.enabled is
	// false. One instance for the daemon's life: the memory tools write
	// through it, the engine injects from it, and the memory.* IPC methods
	// read it — so every surface always agrees on what is remembered.
	memory *memory.Book

	// conversations is the durable archive (ADR 0027). Never nil, and held
	// even with retention off: the off switch stops writing, but listing,
	// reading and deleting what is already kept must keep working — the user's
	// control over their transcripts does not lapse with the recording.
	conversations conversations.Store
	// searcher is full-text search over the same archive (issue #59): one
	// implementation behind the window, the CLI, and the model's tool. Never
	// nil for the same reason the store never is.
	searcher conversations.Searcher

	// Background wake-word listening (ADR 0024), nil unless activation.mode
	// is "wake_word" and its detector is installed. wakeSession is the
	// session the current wake word started, held between the detection and
	// the end of the sentence; wakeState is what the microphone indicator
	// should show. Guarded by wakeMu — a separate lock from the rest because
	// it is taken from the listener's own goroutine on every state change.
	wakeMu      sync.Mutex
	wake        *wake.Listener
	wakeSession string
	wakeState   string
	// wakeDone closes when the listener's goroutine has finished shutting
	// down. The daemon waits on it before exiting: pw-record runs in its own
	// process group and would survive a bare exit, which would leave the
	// user's microphone open after they logged out.
	wakeDone chan struct{}

	// Desktop notification dispatch (ui.notifications); see notifications.go.
	notifier   desktop.Notifier
	openWindow func(context.Context) error
	// compositor is the one window-manager seam this daemon owns, shared by
	// the window tools and the desktop intents. Held here so a settings
	// reload rebuilds the engine's options around the *same* instance and its
	// probed dispatch dialect, rather than making the next "workspace four"
	// pay for a fresh probe.
	compositor desktop.Compositor

	// post tracks the daemon's own post-session goroutines: the session
	// watcher and every notification delivery it dispatches. They outlive the
	// session that produced them by design, so shutdown has to wait for them
	// rather than assume a finished session means a finished daemon.
	post quiesce.Group
	// shutdownGrace bounds the drain in shutdown; DefaultShutdownGrace unless
	// a test shortens it to exercise the give-up path.
	shutdownGrace time.Duration

	// The most recent session failure, retained past session.finished so the
	// conversation window can render it. A window opened from an error
	// notification only connects *after* the click, long after the `error`
	// event went out on the bus, so without this it would open showing an
	// idle, blameless conversation and the failure would be invisible.
	// Cleared when the next session starts. Guarded by errMu.
	errMu          sync.Mutex
	lastErrStage   string
	lastErrMessage string

	// Running configuration plus what a reload needs to rebuild collaborators
	// (settings.go). cfg holds the values the daemon is actually operating
	// with: restart-class settings keep their booted values even after the
	// file changes. injected preserves caller-provided collaborators (test
	// fakes) across reloads.
	paths    config.Paths
	injected Deps
	cfgMu    sync.Mutex
	cfg      config.Config
	// warm are the supervised engine processes the running configuration
	// built. They are the daemon's children in the literal sense: killed on
	// shutdown, and replaced (old ones killed) whenever a reload rebuilds the
	// adapters, so a long-running jarvixd never accumulates engine processes.
	// Guarded by cfgMu, alongside the configuration that produced them.
	warm warmWorkers

	// The most recent session's latency report (the session.timings payload),
	// retained for `jarvix status --last`: the numbers are most wanted right
	// after an interaction felt slow, which is exactly when the event has
	// already gone out on the bus. Guarded by errMu.
	lastTimings map[string]any

	// The most recent typing decision (ADR 0023) — which window, how many
	// characters, whether a human approved it, what happened. Retained for
	// `jarvix status --last` for the same reason as the timings: the question
	// is asked afterwards. It never holds the typed text; the event it comes
	// from does not carry it. Guarded by errMu.
	lastTyping map[string]any
}

// Deps are the engine's collaborators, injectable for tests. Zero-value
// fields are built from configuration.
type Deps struct {
	Provider    ai.Provider
	Transcriber stt.Transcriber
	Synthesizer tts.Synthesizer
	Recorder    audio.Recorder
	Player      audio.Player
	// Notifier delivers desktop notifications; nil uses notify-send.
	Notifier desktop.Notifier
	// Compositor is the window-manager seam (ADR 0022); nil drives Hyprland
	// through hyprctl. Injected by tests so nothing here ever touches a real
	// desktop.
	Compositor desktop.Compositor
	// Keyboard is the keystroke seam (ADR 0023); nil drives wtype. Injected by
	// tests for a stronger reason than the compositor's: a test that reached
	// the real one would type into the session running it.
	Keyboard desktop.Keyboard
	// OpenWindow opens the conversation window after a notification click;
	// nil asks the Omarchy shell plugin.
	OpenWindow func(context.Context) error
	// HistoryStore persists conversation memory; nil uses the JSON file under
	// the XDG state dir. Injectable so a test can hold a write open across
	// shutdown — the one thing a real file store cannot be asked to do.
	HistoryStore history.Store
	// ConversationStore is the durable archive (ADR 0027); nil uses the file
	// store under the XDG state dir. Injectable for the same reason as
	// HistoryStore: only a fake can hold an archive write open.
	ConversationStore conversations.Store
	// WakeSource opens the background capture stream; nil uses pw-record.
	// Injected by tests so no test ever opens the user's microphone.
	WakeSource wake.Source
	// WakeDetector spawns the wake-word detector; nil runs the configured
	// helper process. Injected by tests, which also skips the "is the helper
	// installed" probe — a fake detector is installed by definition.
	WakeDetector func(context.Context) (wake.Detector, error)
}

// New builds a daemon from configuration. cfg must already be validated.
func New(cfg config.Config, paths config.Paths, logger *slog.Logger, deps Deps) (*Daemon, error) {
	if logger == nil {
		logger = slog.Default()
	}

	// Remember what the caller injected before filling from config, so a
	// later config reload rebuilds only what came from config (settings.go).
	injected := deps
	deps, workers, err := fillDeps(cfg, paths, deps, logger)
	if err != nil {
		return nil, err
	}

	// One compositor for the whole daemon. The window tools (ADR 0022) and the
	// deterministic desktop intents (ADR 0017) both act through it, so the
	// dispatch dialect is probed once and remembered for both — a second
	// instance would mean a second probe and, worse, a second place for the
	// dialect decision to live.
	compositor := deps.Compositor
	if compositor == nil {
		compositor = &desktop.Hyprland{}
	}

	bus := session.NewBus(logger)
	registry := tools.NewRegistry(logger)
	if cfg.Tools.Shell {
		registry.Register(&tools.Shell{
			Timeout:   time.Duration(cfg.Tools.ShellTimeoutSec) * time.Second,
			MaxOutput: cfg.Tools.ShellMaxOutputKB * 1024,
			Log:       logger,
		})
		logger.Info("tool enabled", "component", "tools", "tool", "shell.run")
	}
	if cfg.Tools.Artifacts {
		registry.Register(&tools.Artifact{
			Dir:          cfg.Artifacts.Dir,
			OpenCommand:  cfg.Artifacts.OpenCommand,
			OpenCommands: artifactOpenCommands(cfg.Artifacts.OpenCommands),
			Timeout:      time.Duration(cfg.Artifacts.RenderTimeoutSec) * time.Second,
			// Adding a format is exactly this: implement Renderer (plus
			// SourceValidator for structured formats) and append it here.
			// The tool's schema, naming, events, and listing pick it up.
			Renderers: []tools.Renderer{
				&tools.MermaidRenderer{OutputFormat: cfg.Artifacts.DiagramFormat},
				&tools.DocumentRenderer{},
				&tools.SpreadsheetRenderer{},
				&tools.ExcalidrawRenderer{},
			},
			// The event carries the path so the window/notification can link
			// to the file; spoken summaries deliberately never do.
			OnCreated: func(format, path string) {
				bus.Publish(session.Event{Type: "artifact.created",
					Data: map[string]any{"type": format, "path": path}})
			},
			Log: logger,
		})
		logger.Info("tool enabled", "component", "tools", "tool", "artifact.create")
	}
	// The window tools (ADR 0022). Registered as five verbs sharing one
	// compositor and one inventory cache, so the gate can allow the reads and
	// ask about the changes without either being a special case.
	//
	// The shared state is built whenever *either* family is on, because typing
	// borrows every window decision from it (ADR 0023): one inventory, one
	// matcher, one definition of which window is being acted on. Only the
	// desktop flag decides whether the five verbs are offered to the model.
	var windows *tools.Desktop
	if cfg.Tools.Desktop || cfg.Tools.Typing.Enable {
		windows = tools.NewDesktop(tools.DesktopOptions{
			Compositor: compositor,
			Apps:       cfg.Tools.DesktopApps,
			ScrubEnv:   providerKeyEnvNames(cfg),
			// The event carries what was done to which window so the overlay
			// can show it; addresses stay daemon-side, and spoken summaries
			// never mention either.
			OnAction: func(verb, target string) {
				bus.Publish(session.Event{Type: "desktop.action",
					Data: map[string]any{"verb": verb, "target": target}})
			},
			Log: logger,
		})
	}
	if cfg.Tools.Desktop {
		for _, t := range windows.Tools() {
			registry.Register(t)
		}
		logger.Info("tool enabled", "component", "tools",
			"tools", strings.Join(windows.Names(), ","), "apps", cfg.Tools.DesktopApps)
	}
	// The typing tools (ADR 0023). Opt-in, like shell.run: the user turned
	// this on deliberately, and the startup log says so in the journal, once,
	// because a machine that types on its owner's behalf should never be
	// something they discover by watching it happen.
	if cfg.Tools.Typing.Enable {
		keyboard := deps.Keyboard
		if keyboard == nil {
			keyboard = &desktop.Wtype{Binary: cfg.Tools.Typing.Binary}
		}
		typing := tools.NewTyping(tools.TypingOptions{
			Windows:         windows,
			Keyboard:        keyboard,
			MaxChars:        cfg.Tools.Typing.MaxChars,
			RateLimit:       cfg.Tools.Typing.RateLimit,
			RateWindow:      cfg.Tools.Typing.RateWindow(),
			TerminalClasses: cfg.Tools.Typing.TerminalClasses,
			// The audit event carries the window, the length and the outcome —
			// never the characters. The user may have dictated a password, and
			// a bus event reaches every subscriber.
			OnAudit: func(a tools.TypingAudit) {
				d := map[string]any{
					"tool": a.Tool, "window": a.Window, "chars": a.Chars,
					"approved": a.Approved, "terminal": a.Terminal, "outcome": a.Outcome,
				}
				if a.Key != "" {
					d["key"] = a.Key
				}
				if a.Reason != "" {
					d["reason"] = a.Reason
				}
				bus.Publish(session.Event{Type: "typing.audit", Data: d})
			},
			Log: logger,
		})
		for _, t := range typing.Tools() {
			registry.Register(t)
		}
		logger.Info("tool enabled", "component", "tools",
			"tools", strings.Join(typing.Names(), ","),
			"max_chars", cfg.Tools.Typing.MaxChars,
			"rate_limit", cfg.Tools.Typing.RateLimit,
			"rate_window_sec", cfg.Tools.Typing.RateWindowSec)
	}
	// Advisor delegation is enabled by configuring an advisor and nothing
	// else: `jarvix setup` writes the tables, and each advisor carries its
	// own authentication (ADR 0016).
	advisorTiers := advisorPolicyTiers(cfg)
	if len(cfg.Advisors) > 0 {
		registry.Register(&tools.Advisor{
			Advisors: advisorSpecs(cfg),
			ScrubEnv: providerKeyEnvNames(cfg),
			Log:      logger,
		})
		logger.Info("tool enabled", "component", "tools", "tool", tools.AdvisorToolName,
			"advisors", cfg.AdvisorNames())
	}
	// The permission gate is always installed — even with no tools enabled,
	// so a tool added later can never ship ungated (ADR 0014).
	perTool := make(map[string]tools.PolicyDecision, len(cfg.Tools.Policy.Tool))
	for name, d := range cfg.Tools.Policy.Tool {
		perTool[name] = tools.PolicyDecision(d)
	}
	policy, err := tools.NewPolicy(tools.PolicyConfig{
		Default:    tools.PolicyDecision(cfg.Tools.Policy.Default),
		Tools:      perTool,
		ShellAllow: cfg.Tools.Policy.ShellAllow,
		ShellDeny:  cfg.Tools.Policy.ShellDeny,
		Advisors:   advisorTiers,
	})
	if err != nil {
		return nil, err // Validate catches this first; belt and braces
	}
	registry.SetPolicy(policy)

	// The deterministic intent router sits in front of the model (ADR 0017).
	// The engine receives it through engineOptions, so a live reload rebuilds
	// it too; compiling it here is the startup error path.
	if cfg.Intents.Enabled {
		router, err := intent.New(cfg.IntentOptions())
		if err != nil {
			return nil, err // Validate catches this first; belt and braces
		}
		logger.Info("intent router enabled", "component", "intent",
			"intents", len(router.Names()))
	}
	// Routines (ADR 0026) are stated at startup like the intents they route
	// through: names only, never the steps — a journal line should say what
	// the daemon can do, not inventory the user's desktop habits.
	if len(cfg.Routines) > 0 {
		names := make([]string, 0, len(cfg.Routines))
		for _, r := range cfg.Routines {
			names = append(names, r.Name)
		}
		logger.Info("routines enabled", "component", "routine",
			"routines", strings.Join(names, ","))
	}

	// What Jarvix may look at is stated at startup, once, in the journal: an
	// ambient-capture feature should never be something a user discovers by
	// reading the source (ADR 0019).
	if sources := cfg.Context.EnabledSources(); len(sources) > 0 {
		logger.Info("desktop context enabled", "component", "context",
			"sources", strings.Join(sources, ","),
			"max_chars", cfg.Context.MaxChars, "timeout_ms", cfg.Context.TimeoutMs)
	} else {
		logger.Info("desktop context disabled", "component", "context")
	}

	// The knowledge base (ADR 0025): facts the user explicitly asked Jarvix
	// to keep, in one hand-editable file under the XDG state dir. Disabled
	// means absent — no store consulted, no tools registered — but never
	// deleted: unlike conversation history, the store holds facts the user
	// deliberately curated, and only an explicit forget removes them.
	var book *memory.Book
	if cfg.Memory.Enabled {
		book = memory.NewBook(paths.MemoryFile(), memory.BookOptions{
			MaxFacts:          cfg.Memory.MaxFacts,
			MaxInjectedTokens: cfg.Memory.MaxInjectedTokens,
		}, logger)
		logger.Info("memory enabled", "component", "memory", "path", paths.MemoryFile(),
			"max_facts", cfg.Memory.MaxFacts, "max_injected_tokens", cfg.Memory.MaxInjectedTokens)
	} else {
		logger.Info("memory disabled", "component", "memory")
	}

	// Conversation memory persists under the XDG state dir so a follow-up
	// still has its context after a daemon restart (ADR 0011).
	var store history.Store = &history.File{Path: paths.HistoryFile()}
	if deps.HistoryStore != nil {
		store = deps.HistoryStore
	}
	// The durable archive (ADR 0027). Whether the engine *writes* to it is
	// the retention switch, decided in engineOptions; the store itself always
	// exists so listing and deleting past conversations work regardless.
	var convs conversations.Store = &conversations.FileStore{Dir: paths.ConversationsDir()}
	if deps.ConversationStore != nil {
		convs = deps.ConversationStore
	}
	// Search shares the store when it can search (the file store and the test
	// fake both do). An injected wrapper that cannot still has the real files
	// underneath, so searching the directory keeps the one-implementation
	// promise (issue #59) rather than declaring search absent.
	searcher, ok := convs.(conversations.Searcher)
	if !ok {
		searcher = &conversations.FileStore{Dir: paths.ConversationsDir()}
	}
	if cfg.Conversation.Retention == config.RetentionOff {
		logger.Info("conversation retention off", "component", "session")
	} else {
		logger.Info("conversation retention on", "component", "session",
			"dir", paths.ConversationsDir())
	}
	engine := session.NewEngine(deps.Provider, deps.Transcriber, deps.Synthesizer,
		deps.Recorder, deps.Player, registry, store, bus, logger,
		engineOptions(cfg, compositor, bus, book, convs, logger))

	// The memory tools are registered after the engine exists because a
	// stored fact carries its source turn, and only the engine knows which
	// session is asking. Registration still precedes serving — the registry
	// is only read once sessions run.
	if book != nil {
		mem := tools.NewMemory(tools.MemoryOptions{
			Book: book,
			Source: func() string {
				_, id := engine.State()
				return id
			},
			Log: logger,
		})
		for _, t := range mem.Tools() {
			registry.Register(t)
		}
		logger.Info("tool enabled", "component", "tools",
			"tools", strings.Join(mem.Names(), ","))
	}

	if deps.Notifier == nil {
		deps.Notifier = &desktop.NotifySend{}
	}
	if deps.OpenWindow == nil {
		windows := &desktop.WindowClient{}
		deps.OpenWindow = windows.Open
	}

	server := ipc.NewServer(paths.Socket, bus, logger)
	d := &Daemon{
		engine: engine, server: server, bus: bus, log: logger,
		registry: registry, policy: cfg.Tools.Policy, memory: book,
		conversations: convs, searcher: searcher,
		notifier: deps.Notifier, openWindow: deps.OpenWindow,
		compositor: compositor,
		paths:      paths, injected: injected, cfg: cfg, warm: workers,
		shutdownGrace: DefaultShutdownGrace,
	}
	if len(cfg.Activation.PTTChord) > 0 {
		codes, err := hotkey.ResolveChord(cfg.Activation.PTTChord)
		if err != nil {
			return nil, err // Validate catches this first; belt and braces
		}
		d.pttChord = codes
	}
	// The archive search tool (issue #59), registered after the daemon exists
	// because "earlier in this conversation" needs the engine's live thread id
	// and the wording for an empty archive needs the live retention switch.
	// Registration still precedes serving, like the memory tools above.
	registry.Register(tools.NewConversationSearch(tools.ConversationSearchOptions{
		Searcher:  searcher,
		ActiveID:  engine.ActiveConversationID,
		Retention: d.retentionOn,
		Log:       logger,
	}))
	logger.Info("tool enabled", "component", "tools", "tool", tools.ConversationsSearchToolName)
	d.registerMethods()
	return d, nil
}

// artifactOpenCommands converts the config's per-format viewer overrides into
// the plain argv map the tool takes. The tool package deliberately does not
// import config — it is reusable on its own terms — so the named
// config.Command element type is shed here.
func artifactOpenCommands(in map[string]config.Command) map[string][]string {
	if in == nil {
		return nil
	}
	out := make(map[string][]string, len(in))
	for format, argv := range in {
		out[format] = argv
	}
	return out
}

// Run listens on the socket and serves until ctx is cancelled, then drains
// the work the daemon still owes before returning (see shutdown).
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.server.Listen(); err != nil {
		return err
	}
	defer d.shutdown()
	// Subscribe before serving so no session that a client starts can finish
	// unobserved. The watcher checks the live ui.notifications switch per
	// session, so toggling notifications needs no restart (settings.go).
	events, unsubscribe := d.bus.Subscribe()
	d.post.Go(func() { d.watchSessions(ctx, events, unsubscribe) })
	d.startPTT(ctx)
	d.startWake(ctx)
	d.log.Info("jarvixd ready", "component", "daemon", "version", build.Version)
	return d.server.Serve(ctx)
}

// shutdown drains everything a stopping daemon still owes, then kills its
// children. It runs once Serve has stopped accepting, and Run does not return
// until it is done — so "jarvixd has exited" means the work is finished, not
// merely abandoned.
//
// The reason this exists at all is that jarvixd does work after the part of an
// interaction the user can see. Conversation history is written *after*
// session.finished and off the engine's lock, so the disk never delays a
// spoken answer (ADR 0011). That design is right and stays; what was missing
// was anyone waiting for it, so a restart landing in the gap silently lost the
// last exchange.
//
// The stages are drained in dependency order, and share one deadline so total
// shutdown is bounded by the grace period however many stages there are:
//
//	sessions       cancel what is in flight and wait for the tails, which is
//	               where the pending history write lives.
//	ipc            connections still dispatching a request or pushing events.
//	notifications  the session watcher and its delivery goroutines; a cancelled
//	               context kills the notify-send child, so this is quick.
//	warm workers   killed last, because a draining session may still be
//	               speaking or transcribing through one (ADR 0018).
//
// A stage that does not settle is logged with what it left outstanding, and
// shutdown moves on: a wedged write must never be able to keep the daemon
// alive. Deliberately not drained: the push-to-talk watcher (ADR 0008). Its
// goroutines are parked in a blocking read on an evdev device that only a
// keystroke returns from, so waiting for them would spend the entire grace
// period on every shutdown — and they hold nothing a restart could lose.
func (d *Daemon) shutdown() {
	// Background, not the cancelled context Run was given: this is the drain
	// that runs *because* that context is done.
	ctx, cancel := context.WithTimeout(context.Background(), d.shutdownGrace)
	defer cancel()

	for _, stage := range []struct {
		name      string
		wait      func(context.Context) error
		remaining func() int
	}{
		{"sessions", d.engine.Shutdown, d.engine.InFlight},
		{"ipc", d.server.Drain, d.server.InFlight},
		{"notifications", d.post.Wait, d.post.InFlight},
	} {
		if err := stage.wait(ctx); err != nil {
			d.log.Warn("shutdown drain gave up waiting; exiting anyway",
				"component", "daemon", "stage", stage.name,
				"grace_ms", d.shutdownGrace.Milliseconds(),
				"outstanding", stage.remaining(), "error", err.Error())
		}
	}

	// The wake listener goes first, and it is the one stage of shutdown that
	// is urgent rather than tidy: one of its children is holding the
	// microphone open, and pw-record's process group would survive a bare
	// exit (ADR 0024).
	d.stopWake()
	// Warm workers die with the daemon. Their process groups would survive a
	// bare exit — that is the whole point of a persistent worker — so the exit
	// path has to say so explicitly (ADR 0018).
	d.closeWarm()
	d.log.Info("jarvixd stopped", "component", "daemon")
}

// startPTT runs the daemon-side hold-to-talk watcher when a chord is
// configured and input devices are readable. Without device access the
// Hyprland tap-to-toggle binding remains the activation path; doctor
// explains how to grant access.
func (d *Daemon) startPTT(ctx context.Context) {
	if len(d.pttChord) == 0 {
		return
	}
	if !hotkey.Accessible() {
		d.log.Warn("push-to-talk chord configured but input devices are not readable; "+
			"falling back to the tap-to-toggle keybinding (run: jarvix doctor)",
			"component", "hotkey")
		return
	}
	watcher := hotkey.NewWatcher(d.pttChord,
		func() { // chord held down → listen
			if state, _ := d.engine.State(); state == session.StateAwaitingConfirmation {
				// A tool confirmation is pending: this hold answers it. The
				// session must keep waiting, so no new session is started —
				// the capture flows into the pending confirmation instead.
				if err := d.engine.StartVoice(); err != nil {
					d.log.Error("ptt press", "component", "hotkey", "error", err.Error())
				}
				return
			}
			if _, err := d.engine.StartSession(); err != nil {
				d.log.Error("ptt press", "component", "hotkey", "error", err.Error())
				return
			}
			if err := d.engine.StartVoice(); err != nil {
				d.log.Error("ptt press", "component", "hotkey", "error", err.Error())
			}
		},
		func() { // any chord key released → submit
			discarded, err := d.engine.StopVoice()
			if err != nil {
				// Session may already be gone (cancelled mid-hold); not an event.
				d.log.Debug("ptt release", "component", "hotkey", "error", err.Error())
				return
			}
			if !discarded {
				if err := d.engine.Submit(""); err != nil {
					d.log.Error("ptt release", "component", "hotkey", "error", err.Error())
				}
			}
		},
		d.log)
	d.pttLive = true
	d.log.Info("daemon-side push-to-talk active", "component", "hotkey")
	go watcher.Run(ctx)
}

// Bus exposes the event bus (used by tests).
func (d *Daemon) Bus() *session.Bus { return d.bus }

// closeWarm shuts down every warm worker of the running configuration.
func (d *Daemon) closeWarm() {
	d.cfgMu.Lock()
	workers := d.warm
	d.warm = warmWorkers{}
	d.cfgMu.Unlock()
	workers.Close()
}

// warmStatus reports the running configuration's warm workers.
func (d *Daemon) warmStatus() []warm.Status {
	d.cfgMu.Lock()
	workers := d.warm
	d.cfgMu.Unlock()
	return workers.Status()
}

// warmReport renders the warm workers for status.get and doctor.get. The
// shape is deliberately flat and self-describing: `jarvix doctor` prints it
// verbatim, and a settings screen can too.
func (d *Daemon) warmReport() []map[string]any {
	statuses := d.warmStatus()
	out := make([]map[string]any, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, map[string]any{
			"name":       s.Name,
			"running":    s.Running,
			"pid":        s.PID,
			"rss_mb":     s.RSSBytes >> 20,
			"uptime_sec": s.UptimeSec,
			"restarts":   s.Restarts,
			"last_error": s.LastError,
		})
	}
	return out
}

func (d *Daemon) registerMethods() {
	type submitParams struct {
		Text string `json:"text"`
	}

	d.server.Handle("session.start", func(json.RawMessage) (any, error) {
		id, err := d.engine.StartSession()
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeSessionError, "%v", err)
		}
		return map[string]string{"session_id": id}, nil
	})
	d.server.Handle("session.submit", func(params json.RawMessage) (any, error) {
		var p submitParams
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "session.submit params: %v", err)
			}
		}
		if err := d.engine.Submit(p.Text); err != nil {
			return nil, ipc.Errorf(ipc.CodeSessionError, "%v", err)
		}
		return nil, nil
	})
	d.server.Handle("session.cancel", func(json.RawMessage) (any, error) {
		if err := d.engine.Cancel(); err != nil {
			return nil, ipc.Errorf(ipc.CodeSessionError, "%v", err)
		}
		return nil, nil
	})
	d.server.Handle("voice.start", func(json.RawMessage) (any, error) {
		if err := d.engine.StartVoice(); err != nil {
			return nil, ipc.Errorf(ipc.CodeSessionError, "%v", err)
		}
		return nil, nil
	})
	d.server.Handle("voice.stop", func(json.RawMessage) (any, error) {
		discarded, err := d.engine.StopVoice()
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeSessionError, "%v", err)
		}
		return map[string]bool{"discarded": discarded}, nil
	})
	d.server.Handle("speech.cancel", func(json.RawMessage) (any, error) {
		// stopped: false is a no-op, not an error — there was nothing playing
		// to stop, and the caller deserves to know which happened (issue #54).
		return map[string]bool{"stopped": d.engine.CancelSpeech()}, nil
	})
	d.server.Handle("conversation.reset", func(json.RawMessage) (any, error) {
		d.engine.ResetConversation()
		return nil, nil
	})
	// conversation.get gives the conversation window its opening snapshot:
	// the current turns straight from the engine's in-memory history (this
	// method's shape is deliberately independent of any storage), plus the
	// state so one call is enough to render. Clients live-append from
	// assistant.delta / transcript.final / state.changed / error events.
	d.server.Handle("conversation.get", func(json.RawMessage) (any, error) {
		state, id := d.engine.State()
		stage, message := d.lastError()
		return map[string]any{
			"turns":      d.engine.Conversation(),
			"state":      string(state),
			"session_id": id,
			// The last failure, if the session that just ended failed: the
			// window is normally opened by clicking the notification about
			// it, so the snapshot must carry what the `error` event said.
			"error_stage":   stage,
			"error_message": message,
		}, nil
	})
	d.server.Handle("session.confirm", func(params json.RawMessage) (any, error) {
		// Approved defaults to true: `jarvix confirm` is the affirmative;
		// declining is the explicit case.
		p := struct {
			Approved *bool `json:"approved"`
		}{}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "session.confirm params: %v", err)
			}
		}
		approved := p.Approved == nil || *p.Approved
		if err := d.engine.Confirm(approved); err != nil {
			return nil, ipc.Errorf(ipc.CodeSessionError, "%v", err)
		}
		return map[string]bool{"approved": approved}, nil
	})
	d.server.Handle("status.get", func(json.RawMessage) (any, error) {
		state, id := d.engine.State()
		ptt := "external" // activation comes from keybindings (toggle/hold CLI)
		if d.pttLive {
			ptt = "daemon" // the daemon watches the chord itself
		}
		return map[string]any{
			"state":      string(state),
			"session_id": id,
			"version":    build.Version,
			"protocol":   ipc.ProtocolVersion,
			"ptt":        ptt,
			"policy":     d.effectivePolicy(),
			"warm":       d.warmReport(),
			// Background listening: the mode, whether it is muted, and the
			// pid of the capture process. A client that connects mid-life
			// gets the microphone indicator right from this one call rather
			// than waiting for the next wake.changed.
			"wake":       d.wakeReport(),
			"wake_state": d.wakeStateKey(),
			// The last session's latency budget, so `jarvix status --last`
			// answers "why did that feel slow" without tailing the journal.
			"last_timings": d.lastTimingsReport(),
			// The typing audit trail: what Jarvix last did with the keyboard,
			// and never what it typed (ADR 0023).
			"last_typing": d.lastTypingReport(),
			// The archive and its search: counts and states only — whether
			// search is active is status's business, what was searched is not.
			"conversations": d.conversationsReport(),
		}, nil
	})
	d.registerConfigMethods()
	d.registerContextMethods()
	d.registerConversationMethods()
	d.registerMemoryMethods()
	d.registerTextMethods()
	d.registerWakeMethods()
	d.registerRoutineMethods()
}

// effectivePolicy reports the permission gate as it actually applies: the
// gate-wide settings plus the resolved decision for every registered tool,
// so `jarvix status` shows what would happen, not just what the file says.
func (d *Daemon) effectivePolicy() map[string]any {
	perTool := map[string]string{}
	for _, name := range d.registry.Names() {
		perTool[name] = string(d.registry.Policy().ToolDecision(name))
	}
	return map[string]any{
		"default":                   d.policy.Default,
		"confirm_timeout_sec":       d.policy.ConfirmTimeoutSec,
		"remember_for_conversation": d.policy.RememberForConversation,
		"tools":                     perTool,
	}
}
