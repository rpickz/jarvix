package wake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/warm"
)

// Defaults for the tunable parts of background listening. Each is a shipped
// value someone can live with rather than a placeholder: a user who enables
// wake-word activation and changes nothing else should get a working
// assistant.
const (
	// DefaultSilence is how long a lull ends an utterance. 800 ms is the
	// figure the acceptance criteria name, and it matches the pause people
	// leave between a request and expecting an answer.
	DefaultSilence = 800 * time.Millisecond
	// DefaultLead is how long a capture waits for speech to start before
	// concluding the activation was spurious.
	DefaultLead = 2500 * time.Millisecond
	// DefaultRing is how much audio is held before a wake word. Deliberately
	// well under the 3 s ceiling: the pre-roll is the only ambient audio that
	// can ever reach a transcript, so it should be as short as recognising
	// the wake word allows, not as long as the privacy budget permits.
	DefaultRing = 1200 * time.Millisecond
	// DefaultMaxUtterance bounds one request. Also the bound on how much
	// audio exists in the process at once.
	DefaultMaxUtterance = 15 * time.Second
	// DefaultSensitivity maps to a 0.5 score threshold (see ThresholdFor).
	DefaultSensitivity = 0.5
)

// DetectorMemoryCap is the resident-set ceiling for the detector process,
// enforced by the warm supervisor: a detector that outgrows it is retired and
// respawned rather than left to grow. It is the NFR ("memory ≤ 200 MB")
// expressed as a mechanism instead of a hope.
const DetectorMemoryCap = 200 << 20

// detectorCheckFrames is how often the detector is re-acquired from its
// supervisor — every ~30 s of audio. Re-acquiring is what runs the memory-cap
// check and picks up a replacement after a crash, and it costs one small
// /proc read; doing it per frame would cost twelve of those a second, forever,
// on a path whose entire justification is that it is unnoticeable.
const detectorCheckFrames = int(30 * time.Second / FrameDuration)

// defaultRetry paces the listener's attempts to get capture back. Escalating
// backoff belongs to the supervisor (it refuses to spawn until its own timer
// elapses); this is only how often the listener asks.
const defaultRetry = time.Second

// errMuted ends a capture loop because the user muted, not because anything
// went wrong.
var errMuted = errors.New("background listening muted")

// State is what the microphone indicator shows. Deliberately three values
// rather than a boolean: "the feature is off" and "the feature is on and I
// have muted it" are different things to tell a user looking at a bar icon.
type State string

// Listener states.
const (
	// StateOff — background listening is not running: not configured, not
	// installed, or between restarts. The microphone is closed.
	StateOff State = "off"
	// StateArmed — a capture process is running and audio is being examined
	// on this machine. This is the state the indicator must never fail to
	// show.
	StateArmed State = "armed"
	// StateMuted — the user muted; the capture process has been killed.
	StateMuted State = "muted"
)

// Options configures the listener. Everything here comes from [activation] in
// config.toml.
type Options struct {
	// Word is the wake word, passed to the detector and reported by status.
	// Jarvix does not match it itself — the model does.
	Word string
	// Sensitivity is 0..1, higher being more eager. See ThresholdFor.
	Sensitivity float64
	// Silence is the endpoint threshold; zero uses DefaultSilence.
	Silence time.Duration
	// Lead is how long to wait for speech after a wake; zero uses DefaultLead.
	Lead time.Duration
	// RingDuration is the pre-roll window; zero uses DefaultRing, and
	// anything above MaxRingDuration is clamped to it.
	RingDuration time.Duration
	// MaxUtterance bounds one request; zero uses DefaultMaxUtterance.
	MaxUtterance time.Duration
}

func (o Options) normalise() Options {
	if o.Sensitivity <= 0 {
		o.Sensitivity = DefaultSensitivity
	}
	if o.Silence <= 0 {
		o.Silence = DefaultSilence
	}
	if o.Lead <= 0 {
		o.Lead = DefaultLead
	}
	if o.RingDuration <= 0 {
		o.RingDuration = DefaultRing
	}
	if o.RingDuration > MaxRingDuration {
		o.RingDuration = MaxRingDuration
	}
	if o.MaxUtterance <= 0 {
		o.MaxUtterance = DefaultMaxUtterance
	}
	return o
}

