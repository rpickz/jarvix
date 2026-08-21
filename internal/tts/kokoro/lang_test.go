package kokoro

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/tts"
)

// The defect this ticket exists to fix was invisible from Go: the helper was
// handed lang="en-us" inside Python, so no amount of configuration changed it
// and no test could see it. The fix is only real if the derived code is
// asserted at the boundary — the argv that reaches the helper — which is what
// these tests do, on both the cold path and the warm one.

func argvFor(t *testing.T, voiceID string) string {
	t.Helper()
	s, dir := installKokoroStub(t)
	s.Voice = voiceID
	_, ch, err := s.Speak(context.Background(), tts.Request{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	args, err := os.ReadFile(filepath.Join(dir, "kokoro.args"))
	if err != nil {
		t.Fatal(err)
	}
	return string(args)
}

// Kokoro's whole family table, asserted where it matters: as the --lang value
// on the command line of the process that does the phonemisation.
func TestSpeakDerivesTheHelperLanguageFromTheVoice(t *testing.T) {
	cases := map[string]string{
		"af_heart":   "en-us",
		"am_adam":    "en-us",
		"bf_emma":    "en-gb",
		"bm_george":  "en-gb",
		"ef_dora":    "es",
		"ff_siwis":   "fr-fr",
		"hf_alpha":   "hi",
		"if_sara":    "it",
		"jf_alpha":   "ja",
		"pf_dora":    "pt-br",
		"zf_xiaobei": "zh",
	}
	for voiceID, want := range cases {
		t.Run(voiceID, func(t *testing.T) {
			argv := argvFor(t, voiceID)
			if !strings.Contains(argv, "--lang\n"+want+"\n") {
				t.Errorf("argv for %s did not carry --lang %s:\n%s", voiceID, want, argv)
			}
		})
	}
}

// The regression in one test: a British voice must never be phonemised as
// American again.
func TestBritishVoiceIsNotPhonemisedAsAmerican(t *testing.T) {
	argv := argvFor(t, "bf_emma")
	if strings.Contains(argv, "en-us") {
		t.Errorf("bf_emma was handed American phonemisation:\n%s", argv)
	}
}

// An unrecognised voice still speaks. Falling back to the default language is
// the right failure: a custom embedding somebody added to the archive is a
// voice, and refusing to say anything would be a worse answer than an accent.
func TestUnknownVoiceFallsBackToTheDefaultLanguage(t *testing.T) {
	argv := argvFor(t, "qq_custom")
	if !strings.Contains(argv, "--lang\nen-us\n") {
		t.Errorf("unknown voice did not fall back to en-us:\n%s", argv)
	}
}

func TestDefaultVoiceStillGetsALanguage(t *testing.T) {
	s, dir := installKokoroStub(t) // no Voice configured at all
	_, ch, err := s.Speak(context.Background(), tts.Request{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "kokoro.args"))
	for _, want := range []string{"--voice\naf_heart\n", "--lang\nen-us\n"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("argv %q missing %q", argv, want)
		}
	}
}

// The warm path spawns its helper once and keeps it, so the language has to be
// right at spawn time or every answer for the life of that worker is wrong.
func TestWarmHelperIsSpawnedWithTheDerivedLanguage(t *testing.T) {
	w, dir := installWarmStub(t, "normal")
	w.Cold.Voice = "bm_george"
	_, ch, err := w.Speak(context.Background(), tts.Request{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	args, err := os.ReadFile(filepath.Join(dir, "serve.args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--lang en-gb") {
		t.Errorf("serve argv did not carry the derived language: %q", args)
	}
	if !strings.Contains(string(args), "--voice bm_george") {
		t.Errorf("serve argv lost the voice: %q", args)
	}
}

// The cold fallback must agree with the warm path: a sentence rescued by a
// one-shot helper cannot come out in a different accent from the rest.
func TestColdFallbackKeepsTheSameLanguage(t *testing.T) {
	w, dir := installWarmStub(t, "oldscript") // --serve rejected; falls back cold
	w.Cold.Voice = "bf_emma"
	_, ch, err := w.Speak(context.Background(), tts.Request{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	serve, oneShot := spawns(t, dir)
	if oneShot == 0 {
		t.Fatalf("expected a one-shot fallback (serve=%d, oneshot=%d)", serve, oneShot)
	}
	args, err := os.ReadFile(filepath.Join(dir, "oneshot.args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--lang en-gb") {
		t.Errorf("cold fallback argv did not carry en-gb: %q", args)
	}
}

// setup-kokoro.sh copies the helper into ~/.local/share/jarvix, so upgrading
// Jarvix does not upgrade it. Handing --lang to a helper that predates the
// flag would make argparse exit 2 and take the user's voice away entirely as
// the price of an upgrade they did not know changed anything — so the flag is
// dropped instead, the old pronunciation returns, and doctor explains it.
func TestStaleInstalledHelperKeepsSpeaking(t *testing.T) {
	s, dir := installKokoroStub(t)
	s.Voice = "bf_emma"
	if err := os.WriteFile(s.Script, []byte("# a helper from before language selection\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, ch, err := s.Speak(context.Background(), tts.Request{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var pcm []byte
	for c := range ch {
		if c.Err != nil {
			t.Fatalf("a stale helper must still speak: %v", c.Err)
		}
		pcm = append(pcm, c.PCM...)
	}
	if len(pcm) == 0 {
		t.Fatal("no audio came out")
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "kokoro.args"))
	if strings.Contains(string(argv), "--lang") {
		t.Errorf("--lang was handed to a helper that would reject it:\n%s", argv)
	}
	if !strings.Contains(string(argv), "bf_emma") {
		t.Errorf("the voice was lost along with the language:\n%s", argv)
	}
}

func TestStaleInstalledHelperAlsoDegradesOnTheWarmPath(t *testing.T) {
	w, dir := installWarmStub(t, "normal")
	w.Cold.Voice = "bm_george"
	if err := os.WriteFile(w.Cold.Script, []byte("# old helper\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, ch, err := w.Speak(context.Background(), tts.Request{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	for c := range ch {
		if c.Err != nil {
			t.Fatalf("warm path lost its voice to a stale helper: %v", c.Err)
		}
	}
	args, err := os.ReadFile(filepath.Join(dir, "serve.args"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "--lang") {
		t.Errorf("serve argv passed --lang to a stale helper: %q", args)
	}
}

func TestScriptPathReportsTheInstalledHelper(t *testing.T) {
	s := &Synthesizer{Script: "/somewhere/kokoro_stream.py"}
	if got := s.ScriptPath(); got != "/somewhere/kokoro_stream.py" {
		t.Errorf("ScriptPath = %q", got)
	}
	t.Setenv("XDG_DATA_HOME", "/data")
	if got := (&Synthesizer{}).ScriptPath(); got != filepath.Join("/data", "jarvix", "kokoro_stream.py") {
		t.Errorf("default ScriptPath = %q; setup-kokoro.sh honours XDG_DATA_HOME and so must this", got)
	}
}
