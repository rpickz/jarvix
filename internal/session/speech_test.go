package session

import (
	"strings"
	"testing"
)

func TestSpeechTextStripsMarkdown(t *testing.T) {
	cases := map[string]struct {
		in       string
		mustHave []string
		mustNot  []string
	}{
		"inline code and bold": {
			in:       "The **web** service runs `sail-8.5/app` on port 80.",
			mustHave: []string{"web", "sail-8.5/app", "port 80"},
			mustNot:  []string{"*", "`"},
		},
		"bullet list": {
			in:       "Containers:\n- web on port 80\n- db on port 5432",
			mustHave: []string{"web on port 80", "db on port 5432"},
			mustNot:  []string{"- ", "*"},
		},
		"numbered list": {
			in:      "Steps:\n1. build\n2. test",
			mustNot: []string{"1.", "2."},
		},
		"code fence": {
			in:       "Run this:\n```\ndocker ps\n```\nand you're done.",
			mustHave: []string{"Run this", "done"},
			mustNot:  []string{"`"},
		},
		"heading and link": {
			in:       "## Status\nSee [the docs](https://x.y) for more.",
			mustHave: []string{"Status", "the docs"},
			mustNot:  []string{"#", "https://", "]("},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := speechText(c.in)
			for _, want := range c.mustHave {
				if !strings.Contains(got, want) {
					t.Errorf("%q missing from %q", want, got)
				}
			}
			for _, bad := range c.mustNot {
				if strings.Contains(got, bad) {
					t.Errorf("%q should not appear in %q", bad, got)
				}
			}
		})
	}
}

// A model that ignores the artifact prompt and pastes its diagram source into
// the answer must not have the source read aloud: fenced blocks are dropped
// entirely from the spoken form (the overlay still shows them).
func TestSpeechTextDropsFencedDiagramSource(t *testing.T) {
	got := speechText("Here is the pipeline.\n```mermaid\ngraph TD\n  A[Build] --> B[Deploy]\n```\nIt has two stages.")
	for _, bad := range []string{"graph TD", "-->", "A[Build]", "`"} {
		if strings.Contains(got, bad) {
			t.Errorf("diagram source leaked into speech: %q in %q", bad, got)
		}
	}
	for _, want := range []string{"Here is the pipeline", "two stages"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from %q", want, got)
		}
	}
}

func TestSpeechTextListItemsBecomeSentences(t *testing.T) {
	got := speechText("- web\n- db\n- cache")
	// Each item should be period-terminated so TTS pauses between them.
	if strings.Count(got, ".") < 3 {
		t.Errorf("list items not sentence-separated: %q", got)
	}
}

func TestSpeechTextPlainProseUnchanged(t *testing.T) {
	in := "You have three containers running."
	if got := speechText(in); got != in {
		t.Errorf("plain prose changed: %q", got)
	}
}
