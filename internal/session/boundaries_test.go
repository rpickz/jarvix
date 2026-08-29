package session

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The sentencer's surviving mutants from the first mutation report that was
// read (issue #172, docs/mutation.md), killed by the examples the properties
// could not reach on their own.
//
// All three were inside incompleteTail, and all three had the same effect:
// hold back nothing, and let the run-on flush cut a multi-byte rune in half.
// The fuzzer reaches this eventually — it has to find a buffer that crosses
// maxSentenceRunon on the exact chunk that ends inside an encoding — so the
// arithmetic is done here instead, once, in a test that says what it is
// arranging.

// TestARunOnFlushNeverCutsARune kills CONDITIONALS_BOUNDARY at speaker.go:68
// (`s[len(s)-1] < utf8.RuneSelf`) and at speaker.go:71 (`n <= 3`).
//
// The run-on rule flushes an unpunctuated buffer once it grows past
// maxSentenceRunon so a wall of text does not hold speech hostage. It cuts on
// LENGTH, which means the cut lands wherever the bytes happened to reach — and
// the bytes are a byte stream, not a rune stream. incompleteTail is the whole
// of the answer: it reports how many trailing bytes begin an encoding whose
// rest has not arrived, and those bytes stay in the buffer.
//
// Each case below is one push that takes the buffer from under the cap to over
// it while a 2-, 3- and 4-byte rune is exactly one byte short of complete.
func TestARunOnFlushNeverCutsARune(t *testing.T) {
	// The truncated heads of "é", "　" and "🎉" — every byte but the last —
	// beside the byte that completes each one.
	for _, tc := range []struct {
		name      string
		truncated string
		last      string
		whole     string
	}{
		{"two-byte", "\xc3", "\xa9", "é"},
		{"three-byte", "\xe3\x80", "\x80", "　"},
		{"four-byte", "\xf0\x9f\x8e", "\x89", "\U0001f389"},
	} {
		name, truncated := tc.name, tc.truncated
		t.Run(name, func(t *testing.T) {
			var sc sentencer
			// One push, deliberately: the buffer crosses the cap and ends
			// mid-encoding in the same call, which is the only moment the
			// held-back count is consulted.
			out := sc.push(strings.Repeat("a", maxSentenceRunon+1) + truncated)
			if len(out) == 0 {
				t.Fatalf("nothing was flushed; the run-on rule did not fire at %d bytes",
					maxSentenceRunon+1+len(truncated))
			}
			for _, s := range out {
				if !utf8.ValidString(s) {
					t.Errorf("the run-on flush emitted %d bytes ending mid-rune: %q",
						len(s), s[max(0, len(s)-8):])
				}
			}
			// And the held bytes are still there to be completed: finishing the
			// encoding must produce the rune whole, not a replacement
			// character and not nothing.
			rest := sc.push(tc.last + ". done")
			joined := strings.Join(append(out, rest...), "")
			if !strings.Contains(joined, tc.whole) {
				t.Errorf("the rune did not survive the flush: %q", joined[max(0, len(joined)-12):])
			}
		})
	}
}

// TestAWholeRuneIsNeverHeldBack is the other side of the same bound: bytes
// that already form a complete rune must not be kept out of the current
// sentence, or a terminator sitting behind them is delayed by a chunk.
func TestAWholeRuneIsNeverHeldBack(t *testing.T) {
	for _, text := range []string{"é. done", "🎉. done", "a. done", "　. done"} {
		var sc sentencer
		out := sc.push(text)
		if len(out) == 0 {
			t.Errorf("%q produced no sentence; the terminator is right there", text)
		}
	}
}
