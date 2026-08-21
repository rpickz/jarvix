package wake

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Every test in this file runs the complete listener — supervision, capture,
// detection, endpointing, mute — against fakes. No microphone is opened, no
// process is spawned, no model is loaded, and nothing sleeps: FakeStream's
// frame channel is unbuffered, so handing over frame N+1 is itself the proof
// that frame N has been read *and processed*.

// wakeMarker stands in for someone saying the wake word: a burst loud enough
// that the fixture detector recognises it, which is also what a wake word is
// acoustically — the loudest thing in an otherwise quiet room.
func wakeMarker(frames int) []int16 { return noise(frames, 99, 30000) }

// markerDetector scores the marker high and everything else near zero. It is
// standing in for a model, and the substitution is the point: the state
// machine below must be correct for *any* detector, so the tests describe
// what Jarvix does with scores rather than how a model produces them.
func markerDetector() *ScriptedDetector {
	return &ScriptedDetector{Label: "fixture", ScoreFunc: func(frame []int16, _ int) (float64, error) {
		if rms(frame) > 15000 {
			return 0.99, nil
		}
		return 0.02, nil
	}}
}

// testOptions are small, whole-frame values so an expectation reads as a
// number of frames rather than as arithmetic.
func testOptions() Options {
	return Options{
		Word:         "jarvix",
		Sensitivity:  DefaultSensitivity,
		RingDuration: 3 * FrameDuration,  // 240 ms of pre-roll
		Silence:      4 * FrameDuration,  // 320 ms ends the request
		Lead:         6 * FrameDuration,  // 480 ms with nothing said gives up
		MaxUtterance: 25 * FrameDuration, // 2 s ceiling
	}
}

type harness struct {
	t        *testing.T
	listener *Listener
	source   *FakeSource
	detector *ScriptedDetector

	wakes      chan float64
	utterances chan []int16
	aborts     chan string
	states     chan State

	fire    chan time.Time
	stopped chan struct{}

	// The supervisors' clock. Injecting it is what makes the restart backoff
	// steppable: warm.Supervisor refuses to spawn until its own deadline, and
	// a test that waited that out would be a sleep by another name.
	clockMu sync.Mutex
	clock   time.Time
}

func (h *harness) now() time.Time {
	h.clockMu.Lock()
	defer h.clockMu.Unlock()
	return h.clock
}

func (h *harness) advance(d time.Duration) {
	h.clockMu.Lock()
	h.clock = h.clock.Add(d)
	h.clockMu.Unlock()
}

// releaseBackoff steps over every restart delay from here on. The send blocks
// until the run loop is actually waiting, so this paces the listener rather
// than spinning beside it.
func (h *harness) releaseBackoff() {
	go func() {
		for {
			h.advance(time.Minute)
			select {
			case h.fire <- time.Time{}:
			case <-h.stopped:
				return
			}
		}
	}()
}

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	h := &harness{
		t:          t,
		source:     &FakeSource{},
		detector:   markerDetector(),
		wakes:      make(chan float64, 8),
		utterances: make(chan []int16, 8),
		aborts:     make(chan string, 8),
		states:     make(chan State, 32),
		fire:       make(chan time.Time, 1),
		stopped:    make(chan struct{}),
		clock:      time.Now(),
	}
	hooks := Hooks{
		OnWake:      func(c float64) { send(h.wakes, c) },
		OnUtterance: func(pcm []int16) { send(h.utterances, pcm) },
		OnAbort:     func(r string) { send(h.aborts, r) },
		OnState:     func(s State) { send(h.states, s) },
	}
	h.listener = New(h.source, func(context.Context) (Detector, error) { return h.detector, nil },
		opts, hooks, discardLogger())
	// The retry pace is the test's to control: nothing here waits on a clock.
	h.listener.timer = func(time.Duration) (<-chan time.Time, func()) { return h.fire, func() {} }
	h.listener.capture.Now = h.now
	h.listener.detectors.Now = h.now
	return h
}

// start runs the listener until the test finishes.
func (h *harness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(func() {
		cancel()
		select {
		case <-h.stopped:
		case <-time.After(5 * time.Second):
			h.t.Error("the listener did not stop when its context was cancelled")
		}
	})
	go func() {
		h.listener.Run(ctx)
		close(h.stopped)
	}()
}

// stream queues a capture the listener will pick up on its next attempt.
func (h *harness) stream(pid int) *FakeStream {
	s := NewFakeStream(pid)
	h.source.Push(s)
	return s
}

// retry releases the listener from its backoff — the test's clock.
func (h *harness) retry() {
	select {
	case h.fire <- time.Time{}:
	default:
	}
}

