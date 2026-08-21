package session

import (
	"context"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// collectTimings runs one interaction and returns the session.timings payload.
func collectTimings(t *testing.T, run func(e *Engine), opts Options) map[string]any {
	t.Helper()
	bus := NewBus(discardLogger())
	events, unsub := bus.Subscribe()
	defer unsub()
	engine := NewEngine(&ai.Fake{Response: "The answer is ready."}, &stt.Fake{Text: "what time is it"},
		&tts.Fake{}, &audio.FakeRecorder{}, &audio.FakePlayer{}, nil, nil, bus, discardLogger(), opts)

	run(engine)

	var timings map[string]any
	for ev := range events {
		if ev.Type == "session.timings" {
			timings = ev.Data
		}
		if ev.Type == "session.finished" {
			break
		}
	}
	return timings
}

func TestTimingsPublishedForASpokenVoiceSession(t *testing.T) {
	timings := collectTimings(t, func(e *Engine) {
		if _, err := e.StartSession(); err != nil {
			t.Fatal(err)
		}
		if err := e.StartVoice(); err != nil {
			t.Fatal(err)
		}
		if _, err := e.StopVoice(); err != nil {
			t.Fatal(err)
		}
		if err := e.Submit(""); err != nil {
			t.Fatal(err)
		}
	}, Options{Model: "m", SpeakResponses: true})

	if timings == nil {
		t.Fatal("no session.timings event was published")
	}
	// Every stage of the pipeline the ticket set a budget for.
	for _, stage := range []string{
		StageCaptureToTranscript,
		StageTranscriptToDelta,
		StageDeltaToFirstPCM,
		StageFirstPCMToAudioOut,
		StageReleaseToFirstAudio,
		StageJarvixOverhead,
	} {
		if _, ok := timings[stage]; !ok {
			t.Errorf("stage %q missing from %v", stage, timings)
		}
	}
	if timings["session_id"] != "s1" {
		t.Errorf("session_id = %v", timings["session_id"])
	}
}

func TestTimingsOmitStagesThatDidNotHappen(t *testing.T) {
	// `jarvix ask` never captures audio, so reporting a capture stage would be
	// a fabricated zero rather than a measurement.
	timings := collectTimings(t, func(e *Engine) {
		if _, err := e.StartSession(); err != nil {
			t.Fatal(err)
		}
		if err := e.Submit("what time is it"); err != nil {
			t.Fatal(err)
		}
	}, Options{Model: "m", SpeakResponses: true})

	if timings == nil {
		t.Fatal("no session.timings event was published")
	}
	if _, ok := timings[StageCaptureToTranscript]; ok {
		t.Error("a typed question reported a capture stage")
	}
	if _, ok := timings[StageReleaseToFirstAudio]; ok {
		t.Error("a typed question reported a release-to-audio total")
	}
	if _, ok := timings[StageDeltaToFirstPCM]; !ok {
		t.Error("the synthesis stage is measurable for a typed question and must be reported")
	}
}

func TestTimingsOmitAudioStagesWhenSpeechIsOff(t *testing.T) {
	timings := collectTimings(t, func(e *Engine) {
		if _, err := e.StartSession(); err != nil {
			t.Fatal(err)
		}
		if err := e.Submit("what time is it"); err != nil {
			t.Fatal(err)
		}
	}, Options{Model: "m", SpeakResponses: false})

	if timings == nil {
		t.Fatal("no session.timings event was published")
	}
	if _, ok := timings[StageFirstPCMToAudioOut]; ok {
		t.Error("a silent answer reported an audio-out stage")
	}
	if _, ok := timings[StageTranscriptToDelta]; !ok {
		t.Error("the provider stage still applies to a silent answer")
	}
}

func TestMarksAreOneWay(t *testing.T) {
	// "First delta" and "first PCM" must mean the first, even though the tool
	// loop streams several times in one session.
	var ti timings
	ti.markFirstDelta()
	first := ti.firstDelta
	time.Sleep(time.Millisecond)
	ti.markFirstDelta()
	if !ti.firstDelta.Equal(first) {
		t.Error("a later mark overwrote the first one")
	}
}

func TestReportSkipsInvertedSpans(t *testing.T) {
	// Marks land from several goroutines; a report must never invent a
	// negative duration out of an out-of-order pair.
	now := time.Now()
	ti := timings{captureStop: now, transcript: now.Add(-time.Second)}
	if _, ok := ti.report()[StageCaptureToTranscript]; ok {
		t.Error("an inverted span was reported")
	}
}

func TestAudioTraceReportsThroughTheFakePlayer(t *testing.T) {
	// The audio.Trace seam is what makes "first PCM → audio out" measurable at
	// all; if a player stops honouring it the stage silently disappears.
	fired := make(chan struct{}, 1)
	ctx := audio.WithTrace(context.Background(), &audio.Trace{
		FirstAudio: func() { fired <- struct{}{} },
	})
	chunks := make(chan []byte, 1)
	chunks <- []byte{0, 1, 2, 3}
	close(chunks)
	if err := (&audio.FakePlayer{}).Play(ctx, 24000, 1, chunks); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
	default:
		t.Error("the player never reported its first chunk")
	}
}
