package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/voice"
)

// kokoroConfig is a valid configuration speaking through Kokoro with the
// given voice, and a fake catalog standing in for the installed archive.
func kokoroConfig(voiceID string, installed ...string) Config {
	cfg := Default()
	cfg.TTS.Provider = "kokoro"
	cfg.TTS.Kokoro.Voice = voiceID
	if len(installed) > 0 {
		cfg.Voices = voice.FakeKokoro(installed...)
	}
	return cfg
}

func problems(t *testing.T, cfg Config) string {
	t.Helper()
	err := cfg.Validate()
	if err == nil {
		return ""
	}
	return err.Error()
}

// The point of validating against the installed list is timing: a wrong voice
// id otherwise surfaces seconds after a question, as a failed answer, with the
// helper's error rather than the user's mistake in it.
func TestUninstalledVoiceIsRejectedWithAlternatives(t *testing.T) {
	cfg := kokoroConfig("bf_emily", "af_heart", "bf_emma", "bf_alice", "bm_george")
	got := problems(t, cfg)
	if !strings.Contains(got, "bf_emily") || !strings.Contains(got, "not installed") {
		t.Fatalf("validation did not reject the voice: %v", got)
	}
	// The alternatives must be useful, which means British ones for a British
	// id — not the alphabetically first thing in the archive.
	for _, want := range []string{"bf_emma", "bf_alice", "bm_george"} {
		if !strings.Contains(got, want) {
			t.Errorf("message did not offer %s: %v", want, got)
		}
	}
	if !strings.Contains(got, "jarvix voices") {
		t.Errorf("message did not say how to see the rest: %v", got)
	}
}

func TestInstalledVoicePasses(t *testing.T) {
	if got := problems(t, kokoroConfig("bf_emma", "af_heart", "bf_emma")); got != "" {
		t.Errorf("an installed voice was rejected: %v", got)
	}
}

// Validation runs on machines that never installed Kokoro, and inside tests
// that must not read a 27 MB archive. "Cannot tell" has to mean "do not
// object", or the Piper default would stop validating the moment this check
// existed.
func TestUnknownCatalogNeverBlocksValidation(t *testing.T) {
	cfg := kokoroConfig("bf_emma")
	cfg.Voices = nil
	if got := problems(t, cfg); got != "" {
		t.Errorf("a nil catalog objected: %v", got)
	}
	cfg.Voices = voice.Fake{Err: errors.New("not installed")}
	if got := problems(t, cfg); got != "" {
		t.Errorf("an unreadable catalog objected: %v", got)
	}
}

// The second-order requirement, and the one a user experiences as a broken
// assistant rather than a misconfiguration: a French voice with the
// English-only base.en would keep transcribing English, silently.
func TestNonEnglishVoiceWithAnEnglishOnlyModelIsRefused(t *testing.T) {
	cfg := kokoroConfig("ff_siwis", "ff_siwis")
	got := problems(t, cfg)
	if got == "" {
		t.Fatal("a French voice with base.en was accepted")
	}
	for _, want := range []string{"French", "base.en", "jarvix setup whisper base", "stt.whisper.language=fr"} {
		if !strings.Contains(got, want) {
			t.Errorf("message is not actionable — missing %q: %v", want, got)
		}
	}
}

func TestNonEnglishVoiceWithAMultilingualModelAndMatchingLanguagePasses(t *testing.T) {
	cfg := kokoroConfig("ff_siwis", "ff_siwis")
	cfg.STT.Whisper.Model = "base"
	cfg.STT.Whisper.Language = "fr"
	if got := problems(t, cfg); got != "" {
		t.Errorf("a correctly configured French setup was rejected: %v", got)
	}
	// "auto" is whisper's own detection and serves any language.
	cfg.STT.Whisper.Language = "auto"
	if got := problems(t, cfg); got != "" {
		t.Errorf("auto-detection was rejected: %v", got)
	}
}

func TestMultilingualModelStillListeningInEnglishIsRefused(t *testing.T) {
	cfg := kokoroConfig("ef_dora", "ef_dora")
	cfg.STT.Whisper.Model = "base"
	cfg.STT.Whisper.Language = "en"
	got := problems(t, cfg)
	if !strings.Contains(got, "Spanish") || !strings.Contains(got, "stt.whisper.language=es") {
		t.Errorf("a Spanish voice listening in English was accepted or unhelpfully rejected: %v", got)
	}
}

func TestEmptySpeechLanguageIsRefusedForANonEnglishVoice(t *testing.T) {
	cfg := kokoroConfig("jf_alpha", "jf_alpha")
	cfg.STT.Whisper.Model = "base"
	cfg.STT.Whisper.Language = ""
	got := problems(t, cfg)
	if !strings.Contains(got, "defaults to English") {
		t.Errorf("an empty language was accepted for a Japanese voice: %v", got)
	}
}