// Hooks are what the listener tells the daemon. They are invoked from the
// listener's own goroutine, one at a time and in order, so an implementation
// needs no locking of its own — but it must not block, because the next audio
// frame is 80 ms away and the microphone does not wait.
type Hooks struct {
	// OnWake fires the instant the wake word is recognised, before any of the
	// request has been captured. This is where the session is started and any
	// speech in flight is interrupted: waiting for the utterance to end first
	// would leave Jarvix talking over the user for the length of their
	// sentence.
	OnWake func(confidence float64)
	// OnUtterance delivers the captured request — the pre-roll plus
	// everything up to the endpoint — as 16 kHz mono s16 samples. The slice
	// belongs to the callee; the listener's own copy is wiped as it is
	// handed over.
	OnUtterance func(pcm []int16)
	// OnAbort reports a capture that produced nothing worth transcribing
	// (silence after a false activation). The session started by OnWake
	// should be cancelled.
	OnAbort func(reason string)
	// OnState reports every change to what the indicator should show.
	OnState func(state State)
}

// Listener is background wake-word listening: one supervised capture process,
// one supervised detector, and the state machine between them.
type Listener struct {
	opts  Options
	hooks Hooks
	log   *slog.Logger

	// The two supervised children (ADR 0018). Capture is a pw-record reading
	// the default source; the detector is whatever engine is installed. Each
	// dies and restarts independently, and neither can take the daemon — or
	// push-to-talk — down with it.
	capture   *warm.Supervisor[Stream]
	detectors *warm.Supervisor[Detector]

	// timer paces retries; injectable so restart behaviour is tested without
	// sleeping, exactly as the warm supervisor does it.
	timer func(d time.Duration) (<-chan time.Time, func())
	retry time.Duration

	// resume wakes the run loop out of a mute or a backoff.
	resume chan struct{}

	// mu guards everything below, audio included. Taking a lock per 80 ms
	// frame is free at this cadence, and it is what lets Mute wipe the
	// buffers synchronously instead of asking the run goroutine to get round
	// to it.
	mu    sync.Mutex
	muted bool
	// stream and capturing describe the *capture process*: whether the
	// microphone is open. inUtterance describes the state machine: whether a
	// request is being recorded right now. They are separate facts — the
	// microphone is open almost all the time and a request is being recorded
	// almost none of it — and the indicator cares about the first.
	stream      Stream
	state       State
	capturing   bool
	inUtterance bool
	ring        *Ring
	utterance   []int16
	maxSamples  int
	endpointer  Endpointer
	policy      Policy
	detector    string
	activations int
	lastReason  string
}

// New builds a listener. source and spawn are the two seams every test
// replaces: a fake source feeding PCM fixtures, and a fake detector scoring
// them.
//
// Nothing is started here and no process is spawned — Run does that — so a
// daemon that is never unmuted never opens the microphone.
func New(source Source, spawn func(context.Context) (Detector, error),
	opts Options, hooks Hooks, logger *slog.Logger) *Listener {
	if logger == nil {
		logger = slog.Default()
	}
	opts = opts.normalise()
	maxSamples := int(opts.MaxUtterance / time.Second * SampleRate)
	ring := NewRing(opts.RingDuration)

	l := &Listener{
		opts:  opts,
		hooks: hooks,
		log:   logger,
		retry: defaultRetry,
		timer: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTimer(d)
			return t.C, func() { t.Stop() }
		},
		resume: make(chan struct{}, 1),
		state:  StateOff,
		ring:   ring,
		// One allocation, sized for the worst case, for the life of the
		// daemon. A buffer that grew by re-allocating would leave copies of
		// somebody's speech in unreachable heap for the garbage collector to
		// get round to, which is not the kind of thing this feature is
		// allowed to do casually.
		utterance:  make([]int16, 0, ring.Cap()+maxSamples+FrameSamples),
		maxSamples: maxSamples,
		endpointer: Endpointer{Silence: opts.Silence, Lead: opts.Lead, Max: opts.MaxUtterance},
		policy: Policy{
			Threshold:   ThresholdFor(opts.Sensitivity),
			Consecutive: DefaultConsecutive,
			Refractory:  DefaultRefractory,
		},
	}
	l.capture = &warm.Supervisor[Stream]{
		Name: "wake-capture",
		Spawn: func(ctx context.Context) (Stream, error) {
			return source.Open(ctx)
		},
		Log: logger,
	}
	l.detectors = &warm.Supervisor[Detector]{
		Name:      "wake-detector",
		Spawn:     spawn,
		MemoryCap: DetectorMemoryCap,
		Log:       logger,
	}
	return l
}

