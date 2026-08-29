package voicecorpus

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/intent"
)

// defaultRig builds the downstream half of the pipeline from the SHIPPED
// configuration — the shipped intent grammar, the shipped assistant name and
// its shipped aliases.
//
// Deliberately not the live one. The harness grades recordings against the
// machine's own configuration, because that is the pipeline the user actually
// speaks to; these hermetic tests grade the manifest against the defaults,
// because a manifest that only made sense on one laptop would be a manifest
// nobody else could read a failure from.
func defaultRig(t *testing.T) Rig {
	t.Helper()
	cfg := config.Default()
	router, err := intent.New(cfg.IntentOptions())
	if err != nil {
		t.Fatalf("compile the shipped intent table: %v", err)
	}
	return Rig{
		Router:      router,
		WakeWord:    cfg.Assistant.Name,
		WakeAliases: cfg.Assistant.EffectiveAliases(),
		BiasPrompt:  cfg.STTBiasPrompt,
	}
}

func TestShippedManifestIsValid(t *testing.T) {
	m, err := Phrases()
	if err != nil {
		t.Fatalf("Phrases: %v", err)
	}
	if len(m.Phrases) < 20 {
		t.Errorf("the corpus is %d phrases; issue #143 asks for roughly thirty", len(m.Phrases))
	}
	if _, ok := m.Find(m.Phrases[0].ID); !ok {
		t.Errorf("Find cannot find the first phrase, %q", m.Phrases[0].ID)
	}
	if _, ok := m.Find("no-such-phrase"); ok {
		t.Error("Find claims to have found a phrase that is not there")
	}
}

// TestShippedManifestExpectationsHoldOnIdealTranscripts is the test that makes
// the corpus safe to hand to somebody with a microphone.
//
// It feeds each phrase's own words through the real downstream pipeline as if
// whisper had transcribed them perfectly, and requires the declared outcome.
// That separates the two ways a corpus run can go red. If this test passes and
// a recording fails, the recording is the news: the pipeline does that phrase
// correctly when it hears it correctly, so what failed was the hearing. If
// this test fails, the manifest is the news — a typo'd intent name, a phrasing
// the router does not actually claim, an expectation somebody guessed at — and
// no amount of re-recording would have fixed it.
//
// It also makes the manifest a live pin on the intent table: rename
// workspace.switch, or drop the "later {text}" pattern, and this fails on the
// same commit rather than the next time anybody runs the tagged harness.
func TestShippedManifestExpectationsHoldOnIdealTranscripts(t *testing.T) {
	m, err := Phrases()
	if err != nil {
		t.Fatalf("Phrases: %v", err)
	}
	rig := defaultRig(t)
	for _, p := range m.Phrases {
		t.Run(p.ID, func(t *testing.T) {
			rec := Recording{Phrase: p, ID: p.ID}
			got := Evaluate(rec, p.Say, "", 0, rig)
			for _, f := range got.Failures {
				t.Errorf("saying %q exactly does not satisfy this phrase's own expectation: %s", p.Say, f)
			}
			if got.Score != 1 {
				t.Errorf("a perfect transcript of %q scores %.2f, not 1", p.Say, got.Score)
			}
		})
	}
}

// TestNoisyTakesAreMarkedOnPhrasesWorthRepeating keeps the starred set from
// quietly emptying: the noisy-room second take is the only measurement of the
// conditions people actually use a voice assistant in.
func TestNoisyTakesAreMarkedOnPhrasesWorthRepeating(t *testing.T) {
	m, err := Phrases()
	if err != nil {
		t.Fatalf("Phrases: %v", err)
	}
	starred := 0
	for _, p := range m.Phrases {
		if p.Noisy {
			starred++
		}
	}
	if starred == 0 {
		t.Error("no phrase is marked noisy_take; issue #143 starred several as worth a second, noisier take")
	}
}

