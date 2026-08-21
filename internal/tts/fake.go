package tts

import (
	"context"
	"sync"
)

// Fake is a scripted Synthesizer for tests.
type Fake struct {
	// Chunks is the PCM data to stream, one element per chunk. Defaults to a
	// single small chunk when empty.
	Chunks [][]byte
	// Fail, when set, ends the stream with an error chunk.
	Fail error
	// LastRequest records the most recent request for assertions.
	LastRequest Request

	mu     sync.Mutex
	speaks int
	hold   chan struct{}
}

// SetHold installs a gate that blocks chunk delivery until the channel is
// closed (or the speak context is cancelled). Tests use it to hold speech
// "in progress" deterministically — never with timers, which race on loaded
// machines. SetHold(nil) removes the gate for subsequent Speak calls.
func (f *Fake) SetHold(ch chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hold = ch
}

// Name implements Synthesizer.
func (f *Fake) Name() string { return "fake" }

// Speaks reports how many times Speak was called — one per sentence when the
// engine streams speech.
func (f *Fake) Speaks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.speaks
}

// Speak implements Synthesizer.
func (f *Fake) Speak(ctx context.Context, req Request) (Format, <-chan Chunk, error) {
	f.mu.Lock()
	f.speaks++
	hold := f.hold
	f.mu.Unlock()
	f.LastRequest = req
	chunks := f.Chunks
	if len(chunks) == 0 {
		chunks = [][]byte{make([]byte, 32)}
	}
	ch := make(chan Chunk)
	go func() {
		defer close(ch)
		if f.Fail != nil {
			ch <- Chunk{Err: f.Fail}
			return
		}
		if hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
				ch <- Chunk{Err: ctx.Err()}
				return
			}
		}
		for _, c := range chunks {
			select {
			case ch <- Chunk{PCM: c}:
			case <-ctx.Done():
				ch <- Chunk{Err: ctx.Err()}
				return
			}
		}
	}()
	return Format{SampleRate: 22050, Channels: 1}, ch, nil
}
