package daemon

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/wake"
)

// This file wires background wake-word listening into the daemon (ADR 0024).
//
// It is deliberately the same shape as startPTT: an optional, self-contained
// activation path that is started if it can be and skipped with one honest
// warning if it cannot. Push-to-talk is never conditional on it — a missing
// model, a dead helper, a microphone that vanished all leave the chord
// working exactly as before, which is what "degrades to PTT-only" has to mean
// in practice.

// pipewireSource adapts the audio package's concrete streamer to the
// interface the wake listener consumes. Go has no covariant return types, and
// the alternative — internal/audio importing internal/wake so it can name the
// interface — would point the dependency the wrong way: audio knows about
// PipeWire, and nothing else should have to.
type pipewireSource struct{ *audio.PipeWireStreamer }

// Open implements wake.Source. The explicit nil return on the error path
// matters: handing back a typed-nil *PipeWireStream would leave the listener
// holding an interface that is not nil and panicking on the first read.
func (s pipewireSource) Open(ctx context.Context) (wake.Stream, error) {
	stream, err := s.PipeWireStreamer.Open(ctx)
	if err != nil {
		return nil, err
	}
	return stream, nil
}

// startWake starts background listening when it is configured and its
// detector is installed. A detector that is missing is reported once, with
// the command that fixes it, and the feature simply does not run: a
// supervisor quietly retrying a helper that was never installed is a
// crash-loop with better manners.
func (d *Daemon) startWake(ctx context.Context) {
	cfg := d.runningConfig()
	if !cfg.Activation.WakeWordEnabled() {
		return
	}
	if d.injected.WakeDetector == nil {
		if err := wake.DetectorReady(cfg.Activation.WakeCommand); err != nil {
			d.log.Warn("background listening is enabled but its detector is not installed; "+
				"push-to-talk is unaffected (run: jarvix doctor)",
				"component", "wake", "error", err.Error())
			return
		}
	}

	source := d.injected.WakeSource
	if source == nil {
		source = pipewireSource{&audio.PipeWireStreamer{Device: cfg.Audio.InputDevice}}
	}
	spawn := d.injected.WakeDetector
	if spawn == nil {
		argv := append([]string(nil), cfg.Activation.WakeCommand...)
		// The assistant's name, lowercased — or the activation.wake_word
		// override, for a detector pointed at a bundled word or a
		// self-trained model (issue #103).
		word := cfg.WakeDetectorWord()
		spawn = func(ctx context.Context) (wake.Detector, error) {
			return wake.StartDetector(ctx, argv, word, d.log)
		}
	}

	listener := wake.New(source, spawn, wake.Options{
		Word:         cfg.WakeDetectorWord(),
		Sensitivity:  cfg.Activation.WakeSensitivity,
		Silence:      cfg.Activation.EndpointSilence(),
		RingDuration: cfg.Activation.WakeRing(),
		MaxUtterance: cfg.Activation.MaxUtterance(),
	}, d.wakeHooks(), d.log)

	done := make(chan struct{})
	d.wakeMu.Lock()
	d.wake = listener
	d.wakeDone = done
	d.wakeMu.Unlock()

	// Said once, at startup, in the journal — the same rule desktop context
	// follows (ADR 0019). A microphone that opens itself must never be
	// something a user discovers by reading the source.
	d.log.Info("background listening enabled", "component", "wake",
		"word", cfg.WakeDetectorWord(),
		"sensitivity", cfg.Activation.WakeSensitivity,
		"pre_roll_ms", cfg.Activation.WakeRingMs,
		"endpoint_silence_ms", cfg.Activation.EndpointSilenceMs)
	go func() {
		defer close(done)
		listener.Run(ctx)
	}()
}

// stopWakeGrace bounds how long the exit path waits for the listener to shut
// its children down. Short, because logging out must not feel slow, and
// because the alternative to waiting a moment is a pw-record that outlives
// the session.
const stopWakeGrace = 5 * time.Second

// stopWake waits for the wake listener to finish shutting down, so the daemon
// does not exit while a capture process is still alive. The listener's own
// context is already cancelled by the time this runs — this only waits for it
// to be true.
func (d *Daemon) stopWake() {
	d.wakeMu.Lock()
	done := d.wakeDone
	d.wakeMu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(stopWakeGrace):
		// The children are killed by the listener's own shutdown; if that has
		// not happened in five seconds something is wedged, and saying so is
		// more useful than blocking the exit indefinitely.
		d.log.Warn("the wake listener did not shut down in time; check for a stray pw-record",
			"component", "wake")
	}
}

