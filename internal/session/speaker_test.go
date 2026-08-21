package session

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// pushAll streams text into the sentencer in chunks of the given size,
// collecting every emitted sentence, then flushes.
func pushAll(text string, chunkSize int) []string {
	var sc sentencer
	var out []string
	for len(text) > 0 {
		n := chunkSize
		if n > len(text) {
			n = len(text)
		}
		out = append(out, sc.push(text[:n])...)
		text = text[n:]
	}
	return append(out, sc.flush()...)
}

func TestSentencerSplitsCompleteSentences(t *testing.T) {
	cases := map[string]struct {
		in   string
		want []string
	}{
		"terminators followed by space": {
			in:   "First one. Second one! Third one? Done",
			want: []string{"First one.", "Second one!", "Third one?", "Done"},
		},
		"newline followed by whitespace is a boundary": {
			in:   "line one\n\nline two",
			want: []string{"line one", "line two"},
		},
		"colon before whitespace splits": {
			in:   "Containers: web and db",
			want: []string{"Containers:", "web and db"},
		},
		"decimals do not split": {
			in:   "Version 3.5 shipped. Done",
			want: []string{"Version 3.5 shipped.", "Done"},
		},
		"urls do not split at the colon": {
			in:   "See http://x.test/page for more. Done",
			want: []string{"See http://x.test/page for more.", "Done"},
		},
		"trailing terminator flushes on final": {
			in:   "All done.",
			want: []string{"All done."},
		},
		"whitespace only yields nothing": {
			in:   "   \n\t ",
			want: nil,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			for _, chunkSize := range []int{1, 3, 1 << 20} {
				got := pushAll(c.in, chunkSize)
				if len(got) != len(c.want) {
					t.Fatalf("chunkSize %d: got %q, want %q", chunkSize, got, c.want)
				}
				for i := range c.want {
					if got[i] != c.want[i] {
						t.Fatalf("chunkSize %d: got %q, want %q", chunkSize, got, c.want)
					}
				}
			}
		})
	}
}

func TestSentencerBoundaryNeedsFollowingWhitespace(t *testing.T) {
	var sc sentencer
	// The terminator is the last byte of the buffer: the sentencer must wait
	// for the next token to learn whether whitespace follows ("3." could be
	// the start of "3.5").
	if got := sc.push("It costs 3."); len(got) != 0 {
		t.Fatalf("emitted %q before the boundary was confirmed", got)
	}
	if got := sc.push("5 dollars. "); len(got) != 1 || got[0] != "It costs 3.5 dollars." {
		t.Fatalf("got %q", got)
	}
}

func TestSentencerRunonFlushesWithoutTerminator(t *testing.T) {
	var sc sentencer
	long := strings.Repeat("word ", 60) // 300 chars, no terminator
	var out []string
	for _, ch := range []byte(long) {
		out = append(out, sc.push(string(ch))...)
	}
	if len(out) == 0 {
		t.Fatal("a wall of unpunctuated text must not hold speech hostage")
	}
	// Nothing lost: what was emitted plus what remains is the full text.
	rest := sc.flush()
	all := strings.Join(append(out, rest...), " ")
	if strings.ReplaceAll(all, " ", "") != strings.ReplaceAll(long, " ", "") {
		t.Fatalf("content lost or duplicated:\n got %q\nwant %q", all, long)
	}
}

func TestSentencerRunonBoundaryIsExact(t *testing.T) {
	// Exactly maxSentenceRunon unpunctuated bytes must still be held back;
	// one more byte trips the run-on flush. Pins the > (not >=) boundary,
	// which mutation testing showed was unasserted.
	var sc sentencer
	if got := sc.push(strings.Repeat("a", maxSentenceRunon)); len(got) != 0 {
		t.Fatalf("flushed at exactly the boundary: %q", got)
	}
	if got := sc.push("a"); len(got) != 1 {
		t.Fatalf("did not flush past the boundary: %q", got)
	}
}

// A provider delta ends wherever the network broke it, which can be in the
// middle of a rune. Every multi-byte width, split at every offset, must come
// out the far side intact — no replacement characters, no dropped bytes
// (issue #28).
func TestSentencerReassemblesRunesSplitAcrossChunks(t *testing.T) {
	for _, r := range []rune{'é' /*2*/, '€' /*3*/, '🎉' /*4*/} {
		text := "Cost is " + string(r) + " today. And " + string(r) + " tomorrow"
		t.Run(string(r), func(t *testing.T) {
			// Chunk sizes 1..5 cut each rune at every one of its offsets.
			for chunk := 1; chunk <= 5; chunk++ {
				got := strings.Join(pushAll(text, chunk), " ")
				if want := text; got != want {
					t.Fatalf("chunk %d: got %q, want %q", chunk, got, want)
				}
				if strings.ContainsRune(got, utf8.RuneError) {
					t.Fatalf("chunk %d: replacement character in %q", chunk, got)
				}
			}
		})
	}
}

// A stream that stops mid-rune must still say its last character: flush emits
// the incomplete encoding rather than discarding it (issue #28).
func TestSentencerFlushEmitsIncompleteTrailingRune(t *testing.T) {
	var sc sentencer
	if got := sc.push("done \xe2\x82"); len(got) != 0 {
		t.Fatalf("emitted %q before the rune was complete", got)
	}
	got := sc.flush()
	if len(got) != 1 || got[0] != "done \xe2\x82" {
		t.Fatalf("flush = %q, want the buffered bytes including the truncated rune", got)
	}
}

// The bytes of a rune are never split across two sentences, even when the
// run-on cap fires in the middle of one.
func TestSentencerRunonCutsOnARuneBoundary(t *testing.T) {
	var sc sentencer
	var out []string
	// maxSentenceRunon bytes of filler, then a 4-byte rune fed one byte at a
	// time: the cap trips while the rune is still incomplete.
	out = append(out, sc.push(strings.Repeat("a", maxSentenceRunon))...)
	const party = "🎉"
	for i := 0; i < len(party); i++ {
		out = append(out, sc.push(party[i:i+1])...)
	}
	out = append(out, sc.flush()...)
	joined := strings.Join(out, "")
	if want := strings.Repeat("a", maxSentenceRunon) + party; joined != want {
		t.Fatalf("content changed across the run-on cut: %q", joined)
	}
	for _, s := range out {
		if !utf8.ValidString(s) {
			t.Fatalf("emitted invalid UTF-8: %q (sentences %q)", s, out)
		}
	}
}

// The sentencer trims only ASCII whitespace: a Unicode space is content, and
// removing it would make the splitter's output depend on how bytes decode
// after the seams are gone (see the sentencer doc comment).
func TestSentencerKeepsUnicodeWhitespace(t *testing.T) {
	const nbsp = "\u00a0"
	got := pushAll(nbsp+" hello "+nbsp, 3)
	want := nbsp + " hello " + nbsp
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %q, want %q", got, []string{want})
	}
}

func TestSentencerFlushIsTerminal(t *testing.T) {
	var sc sentencer
	sc.push("leftover text")
	if got := sc.flush(); len(got) != 1 || got[0] != "leftover text" {
		t.Fatalf("flush = %q", got)
	}
	if got := sc.flush(); len(got) != 0 {
		t.Fatalf("second flush must be empty, got %q", got)
	}
}
