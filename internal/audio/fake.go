package audio

import (
	"context"
	"sync"
)

// FakeRecorder is a scripted Recorder for tests.
type FakeRecorder struct {
	// Clip is returned by Stop.
	Clip Clip
	// StartErr, when set, makes Start fail.
	StartErr error
	// StopErr, when set, makes Stop fail.
	StopErr error

	mu        sync.Mutex
	started   int
	stopped   int
	cancelled int
}

// Start implements Recorder.
func (f *FakeRecorder) Start(ctx context.Context) (Recording, error) {
	if f.StartErr != nil {
		return nil, f.StartErr
	}
	f.mu.Lock()
	f.started++
	f.mu.Unlock()
	return &fakeRecording{parent: f}, nil
}

// Counts reports how many recordings were started, stopped, and cancelled.
func (f *FakeRecorder) Counts() (started, stopped, cancelled int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started, f.stopped, f.cancelled
}

type fakeRecording struct{ parent *FakeRecorder }

func (r *fakeRecording) Stop() (Clip, error) {
	r.parent.mu.Lock()
	r.parent.stopped++
	r.parent.mu.Unlock()
	if r.parent.StopErr != nil {
		return Clip{}, r.parent.StopErr
	}
	return r.parent.Clip, nil
}

func (r *fakeRecording) Cancel() {
	r.parent.mu.Lock()
	r.parent.cancelled++
	r.parent.mu.Unlock()
}

// FakePlayer records what it is asked to play.
type FakePlayer struct {
	// PlayErr, when set, is returned by Play.
	PlayErr error

	mu     sync.Mutex
	played [][]byte
	plays  int
}

// Play implements Player. It drains the channel, honouring cancellation.
func (f *FakePlayer) Play(ctx context.Context, sampleRate, channels int, chunks <-chan []byte) error {
	f.mu.Lock()
	f.plays++
	f.mu.Unlock()
	if f.PlayErr != nil {
		// Drain so producers do not block.
		for range chunks {
		}
		return f.PlayErr
	}
	first := true
	for {
		select {
		case c, ok := <-chunks:
			if !ok {
				return nil
			}
			f.mu.Lock()
			f.played = append(f.played, c)
			f.mu.Unlock()
			if first {
				// The fake honours audio.Trace too, so the latency plumbing is
				// exercised by every engine test rather than only in front of
				// a real sound card.
				first = false
				firstAudio(ctx)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Played returns the chunks played and the number of Play calls.
func (f *FakePlayer) Played() (chunks [][]byte, plays int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.played, f.plays
}
