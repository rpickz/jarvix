package session

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// stripSpace removes every ASCII whitespace byte and preserves all other
// bytes exactly — including invalid UTF-8, which strings.Map would re-encode
// to U+FFFD and thereby fake a content change the sentencer never made.
//
// It is deliberately byte-level rather than rune-level. A rune-level rule is
// not stable under concatenation, which is exactly what this invariant does:
// stripping the seam between "\xc2" and "\xa0" fuses two bytes that each
// decode as invalid into one valid U+00A0, so a rune-aware stripper would
// delete content on one side of the comparison and not the other and call it
// a sentencer bug (issue #28's minimised crasher). The sentencer's contract
// is byte-exact and only ASCII whitespace is ever trimmed, so this is the
// definition that actually matches it.
func stripSpace(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\v', '\f', '\r':
		default:
			out.WriteByte(s[i])
		}
	}
	return out.String()
}

// FuzzSentencer feeds arbitrary text through push/flush in arbitrary chunk
// sizes. Invariants: no panic, and no non-whitespace content is ever lost,
// duplicated, or reordered across the split.
func FuzzSentencer(f *testing.F) {
	f.Add("First one. Second one! Third one? Done", uint8(3))
	f.Add("Version 3.5 costs $3. See http://x.test: it helps.\n\nBye", uint8(1))
	f.Add("Containers:\n- web\n- db", uint8(7))
	f.Add(strings.Repeat("no terminators here ", 20), uint8(5))
	f.Add("unicode — émojis 🎉 and   spaces. done", uint8(2))
	f.Add("....!!??::\n\n. . . ", uint8(1))
	// Multi-byte runes cut at every offset by the chunker: the bytes must be
	// reassembled, never emitted as half a rune (issue #28). The minimised
	// crasher itself is committed under testdata/fuzz/FuzzSentencer.
	f.Add("café €9 \U0001f389 straddles. every boundary", uint8(0))
	f.Add("\xc2\n \xa0", uint8(0))
	// Run-on flushes with a partial rune pending: the buffer crosses
	// maxSentenceRunon on a chunk that ends inside a two-, three- and
	// four-byte encoding. incompleteTail is the only thing standing between
	// these and half a rune going to the speech engine.
	f.Add(strings.Repeat("a", 245)+"é and on it goes", uint8(1))
	f.Add(strings.Repeat("a", 245)+"\u3000 and on it goes", uint8(1))
	f.Add(strings.Repeat("a", 245)+"\U0001f389 and on it goes", uint8(1))
	f.Add(strings.Repeat("a", 239)+"\U0001f389 and on it goes", uint8(3))
	f.Fuzz(func(t *testing.T, text string, chunk uint8) {
		size := int(chunk%16) + 1
		out := sentences(t, text, size)

		got := stripSpace(strings.Join(out, ""))
		want := stripSpace(text)
		if got != want {
			t.Fatalf("sentencer lost/duplicated content:\n got %q\nwant %q\n(sentences %q)", got, want, out)
		}
		// A rune that arrived whole must leave whole. The sentencer buffers a
		// truncated trailing encoding until the rest of it arrives (issue
		// #28), and the place that promise is easiest to lose is the run-on
		// flush, which cuts on length rather than on a boundary — the mutation
		// report had three surviving mutants inside incompleteTail, all of
		// them "hold back nothing" (issue #172). Stated over valid input,
		// where "whole" needs no interpretation: a stray continuation byte in
		// the INPUT is content and is passed through, so this law says nothing
		// about it.
		if utf8.ValidString(text) {
			for _, s := range out {
				if !utf8.ValidString(s) {
					t.Fatalf("valid input was cut mid-rune: %q from %q", s, text)
				}
			}
		}
		for _, s := range out {
			// Blank and trimmed are judged by the sentencer's own definition
			// of whitespace (ASCII only): it deliberately passes a Unicode
			// space through as content rather than making its output depend
			// on decoding, and speak() drops anything blank downstream.
			if trimSeam(s) == "" {
				t.Fatalf("emitted a blank sentence: %q", out)
			}
			if s != trimSeam(s) {
				t.Fatalf("sentence not trimmed: %q", s)
			}
		}
		// Where the chunk boundaries fell is an accident of the model's
		// streaming, and the split must not depend on it: the same text
		// arriving byte by byte, in this run's chunk size, or all at once has
		// to produce the same sentences. Without this law "loses no content"
		// is satisfied by a sentencer that breaks in a different place every
		// time — and the place is what the listener hears (issue #172).
		//
		// The law is stated for buffers that cannot reach maxSentenceRunon,
		// and that scope is a real exemption rather than a convenience. The
		// run-on rule exists so a wall of unpunctuated text does not hold
		// speech hostage, and it necessarily cuts wherever the buffer happened
		// to cross the cap — which is a chunk boundary. A streaming cutter with
		// a length safeguard cannot be chunk-independent past that safeguard,
		// so the honest law is the one below plus the content law above, which
		// holds at every length. FuzzSentencer found the boundary immediately;
		// the seeds that cross it are in the corpus.
		if len(text) <= maxSentenceRunon {
			for _, other := range []int{1, 2, 16, len(text) + 1} {
				if again := sentences(t, text, other); !equalSentences(again, out) {
					t.Fatalf("chunking changed the split: size %d gave %q, size %d gave %q",
						size, out, other, again)
				}
			}
			return
		}
		// Past the cap, the content still cannot depend on chunking — only
		// where the run-on safeguard chose to breathe.
		for _, other := range []int{1, 2, 16, len(text) + 1} {
			again := stripSpace(strings.Join(sentences(t, text, other), ""))
			if again != got {
				t.Fatalf("chunking changed the content: size %d gave %q, size %d gave %q",
					size, got, other, again)
			}
		}
	})
}

