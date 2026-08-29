package voicecorpus

import (
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/vocabulary"
)

// evaluate is the shorthand these tests are written in: one phrase, one
// transcript, the shipped downstream pipeline.
func evaluate(t *testing.T, expect Expect, say, transcript string) Result {
	t.Helper()
	p := Phrase{ID: "01-under-test", Say: say, Expect: expect}
	return Evaluate(Recording{Phrase: p, ID: p.ID}, transcript, "", 0, defaultRig(t))
}

func boolp(b bool) *bool { return &b }
func intp(n int) *int    { return &n }

// TestEvaluateAssertsOutcomesNotTranscripts is the acceptance criterion stated
// as a test: two transcripts that read nothing alike both satisfy the phrase,
// because both reach the router as the same intent with the same slot.
func TestEvaluateAssertsOutcomesNotTranscripts(t *testing.T) {
	expect := Expect{Intent: "workspace.switch", Slot: intp(4)}
	for _, transcript := range []string{"workspace four", " Workspace four.", "WORKSPACE 4"} {
		got := evaluate(t, expect, "workspace four", transcript)
		if !got.Pass() {
			t.Errorf("transcript %q failed: %v", transcript, got.Failures)
		}
	}
}

func TestEvaluateReportsTheIntentTheRouterActuallyMatched(t *testing.T) {
	got := evaluate(t, Expect{Intent: "workspace.switch"}, "workspace four", "volume up")
	if got.Pass() {
		t.Fatal("Evaluate passed an utterance that routed somewhere else entirely")
	}
	if !strings.Contains(got.Failures[0], `matched "volume.up", not "workspace.switch"`) {
		t.Errorf("failure %q does not name both intents", got.Failures[0])
	}
}

func TestEvaluateCatchesASlotThatWasHeardWrong(t *testing.T) {
	got := evaluate(t, Expect{Intent: "workspace.switch", Slot: intp(4)}, "workspace four", "workspace five")
	if got.Pass() {
		t.Fatal("Evaluate passed workspace five for a phrase expecting workspace four")
	}
	if !strings.Contains(got.Failures[0], "slot is 5") {
		t.Errorf("failure %q does not name the slot that was heard", got.Failures[0])
	}
}

func TestEvaluateFailsWhenNothingRouted(t *testing.T) {
	got := evaluate(t, Expect{Intent: "volume.up"}, "volume up", "turn it up a bit")
	if got.Pass() {
		t.Fatal("Evaluate passed an utterance the router did not claim")
	}
	if !strings.Contains(got.Failures[0], "gone to the model") {
		t.Errorf("failure %q does not say what would have happened instead", got.Failures[0])
	}
}

// TestEvaluateGuardsTheRoutersRestraint: the router's discipline is that
// ambiguity belongs to the model, and a new pattern that swallowed an ordinary
// question would be a regression no other test in the tree would notice.
func TestEvaluateGuardsTheRoutersRestraint(t *testing.T) {
	pass := evaluate(t, Expect{NoIntent: true}, "what did we talk about yesterday",
		"What did we talk about yesterday?")
	if !pass.Pass() {
		t.Errorf("an unclaimed utterance failed a no_intent expectation: %v", pass.Failures)
	}
	fail := evaluate(t, Expect{NoIntent: true}, "mute", "mute")
	if fail.Pass() {
		t.Fatal("Evaluate passed a no_intent phrase that the router claimed")
	}
	if !strings.Contains(fail.Failures[0], `claimed this utterance as "volume.mute"`) {
		t.Errorf("failure %q does not name the claim", fail.Failures[0])
	}
}

func TestEvaluateRequiresTheNameToBeRecognised(t *testing.T) {
	// "Jarvis" is a shipped alias precisely because whisper keeps writing it.
	if got := evaluate(t, Expect{Wake: WakeName}, "Jarvix", "Jarvis"); !got.Pass() {
		t.Errorf("an accepted mishearing failed: %v", got.Failures)
	}
	got := evaluate(t, Expect{Wake: WakeName}, "Jarvix", "Harvest")
	if got.Pass() {
		t.Fatal("Evaluate accepted a transcript no alias covers")
	}
	if !strings.Contains(got.Failures[0], "assistant.aliases") {
		t.Errorf("failure %q does not say what to do about it", got.Failures[0])
	}
}

