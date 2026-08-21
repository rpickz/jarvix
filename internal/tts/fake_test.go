package tts

import (
	"context"
	"errors"
	"testing"
)

func collect(t *testing.T, ch <-chan Chunk) []Chunk {
	t.Helper()
	var out []Chunk
	for c := range ch {
		out = append(out, c)
	}
	return out
}

func TestFakeStreamsScriptedChunks(t *testing.T) {
	f := &Fake{Chunks: [][]byte{[]byte("ab"), []byte("cd")}}
	format, ch, err := f.Speak(context.Background(), Request{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if format.SampleRate != 22050 || format.Channels != 1 {
		t.Errorf("format = %+v", format)
	}
	chunks := collect(t, ch)
	if len(chunks) != 2 || string(chunks[0].PCM) != "ab" || string(chunks[1].PCM) != "cd" {
		t.Errorf("chunks = %+v", chunks)
	}
	if f.LastRequest.Text != "hello" {
		t.Errorf("LastRequest = %+v", f.LastRequest)
	}
	if f.Speaks() != 1 {
		t.Errorf("speaks = %d", f.Speaks())
	}
	if f.Name() != "fake" {
		t.Errorf("name = %q", f.Name())
	}
}

func TestFakeDefaultsToOneChunk(t *testing.T) {
	f := &Fake{}
	_, ch, err := f.Speak(context.Background(), Request{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	chunks := collect(t, ch)
	if len(chunks) != 1 || len(chunks[0].PCM) == 0 {
		t.Errorf("chunks = %+v, want one non-empty default chunk", chunks)
	}
}

func TestFakeFailEndsStreamWithErrorChunk(t *testing.T) {
	boom := errors.New("synth exploded")
	f := &Fake{Fail: boom}
	_, ch, err := f.Speak(context.Background(), Request{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	chunks := collect(t, ch)
	if len(chunks) != 1 || !errors.Is(chunks[0].Err, boom) {
		t.Fatalf("chunks = %+v, want only the scripted error", chunks)
	}
}

func TestFakeCancellationEndsStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &Fake{Chunks: [][]byte{[]byte("a"), []byte("b"), []byte("c")}}
	_, ch, err := f.Speak(ctx, Request{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	chunks := collect(t, ch) // channel must close even when cancelled
	last := chunks[len(chunks)-1]
	if last.Err != nil && !errors.Is(last.Err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", last.Err)
	}
}

func TestFakeCountsEverySpeak(t *testing.T) {
	f := &Fake{}
	for i := 0; i < 3; i++ {
		_, ch, err := f.Speak(context.Background(), Request{Text: "x"})
		if err != nil {
			t.Fatal(err)
		}
		collect(t, ch)
	}
	if f.Speaks() != 3 {
		t.Errorf("speaks = %d, want 3", f.Speaks())
	}
}
