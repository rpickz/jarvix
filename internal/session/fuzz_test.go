package session

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// stripSpace removes every whitespace rune while preserving all other bytes
// exactly — including invalid UTF-8, which strings.Map would re-encode to
// U+FFFD and thereby fake a content change the sentencer never made.
func stripSpace(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r != utf8.RuneError || size != 1) && unicode.IsSpace(r) {
			i += size
			continue
		}
		out.WriteString(s[i : i+size])
		i += size
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
			if strings.TrimSpace(s) == "" {
				t.Fatalf("emitted a blank sentence: %q", out)
			}
			if s != strings.TrimSpace(s) {
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
