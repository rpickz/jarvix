package tts

import (
	"context"
	"sync"
	"time"
)

// Fake is a scripted Synthesizer for tests.
type Fake struct {
	// Chunks is the PCM data to stream, one element per chunk. Defaults to a
	// single small chunk when empty.
	Chunks [][]byte
	// Fail, when set, ends the stream with an error chunk.
	Fail error
	// Delay simulates synthesis/playback duration by pausing before each
	// chunk, giving tests a window to cancel while speech is in progress.
	Delay time.Duration
	// LastRequest records the most recent request for assertions.
	LastRequest Request

	mu     sync.Mutex
	speaks int
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
	f.mu.Unlock()
	f.LastRequest = req
	chunks := f.Chunks
	if len(chunks) == 0 {
		chunks = [][]byte{make([]byte, 32)}
	}
	delay := f.Delay
	ch := make(chan Chunk)
	go func() {
		defer close(ch)
		if f.Fail != nil {
			ch <- Chunk{Err: f.Fail}
			return
		}
		for _, c := range chunks {
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					ch <- Chunk{Err: ctx.Err()}
					return
				}
			}
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
