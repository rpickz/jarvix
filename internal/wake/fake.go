package wake

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// The fakes below are what keep this package's tests hermetic. Between them
// they stand in for the microphone and the model, so the whole feature —
// ring, detection, endpointing, supervision, mute — is exercised without a
// sound card, a subprocess, or a downloaded model, and without a single
// sleep: a FakeStream's frame channel is unbuffered, so handing it the next
// frame is itself the proof that the previous one has been processed.
//
// They live in the package rather than in a _test.go file for the same reason
// audio.FakeRecorder and intent.Fake do: the daemon's own tests need them
// too, and a wake listener that could only be faked from inside this package
// would leave the daemon wiring untested.

// ScriptedDetector is a Detector that scores frames with a caller-supplied
// function.
type ScriptedDetector struct {
	// Label is reported by Name; empty means "fake".
	Label string
	// ScoreFunc decides each frame's score. Nil scores every frame 0, which
	// is the "ambient room, nobody said the wake word" fixture.
	ScoreFunc func(frame []int16, n int) (float64, error)

	mu     sync.Mutex
	frames int
	closed int
}

// NewScriptedDetector returns a detector that plays back a fixed score per
// frame, repeating the last value once the script runs out. It is the
// shortest way to express "this is what the model said" in a test.
func NewScriptedDetector(scores ...float64) *ScriptedDetector {
	return &ScriptedDetector{ScoreFunc: func(_ []int16, n int) (float64, error) {
		switch {
		case len(scores) == 0:
			return 0, nil
		case n < len(scores):
			return scores[n], nil
		default:
			return scores[len(scores)-1], nil
		}
	}}
}

// Name implements Detector.
func (d *ScriptedDetector) Name() string {
	if d.Label == "" {
		return "fake"
	}
	return d.Label
}

// PID implements Detector. Zero: an in-process detector has no process, and
// opts out of the supervisor's memory cap.
func (d *ScriptedDetector) PID() int { return 0 }

// Score implements Detector.
func (d *ScriptedDetector) Score(frame []int16) (float64, error) {
	d.mu.Lock()
	n := d.frames
	d.frames++
	d.mu.Unlock()
	if d.ScoreFunc == nil {
		return 0, nil
	}
	return d.ScoreFunc(frame, n)
}

// Close implements Detector.
func (d *ScriptedDetector) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed++
}

// Frames reports how many frames have been scored.
func (d *ScriptedDetector) Frames() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.frames
}

// Closes reports how many times the detector was closed.
func (d *ScriptedDetector) Closes() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// FakeSource serves a queue of streams, one per Open, so a test decides
// exactly which capture the listener gets and when. An exhausted queue fails
// the Open, which is how "the microphone went away" is expressed.
type FakeSource struct {
	mu    sync.Mutex
	queue []fakeOpen
	opens int
}

type fakeOpen struct {
	stream *FakeStream
	err    error
}

// Push enqueues a successful Open.
func (s *FakeSource) Push(stream *FakeStream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, fakeOpen{stream: stream})
}

// PushError enqueues a failing Open — a microphone that is not there.
func (s *FakeSource) PushError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, fakeOpen{err: err})
}

// Opens reports how many times a capture has been started.
func (s *FakeSource) Opens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens
}

// Open implements Source.
func (s *FakeSource) Open(context.Context) (Stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opens++
	if len(s.queue) == 0 {
		return nil, fmt.Errorf("fake source: no capture available")
	}
	next := s.queue[0]
	s.queue = s.queue[1:]
	if next.err != nil {
		return nil, next.err
	}
	return next.stream, nil
}

// FakeStream is a scripted capture. Frames are handed to it one at a time and
// the channel is unbuffered, so a test that has pushed frame N+1 knows frame
// N has been read and processed — deterministic ordering with no sleeps
// anywhere.
type FakeStream struct {
	// Pid is what PID reports; a non-zero value lets a test assert that the
	// process a status report names is the one that was closed.
	Pid int

	frames  chan []int16
	closed  chan struct{}
	pending []byte

	mu        sync.Mutex
	closes    int
	closeOnce sync.Once
}

// NewFakeStream builds a stream a test feeds frame by frame.
func NewFakeStream(pid int) *FakeStream {
	return &FakeStream{Pid: pid, frames: make(chan []int16), closed: make(chan struct{})}
}

// Feed hands one frame to the listener, blocking until it has been read. It
// reports false if the stream was closed first, so a test never deadlocks on
// a capture the listener has already given up on.
func (s *FakeStream) Feed(frame []int16) bool {
	select {
	case s.frames <- frame:
		return true
	case <-s.closed:
		return false
	}
}

// End makes the stream report EOF — a pw-record that exited on its own,
// which is what a headset being unplugged looks like from here.
func (s *FakeStream) End() {
	s.closeOnce.Do(func() { close(s.closed) })
}

// Read implements Stream, returning exactly one frame per call.
func (s *FakeStream) Read(p []byte) (int, error) {
	if len(s.pending) == 0 {
		select {
		case frame := <-s.frames:
			s.pending = make([]byte, len(frame)*2)
			for i, v := range frame {
				u := uint16(v)
				s.pending[2*i] = byte(u)
				s.pending[2*i+1] = byte(u >> 8)
			}
		case <-s.closed:
			return 0, io.EOF
		}
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// PID implements Stream.
func (s *FakeStream) PID() int {
	select {
	case <-s.closed:
		return 0
	default:
		return s.Pid
	}
}

// Close implements Stream.
func (s *FakeStream) Close() {
	s.mu.Lock()
	s.closes++
	s.mu.Unlock()
	s.End()
}

// Closes reports how many times the stream was closed — the assertion behind
// "muting killed the capture process".
func (s *FakeStream) Closes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closes
}