// Run listens until ctx is cancelled. It never returns an error: a wake
// listener that cannot run is a feature that is unavailable, not a daemon
// that should stop — push-to-talk keeps working either way.
func (l *Listener) Run(ctx context.Context) {
	defer l.shutdown()
	// A blocking read on a live capture is not interruptible by a context —
	// the pipe simply has nothing in it, and cancellation is invisible from
	// inside io.ReadFull. So shutdown closes the stream out from under the
	// read, which is the same thing the push-to-talk watcher does to unblock
	// a device read (internal/hotkey/watcher.go). Without this, logging out
	// leaves the daemon waiting on a microphone nobody is speaking into.
	go func() {
		<-ctx.Done()
		l.mu.Lock()
		stream := l.stream
		l.stream = nil
		l.mu.Unlock()
		if stream != nil {
			stream.Close()
		}
	}()

	raw := make([]byte, FrameBytes)
	frame := make([]int16, FrameSamples)

	for ctx.Err() == nil {
		if l.Muted() {
			l.setState(StateMuted)
			if !l.waitForResume(ctx) {
				return
			}
			continue
		}
		// The detector first, and deliberately: without one there is nothing
		// to do with audio, so the microphone should not be open at all.
		detector, err := l.detectors.Get(ctx)
		if err != nil {
			l.setState(StateOff)
			l.note(err.Error())
			l.wait(ctx)
			continue
		}
		l.setDetectorName(detector.Name())

		stream, err := l.capture.Get(ctx)
		if err != nil {
			l.setState(StateOff)
			l.note(err.Error())
			l.wait(ctx)
			continue
		}
		if !l.streamUp(stream) {
			// Muted between the spawn and here; the stream has been closed.
			l.capture.Discard(errMuted.Error())
			continue
		}

		err = l.consume(ctx, stream, detector, raw, frame)
		muted := l.streamDown()
		if ctx.Err() != nil {
			return
		}
		switch {
		case muted || errors.Is(err, errMuted):
			l.capture.Discard(errMuted.Error())
		default:
			// A capture that ends on its own is the normal shape of a headset
			// being unplugged: pw-record exits, and the next one attaches to
			// whatever the default source has become. That is the whole
			// device-change story — no daemon restart, one line in the log.
			reason := "capture stream ended"
			if err != nil && !errors.Is(err, io.EOF) {
				reason = err.Error()
			}
			l.note(reason)
			l.capture.Discard(reason)
			l.log.Info("wake capture ended; reopening", "component", "wake", "reason", reason)
			l.wait(ctx)
		}
	}
}

// consume reads frames until the stream ends, the user mutes, or the detector
// has to be replaced.
func (l *Listener) consume(ctx context.Context, stream Stream, detector Detector,
	raw []byte, frame []int16) error {
	since := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if l.Muted() {
			return errMuted
		}
		if _, err := io.ReadFull(stream, raw); err != nil {
			return err
		}
		decodeFrame(raw, frame)

		if since++; since >= detectorCheckFrames {
			since = 0
			replacement, err := l.detectors.Get(ctx)
			if err != nil {
				return fmt.Errorf("wake detector unavailable: %w", err)
			}
			detector = replacement
			l.setDetectorName(detector.Name())
		}
		if err := l.processFrame(detector, frame); err != nil {
			l.detectors.Discard(err.Error())
			return fmt.Errorf("wake detector failed: %w", err)
		}
	}
}

