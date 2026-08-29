package voicecorpus

import (
	"fmt"
	"strings"
)

// Render turns a run into the text a person reads: one line per recording with
// its verdict and score, the transcript and the reasons under any failure, and
// then the baseline's findings.
//
// It is a plain string rather than a stream of t.Log calls so it can be tested
// without a testing.T, and so the same rendering serves a future `jarvix
// doctor --corpus` without the report living in two places.
func Render(c Corpus, results []Result, findings []Finding) string {
	var b strings.Builder
	passing := 0
	for _, r := range results {
		if r.Pass() {
			passing++
		}
	}
	fmt.Fprintf(&b, "voice corpus: %d of %d recordings pass (%s)\n",
		passing, len(results), c.Dir)

	for _, r := range results {
		mark := "ok  "
		if !r.Pass() {
			mark = "FAIL"
		}
		noisy := ""
		if r.Recording.Noisy {
			noisy = "  [noisy room]"
		}
		fmt.Fprintf(&b, "  %s %-42s score %.2f  %4dms%s\n",
			mark, r.Recording.ID, r.Score, r.Elapsed.Milliseconds(), noisy)
		if r.Pass() {
			continue
		}
		fmt.Fprintf(&b, "       said:       %q\n", r.Recording.Phrase.Say)
		fmt.Fprintf(&b, "       transcript: %q\n", r.Transcript)
		if r.Stripped != "" && r.Stripped != r.Transcript {
			fmt.Fprintf(&b, "       stripped:   %q\n", r.Stripped)
		}
		for _, f := range r.Failures {
			fmt.Fprintf(&b, "       → %s\n", f)
		}
		if note := strings.TrimSpace(r.Recording.Phrase.Note); note != "" {
			fmt.Fprintf(&b, "       note: %s\n", collapse(note))
		}
	}

	if len(c.Missing) > 0 {
		ids := make([]string, 0, len(c.Missing))
		for _, p := range c.Missing {
			ids = append(ids, p.ID)
		}
		fmt.Fprintf(&b, "  %d phrases not recorded yet: %s\n", len(ids), strings.Join(ids, ", "))
	}

	if len(findings) > 0 {
		b.WriteString("  against the committed baseline:\n")
		for _, f := range findings {
			mark := "note"
			if f.Regression {
				mark = "REGRESSION"
			}
			fmt.Fprintf(&b, "    %s: %s\n", mark, f.Message)
		}
	}
	return b.String()
}

// collapse folds a multi-line note onto one line, so the report stays a table.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// Summary is the one line `jarvix doctor` prints about the corpus.
//
// It reads the manifest and baseline compiled into the binary, so it says
// something true on a machine with no source tree — and what it has to say
// while the corpus is empty is the whole reason the line exists. "Nothing has
// been recorded" is not a neutral fact about a test fixture; it means every
// claim this daemon makes about hearing its own name rests on transcripts a
// developer typed. Doctor is where a user finds out things like that.
func Summary() string {
	m, err := Phrases()
	if err != nil {
		return "the phrase manifest cannot be read: " + err.Error()
	}
	b, err := CommittedBaseline()
	if err != nil {
		return fmt.Sprintf("%d phrases defined, but the baseline cannot be read: %v", len(m.Phrases), err)
	}
	return summaryFrom(m, b)
}

// summaryFrom is Summary's judgement, over inputs a test can supply. Summary
// itself is then only the two embedded reads.
func summaryFrom(m Manifest, b Baseline) string {
	starred := 0
	for _, p := range m.Phrases {
		if p.Noisy {
			starred++
		}
	}
	passing, total := b.Passing()
	if total == 0 {
		return fmt.Sprintf(
			"%d phrases defined (%d worth a second, noisy-room take), none recorded — "+
				"speech recognition is proven only against faked transcripts; see docs/voice-corpus.md",
			len(m.Phrases), starred)
	}
	line := fmt.Sprintf("%d of %d recordings pass in the committed baseline", passing, total)
	if b.Updated != "" {
		line += ", taken " + b.Updated
	}
	if b.Model != "" {
		line += " with " + b.Model
	}
	// Counted over phrases, not entries: a starred phrase contributes two
	// baseline entries, so subtracting entry counts would quietly report a
	// corpus as more complete than it is.
	covered := make(map[string]bool, total)
	for _, e := range b.Entries {
		id, _ := strings.CutSuffix(e.ID, NoisySuffix)
		covered[id] = true
	}
	if missing := len(m.Phrases) - len(covered); missing > 0 {
		line += fmt.Sprintf("; %d phrases still unrecorded", missing)
	}
	return line
}
