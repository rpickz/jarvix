package audio

import (
	"context"
	"errors"
	"testing"
)

// The fakes back every session-engine test; these contract tests prove the
// fakes themselves honour the Recorder/Player interfaces they stand in for.

func TestFakeRecorderScriptsTheClip(t *testing.T) {
	clip := Clip{WAVPath: "/tmp/x.wav", SampleRate: 16000, Channels: 1}
	f := &FakeRecorder{Clip: clip}
	rec, err := f.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := rec.Stop()
	if err != nil {
		t.Fatal(err)
	}
	if got != clip {
		t.Errorf("clip = %+v, want %+v", got, clip)
	}
	if started, stopped, cancelled := f.Counts(); started != 1 || stopped != 1 || cancelled != 0 {
		t.Errorf("counts = %d/%d/%d", started, stopped, cancelled)
	}
}

func TestFakeRecorderErrorsAndCancel(t *testing.T) {
	f := &FakeRecorder{StartErr: errors.New("no mic")}
	if _, err := f.Start(context.Background()); err == nil {
		t.Error("StartErr must surface")
	}

	f = &FakeRecorder{StopErr: errors.New("bad stop")}
	rec, err := f.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Stop(); err == nil {
		t.Error("StopErr must surface")
	}
	rec.Cancel()
	if _, _, cancelled := f.Counts(); cancelled != 1 {
		t.Errorf("cancelled = %d", cancelled)
	}
}

func TestFakePlayerRecordsChunks(t *testing.T) {
	f := &FakePlayer{}
	chunks := make(chan []byte, 2)
	chunks <- []byte("ab")
	chunks <- []byte("cd")
	close(chunks)
	if err := f.Play(context.Background(), 22050, 1, chunks); err != nil {
		t.Fatal(err)
	}
	played, plays := f.Played()
	if plays != 1 || len(played) != 2 || string(played[0]) != "ab" || string(played[1]) != "cd" {
		t.Errorf("played = %q, plays = %d", played, plays)
	}
}

func TestFakePlayerErrorStillDrains(t *testing.T) {
	f := &FakePlayer{PlayErr: errors.New("sink gone")}
	chunks := make(chan []byte, 1)
	chunks <- []byte("x")
	close(chunks)
	// A failing player must still drain so producers never block.
	if err := f.Play(context.Background(), 22050, 1, chunks); err == nil {
		t.Error("PlayErr must surface")
	}
}

func TestFakePlayerHonoursCancellation(t *testing.T) {
	f := &FakePlayer{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	chunks := make(chan []byte) // never closed: only cancellation can end Play
	if err := f.Play(ctx, 22050, 1, chunks); err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
