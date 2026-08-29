package voicecorpus

import (
	"strings"
	"testing"
	"time"
)

// TestRenderShowsAPerPhraseVerdictAndTheEvidenceUnderAFailure: the report is
// the whole user interface of a corpus run, and a failure that does not print
// what was said, what was heard and why it did not count is a failure nobody
// can act on.
func TestRenderShowsAPerPhraseVerdictAndTheEvidenceUnderAFailure(t *testing.T) {
	corpus := Corpus{
		Dir:     "testdata/voicecorpus",
		Missing: []Phrase{{ID: "03-not-yet"}},
	}
	pass := Result{
		Recording:  Recording{ID: "01-mute", Phrase: Phrase{ID: "01-mute", Say: "mute"}},
		Transcript: "Mute.", Score: 1, Elapsed: 420 * time.Millisecond,
	}
	fail := Result{
		Recording: Recording{ID: "02-workspace-four", Noisy: true,
			Phrase: Phrase{ID: "02-workspace-four", Say: "workspace four", Note: "the\nnumber\nword"}},
		Transcript: "Jarvix, workspace for.", Stripped: "workspace for.",
		Score:    0.5,
		Failures: []string{"the router matched nothing"},
	}
	findings := []Finding{
		{ID: "02-workspace-four", Regression: true, Message: "02-workspace-four used to pass and now fails"},
		{Message: "the bias prompt has changed"},
	}

	got := Render(corpus, []Result{pass, fail}, findings)
	for _, want := range []string{
		"1 of 2 recordings pass",
		"testdata/voicecorpus",
		"ok   01-mute",
		"FAIL 02-workspace-four",
		"[noisy room]",
		`said:       "workspace four"`,
		`transcript: "Jarvix, workspace for."`,
		`stripped:   "workspace for."`,
		"→ the router matched nothing",
		"note: the number word", // folded onto one line
		"1 phrases not recorded yet: 03-not-yet",
		"REGRESSION: 02-workspace-four used to pass",
		"note: the bias prompt has changed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the report does not contain %q:\n%s", want, got)
		}
	}
	// A passing recording contributes one line and no evidence dump.
	if strings.Contains(got, `said:       "mute"`) {
		t.Errorf("the report dumps evidence for a passing recording:\n%s", got)
	}
}

func TestRenderOnACleanRunIsOneLine(t *testing.T) {
	got := Render(Corpus{Dir: "somewhere"}, nil, nil)
	if lines := strings.Count(strings.TrimSpace(got), "\n"); lines != 0 {
		t.Errorf("a clean, empty run rendered %d extra lines:\n%s", lines, got)
	}
}
