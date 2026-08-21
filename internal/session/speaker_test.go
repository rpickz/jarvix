package session

import (
	"strings"
	"testing"
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
