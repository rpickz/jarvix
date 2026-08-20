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
	// ToolCallsByRound scripts tool calls per Chat invocation: the calls for
	// round N are emitted on the (N+1)th Chat call, then the provider streams
	// Response on the round with no scripted calls. This drives the engine's
	// tool loop in tests.
	ToolCallsByRound [][]ToolCall
	// Requests records every request for assertions (one per round).
	Requests []ChatRequest
	// LastRequest is the most recent request.
	LastRequest ChatRequest

	round int
}

// Name implements Provider.
func (f *Fake) Name() string { return "fake" }

// Chat implements Provider.
func (f *Fake) Chat(ctx context.Context, req ChatRequest) (<-chan Event, error) {
	f.LastRequest = req
	f.Requests = append(f.Requests, req)
	round := f.round
	f.round++

	// A scripted tool round: emit the calls (after a short text preamble so
	// tool rounds exercise the responded→thinking path) instead of an answer.
	var calls []ToolCall
	if round < len(f.ToolCallsByRound) {
		calls = f.ToolCallsByRound[round]
	}

	text := f.Response
	if text == "" {
		text = "This is a fake assistant response."
	}
	ch := make(chan Event)
	go func() {
		defer close(ch)
		if len(calls) == 0 {
			// Final answer round: stream the text word by word.
			for i, w := range strings.SplitAfter(text, " ") {
				if !f.pace(ctx, ch) {
					return
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
		} else {
			if f.Fail != nil {
				ch <- Event{Type: EventError, Err: f.Fail}
				return
			}
			for _, call := range calls {
				select {
				case ch <- Event{Type: EventToolCall, Call: call}:
				case <-ctx.Done():
					ch <- Event{Type: EventError, Err: ctx.Err()}
					return
				}
			}
		}
		ch <- Event{Type: EventDone}
	}()
	return ch, nil
}

func (f *Fake) pace(ctx context.Context, ch chan<- Event) bool {
	if f.Delay <= 0 {
		return true
	}
	select {
	case <-time.After(f.Delay):
		return true
	case <-ctx.Done():
		ch <- Event{Type: EventError, Err: ctx.Err()}
		return false
	}
}