// Both English accents are English to whisper.cpp, which has no notion of
// accent — so switching to a British voice must not demand a new model.
func TestBritishVoiceNeedsNoSpeechRecognitionChange(t *testing.T) {
	if got := problems(t, kokoroConfig("bf_emma", "bf_emma")); got != "" {
		t.Errorf("a British voice was made to justify itself to whisper: %v", got)
	}
}

// The default configuration must stay valid; it is what an empty config.toml
// produces on every machine.
func TestDefaultConfigurationIsUnaffected(t *testing.T) {
	if got := problems(t, Default()); got != "" {
		t.Errorf("defaults stopped validating: %v", got)
	}
}

func TestSpokenLanguageDerivesFromTheSelectedEngine(t *testing.T) {
	cfg := Default()
	cfg.TTS.Provider = "kokoro"
	cfg.TTS.Kokoro.Voice = "bm_lewis"
	if lang, ok := cfg.SpokenLanguage(); !ok || lang.Code != "en-gb" {
		t.Errorf("kokoro: %+v %v", lang, ok)
	}
	cfg.TTS.Provider = "piper"
	cfg.TTS.Piper.Voice = "en_GB-alba-medium"
	if lang, ok := cfg.SpokenLanguage(); !ok || lang.Code != "en-gb" {
		t.Errorf("piper: %+v %v", lang, ok)
	}
	// A path to an arbitrary model says nothing, and must not be guessed at.
	cfg.TTS.Piper.Voice = "/opt/voices/custom.onnx"
	if _, ok := cfg.SpokenLanguage(); ok {
		t.Error("an arbitrary model path was assigned a language")
	}
}

// Piper voices are an open set — any .onnx from the upstream collection, at
// any path, from packages that differ by distribution — so a config-time
// whitelist would refuse working setups more often than it caught broken
// ones. The language derived from a Piper voice is still enforced against
// speech recognition, which is the part that silently breaks.
func TestPiperVoicesAreNotWhitelistedButTheirLanguageStillCounts(t *testing.T) {
	cfg := Default()
	cfg.Voices = voice.Fake{List: []voice.Voice{
		{ID: "en_US-amy-medium", Language: mustLanguage(t, "en-us")},
	}}
	cfg.TTS.Piper.Voice = "/opt/voices/whatever-i-downloaded.onnx"
	if got := problems(t, cfg); got != "" {
		t.Errorf("an arbitrary Piper model path was rejected: %v", got)
	}
	cfg.TTS.Piper.Voice = "fr_FR-siwis-medium"
	got := problems(t, cfg)
	if !strings.Contains(got, "French") || !strings.Contains(got, "base.en") {
		t.Errorf("a French Piper voice did not pull speech recognition with it: %v", got)
	}
}

func mustLanguage(t *testing.T, code string) voice.Language {
	t.Helper()
	l, ok := voice.LanguageByCode(code)
	if !ok {
		t.Fatalf("no language %q", code)
	}
	return l
}

// whisper.cpp encodes English-only-ness in the model name and nowhere else,
// including when the name is an absolute path to a ggml file.
func TestEnglishOnlyWhisperModel(t *testing.T) {
	cases := map[string]bool{
		"base.en":                          true,
		"tiny.en":                          true,
		"small.en":                         true,
		"/models/whisper/ggml-base.en.bin": true,
		"base":                             false,
		"large-v3":                         false,
		"large-v3-turbo":                   false,
		"/models/whisper/ggml-medium.bin":  false,
		"":                                 false,
	}
	for model, want := range cases {
		if got := EnglishOnlyWhisperModel(model); got != want {
			t.Errorf("EnglishOnlyWhisperModel(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestInstalledVoicesFollowsTheConfiguredEngine(t *testing.T) {
	paths := Paths{Data: "/data/jarvix"}
	if want := "/data/jarvix/models/kokoro/voices-v1.0.bin"; paths.KokoroVoicesFile() != want {
		t.Errorf("KokoroVoicesFile = %q, want %q", paths.KokoroVoicesFile(), want)
	}
	cfg := Default()
	cfg.TTS.Provider = "kokoro"
	if _, ok := cfg.InstalledVoices(paths).(*voice.KokoroArchive); !ok {
		t.Errorf("kokoro config got %T", cfg.InstalledVoices(paths))
	}
	cfg.TTS.Provider = "piper"
	if _, ok := cfg.InstalledVoices(paths).(*voice.PiperDir); !ok {
		t.Errorf("piper config got %T", cfg.InstalledVoices(paths))
	}
}