// feed hands frames to the listener one at a time. Because the channel is
// unbuffered, the call for frame N+1 returns only once the listener has come
// back for it, which means frame N has been fully processed — the whole
// synchronisation story of this file, with no sleeps in it.
func (h *harness) feed(s *FakeStream, pcm []int16) {
	h.t.Helper()
	for i, frame := range chunk(pcm) {
		accepted := make(chan bool, 1)
		go func(f []int16) { accepted <- s.Feed(f) }(frame)
		select {
		case ok := <-accepted:
			if !ok {
				h.t.Fatalf("the capture was closed before frame %d was read", i)
			}
		case <-time.After(5 * time.Second):
			h.t.Fatalf("timed out handing over frame %d", i)
		}
	}
}

func (h *harness) waitWake() float64 {
	h.t.Helper()
	select {
	case c := <-h.wakes:
		return c
	case <-time.After(5 * time.Second):
		h.t.Fatal("the wake word never fired")
		return 0
	}
}

func (h *harness) waitUtterance() []int16 {
	h.t.Helper()
	select {
	case pcm := <-h.utterances:
		return pcm
	case <-time.After(5 * time.Second):
		h.t.Fatal("no captured request arrived")
		return nil
	}
}

func (h *harness) waitState(want State) {
	h.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-h.states:
			if got == want {
				return
			}
		case <-deadline:
			h.t.Fatalf("the indicator never reached %q", want)
		}
	}
}

func send[T any](ch chan T, v T) {
	select {
	case ch <- v:
	default:
	}
}

// The headline acceptance criterion: the wake word activates, the rest of the
// sentence becomes the request, and the silence afterwards submits it. The
// pre-roll matters as much as the tail — without it the first syllables of
// every request would be lost, because people do not pause after saying a
// wake word.
func TestSayingTheWakeWordCapturesTheRequest(t *testing.T) {
	h := newHarness(t, testOptions())
	stream := h.stream(4242)
	h.start()

	h.feed(stream, roomTone(6, 1)) // fills the ring with ambient audio
	h.feed(stream, wakeMarker(2))  // "Jarvix"
	if c := h.waitWake(); c < 0.9 {
		t.Errorf("wake confidence %.2f, want the detector's own score", c)
	}
	h.feed(stream, utterance(6, 2)) // "...what's my disk usage?"
	h.feed(stream, roomTone(6, 3))  // ...and stops talking

	pcm := h.waitUtterance()
	// Three frames of pre-roll (the ring) plus everything after the wake
	// word. The exact tail length depends on where the endpointer stopped, so
	// the assertion is the shape: it must contain the pre-roll *and* the
	// speech, not one or the other.
	if len(pcm) < (3+6)*FrameSamples {
		t.Fatalf("the request is %d samples; too short to hold the pre-roll and the speech",
			len(pcm))
	}
	if got := h.listener.Status().Activations; got != 1 {
		t.Errorf("activations: got %d, want 1", got)
	}
	// The pre-roll ends where the wake word was, so its last samples must be
	// the marker rather than room tone: that is what proves the look-back is
	// wired the right way round.
	if rms(pcm[2*FrameSamples:3*FrameSamples]) < 15000 {
		t.Error("the pre-roll does not contain the wake word; the look-back is the wrong way round")
	}
}

// Eight hours of a room with people in it and no wake word must produce
// nothing at all: no activation, no capture, no session — and no growth in
// what Jarvix is holding.
func TestAmbientConversationNeverActivates(t *testing.T) {
	h := newHarness(t, testOptions())
	stream := h.stream(1)
	h.start()

	h.feed(stream, roomTone(40, 4))
	h.feed(stream, utterance(40, 5)) // people talking, just not to Jarvix
	h.feed(stream, roomTone(4, 6))   // ...and a sync frame so the above is processed

	select {
	case c := <-h.wakes:
		t.Fatalf("ambient audio activated Jarvix (confidence %.2f)", c)
	default:
	}
	h.listener.mu.Lock()
	held := h.listener.ring.Len()
	capacity := h.listener.ring.Cap()
	utteranceHeld := len(h.listener.utterance)
	h.listener.mu.Unlock()
	if held > capacity {
		t.Errorf("the ring holds %d samples with capacity %d", held, capacity)
	}
	if capacity != 3*FrameSamples {
		t.Errorf("the ring grew to %d samples; it was built for %d", capacity, 3*FrameSamples)
	}
	if utteranceHeld != 0 {
		t.Errorf("%d samples are being held as a request that was never made", utteranceHeld)
	}
}

