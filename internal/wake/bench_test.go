package wake

import (
	"testing"
	"time"
)

// Benchmarks for the idle-CPU budget: "detection ≤5% of one core".
//
// That budget has two halves and only one of them is Jarvix's. The model's
// inference cost belongs to whichever detector is installed and is measured
// by running it (ADR 0024 says how); what is measured here is everything
// else — the per-frame work between the capture process and the detector,
// which runs 12.5 times a second for as long as background listening is on.
//
// The useful figure is the ratio: nanoseconds of work per 80 ms of audio.

// BenchmarkFramePipeline is one frame through the listening path: decode from
// the wire, hold it in the ring, score it (with a detector that costs
// nothing, so the number is Jarvix's own overhead), and gate the result.
func BenchmarkFramePipeline(b *testing.B) {
	l := New(&FakeSource{}, nil, testOptionsForBench(), Hooks{}, discardLogger())
	detector := &ScriptedDetector{ScoreFunc: func([]int16, int) (float64, error) { return 0.01, nil }}
	raw := make([]byte, FrameBytes)
	frame := make([]int16, FrameSamples)
	fixture := roomTone(1, 1)
	for i, s := range fixture {
		raw[2*i] = byte(uint16(s))
		raw[2*i+1] = byte(uint16(s) >> 8)
	}

	b.ReportAllocs()
	for b.Loop() {
		decodeFrame(raw, frame)
		if err := l.processFrame(detector, frame); err != nil {
			b.Fatal(err)
		}
	}
	perFrame := float64(b.Elapsed().Nanoseconds()) / float64(b.N)
	b.ReportMetric(perFrame/float64(FrameDuration.Nanoseconds())*100, "%core")
}

// BenchmarkEndpointFrame is the other per-frame cost: deciding whether the
// user is still talking. It runs only during a request rather than
// continuously, so it is not part of the idle budget — but it is on the path
// between someone finishing a sentence and Jarvix noticing.
func BenchmarkEndpointFrame(b *testing.B) {
	e := &Endpointer{Silence: DefaultSilence, Lead: DefaultLead, Max: DefaultMaxUtterance}
	frame := utterance(1, 2)
	b.ReportAllocs()
	for b.Loop() {
		e.Push(frame)
	}
}

func testOptionsForBench() Options {
	return Options{Word: "jarvix", Sensitivity: DefaultSensitivity, RingDuration: DefaultRing}
}

// The budget as a guard rather than as a report. The real figure is
// microseconds — four orders of magnitude inside the frame period — so a
// bound of one tenth of a frame cannot flake, and it would catch the kind of
// regression that matters here: an allocation per frame, an accidental copy
// of the whole ring, a lock held across the detector.
func TestFrameWorkFitsComfortablyInsideTheFramePeriod(t *testing.T) {
	l := New(&FakeSource{}, nil, testOptionsForBench(), Hooks{}, discardLogger())
	detector := &ScriptedDetector{ScoreFunc: func([]int16, int) (float64, error) { return 0.01, nil }}
	frame := roomTone(1, 3)

	const rounds = 2000
	start := time.Now()
	for i := 0; i < rounds; i++ {
		if err := l.processFrame(detector, frame); err != nil {
			t.Fatal(err)
		}
	}
	perFrame := time.Since(start) / rounds
	t.Logf("per-frame pipeline cost: %v (%.4f%% of one core; the frame period is %v)",
		perFrame, float64(perFrame)/float64(FrameDuration)*100, FrameDuration)
	if budget := FrameDuration / 10; perFrame > budget {
		t.Errorf("one frame costs %v, budget %v — background listening is meant to be unnoticeable",
			perFrame, budget)
	}
}
