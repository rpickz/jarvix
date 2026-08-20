// Package stt defines the provider-independent speech-to-text interface.
//
// The rest of Jarvix does not know which engine transcribes audio. V1 ships a
// whisper.cpp adapter; future implementations (OpenAI transcription,
// faster-whisper, Deepgram, ...) implement the same interface.
package stt

import "context"

// AudioInput describes captured audio to transcribe. V1 passes a WAV file on
// disk (recordings land in $XDG_RUNTIME_DIR, i.e. tmpfs); a future streaming
// engine can add a reader-based input alongside without breaking callers.
type AudioInput struct {
	// WAVPath is the path to a RIFF/WAV file.
	WAVPath string
	// SampleRate and Channels describe the PCM data (informational; the WAV
	// header is authoritative).
	SampleRate int
	Channels   int
}

// EventType classifies transcript events.
type EventType string

// Transcript event types.
const (
	EventPartial EventType = "partial" // interim hypothesis, may be revised
	EventFinal   EventType = "final"   // completed transcript segment
	EventError   EventType = "error"
)

// TranscriptEvent is one element of a transcription stream. The channel is
// closed after a final or error event.
type TranscriptEvent struct {
	Type EventType
	Text string
	Err  error
}

// Transcriber converts audio to text.
//
// Implementations must close the returned channel when transcription ends and
// stop promptly when ctx is cancelled.
type Transcriber interface {
	Name() string
	Transcribe(ctx context.Context, input AudioInput) (<-chan TranscriptEvent, error)
}