func TestEvaluateRequiresTheSummonsToComeOff(t *testing.T) {
	got := evaluate(t, Expect{Wake: WakeStrip, NoIntent: true},
		"Jarvix, what time is it?", "Jarvix, what time is it?")
	if !got.Pass() {
		t.Fatalf("a well-formed wake utterance failed: %v", got.Failures)
	}
	if got.Stripped != "what time is it?" {
		t.Errorf("stripped transcript is %q", got.Stripped)
	}

	t.Run("name not recognised", func(t *testing.T) {
		got := evaluate(t, Expect{Wake: WakeStrip}, "Jarvix, what time is it?", "Harvest, what time is it?")
		if got.Pass() {
			t.Fatal("Evaluate passed a transcript whose summons was not recognised")
		}
		if !strings.Contains(got.Failures[0], "the router never sees a clean utterance") {
			t.Errorf("failure %q does not say what the consequence is", got.Failures[0])
		}
	})

	t.Run("nothing left after the name", func(t *testing.T) {
		got := evaluate(t, Expect{Wake: WakeStrip}, "Jarvix, what time is it?", "Jarvix.")
		if got.Pass() {
			t.Fatal("Evaluate passed a transcript that lost everything but the name")
		}
		if !strings.Contains(got.Failures[0], "nothing was left after it") {
			t.Errorf("failure %q does not describe what happened", got.Failures[0])
		}
	})
}

func TestEvaluateRequiresTaughtWordsToSurvive(t *testing.T) {
	expect := Expect{NoIntent: true, Words: []string{"quid"}}
	if got := evaluate(t, expect, "how many quid did I spend", "How many quid did I spend?"); !got.Pass() {
		t.Errorf("a surviving taught word failed: %v", got.Failures)
	}
	got := evaluate(t, expect, "how many quid did I spend", "How many kid did I spend?")
	if got.Pass() {
		t.Fatal("Evaluate passed a transcript that lost the taught word")
	}
	if !strings.Contains(got.Failures[0], `the word "quid" did not survive`) {
		t.Errorf("failure %q does not name the lost word", got.Failures[0])
	}
}

func TestEvaluateChecksTheConfirmationGate(t *testing.T) {
	cases := []struct {
		transcript string
		want       bool
	}{
		{"Yes.", true}, {"Yes, do it.", true}, {"No.", false}, {"No, don't.", false},
	}
	for _, c := range cases {
		if got := evaluate(t, Expect{Affirmative: boolp(c.want)}, "yes", c.transcript); !got.Pass() {
			t.Errorf("%q: %v", c.transcript, got.Failures)
		}
	}
	got := evaluate(t, Expect{Affirmative: boolp(true)}, "yes", "No.")
	if got.Pass() {
		t.Fatal("Evaluate passed a refusal for a phrase expecting approval")
	}
	if !strings.Contains(got.Failures[0], "read this as refusal, not approval") {
		t.Errorf("failure %q does not name both readings", got.Failures[0])
	}
}

// TestEvaluateNeverPassesACaptureThePipelineDeclinedToTranscribe pins the
// #191 interaction: the voice gate and the prompt-echo rule both hand back an
// empty transcript with a reason, and a harness that read that as "no failures
// found" would report a silent microphone as a passing corpus.
func TestEvaluateNeverPassesACaptureThePipelineDeclinedToTranscribe(t *testing.T) {
	rec := Recording{ID: "01-yes", Phrase: Phrase{ID: "01-yes", Say: "yes", Expect: Expect{Affirmative: boolp(true)}}}
	const reason = "the transcript was only the bias prompt echoed back"
	got := Evaluate(rec, "", reason, 0, defaultRig(t))
	if got.Pass() {
		t.Fatal("Evaluate passed a capture the pipeline refused to transcribe")
	}
	if !strings.Contains(got.Failures[0], reason) {
		t.Errorf("failure %q does not carry the adapter's reason", got.Failures[0])
	}
}

