package session

import (
	"strings"
	"testing"
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
	f.Fuzz(func(t *testing.T, text string, chunk uint8) {
		size := int(chunk%16) + 1
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

		got := stripSpace(strings.Join(out, ""))
		want := stripSpace(text)
		if got != want {
			t.Fatalf("sentencer lost/duplicated content:\n got %q\nwant %q\n(sentences %q)", got, want, out)
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
		if extra := sc.flush(); len(extra) != 0 {
			t.Fatalf("flush was not terminal: %q", extra)
		}
	})
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
	f.Fuzz(func(t *testing.T, text string) {
		got := speechText(text)
		if strings.ContainsAny(got, "`*") {
			t.Fatalf("speech text contains literal markdown: %q -> %q", text, got)
		}
		if got != strings.TrimSpace(got) {
			t.Fatalf("speech text not trimmed: %q", got)
		}
	})
}
