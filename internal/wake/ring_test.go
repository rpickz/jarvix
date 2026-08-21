package wake

import (
	"testing"
	"time"
)

// The ring is where the privacy promise is actually kept, so these tests are
// about the promise rather than about circular arithmetic: it holds what it
// was built to hold, it never holds more, and erasing it erases it.

// The pre-roll window is a privacy ceiling, and a ceiling that a config value
// can raise is not a ceiling. Whatever the file says, the ring is built no
// bigger than MaxRingDuration — enforced here as well as in validation,
// because a guarantee that depends on validation having run is weaker than
// one that cannot be expressed otherwise.
func TestRingClampsToTheHardCeiling(t *testing.T) {
	r := NewRing(30 * time.Second)
	if r.Duration() > MaxRingDuration {
		t.Fatalf("a 30s ring was built %v long; the ceiling is %v", r.Duration(), MaxRingDuration)
	}
	if r.Duration() < MaxRingDuration-FrameDuration {
		t.Errorf("clamping should give the ceiling, not something arbitrarily smaller: %v", r.Duration())
	}
}

// A zero window is a legitimate — and the most private — configuration: keep
// nothing from before the wake word. It must be a no-op rather than a panic.
func TestRingWithNoWindowKeepsNothing(t *testing.T) {
	r := NewRing(0)
	r.Write(utterance(1, 1))
	if r.Len() != 0 || r.Cap() != 0 {
		t.Fatalf("a zero-length ring retained %d samples", r.Len())
	}
	if got := r.AppendTo(nil); len(got) != 0 {
		t.Errorf("a zero-length ring produced %d samples of pre-roll", len(got))
	}
}

// The whole point of a ring: old audio stops existing as new audio arrives.
// Writing far more than the window must leave exactly the window, in order,
// oldest first.
func TestRingKeepsOnlyTheMostRecentWindowInOrder(t *testing.T) {
	r := NewRing(3 * FrameDuration)
	var written []int16
	for i := 1; i <= 10; i++ {
		frame := make([]int16, FrameSamples)
		for j := range frame {
			frame[j] = int16(i)
		}
		r.Write(frame)
		written = append(written, frame...)
	}
	if r.Len() != r.Cap() {
		t.Fatalf("after 10 frames a 3-frame ring holds %d of %d samples", r.Len(), r.Cap())
	}
	got := r.AppendTo(nil)
	want := written[len(written)-r.Cap():]
	if len(got) != len(want) {
		t.Fatalf("pre-roll is %d samples, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pre-roll sample %d is %d, want %d (frames arrived out of order)", i, got[i], want[i])
		}
	}
}

// A partially filled ring must report only what it has. Reporting the whole
// buffer would put a window of zeroes in front of the first request after a
// restart — audible as a clipped word, and a sign the cursor is being trusted
// over the fill count.
func TestRingReportsOnlyWhatItHolds(t *testing.T) {
	r := NewRing(5 * FrameDuration)
	r.Write(utterance(2, 7))
	if got := len(r.AppendTo(nil)); got != 2*FrameSamples {
		t.Errorf("a half-full ring produced %d samples, want %d", got, 2*FrameSamples)
	}
}

// Reset must erase, not rewind. A cursor reset would leave every sample of
// the last few seconds sitting in the process's memory — reachable by a core
// dump, a debugger, or a bug — while the code claimed the audio was gone.
func TestRingResetErasesTheSamples(t *testing.T) {
	r := NewRing(2 * FrameDuration)
	r.Write(utterance(2, 3))
	if nonZero(r.buf) == 0 {
		t.Fatal("fixture wrote nothing; the test below would pass vacuously")
	}
	r.Reset()
	if n := nonZero(r.buf); n != 0 {
		t.Errorf("%d samples survived Reset; the buffer must be zeroed, not rewound", n)
	}
	if r.Len() != 0 {
		t.Errorf("Reset left %d samples reachable", r.Len())
	}
}

// A frame longer than the whole ring is the degenerate case. Keeping its tail
// (rather than rejecting it, or overrunning) is the same rule every other
// write follows.
func TestRingWriteLongerThanItselfKeepsTheTail(t *testing.T) {
	r := NewRing(FrameDuration)
	long := make([]int16, 3*FrameSamples)
	for i := range long {
		long[i] = int16(i % 100)
	}
	r.Write(long)
	got := r.AppendTo(nil)
	want := long[len(long)-FrameSamples:]
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d is %d, want %d", i, got[i], want[i])
		}
	}
}