// A false activation with nobody speaking afterwards must be abandoned, not
// transcribed. Sending two seconds of room tone to whisper would cost a
// provider call and put whatever it hallucinated into the conversation.
func TestFalseActivationWithNoSpeechIsAbandoned(t *testing.T) {
	h := newHarness(t, testOptions())
	stream := h.stream(1)
	h.start()

	h.feed(stream, roomTone(4, 7))
	h.feed(stream, wakeMarker(2))
	h.waitWake()
	h.feed(stream, roomTone(10, 8))

	select {
	case reason := <-h.aborts:
		if reason == "" {
			t.Error("the abort carried no reason")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a wake word followed by silence was never abandoned")
	}
	select {
	case <-h.utterances:
		t.Error("silence was submitted as a request")
	default:
	}
}

// `jarvix mute` promises the microphone is closed, not that Jarvix is
// politely ignoring it. The call must not return until the capture process is
// gone, because everything else — the indicator, the status report, the pid
// somebody is about to check in `ps` — is downstream of that being true.
func TestMuteClosesTheCaptureBeforeItReturns(t *testing.T) {
	h := newHarness(t, testOptions())
	stream := h.stream(4242)
	h.start()
	h.feed(stream, roomTone(2, 9))
	h.waitState(StateArmed)

	if got := h.listener.Status().PID; got != 4242 {
		t.Fatalf("status reports capture pid %d, want 4242", got)
	}
	h.listener.Mute(true)

	// Not "eventually": by the time Mute has returned. (The run loop closes
	// it again on its way out, which is why this is not an equality — Close
	// is idempotent, and being sure it happened is the point.)
	if stream.Closes() == 0 {
		t.Error("Mute returned with the capture process still running")
	}
	status := h.listener.Status()
	if status.Capturing {
		t.Error("status still reports a capture process after muting")
	}
	if status.PID != 0 {
		t.Errorf("status still names pid %d after muting", status.PID)
	}
	if status.State != StateMuted {
		t.Errorf("indicator state is %q after muting, want %q", status.State, StateMuted)
	}
}

// Muting erases what was already heard. A privacy control that closed the
// microphone but left the last second of the room sitting in the process's
// memory would be keeping only half of its promise.
func TestMuteErasesAudioHeldInMemory(t *testing.T) {
	h := newHarness(t, testOptions())
	stream := h.stream(1)
	h.start()
	h.feed(stream, utterance(4, 10))
	h.feed(stream, roomTone(1, 11)) // sync: the frames above are now processed

	h.listener.mu.Lock()
	before := nonZero(h.listener.ring.buf)
	h.listener.mu.Unlock()
	if before == 0 {
		t.Fatal("the ring is empty, so the assertion below would pass vacuously")
	}

	h.listener.Mute(true)

	h.listener.mu.Lock()
	defer h.listener.mu.Unlock()
	if n := nonZero(h.listener.ring.buf); n != 0 {
		t.Errorf("%d samples of pre-roll survived the mute", n)
	}
	if n := nonZero(h.listener.utterance[:cap(h.listener.utterance)]); n != 0 {
		t.Errorf("%d samples of a request survived the mute", n)
	}
}

// Unmuting must open a *new* capture rather than resurrecting the old one:
// the process was killed, and the microphone may well be a different device
// by now.
func TestUnmuteOpensANewCapture(t *testing.T) {
	h := newHarness(t, testOptions())
	first := h.stream(1)
	second := h.stream(2)
	h.start()
	h.feed(first, roomTone(1, 12))
	h.waitState(StateArmed)

	h.listener.Mute(true)
	h.waitState(StateMuted)
	h.listener.Mute(false)
	h.waitState(StateArmed)

	h.feed(second, roomTone(1, 13))
	if got := h.listener.Status().PID; got != 2 {
		t.Errorf("after unmuting, the capture pid is %d; want the new process (2)", got)
	}
	if got := h.source.Opens(); got != 2 {
		t.Errorf("the microphone was opened %d times, want 2", got)
	}
}

// Plugging in a headset makes pw-record exit. The listener must reopen
// against whatever the default source has become, without the daemon being
// restarted and without push-to-talk noticing anything happened.
func TestCaptureRecoversWhenTheDeviceChanges(t *testing.T) {
	h := newHarness(t, testOptions())
	first := h.stream(1)
	second := h.stream(2)
	h.start()
	h.feed(first, roomTone(1, 14))
	h.waitState(StateArmed)

	first.End() // the headset was unplugged; pw-record exited
	h.waitState(StateOff)
	h.retry()
	h.waitState(StateArmed)

	h.feed(second, roomTone(2, 15))
	h.feed(second, wakeMarker(2))
	h.waitWake() // and the wake word still works on the new device

	status := h.listener.Status()
	if status.PID != 2 {
		t.Errorf("capture pid is %d after the device change, want 2", status.PID)
	}
	if status.Restarts < 1 {
		t.Errorf("the supervisor recorded %d restarts; the device change should be one", status.Restarts)
	}
	if status.LastReason == "" {
		t.Error("nothing was recorded about why the capture stopped")
	}
}

// A microphone that is not there must produce backoff, not a spin. The
// supervisor owns the escalation; what matters here is that the listener asks
// again rather than either giving up or hammering.
func TestCaptureThatCannotStartBacksOffAndRetries(t *testing.T) {
	h := newHarness(t, testOptions())
	h.source.PushError(errors.New("pw-record not found"))
	recovered := h.stream(7)
	h.start()
	h.releaseBackoff()

	h.feed(recovered, roomTone(1, 16))
	h.waitState(StateArmed)
	if got := h.listener.Status().PID; got != 7 {
		t.Errorf("capture pid %d after recovery, want 7", got)
	}
}

// The indicator is a privacy feature, so its states have to be published, in
// order, without anyone polling for them: the bar widget holds its socket
// open precisely so that "is the microphone open?" is answered by an event.
func TestTheIndicatorFollowsTheMicrophone(t *testing.T) {
	h := newHarness(t, testOptions())
	first := h.stream(1)
	second := h.stream(2)
	h.start()

	h.feed(first, roomTone(1, 17))
	h.waitState(StateArmed)
	h.listener.Mute(true)
	h.waitState(StateMuted)
	h.listener.Mute(false)
	h.waitState(StateArmed)
	h.feed(second, roomTone(1, 24))
}

// Muting twice must not close a second capture or publish a second state
// change: `jarvix mute` on an already-muted daemon is a no-op the CLI can
// report quietly.
func TestMutingTwiceIsANoOp(t *testing.T) {
	h := newHarness(t, testOptions())
	stream := h.stream(1)
	h.start()
	h.feed(stream, roomTone(1, 18))
	h.waitState(StateArmed)

	if !h.listener.Mute(true) {
		t.Fatal("the first mute reported no change")
	}
	h.waitState(StateMuted)
	if h.listener.Mute(true) {
		t.Error("muting an already-muted listener reported a change")
	}
	// One mute, one indicator change. A second event would make the bar
	// re-render and, worse, would mean the second call did something.
	select {
	case again := <-h.states:
		t.Errorf("muting twice published a second indicator change (%q)", again)
	default:
	}
	if stream.Closes() == 0 {
		t.Error("the capture was never closed")
	}
}

// Shutting the daemon down closes the microphone. This is the one path the
// exit sequence has to reach, and getting it wrong leaves a pw-record holding
// the user's microphone after they logged out.
func TestShutdownClosesTheCapture(t *testing.T) {
	h := newHarness(t, testOptions())
	stream := h.stream(1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.listener.Run(ctx); close(done) }()

	h.feed(stream, roomTone(1, 19))
	h.waitState(StateArmed)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the listener did not stop")
	}
	if stream.Closes() == 0 {
		t.Error("shutting down left the capture process running")
	}
	if h.listener.Status().PID != 0 {
		t.Error("status still names a capture process after shutdown")
	}
}

