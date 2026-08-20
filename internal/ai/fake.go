package ai

import (
	"context"
	"strings"
	"time"
)

// Fake is a scripted Provider for tests and offline development. It streams
// Response word by word, honouring cancellation between chunks.
type Fake struct {
	// Response is the full text to stream. Defaults to a canned sentence.
	Response string
	// Delay is inserted between chunks to simulate network streaming.
	Delay time.Duration
	// Fail, when set, makes Chat stream an error event after the first chunk.
	Fail error
	// LastRequest records the most recent request for assertions.
	LastRequest ChatRequest
}

// Name implements Provider.
func (f *Fake) Name() string { return "fake" }

// Chat implements Provider.
func (f *Fake) Chat(ctx context.Context, req ChatRequest) (<-chan Event, error) {
	f.LastRequest = req
	text := f.Response
	if text == "" {
		text = "This is a fake assistant response."
	}
	words := strings.SplitAfter(text, " ")
	ch := make(chan Event)
	go func() {
		defer close(ch)
		for i, w := range words {
			if f.Delay > 0 {
				select {
				case <-time.After(f.Delay):
				case <-ctx.Done():
					ch <- Event{Type: EventError, Err: ctx.Err()}
					return
				}
			}
			select {
			case ch <- Event{Type: EventDelta, Content: w}:
			case <-ctx.Done():
				ch <- Event{Type: EventError, Err: ctx.Err()}
				return
			}
			if f.Fail != nil && i == 0 {
				ch <- Event{Type: EventError, Err: f.Fail}
				return
			}
		}
		ch <- Event{Type: EventDone}
	}()
	return ch, nil
}
