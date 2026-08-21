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
	// open is how many Play calls are running right now, and peak the most
	// there have ever been at once. A real machine has one pair of speakers, so
	// two concurrent streams are two voices talking over each other — audible
	// to a user, invisible to a test that only counts what was played. They are
	// tracked here so a test can assert the thing the user actually cares about
	// (issue #52).
	open int
	peak int
}

// Play implements Player. It drains the channel, honouring cancellation.
func (f *FakePlayer) Play(ctx context.Context, sampleRate, channels int, chunks <-chan []byte) error {
	f.mu.Lock()
	f.plays++
	f.open++
	if f.open > f.peak {
		f.peak = f.open
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.open--
		f.mu.Unlock()
	}()
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

// PeakConcurrentPlays reports the most Play calls that were ever in flight at
// the same time. Anything above 1 is two voices at once.
func (f *FakePlayer) PeakConcurrentPlays() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}