// sentences runs text through the sentencer in fixed-size chunks and returns
// everything it emitted, flush included — and proves the flush was terminal,
// which is a property of every run and therefore belongs with the run.
func sentences(t *testing.T, text string, size int) []string {
	t.Helper()
	var sc sentencer
	var out []string
	rest := text
	for len(rest) > 0 {
		n := size
		if n > len(rest) {
			n = len(rest)
		}
		out = append(out, sc.push(rest[:n])...)
		rest = rest[n:]
	}
	out = append(out, sc.flush()...)
	if extra := sc.flush(); len(extra) != 0 {
		t.Fatalf("flush was not terminal at chunk size %d: %q", size, extra)
	}
	return out
}

// equalSentences compares two splits element by element. A nil and an empty
// slice are the same split — the sentencer returns whichever the code path
// happened to build, and the listener cannot tell them apart.
func equalSentences(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// FuzzSpeechText asserts the spoken-text contract on arbitrary markdown: no
// panic, and the output never contains backticks or asterisks — TTS engines
// read them aloud literally.
func FuzzSpeechText(f *testing.F) {
	f.Add("The **web** service runs `sail-8.5/app` on port 80.")
	f.Add("Run this:\n```\ndocker ps\n```\nand you're done.")
	f.Add("## Status\nSee [the docs](https://x.y) for more.")
	f.Add("- one\n- two\n1. three")
	// Regression seeds for the unpaired-marker bug FuzzSpeechText found:
	// lone backticks/asterisks survived the pair-matching regexes.
	f.Add("a ` b")
	f.Add("2 * 3 = 6")
	f.Add("*")
	f.Add("`")
	// Numeric seeds for the number expansion (issue #30): every shape it
	// claims, plus the shapes it must decline. Expansion runs on arbitrary
	// text, so "unparseable numbers pass through, never panic" is a fuzzing
	// question, not a table-test one.
	f.Add("9.2 million files, 82.4% full, £3.50 each")
	f.Add("v1.5.2 took 4.7s and 1.5GB, 3-5 times")
	f.Add("1st 2nd 3rd 21st 100th")
	f.Add("127.0.0.1:8080 /var/log/syslog.1 sail-8.5/app 2026-08-21")
	f.Add("999999999999999999999999.99999999999999")
	f.Add("$-1.--2.3..4")
	f.Add("Golang, Kubernetes, nginx, PostgreSQL, sudo")
	f.Fuzz(func(t *testing.T, text string) {
		got := speechText(text)
		if strings.ContainsAny(got, "`*") {
			t.Fatalf("speech text contains literal markdown: %q -> %q", text, got)
		}
		if got != strings.TrimSpace(got) {
			t.Fatalf("speech text not trimmed: %q", got)
		}
		// Expansion must be a pure function of the text: the same input twice
		// is the same spoken form, whatever order the compiled tables were
		// built in.
		if again := speechText(text); again != got {
			t.Fatalf("speech text is not deterministic: %q -> %q then %q", text, got, again)
		}
	})
}
