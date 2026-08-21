package stt

import (
	"context"
	"errors"
	"testing"
)

func collect(t *testing.T, ch <-chan TranscriptEvent) []TranscriptEvent {
	t.Helper()
	var out []TranscriptEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func TestFakeEmitsPartialsThenFinalAndCloses(t *testing.T) {
	f := &Fake{Text: "hello world", Partials: []string{"hel", "hello wor"}}
	input := AudioInput{WAVPath: "/tmp/x.wav", SampleRate: 16000, Channels: 1}
	ch, err := f.Transcribe(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, ch)

	if len(events) != 3 {
		t.Fatalf("events = %+v", events)
	}
	for i, want := range []string{"hel", "hello wor"} {
		if events[i].Type != EventPartial || events[i].Text != want {
			t.Errorf("event %d = %+v, want partial %q", i, events[i], want)
		}
	}
	final := events[2]
	if final.Type != EventFinal || final.Text != "hello world" {
		t.Errorf("final = %+v", final)
	}
	if f.LastInput != input {
		t.Errorf("LastInput = %+v, want %+v", f.LastInput, input)
	}
	if f.Name() != "fake" {
		t.Errorf("name = %q", f.Name())
	}
}

func TestFakeFailEndsStreamWithError(t *testing.T) {
	boom := errors.New("engine crashed")
	f := &Fake{Text: "unused", Fail: boom}
	ch, err := f.Transcribe(context.Background(), AudioInput{})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, ch)
	if len(events) != 1 || events[0].Type != EventError || !errors.Is(events[0].Err, boom) {
		t.Fatalf("events = %+v, want only the scripted error", events)
	}
}

func TestFakeCancellationEndsStreamWithError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &Fake{Text: "hello", Partials: []string{"h", "he", "hel", "hell", "hello"}}
	ch, err := f.Transcribe(ctx, AudioInput{})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, ch) // the channel must close even when cancelled
	last := events[len(events)-1]
	if last.Type == EventError {
		if !errors.Is(last.Err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", last.Err)
		}
		return
	}
	// A racing select may legitimately deliver everything before noticing
	// cancellation; the contract is only that the stream terminates.
	if last.Type != EventFinal {
		t.Fatalf("stream ended with %+v, want final or cancellation error", last)
	}
}