// processFrame is the state machine, one frame at a time.
//
// The detector is called off the lock — it is a round trip to another process
// — and every buffer mutation happens under it. Hooks fire after the lock is
// released, with a copy of anything they are given.
func (l *Listener) processFrame(detector Detector, frame []int16) error {
	l.mu.Lock()
	if l.muted {
		l.mu.Unlock()
		return nil
	}
	recording := l.inUtterance
	if !recording {
		l.ring.Write(frame)
	}
	l.mu.Unlock()

	if !recording {
		score, err := detector.Score(frame)
		if err != nil {
			return err
		}
		l.mu.Lock()
		// Checked again on the way back in. Scoring a frame is a round trip to
		// another process, and a mute landing during it must not be undone by
		// the result arriving: without this, `jarvix mute` could wipe the
		// buffers and then have the in-flight frame written straight back in.
		if l.muted {
			l.mu.Unlock()
			return nil
		}
		fired := l.policy.Fire(score)
		if fired {
			l.inUtterance = true
			l.activations++
			// The pre-roll becomes the head of the request, and the ring is
			// erased in the same breath: the audio now exists in exactly one
			// place, and that place is bounded and about to be transcribed.
			l.utterance = l.ring.AppendTo(l.utterance[:0])
			l.endpointer.Prime(l.utterance)
			l.endpointer.Reset()
			l.ring.Reset()
		}
		l.mu.Unlock()
		if fired {
			// Confidence, and nothing else. A wake event says that something
			// was said, never what.
			l.log.Info("wake word detected", "component", "wake",
				"confidence", fmt.Sprintf("%.2f", score))
			if l.hooks.OnWake != nil {
				l.hooks.OnWake(score)
			}
		}
		return nil
	}

	l.mu.Lock()
	if l.muted {
		l.mu.Unlock()
		return nil
	}
	if len(l.utterance)+len(frame) <= cap(l.utterance) {
		l.utterance = append(l.utterance, frame...)
	}
	var captured []int16
	reason := ""
	switch l.endpointer.Push(frame) {
	case EndpointComplete, EndpointMax:
		captured = append([]int16(nil), l.utterance...)
		l.endCaptureLocked()
	case EndpointNoSpeech:
		reason = "no speech after the wake word"
		l.endCaptureLocked()
	case EndpointContinue:
	}
	l.mu.Unlock()

	switch {
	case captured != nil:
		if l.hooks.OnUtterance != nil {
			l.hooks.OnUtterance(captured)
		}
	case reason != "":
		l.log.Debug("wake capture discarded", "component", "wake", "reason", reason)
		if l.hooks.OnAbort != nil {
			l.hooks.OnAbort(reason)
		}
	}
	return nil
}

// endCaptureLocked returns to listening and erases the request. The wipe
// before the truncate is the part that matters: `l.utterance[:0]` alone would
// leave every sample of the last thing the user said sitting in the backing
// array until something happened to overwrite it.
func (l *Listener) endCaptureLocked() {
	wipe(l.utterance[:cap(l.utterance)])
	l.utterance = l.utterance[:0]
	l.inUtterance = false
	l.ring.Reset()
}

// Mute stops or restarts background capture. It reports whether anything
// changed, so a caller can stay quiet about a no-op.
//
// Muting is synchronous by design: this returns only once the capture process
// has been killed and reaped, and once every audio buffer has been zeroed. It
// is the difference between "Jarvix stops acting on what it hears" and "the
// microphone is closed", and only the second one is worth promising.
func (l *Listener) Mute(muted bool) bool {
	l.mu.Lock()
	if l.muted == muted {
		l.mu.Unlock()
		return false
	}
	l.muted = muted
	stream := l.stream
	if muted {
		l.stream = nil
		l.capturing = false
		l.inUtterance = false
		l.ring.Reset()
		wipe(l.utterance[:cap(l.utterance)])
		l.utterance = l.utterance[:0]
		l.policy.Reset()
		l.endpointer.Reset()
	}
	l.mu.Unlock()

	if !muted {
		l.log.Info("background listening resumed", "component", "wake")
		select {
		case l.resume <- struct{}{}:
		default:
		}
		return true
	}
	// The process is killed after the buffers are wiped and the flag is set,
	// so there is no window in which a frame read by the run goroutine can
	// land in a buffer the mute has already cleared.
	if stream != nil {
		stream.Close()
	}
	l.setState(StateMuted)
	l.log.Info("background listening muted; the capture process has been killed",
		"component", "wake")
	return true
}

// Muted reports whether background listening is muted.
func (l *Listener) Muted() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.muted
}

// Status is the listener as `jarvix status`, `jarvix doctor`, and the
// settings screen see it. Every field is either a configured value or an
// observable fact; there is nothing here a user has to take on trust.
type Status struct {
	// State is what the indicator shows: off, armed, or muted.
	State State `json:"state"`
	// Muted is the live switch (`jarvix mute`), independent of whether the
	// capture process happens to be up.
	Muted bool `json:"muted"`
	// Capturing reports whether a capture process is running right now.
	Capturing bool `json:"capturing"`
	// PID is that process, or 0. The number to check in `ps`.
	PID int `json:"pid"`
	// Word, Sensitivity, Threshold, SilenceMs, RingMs are the running
	// configuration, so a report never has to be read alongside the file.
	Word        string  `json:"word"`
	Sensitivity float64 `json:"sensitivity"`
	Threshold   float64 `json:"threshold"`
	SilenceMs   int     `json:"endpoint_silence_ms"`
	RingMs      int     `json:"ring_ms"`
	// Detector names the engine; DetectorPID and DetectorRSSMB are its
	// process, so the memory NFR is checkable rather than claimed.
	Detector      string `json:"detector"`
	DetectorPID   int    `json:"detector_pid"`
	DetectorRSSMB int    `json:"detector_rss_mb"`
	// Activations counts wake words heard this daemon lifetime — the number
	// to divide by uptime when measuring the false-activation rate.
	Activations int `json:"activations"`
	// Restarts and LastReason are the supervision story: how often capture
	// has had to be reopened, and why it stopped last.
	Restarts   int    `json:"restarts"`
	LastReason string `json:"last_reason"`
}

