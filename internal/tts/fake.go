package tts

import "context"

// Fake is a scripted Synthesizer for tests.
type Fake struct {
	// Chunks is the PCM data to stream, one element per chunk. Defaults to a
	// single small chunk when empty.
	Chunks [][]byte
	// Fail, when set, ends the stream with an error chunk.
	Fail error
	// LastRequest records the most recent request for assertions.
	LastRequest Request
}

// Name implements Synthesizer.
func (f *Fake) Name() string { return "fake" }

// Speak implements Synthesizer.
func (f *Fake) Speak(ctx context.Context, req Request) (Format, <-chan Chunk, error) {
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