// The pre-roll ceiling is a privacy boundary, so it is enforced where the
// buffer is built, not only where the configuration is validated.
func TestOptionsClampThePreRollToTheCeiling(t *testing.T) {
	opts := testOptions()
	opts.RingDuration = time.Minute
	h := newHarness(t, opts)
	if got := h.listener.ring.Duration(); got > MaxRingDuration {
		t.Errorf("a one-minute pre-roll was built %v long; the ceiling is %v", got, MaxRingDuration)
	}
}

// The request buffer is allocated once, for the worst case, and reused. A
// buffer that grew by re-allocating would leave copies of somebody's speech
// in unreachable heap until the garbage collector got round to them.
func TestTheRequestBufferIsAllocatedOnceAndWipedAfterUse(t *testing.T) {
	h := newHarness(t, testOptions())
	stream := h.stream(1)
	h.start()

	h.listener.mu.Lock()
	capacityBefore := cap(h.listener.utterance)
	h.listener.mu.Unlock()

	h.feed(stream, roomTone(4, 20))
	h.feed(stream, wakeMarker(2))
	h.waitWake()
	h.feed(stream, utterance(6, 21))
	h.feed(stream, roomTone(6, 22))
	h.waitUtterance()
	h.feed(stream, roomTone(1, 23)) // sync: the hand-over above has completed

	h.listener.mu.Lock()
	defer h.listener.mu.Unlock()
	if got := cap(h.listener.utterance); got != capacityBefore {
		t.Errorf("the request buffer was re-allocated (%d → %d samples)", capacityBefore, got)
	}
	if n := nonZero(h.listener.utterance[:cap(h.listener.utterance)]); n != 0 {
		t.Errorf("%d samples of the request survived after it was handed over", n)
	}
}