func TestParseManifestRejectsBrokenDocuments(t *testing.T) {
	cases := []struct {
		name     string
		document string
		want     string
	}{
		{"not toml", "phrase = [", "voice corpus manifest"},
		{"no phrases", "# nothing here\n", "no phrases defined"},
		{"no id", "[[phrase]]\nsay = \"hello\"\nexpect = { no_intent = true }\n", "no id"},
		{"bad id", "[[phrase]]\nid = \"Workspace_Four\"\nsay = \"x\"\nexpect = { no_intent = true }\n",
			"two digits, a dash and a lowercase slug"},
		{"reserved suffix", "[[phrase]]\nid = \"01-thing-noisy\"\nsay = \"x\"\nexpect = { no_intent = true }\n",
			"reserved for the second take"},
		{"duplicate id", "[[phrase]]\nid = \"01-a\"\nsay = \"x\"\nexpect = { no_intent = true }\n" +
			"[[phrase]]\nid = \"01-a\"\nsay = \"y\"\nexpect = { no_intent = true }\n", "duplicate id"},
		{"nothing to say", "[[phrase]]\nid = \"01-a\"\nsay = \"  \"\nexpect = { no_intent = true }\n",
			"no phrase to say"},
		{"unknown wake", "[[phrase]]\nid = \"01-a\"\nsay = \"x\"\nexpect = { wake = \"maybe\" }\n",
			"wake = \"maybe\""},
		{"contradiction", "[[phrase]]\nid = \"01-a\"\nsay = \"x\"\nexpect = { intent = \"volume.up\", no_intent = true }\n",
			"contradict"},
		{"orphan slot", "[[phrase]]\nid = \"01-a\"\nsay = \"x\"\nexpect = { no_intent = true, slot = 4 }\n",
			"needs an expect.intent"},
		{"empty word", "[[phrase]]\nid = \"01-a\"\nsay = \"x\"\nexpect = { words = [\" \"] }\n",
			"empty entry"},
		{"multi-word word", "[[phrase]]\nid = \"01-a\"\nsay = \"x\"\nexpect = { words = [\"two words\"] }\n",
			"more than one word"},
		{"asserts nothing", "[[phrase]]\nid = \"01-a\"\nsay = \"x\"\n", "no expectation"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseManifest(c.document)
			if err == nil {
				t.Fatalf("ParseManifest accepted %q", c.document)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestParseManifestAcceptsAWellFormedPhrase(t *testing.T) {
	m, err := ParseManifest(`
[[phrase]]
id = "01-volume-up"
say = "volume up"
note = "the easy one"
noisy_take = true
expect = { intent = "volume.up" }
`)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	p := m.Phrases[0]
	if p.ID != "01-volume-up" || p.Say != "volume up" || !p.Noisy || p.Expect.Intent != "volume.up" {
		t.Errorf("round trip lost something: %+v", p)
	}
}

// TestSummaryOverAPopulatedBaseline exercises the doctor line's other state,
// which the committed (empty) baseline cannot reach.
func TestSummaryOverAPopulatedBaseline(t *testing.T) {
	m, err := Phrases()
	if err != nil {
		t.Fatalf("Phrases: %v", err)
	}
	first, second := m.Phrases[0].ID, m.Phrases[1].ID
	b := Baseline{
		Updated: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Format(time.DateOnly),
		Model:   "ggml-base.en.bin",
		Entries: []BaselineEntry{
			{ID: first, Pass: true, Score: 1},
			{ID: first + NoisySuffix, Pass: true, Score: 0.9},
			{ID: second, Pass: false, Score: 0.4},
		},
	}
	got := summaryFrom(m, b)
	for _, want := range []string{"2 of 3 recordings pass", "2026-09-01", "ggml-base.en.bin"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q does not mention %q", got, want)
		}
	}
	// Two of the three entries are takes of ONE phrase, so the unrecorded
	// count must be counted over phrases and not over entries.
	if want := len(m.Phrases) - 2; !strings.Contains(got, "; "+strconv.Itoa(want)+" phrases still unrecorded") {
		t.Errorf("summary %q does not report %d phrases still unrecorded", got, want)
	}
}

func TestSummaryOverTheCommittedBaseline(t *testing.T) {
	got := Summary()
	if !strings.Contains(got, "phrases defined") {
		t.Errorf("with nothing recorded the summary should say so plainly; got %q", got)
	}
	if !strings.Contains(got, "faked transcripts") {
		t.Errorf("the summary should name what is unproven while the corpus is empty; got %q", got)
	}
}
