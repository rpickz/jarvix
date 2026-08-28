// Package audio defines Jarvix's capture and playback interfaces and their
// PipeWire implementation.
//
// The interfaces are deliberately narrow so audio implementation details do
// not leak into the session engine, and so tests run without hardware.
// Jarvix targets PipeWire on Linux directly; there is no cross-platform
// abstraction (ADR 0003).
package audio

import "context"

// Clip is a finished recording.
type Clip struct {
	// WAVPath points at a RIFF/WAV file. Recordings live under
	// $XDG_RUNTIME_DIR/jarvix (tmpfs) and are deleted after transcription.
	WAVPath    string
	SampleRate int
	Channels   int
}

// Recording is an in-progress capture.
type Recording interface {
	// Stop ends capture and returns the recorded clip.
	Stop() (Clip, error)
	// Cancel ends capture and discards the clip.
	Cancel()
}

// Recorder captures microphone audio.
type Recorder interface {
	// Start begins capturing. Capture ends via the Recording, or when ctx is
	// cancelled (equivalent to Cancel).
	Start(ctx context.Context) (Recording, error)
}

// Player renders raw s16le PCM.
type Player interface {
	// Play consumes PCM chunks until the channel closes, then returns once
	// playback has drained. Cancelling ctx stops playback immediately.
	//
	// Play consumes the channel even when playback fails: producers block on
	// the handoff and read the result only after the channel closes (the
	// speaker's shape), so an implementation that returned early without
	// draining would wedge its caller for the life of the session (issue
	// #142). On failure, keep consuming until the channel closes or ctx is
	// cancelled, then return the error.
	Play(ctx context.Context, sampleRate, channels int, chunks <-chan []byte) error
}