// Status snapshots the listener.
func (l *Listener) Status() Status {
	captureStatus := l.capture.Status()
	detectorStatus := l.detectors.Status()

	l.mu.Lock()
	defer l.mu.Unlock()
	return Status{
		State:         l.state,
		Muted:         l.muted,
		Capturing:     l.capturing,
		PID:           captureStatus.PID,
		Word:          l.opts.Word,
		Sensitivity:   l.opts.Sensitivity,
		Threshold:     l.policy.Threshold,
		SilenceMs:     int(l.opts.Silence / time.Millisecond),
		RingMs:        int(l.ring.Duration() / time.Millisecond),
		Detector:      l.detector,
		DetectorPID:   detectorStatus.PID,
		DetectorRSSMB: int(detectorStatus.RSSBytes >> 20),
		Activations:   l.activations,
		Restarts:      captureStatus.Restarts,
		LastReason:    l.lastReason,
	}
}

// ---------------------------------------------------------------- internals

// streamUp records a live capture, or refuses it because a mute landed while
// it was starting — in which case the stream is closed here and now, so a
// race can never leave the microphone open after `jarvix mute` returned.
func (l *Listener) streamUp(stream Stream) bool {
	l.mu.Lock()
	if l.muted {
		l.mu.Unlock()
		stream.Close()
		return false
	}
	l.stream = stream
	l.capturing = true
	l.inUtterance = false
	l.mu.Unlock()
	l.setState(StateArmed)
	l.log.Info("background listening active", "component", "wake",
		"word", l.opts.Word, "pid", stream.PID(),
		"pre_roll_ms", int(l.ring.Duration()/time.Millisecond))
	return true
}

// streamDown forgets the capture and erases whatever it left behind. It
// reports whether the listener is muted, which is how the run loop tells an
// intentional stop from a failure.
func (l *Listener) streamDown() bool {
	l.mu.Lock()
	l.stream = nil
	l.capturing = false
	l.inUtterance = false
	l.ring.Reset()
	wipe(l.utterance[:cap(l.utterance)])
	l.utterance = l.utterance[:0]
	l.policy.Reset()
	muted := l.muted
	l.mu.Unlock()
	if !muted {
		l.setState(StateOff)
	}
	return muted
}

// shutdown kills both children and leaves nothing in memory. The supervisors
// own the processes, so this is the one place the daemon's exit path has to
// reach to be sure the microphone is closed.
func (l *Listener) shutdown() {
	l.capture.Close()
	l.detectors.Close()
	l.mu.Lock()
	stream := l.stream
	l.stream = nil
	l.capturing = false
	l.inUtterance = false
	l.ring.Reset()
	wipe(l.utterance[:cap(l.utterance)])
	l.utterance = l.utterance[:0]
	l.mu.Unlock()
	if stream != nil {
		stream.Close()
	}
	l.setState(StateOff)
}

// setState publishes an indicator change, once per actual change.
func (l *Listener) setState(state State) {
	l.mu.Lock()
	if l.state == state {
		l.mu.Unlock()
		return
	}
	l.state = state
	hook := l.hooks.OnState
	l.mu.Unlock()
	if hook != nil {
		hook(state)
	}
}

func (l *Listener) setDetectorName(name string) {
	l.mu.Lock()
	l.detector = name
	l.mu.Unlock()
}

// note records why capture last stopped, for status and doctor. Reasons are
// process-level facts — "pw-record exited", "helper not found" — never
// anything derived from audio.
func (l *Listener) note(reason string) {
	l.mu.Lock()
	l.lastReason = reason
	l.mu.Unlock()
}

// wait paces a retry, cut short by an unmute or by shutdown.
func (l *Listener) wait(ctx context.Context) {
	fire, stop := l.timer(l.retry)
	defer stop()
	select {
	case <-fire:
	case <-l.resume:
	case <-ctx.Done():
	}
}

// waitForResume blocks until Mute(false) or shutdown. It reports false when
// the daemon is going away.
func (l *Listener) waitForResume(ctx context.Context) bool {
	select {
	case <-l.resume:
		return true
	case <-ctx.Done():
		return false
	}
}