// wakeHooks turns detector events into session lifecycle. The split between
// OnWake and OnUtterance is the whole latency story: the session starts (and
// anything Jarvix was saying stops) on the wake word, while the request is
// still being spoken, rather than a sentence and an 800 ms endpoint later.
func (d *Daemon) wakeHooks() wake.Hooks {
	return wake.Hooks{
		OnWake: func(confidence float64) {
			// Out before the session, so the overlay can acknowledge the wake
			// word while whisper is still thinking about it. Confidence and a
			// timestamp; never audio, never text.
			d.bus.Publish(session.Event{Type: "wake.detected",
				Data: map[string]any{"confidence": confidence}})
			id, err := d.engine.StartWake()
			if err != nil {
				// A capture is already in progress (a held chord). The
				// deliberate gesture wins; nothing to report to the user.
				d.log.Debug("wake ignored", "component", "wake", "error", err.Error())
				d.setWakeSession("")
				return
			}
			d.setWakeSession(id)
		},
		OnUtterance: func(pcm []int16) {
			id := d.wakeSessionID()
			if id == "" {
				return
			}
			spoken := time.Duration(len(pcm)) * time.Second / wake.SampleRate
			rec, err := audio.SaveClip(d.paths.Runtime, pcm)
			if err != nil {
				d.log.Error("could not save the wake capture", "component", "wake",
					"error", err.Error())
				d.engine.AbortWake(id, "the capture could not be saved")
				d.setWakeSession("")
				return
			}
			if _, err := d.engine.FinishWake(id, rec, spoken); err != nil {
				d.log.Error("wake submit", "component", "wake", "error", err.Error())
			}
			d.setWakeSession("")
		},
		OnAbort: func(reason string) {
			if id := d.wakeSessionID(); id != "" {
				d.engine.AbortWake(id, reason)
				d.setWakeSession("")
			}
		},
		OnState: func(state wake.State) {
			d.wakeMu.Lock()
			d.wakeState = string(state)
			d.wakeMu.Unlock()
			// The indicator's feed. The bar widget holds its socket open
			// continuously (ADR 0020) precisely so that "is the microphone
			// open?" is answered by an event rather than by a poll.
			d.bus.Publish(session.Event{Type: "wake.changed",
				Data: map[string]any{"state": string(state)}})
		},
	}
}

// registerWakeMethods adds the wake surface to the IPC server. It is
// registered unconditionally: `jarvix mute` must answer on a daemon that has
// background listening switched off, because "nothing is listening" is the
// answer the user came for.
func (d *Daemon) registerWakeMethods() {
	d.server.Handle("wake.mute", func(params json.RawMessage) (any, error) {
		// Muted defaults to true: `jarvix mute` is the whole point, and
		// unmuting is the explicit case — the same shape session.confirm uses.
		p := struct {
			Muted *bool `json:"muted"`
		}{}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "wake.mute params: %v", err)
			}
		}
		muted := p.Muted == nil || *p.Muted
		listener := d.wakeListener()
		if listener == nil {
			return d.wakeReport(), nil
		}
		// Returns only once the capture process has been killed and reaped,
		// so the report below is a statement of fact rather than an intention.
		listener.Mute(muted)
		return d.wakeReport(), nil
	})
	d.server.Handle("wake.status", func(json.RawMessage) (any, error) {
		return d.wakeReport(), nil
	})
}

// wakeReport renders background listening for status.get, `jarvix mute`, and
// doctor. Flat and self-describing, like warmReport: the CLI prints it and
// the settings screen can too.
func (d *Daemon) wakeReport() map[string]any {
	cfg := d.runningConfig()
	listener := d.wakeListener()
	if listener == nil {
		// Enabled-but-not-running is a real and important state: the mode is
		// on and the detector is missing. Saying "off" without saying why
		// would leave the user believing a microphone is open when it is not,
		// or the reverse.
		reason := ""
		if cfg.Activation.WakeWordEnabled() {
			if err := wake.DetectorReady(cfg.Activation.WakeCommand); err != nil {
				reason = err.Error()
			}
		}
		return map[string]any{
			"mode":        cfg.Activation.Mode,
			"enabled":     cfg.Activation.WakeWordEnabled(),
			"running":     false,
			"state":       string(wake.StateOff),
			"muted":       false,
			"capturing":   false,
			"pid":         0,
			"word":        cfg.WakeDetectorWord(),
			"last_reason": reason,
		}
	}
	s := listener.Status()
	return map[string]any{
		"mode":                cfg.Activation.Mode,
		"enabled":             true,
		"running":             true,
		"state":               string(s.State),
		"muted":               s.Muted,
		"capturing":           s.Capturing,
		"pid":                 s.PID,
		"word":                s.Word,
		"sensitivity":         s.Sensitivity,
		"threshold":           s.Threshold,
		"endpoint_silence_ms": s.SilenceMs,
		"ring_ms":             s.RingMs,
		"detector":            s.Detector,
		"detector_pid":        s.DetectorPID,
		"detector_rss_mb":     s.DetectorRSSMB,
		"activations":         s.Activations,
		"restarts":            s.Restarts,
		"last_reason":         s.LastReason,
	}
}

// wakeStateKey is what the bar widget shows, folded into status.get so a
// client that connects mid-life gets the indicator right without waiting for
// the next wake.changed.
func (d *Daemon) wakeStateKey() string {
	d.wakeMu.Lock()
	defer d.wakeMu.Unlock()
	if d.wakeState == "" {
		return string(wake.StateOff)
	}
	return d.wakeState
}

func (d *Daemon) wakeListener() *wake.Listener {
	d.wakeMu.Lock()
	defer d.wakeMu.Unlock()
	return d.wake
}

func (d *Daemon) setWakeSession(id string) {
	d.wakeMu.Lock()
	d.wakeSession = id
	d.wakeMu.Unlock()
}

func (d *Daemon) wakeSessionID() string {
	d.wakeMu.Lock()
	defer d.wakeMu.Unlock()
	return d.wakeSession
}
