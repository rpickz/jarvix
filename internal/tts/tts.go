// Package tts defines the provider-independent text-to-speech interface.
//
// A Synthesizer streams raw PCM audio chunks so playback can begin before the
// whole utterance is rendered, and so a future engine can speak sentence
// fragments while the assistant is still responding. V1 ships a Piper
// adapter; Kokoro is planned behind the same interface.
package tts

import "context"

// Format describes PCM audio: signed 16-bit little-endian samples.
type Format struct {
	SampleRate int
	Channels   int
}

// Request is one utterance to synthesize.
type Request struct {
	Text  string
	Voice string // engine-specific voice identifier; empty = configured default
}

// Chunk is one element of a synthesized audio stream. The channel is closed
// when synthesis ends; a failed synthesis ends with a chunk carrying Err.
type Chunk struct {
	PCM []byte
	Err error
}

// Synthesizer converts text to audio.
//
// Speak returns the stream format alongside the channel because the format is
// a property of the engine/voice, known before audio arrives. Implementations
// must close the channel when done and stop promptly when ctx is cancelled —
// interrupting speech is a first-class operation in Jarvix.
type Synthesizer interface {
	Name() string
	Speak(ctx context.Context, req Request) (Format, <-chan Chunk, error)
}
