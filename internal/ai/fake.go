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
	// Preamble is streamed as text at the start of a scripted tool round, the
	// way a real model narrates before it acts ("I'll check that for you. ").
	// Empty — the default — means a tool round emits no text at all.
	//
	// It matters because a preamble is what makes the engine start *speaking*
	// before the tool call arrives, which is the ordering issue #52 was about:
	// without it no test could reach the state a live session reaches
	// routinely. End it with punctuation and a trailing space if the engine's
	// sentence splitter should emit it as a complete sentence.
	Preamble string
	// BeforeToolCalls, when set, runs after the preamble has been streamed and
	// before the round's tool calls are emitted. Tests use it to park the
	// provider until the engine has reached a particular state — speech
	// already playing, say — so the ordering under test is guaranteed rather
	// than raced for with a sleep.
	BeforeToolCalls func(ctx context.Context)
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

	// A scripted tool round: emit the calls (preceded by Preamble, when set)
	// instead of an answer.
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
			if f.Preamble != "" {
				select {
				case ch <- Event{Type: EventDelta, Content: f.Preamble}:
				case <-ctx.Done():
					ch <- Event{Type: EventError, Err: ctx.Err()}
					return
				}
			}
			if f.BeforeToolCalls != nil {
				f.BeforeToolCalls(ctx)
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
