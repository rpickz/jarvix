package voicecorpus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// result builds a finished Result for a recording id, without an engine.
func result(id string, pass bool, score float64) Result {
	r := Result{
		Recording: Recording{ID: id, Phrase: Phrase{ID: id, Say: "something"}},
		Score:     score,
	}
	if !pass {
		r.Failures = []string{"the router matched nothing"}
	}
	return r
}

func TestTheCommittedBaselineParses(t *testing.T) {
	b, err := CommittedBaseline()
	if err != nil {
		t.Fatalf("the committed baseline does not parse: %v", err)
	}
	// It is empty today, and it must stay honest about that rather than
	// carrying entries nobody recorded.
	if passing, total := b.Passing(); total != len(b.Entries) || passing > total {
		t.Errorf("Passing() = %d of %d over %d entries", passing, total, len(b.Entries))
	}
}

func TestParseBaselineRejectsBrokenFiles(t *testing.T) {
	cases := []struct{ name, document, want string }{
		{"not toml", "entry = [", "voice corpus baseline"},
		{"entry with no id", "[[entry]]\npass = true\n", "no id"},
		{"two entries for one recording",
			"[[entry]]\nid = \"01-a\"\npass = true\n[[entry]]\nid = \"01-a\"\npass = false\n",
			"two entries"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseBaseline(c.document)
			if err == nil {
				t.Fatalf("ParseBaseline accepted %q", c.document)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestBaselineLookups(t *testing.T) {
	b := Baseline{Entries: []BaselineEntry{
		{ID: "01-a", Pass: true, Score: 1}, {ID: "02-b", Pass: false, Score: 0.5},
	}}
	if e, ok := b.Entry("02-b"); !ok || e.Score != 0.5 {
		t.Errorf("Entry(02-b) = %+v, %v", e, ok)
	}
	if _, ok := b.Entry("03-c"); ok {
		t.Error("Entry found a recording that is not in the baseline")
	}
	if passing, total := b.Passing(); passing != 1 || total != 2 {
		t.Errorf("Passing() = %d of %d, want 1 of 2", passing, total)
	}
}

// TestCompareToBaselineFailsOnlyOnThingsThatGotWorse is the contract stated as
// a test: recognition that regressed fails, recognition that improved does not,
// and a recording nobody has agreed to yet fails until somebody does.
func TestCompareToBaselineFailsOnlyOnThingsThatGotWorse(t *testing.T) {
	base := Baseline{PromptHash: "abc123", Entries: []BaselineEntry{
		{ID: "01-was-passing", Pass: true, Score: 1},
		{ID: "02-was-failing", Pass: false, Score: 0.4},
		{ID: "03-score-slipped", Pass: true, Score: 0.9},
		{ID: "04-still-fine", Pass: true, Score: 1},
		{ID: "05-not-recorded-this-time", Pass: true, Score: 1},
	}}
	results := []Result{
		result("01-was-passing", false, 1),
		result("02-was-failing", true, 0.4),
		result("03-score-slipped", true, 0.5),
		result("04-still-fine", true, 1),
		result("06-brand-new", true, 0.8),
	}
	findings := CompareToBaseline(base, results, "abc123")

	want := map[string]bool{ // id → must it fail the run?
		"01-was-passing":            true,
		"02-was-failing":            false,
		"03-score-slipped":          true,
		"05-not-recorded-this-time": false,
		"06-brand-new":              true,
	}
	got := make(map[string]bool, len(findings))
	for _, f := range findings {
		if _, dup := got[f.ID]; dup {
			t.Errorf("two findings for %q", f.ID)
		}
		got[f.ID] = f.Regression
	}
	for id, regression := range want {
		if seen, ok := got[id]; !ok {
			t.Errorf("no finding for %q", id)
		} else if seen != regression {
			t.Errorf("%q: Regression = %v, want %v", id, seen, regression)
		}
	}
	if _, ok := got["04-still-fine"]; ok {
		t.Error("a recording that did not change produced a finding")
	}
	if n := len(Regressions(findings)); n != 3 {
		t.Errorf("Regressions() = %d findings, want 3", n)
	}
}

// TestCompareToBaselineToleratesASmallScoreWobble: the tolerance exists so a
// whisper upgrade does not redden the whole corpus for a reason nobody wants
// to read about.
func TestCompareToBaselineToleratesASmallScoreWobble(t *testing.T) {
	base := Baseline{Entries: []BaselineEntry{{ID: "01-a", Pass: true, Score: 1}}}
	within := CompareToBaseline(base, []Result{result("01-a", true, 1-ScoreTolerance)}, "")
	if len(within) != 0 {
		t.Errorf("a drop of exactly the tolerance produced %+v", within)
	}
	beyond := CompareToBaseline(base, []Result{result("01-a", true, 1-ScoreTolerance-0.01)}, "")
	if len(Regressions(beyond)) != 1 {
		t.Errorf("a drop past the tolerance produced %+v", beyond)
	}
}

// TestCompareToBaselineNamesAChangedBiasPrompt: when recognition moves, the
// prompt is the first thing to look at, so the report says whether it moved
// too — without failing on it, because teaching a word is a normal thing to do.
func TestCompareToBaselineNamesAChangedBiasPrompt(t *testing.T) {
	base := Baseline{PromptHash: "abc123", Entries: []BaselineEntry{{ID: "01-a", Pass: true, Score: 1}}}
	findings := CompareToBaseline(base, []Result{result("01-a", true, 1)}, "def456")
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want just the prompt note", findings)
	}
	if findings[0].Regression {
		t.Error("a changed bias prompt failed the run on its own")
	}
	if !strings.Contains(findings[0].Message, "abc123 → def456") {
		t.Errorf("the note %q does not show both fingerprints", findings[0].Message)
	}
}

func TestCompareToBaselineWithNoBaselineFailsEveryRecording(t *testing.T) {
	findings := CompareToBaseline(Baseline{}, []Result{
		result("01-a", true, 1), result("02-b", true, 1),
	}, "abc123")
	if n := len(Regressions(findings)); n != 2 {
		t.Fatalf("an empty baseline produced %d regressions, want 2: %+v", n, findings)
	}
	if !strings.Contains(findings[0].Message, "-voicecorpus.update-baseline") {
		t.Errorf("the finding %q does not say how to resolve it", findings[0].Message)
	}
}

func TestPromptFingerprintIdentifiesAPromptWithoutQuotingIt(t *testing.T) {
	const prompt = "The assistant is called Jarvix. Conversations may mention: quid."
	got := PromptFingerprint(prompt)
	if got == "" || len(got) != 12 {
		t.Errorf("PromptFingerprint = %q, want twelve hex characters", got)
	}
	if strings.Contains(got, "quid") || strings.Contains(got, "Jarvix") {
		t.Errorf("PromptFingerprint %q leaks the prompt it summarises", got)
	}
	if PromptFingerprint(prompt+" ") == got {
		t.Error("PromptFingerprint does not distinguish two different prompts")
	}
	if PromptFingerprint("") != "" {
		t.Error("an absent prompt should fingerprint to nothing, not to a hash of nothing")
	}
}

func TestNewBaselineRoundsSortsAndRecordsTheConditions(t *testing.T) {
	now := time.Date(2026, 9, 1, 14, 30, 0, 0, time.UTC)
	b := NewBaseline([]Result{
		result("02-b", false, 0.666666),
		result("01-a", true, 1),
	}, "/home/someone/.local/share/jarvix/models/whisper/ggml-base.en.bin", "abc123", now)

	if b.Updated != "2026-09-01" {
		t.Errorf("Updated = %q", b.Updated)
	}
	if b.Model != "ggml-base.en.bin" {
		t.Errorf("Model = %q, want just the file name", b.Model)
	}
	if b.PromptHash != "abc123" {
		t.Errorf("PromptHash = %q", b.PromptHash)
	}
	if b.Entries[0].ID != "01-a" || b.Entries[1].ID != "02-b" {
		t.Errorf("entries are not sorted by id: %+v", b.Entries)
	}
	if b.Entries[1].Score != 0.67 {
		t.Errorf("score %v was not rounded for a readable diff", b.Entries[1].Score)
	}
	if !b.Entries[0].Pass || b.Entries[1].Pass {
		t.Errorf("outcomes did not survive: %+v", b.Entries)
	}
}

func TestWriteBaselineRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), BaselineFile)
	want := NewBaseline([]Result{result("01-a", true, 1), result("02-b", false, 0.5)},
		"ggml-base.en.bin", "abc123", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err := WriteBaseline(path, want); err != nil {
		t.Fatalf("WriteBaseline: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "GENERATED") {
		t.Error("the written baseline does not say it is generated")
	}
	if !strings.Contains(string(raw), "-voicecorpus.update-baseline") {
		t.Error("the written baseline does not say what wrote it")
	}
	got, err := ParseBaseline(string(raw))
	if err != nil {
		t.Fatalf("the baseline we wrote does not parse: %v", err)
	}
	if got.Updated != want.Updated || got.Model != want.Model || got.PromptHash != want.PromptHash {
		t.Errorf("header did not round trip: %+v", got)
	}
	if len(got.Entries) != 2 || got.Entries[0] != want.Entries[0] || got.Entries[1] != want.Entries[1] {
		t.Errorf("entries did not round trip: %+v", got.Entries)
	}
}

func TestWriteBaselineReportsAPathItCannotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-directory", BaselineFile)
	if err := WriteBaseline(path, Baseline{}); err == nil {
		t.Error("WriteBaseline silently succeeded on an unwritable path")
	}
}
