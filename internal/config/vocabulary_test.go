package config

import (
	"strings"
	"testing"
)

// The config half of issue #129: the [vocabulary] table's defaults and
// validation, the settings-registry rows (speak_back's class and default in
// particular), and the one-copy bias composition — taught hard-to-hear
// phrases enter whisper's prompt through the same sentence rule as
// stt.vocabulary terms.

func TestVocabularyDefaults(t *testing.T) {
	cfg := Default()
	if !cfg.Vocabulary.Enabled {
		t.Error("vocabulary.enabled default = false, want on (explicit teaching is the trust gate)")
	}
	if cfg.Vocabulary.SpeakBack {
		t.Error("vocabulary.speak_back default = true, want false — mirrored slang is opt-in")
	}
	if cfg.Vocabulary.MaxEntries != 200 || cfg.Vocabulary.MaxInjectedTokens != 300 {
		t.Errorf("vocabulary caps = %d/%d, want 200/300",
			cfg.Vocabulary.MaxEntries, cfg.Vocabulary.MaxInjectedTokens)
	}
}

func TestVocabularyValidation(t *testing.T) {
	cfg := Default() // nil Voices: the catalog does not object (voice.go)
	cfg.Vocabulary.MaxEntries = 0
	cfg.Vocabulary.MaxInjectedTokens = MinVocabularyInjectedTokens - 1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("an invalid [vocabulary] table validated")
	}
	for _, want := range []string{"vocabulary.max_entries", "vocabulary.max_injected_tokens"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation error is missing %q: %v", want, err)
		}
	}
}

// TestVocabularySettingsRows pins the registry: the three construction-wired
// keys are restart-class, speak_back is idle-class (it only shapes the
// block's stance sentence, rebuilt with the engine's collaborators), and
// none of them is dangerous — teaching writes only the user's own store.
func TestVocabularySettingsRows(t *testing.T) {
	classes := map[string]Reload{
		"vocabulary.enabled":             ReloadRestart,
		"vocabulary.max_entries":         ReloadRestart,
		"vocabulary.max_injected_tokens": ReloadRestart,
		"vocabulary.speak_back":          ReloadIdle,
	}
	for key, wantClass := range classes {
		s, ok := SettingFor(key)
		if !ok {
			t.Errorf("no registry row for %s", key)
			continue
		}
		if s.Reload != wantClass {
			t.Errorf("%s reload = %s, want %s", key, s.Reload, wantClass)
		}
		if s.Dangerous {
			t.Errorf("%s is marked dangerous; it only affects the user's own store", key)
		}
	}
	// The rows read and write the struct.
	cfg := Default()
	s, _ := SettingFor("vocabulary.speak_back")
	if err := s.Apply(&cfg, true); err != nil {
		t.Fatal(err)
	}
	if !cfg.Vocabulary.SpeakBack || s.Get(cfg) != true {
		t.Errorf("speak_back did not round-trip through its row")
	}
}

// TestBiasPromptMergesTaughtPhrases is the one-copy seam (#107/#129): taught
// hard-to-hear phrases join the same capitalised "Conversations may mention"
// sentence as stt.vocabulary terms — never a second sentence shape whisper
// could absorb — deduplicated case-insensitively across the two sources.
func TestBiasPromptMergesTaughtPhrases(t *testing.T) {
	cfg := Default()
	cfg.Assistant.Name = "jarvix"
	cfg.STT.Vocabulary = []string{"Hyprland", " quid "}

	got := cfg.STTBiasPromptWith([]string{"Quid", "telly"})
	want := "The assistant is called Jarvix. Conversations may mention: Hyprland, quid, telly."
	if got != want {
		t.Errorf("STTBiasPromptWith = %q, want %q", got, want)
	}

	// Nil taught phrases compose exactly the pre-#129 prompt — the shared
	// composition cannot drift between its two callers.
	if with, without := cfg.STTBiasPromptWith(nil), cfg.STTBiasPrompt(); with != without {
		t.Errorf("STTBiasPromptWith(nil) = %q, STTBiasPrompt() = %q; one copy must mean one result",
			with, without)
	}
}

// TestBiasPromptTaughtOnly: with no [stt] vocabulary at all, taught phrases
// still get the full sentence — a bare term list would be absorbed (#83).
func TestBiasPromptTaughtOnly(t *testing.T) {
	cfg := Default()
	cfg.STT.Vocabulary = nil
	got := cfg.STTBiasPromptWith([]string{"telly"})
	if !strings.Contains(got, "Conversations may mention: telly.") {
		t.Errorf("STTBiasPromptWith = %q, want the sentence around the taught phrase", got)
	}
}
