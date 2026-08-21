package ai

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// collect drains a stream into its events.
func collect(t *testing.T, ch <-chan Event) []Event {
	t.Helper()
	var out []Event
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func TestFakeStreamsResponseWordByWordThenDone(t *testing.T) {
	f := &Fake{Response: "one two three"}
	ch, err := f.Chat(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, ch)

	var text strings.Builder
	for i, ev := range events {
		switch {
		case i == len(events)-1:
			if ev.Type != EventDone {
				t.Fatalf("last event = %+v, want done", ev)
			}
		case ev.Type == EventDelta:
			text.WriteString(ev.Content)
		default:
			t.Fatalf("unexpected event %+v", ev)
		}
	}
	if text.String() != "one two three" {
		t.Errorf("streamed %q", text.String())
	}
	// More than one delta: it must actually stream, not send one blob.
	if len(events) < 3 {
		t.Errorf("only %d events; response did not stream", len(events))
	}
	if f.Name() != "fake" {
		t.Errorf("name = %q", f.Name())
	}
}

func TestFakeDefaultsResponseWhenEmpty(t *testing.T) {
	f := &Fake{}
	ch, err := f.Chat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for ev := range ch {
		if ev.Type == EventDelta {
			text.WriteString(ev.Content)
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		t.Error("empty Response must stream a canned default")
	}
}

func TestFakeRecordsEveryRequest(t *testing.T) {
	f := &Fake{Response: "hi"}
	req1 := ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "q1"}}}
	req2 := ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "q2"}}}
	for _, req := range []ChatRequest{req1, req2} {
		ch, err := f.Chat(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		collect(t, ch)
	}
	if len(f.Requests) != 2 {
		t.Fatalf("recorded %d requests", len(f.Requests))
	}
	if f.LastRequest.Messages[0].Content != "q2" {
		t.Errorf("LastRequest = %+v", f.LastRequest)
	}
}

func TestFakeFailStreamsErrorAfterFirstChunk(t *testing.T) {
	boom := errors.New("model exploded")
	f := &Fake{Response: "one two", Fail: boom}
	ch, err := f.Chat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, ch)
	last := events[len(events)-1]
	if last.Type != EventError || !errors.Is(last.Err, boom) {
		t.Fatalf("last event = %+v, want the scripted error", last)
	}
	if events[0].Type != EventDelta {
		t.Errorf("first event = %+v, want a delta before the failure", events[0])
	}
}

func TestFakeScriptedToolRoundsThenAnswer(t *testing.T) {
	call := ToolCall{ID: "c1", Name: "run", Arguments: `{"command":"docker ps"}`}
	f := &Fake{Response: "three containers", ToolCallsByRound: [][]ToolCall{{call}}}

	// Round 0: the scripted tool call instead of an answer.
	ch, err := f.Chat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, ch)
	if events[0].Type != EventToolCall || events[0].Call != call {
		t.Fatalf("round 0 events = %+v, want the tool call", events)
	}
	if events[len(events)-1].Type != EventDone {
		t.Fatalf("round 0 must end with done: %+v", events)
	}

	// Round 1: no script left, so the final answer streams.
	ch, err = f.Chat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	sawDelta := false
	for ev := range ch {
		if ev.Type == EventToolCall {
			t.Fatalf("round 1 emitted a tool call: %+v", ev)
		}
		if ev.Type == EventDelta {
			sawDelta = true
		}
	}
	if !sawDelta {
		t.Error("round 1 must stream the response text")
	}
}

func TestFakeCancellationEndsStreamWithError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &Fake{Response: strings.Repeat("word ", 100)}
	ch, err := f.Chat(ctx, ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, ch) // channel must still close after cancellation
	last := events[len(events)-1]
	if last.Type != EventError || !errors.Is(last.Err, context.Canceled) {
		t.Fatalf("last event = %+v, want a cancellation error", last)
	}
}

func TestFakePacingDelayStillStreamsAndCancels(t *testing.T) {
	// A tiny delay exercises the pacing path; the content must be unaffected.
	f := &Fake{Response: "a b", Delay: time.Microsecond}
	ch, err := f.Chat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, ch)
	if events[len(events)-1].Type != EventDone {
		t.Fatalf("events = %+v", events)
	}

	// Cancellation during the pacing wait ends the stream with an error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f = &Fake{Response: "a b", Delay: time.Hour}
	ch, err = f.Chat(ctx, ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	events = collect(t, ch)
	last := events[len(events)-1]
	if last.Type != EventError || !errors.Is(last.Err, context.Canceled) {
		t.Fatalf("last = %+v, want cancellation error", last)
	}
}

func TestFakeFailDuringToolRound(t *testing.T) {
	boom := errors.New("tool round failed")
	f := &Fake{Fail: boom, ToolCallsByRound: [][]ToolCall{{{ID: "c", Name: "run"}}}}
	ch, err := f.Chat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, ch)
	if len(events) != 1 || events[0].Type != EventError || !errors.Is(events[0].Err, boom) {
		t.Fatalf("events = %+v, want only the scripted error", events)
	}
}
