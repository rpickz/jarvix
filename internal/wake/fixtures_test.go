package wake

import (
	"log/slog"
	"math"
	"math/rand"
	"testing"
	"time"
)

// The fixture corpus. Every test in this package is fed audio generated here:
// deterministic, seeded, and synthetic, so a run on a laptop and a run on a
// CI box process byte-identical samples.
//
// Synthetic rather than recorded is a deliberate limitation, not an oversight.
// What these fixtures can prove is everything Jarvix owns — that the ring
// holds what it should and nothing more, that the endpointer submits on
// silence, that the activation policy converts a score stream into the
// activations it claims to. What they cannot prove is how well a *model*
// tells "Jarvix" from a cough, because no synthetic waveform stands in for
// that. ADR 0024 says so plainly and says how to measure it for real.

// roomTone is ambient noise: a quiet room, a fan, someone typing next door.
// Loud enough to be a realistic noise floor, far too quiet to be speech.
func roomTone(frames int, seed int64) []int16 {
	return noise(frames, seed, 60)
}

// utterance is speech-shaped audio: two orders of magnitude above room tone,
// which is what the endpointer's ratio test is looking for.
func utterance(frames int, seed int64) []int16 {
	return noise(frames, seed, 6000)
}

// silence is digital silence — every sample zero. The hardest case for an
// energy endpointer, because a naive noise floor collapses to zero and then
// every stray sample looks like speech.
func silence(frames int) []int16 { return make([]int16, frames*FrameSamples) }

// noise builds `frames` frames whose samples are uniform in ±amplitude, from
// a seeded generator so the corpus is byte-identical on every machine.
func noise(frames int, seed int64, amplitude int) []int16 {
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // fixtures, not crypto
	out := make([]int16, frames*FrameSamples)
	for i := range out {
		out[i] = int16(rng.Intn(2*amplitude+1) - amplitude)
	}
	return out
}

// chunk splits a fixture into analysis frames, the way the capture stream
// delivers it.
func chunk(pcm []int16) [][]int16 {
	var out [][]int16
	for off := 0; off+FrameSamples <= len(pcm); off += FrameSamples {
		out = append(out, pcm[off:off+FrameSamples])
	}
	return out
}

// nonZero counts samples that are not zero — how "is this buffer wiped?" is
// asked throughout these tests.
func nonZero(pcm []int16) int {
	n := 0
	for _, s := range pcm {
		if s != 0 {
			n++
		}
	}
	return n
}

// framesIn is how many analysis frames a duration holds, for readable
// expectations ("800 ms of silence" rather than "10 frames").
func framesIn(d time.Duration) int { return int(d / FrameDuration) }

// approx fails unless got is within tolerance of want.
func approx(t *testing.T, label string, got, want, tolerance float64) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Errorf("%s: got %.4f, want %.4f (±%.4f)", label, got, want, tolerance)
	}
}

// discardLogger keeps the daemon's own supervision noise out of test output.
// The listener logs at Info on every capture restart, which is right in a
// journal and unreadable in a test run.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
