package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/voice"
)

// doctor is where a user looks when Jarvix sounds wrong, so these checks have
// to answer the two questions that produce that feeling: what language is it
// speaking, and can it understand that language back? Every one runs against
// a fake catalog — no archive, no model, no engine.

func kokoroWorld(t *testing.T, voiceID string, installed ...string) (config.Config, config.Paths) {
	t.Helper()
	cfg, paths := healthyWorld(t)
	cfg.TTS.Provider = "kokoro"
	cfg.TTS.Kokoro.Voice = voiceID
	cfg.Voices = voice.FakeKokoro(installed...)
	return cfg, paths
}

func TestVoiceLanguageReportsAccentAndPhonemiser(t *testing.T) {
	cfg, paths := kokoroWorld(t, "bf_emma", "af_heart", "bf_emma")
	r := checkVoiceLanguage(cfg, paths)
	if r.Status != OK {
		t.Fatalf("result = %+v", r)
	}
	// The phonemiser code is the whole feature: this line is how a user
	// confirms a British voice is not being spoken with American rules.
	for _, want := range []string{"English (British)", "bf_emma", "en-gb", "female"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail missing %q: %q", want, r.Detail)
		}
	}
}

func TestVoiceLanguageFailsOnAVoiceTheMachineDoesNotHave(t *testing.T) {
	cfg, paths := kokoroWorld(t, "bf_emily", "bf_emma", "bm_george")
	r := checkVoiceLanguage(cfg, paths)
	if r.Status != Fail || !strings.Contains(r.Detail, "bf_emily") {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(r.Fix, "jarvix config set tts.kokoro.voice=b") {
		t.Errorf("fix did not offer an installed alternative: %q", r.Fix)
	}
}

func TestVoiceLanguageWarnsWhenTheVoiceSaysNothing(t *testing.T) {
	cfg, paths := kokoroWorld(t, "qq_custom", "qq_custom")
	// The fake catalog cannot hold an unparseable id, so nothing is
	// "installed" and the language check is what speaks.
	cfg.Voices = voice.Fake{}
	r := checkVoiceLanguage(cfg, paths)
	if r.Status != Warn || !strings.Contains(r.Detail, "cannot tell") {
		t.Errorf("result = %+v", r)
	}
}

func TestSpeechLanguageFailsWhenWhisperCannotServeTheVoice(t *testing.T) {
	cfg, paths := kokoroWorld(t, "ff_siwis", "ff_siwis")
	cfg.STT.Whisper.Model = "base.en"
	r := checkSpeechLanguage(cfg, paths)
	if r.Status != Fail {
		t.Fatalf("result = %+v", r)
	}
	for _, want := range []string{"French", "base.en"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail missing %q: %q", want, r.Detail)
		}
	}
	for _, want := range []string{"jarvix setup whisper base", "stt.whisper.language=fr"} {
		if !strings.Contains(r.Fix, want) {
			t.Errorf("fix missing %q: %q", want, r.Fix)
		}
	}
}

func TestSpeechLanguageFailsWhenTheLanguageSettingDisagrees(t *testing.T) {
	cfg, paths := kokoroWorld(t, "ef_dora", "ef_dora")
	cfg.STT.Whisper.Model = "base"
	cfg.STT.Whisper.Language = "en"
	r := checkSpeechLanguage(cfg, paths)
	if r.Status != Fail || !strings.Contains(r.Fix, "stt.whisper.language=es") {
		t.Errorf("result = %+v", r)
	}
}

func TestSpeechLanguageIsHappyForEnglishAccentsAndMatchedLanguages(t *testing.T) {
	cfg, paths := kokoroWorld(t, "bm_george", "bm_george")
	if r := checkSpeechLanguage(cfg, paths); r.Status != OK {
		t.Errorf("a British voice was made to justify itself: %+v", r)
	}
	cfg.TTS.Kokoro.Voice = "jf_alpha"
	cfg.Voices = voice.FakeKokoro("jf_alpha")
	cfg.STT.Whisper.Model = "large-v3"
	cfg.STT.Whisper.Language = "auto"
	r := checkSpeechLanguage(cfg, paths)
	if r.Status != OK || !strings.Contains(r.Detail, "auto-detected") {
		t.Errorf("result = %+v", r)
	}
}

// The helper is copied out of the repo by setup-kokoro.sh, so upgrading
// Jarvix does not upgrade it. The adapter degrades rather than breaking when
// it finds a stale one, which means the accent is simply wrong — and doctor
// is where that stops being silent.
func TestKokoroHelperWithoutLanguageSupportIsCaughtBeforeItIsNeeded(t *testing.T) {
	cfg, paths := kokoroWorld(t, "bf_emma", "bf_emma")
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	script := filepath.Join(data, "jarvix", "kokoro_stream.py")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("# an old helper\nparser.add_argument(\"--voice\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := checkKokoroHelperLanguage(cfg, paths)
	if r.Status != Fail || !strings.Contains(r.Fix, "setup-kokoro.sh") {
		t.Fatalf("result = %+v", r)
	}
	// The consequence, not the mechanism: the accent is wrong, which is what
	// the user is hearing.
	if !strings.Contains(r.Detail, "English (American) pronunciation") {
		t.Errorf("detail does not say what goes wrong: %q", r.Detail)
	}

	if err := os.WriteFile(script, []byte("parser.add_argument(\"--lang\", default=\"en-us\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := checkKokoroHelperLanguage(cfg, paths); r.Status != OK {
		t.Errorf("a current helper was flagged: %+v", r)
	}
}

func TestKokoroHelperCheckStaysQuietForPiperAndForAMissingHelper(t *testing.T) {
	cfg, paths := healthyWorld(t) // piper
	if r := checkKokoroHelperLanguage(cfg, paths); r.Status != OK {
		t.Errorf("piper config = %+v", r)
	}
	kok, paths := kokoroWorld(t, "bf_emma", "bf_emma")
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // no helper installed at all
	if r := checkKokoroHelperLanguage(kok, paths); r.Status != OK {
		t.Errorf("a missing helper is already checkTTS's Fail: %+v", r)
	}
}

// The settings screen is where a voice is chosen, so it is where the
// consequences of choosing one have to appear.
func TestSettingsChecksIncludeTheLanguageChecks(t *testing.T) {
	cfg, paths := kokoroWorld(t, "bf_emma", "bf_emma")
	related := map[string]string{}
	for _, r := range SettingsChecks(cfg, paths) {
		related[r.Name] = r.Related
	}
	if related["voice language"] != "tts.kokoro.voice" {
		t.Errorf("voice language check not tied to the voice field: %q", related["voice language"])
	}
	if related["speech recognition language"] != "stt.whisper.language" {
		t.Errorf("speech language check not tied to the language field: %q", related["speech recognition language"])
	}
}

func TestRunIncludesTheLanguageChecks(t *testing.T) {
	cfg, paths := kokoroWorld(t, "bf_emma", "bf_emma")
	results := Run(cfg, paths)
	for _, name := range []string{"voice language", "speech recognition language", "Kokoro helper supports accents"} {
		if r := resultByName(t, results, name); r.Name != name {
			t.Errorf("doctor does not run %q", name)
		}
	}
}
