// Package daemon wires configuration into a running jarvixd: engines built
// from config, the session engine, and the IPC surface.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/ai/openaicompat"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/build"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/history"
	"github.com/rpickz/jarvix/internal/hotkey"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/stt/whispercpp"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
	"github.com/rpickz/jarvix/internal/tts/kokoro"
	"github.com/rpickz/jarvix/internal/tts/piper"
)

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

	// Desktop notification dispatch (ui.notifications); see notifications.go.
	notifier   desktop.Notifier
	openWindow func(context.Context) error
	notify     bool // ui.notifications: announce finished sessions at all
	preview    bool // ui.notification_preview: include answer content
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
	// OpenWindow opens the conversation window after a notification click;
	// nil asks the Omarchy shell plugin.
	OpenWindow func(context.Context) error
}

// New builds a daemon from configuration. cfg must already be validated.
func New(cfg config.Config, paths config.Paths, logger *slog.Logger, deps Deps) (*Daemon, error) {
	if logger == nil {
		logger = slog.Default()
	}

	if deps.Provider == nil {
		ep, ok := cfg.Endpoint()
		if !ok {
			return nil, fmt.Errorf("no endpoint for ai.provider %q", cfg.AI.Provider)
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

	bus := session.NewBus(logger)
	registry := tools.NewRegistry(logger)
	systemPrompt := cfg.AI.SystemPrompt
	if cfg.Tools.Shell {
		registry.Register(&tools.Shell{
			Timeout:   time.Duration(cfg.Tools.ShellTimeoutSec) * time.Second,
			MaxOutput: cfg.Tools.ShellMaxOutputKB * 1024,
			Log:       logger,
		})
		systemPrompt += config.ToolSystemPrompt
		logger.Info("tool enabled", "component", "tools", "tool", "shell.run")
	}
	if cfg.Tools.Artifacts {
		registry.Register(&tools.Artifact{
			Dir:          cfg.Artifacts.Dir,
			OpenCommand:  cfg.Artifacts.OpenCommand,
			OpenCommands: cfg.Artifacts.OpenCommands,
			Timeout:      time.Duration(cfg.Artifacts.RenderTimeoutSec) * time.Second,
			// Adding a format is exactly this: implement Renderer (plus
			// SourceValidator for structured formats) and append it here.
			// The tool's schema, naming, events, and listing pick it up.
			Renderers: []tools.Renderer{
				&tools.MermaidRenderer{},
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
		systemPrompt += config.ArtifactSystemPrompt
		logger.Info("tool enabled", "component", "tools", "tool", "artifact.create")
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
	})
	if err != nil {
		return nil, err // Validate catches this first; belt and braces
	}
	registry.SetPolicy(policy)

	// Conversation memory persists under the XDG state dir so a follow-up
	// still has its context after a daemon restart (ADR 0011).
	store := &history.File{Path: paths.HistoryFile()}
	engine := session.NewEngine(deps.Provider, deps.Transcriber, deps.Synthesizer,
		deps.Recorder, deps.Player, registry, store, bus, logger, session.Options{
			Model:             cfg.AI.Model,
			SystemPrompt:      systemPrompt,
			MaxTokens:         cfg.AI.MaxTokens,
			Temperature:       cfg.AI.Temperature,
			SpeakResponses:    cfg.Conversation.SpeakResponses,
			MinRecording:      time.Duration(cfg.Audio.MinRecordingMs) * time.Millisecond,
			HistoryTurns:      cfg.Conversation.HistoryTurns,
			FollowUpWindow:    time.Duration(cfg.Conversation.FollowUpWindowSec) * time.Second,
			ConfirmTimeout:    time.Duration(cfg.Tools.Policy.ConfirmTimeoutSec) * time.Second,
			RememberApprovals: cfg.Tools.Policy.RememberForConversation,
		})

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
		registry: registry, policy: cfg.Tools.Policy,
		notifier: deps.Notifier, openWindow: deps.OpenWindow,
		notify: cfg.UI.Notifications, preview: cfg.UI.NotificationPreview,
	}
	if len(cfg.Activation.PTTChord) > 0 {
		codes, err := hotkey.ResolveChord(cfg.Activation.PTTChord)
		if err != nil {
			return nil, err // Validate catches this first; belt and braces
		}
		d.pttChord = codes
	}
	d.registerMethods()
	return d, nil
}

// Run listens on the socket and serves until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.server.Listen(); err != nil {
		return err
	}
	if d.notify {
		// Subscribe before serving so no session that a client starts can
		// finish unobserved.
		events, unsubscribe := d.bus.Subscribe()
		go d.watchSessions(ctx, events, unsubscribe)
	}
	d.startPTT(ctx)
	d.log.Info("jarvixd ready", "component", "daemon", "version", build.Version)
	return d.server.Serve(ctx)
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
		if err := d.engine.CancelSpeech(); err != nil {
			return nil, ipc.Errorf(ipc.CodeSessionError, "%v", err)
		}
		return nil, nil
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
		return map[string]any{
			"turns":      d.engine.Conversation(),
			"state":      string(state),
			"session_id": id,
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
		}, nil
	})
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
