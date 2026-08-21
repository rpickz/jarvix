// Package wake implements background wake-word listening: saying "Jarvix, …"
// mid-conversation activates the assistant with no keyboard involved.
//
// # What this package promises about your audio
//
// Everything below is the feature. The wake word is the easy part; being
// trustworthy while the microphone is open is the hard part, and it is why
// this doc comment leads with the data lifecycle rather than the algorithm
// (ADR 0024).
//
//  1. **Detection is local.** Frames go to a detector process running on this
//     machine and nowhere else. No network path exists in this package.
//  2. **Pre-wake audio lives only in RAM, in a fixed-size ring** (Ring),
//     allocated once at construction and never grown. Its size is capped at
//     MaxRingDuration. Nothing before a wake detection is ever written to
//     disk, and nothing is ever logged: wake events carry a timestamp and a
//     confidence, never audio and never text.
//  3. **Only post-wake audio is materialised**, and only as far as the tmpfs
//     runtime directory, which the session engine deletes the moment
//     transcription finishes — exactly what a push-to-talk capture does.
//  4. **Every buffer is wiped after use.** The ring is zeroed when an
//     utterance is taken from it, when capture stops, and when the listener
//     is muted; the utterance buffer is zeroed after it is handed over.
//  5. **Muting kills the capture process.** Not a flag that makes Jarvix
//     ignore what it hears: Listener.Mute closes the stream and returns only
//     once pw-record has been reaped, so "no capture process is running" is
//     verifiable in the process table rather than promised in a comment.
//
// # Shape
//
// The listener owns two supervised children (ADR 0018), for the same reason
// the STT and TTS engines are supervised: they are external processes that
// can die, and neither death may take the daemon — or push-to-talk — with it.
//
//	pw-record ──frames──► Ring ──► Detector ──► Policy ──► OnWake
//	                        │                                 │
//	                        └──── pre-roll ──► utterance ◄─────┘
//	                                              │
//	                                        Endpointer ──► OnUtterance
//
// Detection is behind the Detector interface so the whole state machine is
// testable with a fake fed PCM fixtures. No test in this repository opens a
// microphone or loads a model.
package wake

import (
	"context"
	"time"
)

// The capture format, fixed to what whisper.cpp wants and what every
// openWakeWord-class model is trained on. There is no reason to make it
// configurable and one good reason not to: a mismatch between the detector's
// expectation and the stream is silent, and shows up only as a wake word that
// never fires.
const (
	// SampleRate is the capture rate in Hz.
	SampleRate = 16000
	// Channels is the capture channel count.
	Channels = 1
	// FrameSamples is the analysis window handed to the detector at a time.
	// 1280 samples is 80 ms, which is openWakeWord's native chunk size; a
	// detector that wants a different window buffers internally.
	FrameSamples = 1280
	// FrameBytes is FrameSamples as s16le bytes.
	FrameBytes = FrameSamples * 2
	// FrameDuration is how much wall-clock time one frame represents.
	FrameDuration = time.Duration(FrameSamples) * time.Second / SampleRate
)

// MaxRingDuration is the hard ceiling on pre-wake retention, enforced here
// rather than left to configuration validation alone. It is a privacy
// boundary: whatever the config file says, audio from more than this long
// before the wake word cannot be reached, because it no longer exists.
const MaxRingDuration = 3 * time.Second

// Detector scores audio for the presence of the wake word.
//
// It is an interface for two reasons. The first is testing: every test in
// this package drives a fake, so the state machine is exercised deterministically
// without a model, a microphone, or a subprocess. The second is that the
// engine choice is genuinely open — an openWakeWord-class ONNX model today, a
// different one tomorrow — and ADR 0024 records that trade-off as a decision
// rather than an accident of what was easiest to link against.
//
// It embeds the warm supervisor's Child contract (PID, Close) so a detector
// backed by a helper process can be supervised, memory-capped, and restarted
// with backoff by the machinery that already does that for whisper and Kokoro.
type Detector interface {
	// Name identifies the engine for logs, `jarvix status`, and doctor.
	Name() string
	// Score consumes exactly one frame of FrameSamples 16 kHz mono samples
	// and reports the model's confidence, 0..1, that the wake word has just
	// been spoken. An error means the detector is unusable and should be
	// replaced; the caller discards it and re-spawns with backoff.
	//
	// Implementations must not retain the frame: the caller reuses it.
	Score(frame []int16) (float64, error)
	// PID is the detector process id, or 0 for an in-process detector — which
	// also opts it out of the resident-memory cap.
	PID() int
	// Close releases the detector. Safe to call more than once.
	Close()
}

// Source opens a live microphone stream. The wake listener's only contact
// with audio hardware, so a fake Source is all a test needs to run the entire
// feature end to end.
type Source interface {
	// Open starts capture. The context bounds start-up only; the returned
	// stream belongs to the caller until it is closed.
	Open(ctx context.Context) (Stream, error)
}

// Stream is one live capture: an unbounded run of raw s16le PCM.
type Stream interface {
	// Read fills p with PCM bytes, blocking until some arrive.
	Read(p []byte) (int, error)
	// PID is the capture process's id, 0 when it is not a process (a fake) or
	// has already exited. This is the number that makes the privacy claim
	// checkable: `jarvix status` prints it, and `ps` either shows it or does
	// not.
	PID() int
	// Close stops capture and returns once the process is gone.
	Close()
}

// decodeFrame converts a frame of little-endian s16 bytes into samples,
// writing into dst (which must hold len(src)/2 samples) rather than
// allocating: this runs 12.5 times a second for as long as the daemon is
// listening, and the whole point of the idle-CPU budget is that background
// listening costs nothing anyone can feel.
func decodeFrame(src []byte, dst []int16) {
	for i := range dst {
		dst[i] = int16(uint16(src[2*i]) | uint16(src[2*i+1])<<8)
	}
}

// wipe zeroes a sample buffer. Called wherever audio stops being needed, so
// that "the buffer is cleared" is a line of code rather than a claim about
// what will eventually be overwritten.
func wipe(buf []int16) {
	for i := range buf {
		buf[i] = 0
	}
}