func TestEvaluateNamesAnEmptyTranscriptWithNoReason(t *testing.T) {
	rec := Recording{ID: "01-yes", Phrase: Phrase{ID: "01-yes", Say: "yes", Expect: Expect{Affirmative: boolp(true)}}}
	got := Evaluate(rec, "", "", 0, defaultRig(t))
	if got.Pass() || !strings.Contains(got.Failures[0], "empty transcript with no reason") {
		t.Errorf("Evaluate gave %+v for an unexplained empty transcript", got.Failures)
	}
}

func TestEvaluateKeepsTheElapsedTimeForTheReport(t *testing.T) {
	rec := Recording{ID: "01-yes", Phrase: Phrase{ID: "01-yes", Say: "yes", Expect: Expect{Affirmative: boolp(true)}}}
	if got := Evaluate(rec, "yes", "", 1500*time.Millisecond, defaultRig(t)); got.Elapsed != 1500*time.Millisecond {
		t.Errorf("Elapsed is %v", got.Elapsed)
	}
}

// TestBuildRigReadsTheLiveConfiguration checks the thing the harness depends on
// most: the prompt it biases whisper with is the prompt this configuration
// would send, composed from the same function the daemon composes it with, and
// it follows a renamed assistant.
func TestBuildRigReadsTheLiveConfiguration(t *testing.T) {
	cfg := config.Default()
	cfg.Assistant.Name = "Mister Smith"
	cfg.Assistant.Aliases = []string{"Mr Smith"}
	cfg.STT.Vocabulary = []string{"Hyprland"}
	paths := config.Paths{State: t.TempDir()}

	rig, err := BuildRig(cfg, paths)
	if err != nil {
		t.Fatalf("BuildRig: %v", err)
	}
	if rig.WakeWord != "Mister Smith" {
		t.Errorf("rig wake word is %q", rig.WakeWord)
	}
	if len(rig.WakeAliases) != 1 || rig.WakeAliases[0] != "Mr Smith" {
		t.Errorf("rig aliases are %v", rig.WakeAliases)
	}
	want := cfg.STTBiasPromptWith(nil)
	if got := rig.BiasPrompt(); got != want {
		t.Errorf("rig bias prompt is %q, want the configuration's own %q", got, want)
	}
	if !strings.Contains(rig.BiasPrompt(), "Mister Smith") {
		t.Errorf("the bias prompt does not carry the configured name: %q", rig.BiasPrompt())
	}
	if _, ok := rig.Router.Match("workspace four"); !ok {
		t.Error("the rig's router did not compile the shipped grammar")
	}
}

// TestBuildRigCarriesTaughtVocabularyIntoTheBias is the #129 half: a phrase
// the user taught as hard to hear must be in the prompt the corpus runs under,
// or phrase 31 would be measuring a bias nobody uses.
func TestBuildRigCarriesTaughtVocabularyIntoTheBias(t *testing.T) {
	state := t.TempDir()
	paths := config.Paths{State: state}
	cfg := config.Default()
	// Taught through the real store, writing the real file, because the point
	// of the assertion is that the rig reads what the daemon reads.
	store := vocabulary.NewStore(paths.VocabularyFile(), vocabulary.StoreOptions{}, nil)
	entry, _, err := store.Teach("quid", "pounds", "", "test")
	if err != nil {
		t.Fatalf("Teach: %v", err)
	}
	if _, _, err := store.SetHardToHear(entry.ID, true); err != nil {
		t.Fatalf("SetHardToHear: %v", err)
	}
	rig, err := BuildRig(cfg, paths)
	if err != nil {
		t.Fatalf("BuildRig: %v", err)
	}
	if !strings.Contains(rig.BiasPrompt(), "quid") {
		t.Errorf("the taught word is not in the live bias prompt: %q", rig.BiasPrompt())
	}
}

func TestBuildRigRefusesAnIntentTableThatWillNotCompile(t *testing.T) {
	cfg := config.Default()
	cfg.Intents.Custom = []config.CustomIntent{{Match: "mute", Run: "true"}}
	if _, err := BuildRig(cfg, config.Paths{State: t.TempDir()}); err == nil {
		t.Error("BuildRig accepted a configuration whose intent table collides with a built-in")
	}
}
