package config

import (
	"reflect"
	"strings"
	"testing"
)

// The prompt's shape was chosen against the real base.en model (issue #83): a
// bare vocabulary list gets absorbed when the audio opens with the same word,
// and a lowercase name biases toward a token that absorbs too. What must hold
// forever is the shape that worked — a sentence about the name, capitalised —
// with the user's vocabulary appended.
func TestSTTBiasPromptNamesTheAssistant(t *testing.T) {
	got := Default().STTBiasPrompt()
	if got != "The assistant is called Jarvix." {
		t.Errorf("default bias prompt = %q", got)
	}
}

func TestSTTBiasPromptAppendsTheVocabulary(t *testing.T) {
	cfg := Default()
	cfg.STT.Vocabulary = []string{"Hyprland", " kubectl "}
	got := cfg.STTBiasPrompt()
	want := "The assistant is called Jarvix. Conversations may mention: Hyprland, kubectl."
	if got != want {
		t.Errorf("bias prompt = %q, want %q", got, want)
	}
}

// No wake word and no vocabulary means no prompt at all: an empty bias must
// switch the flag/field off entirely, not send whisper an empty string.
func TestSTTBiasPromptIsEmptyWithNothingToBiasToward(t *testing.T) {
	cfg := Default()
	cfg.Activation.WakeWord = "  "
	cfg.STT.Vocabulary = nil
	if got := cfg.STTBiasPrompt(); got != "" {
		t.Errorf("bias prompt = %q, want empty", got)
	}

	cfg.STT.Vocabulary = []string{"Hyprland"}
	if got := cfg.STTBiasPrompt(); got != "Conversations may mention: Hyprland." {
		t.Errorf("vocabulary-only bias prompt = %q", got)
	}
}

func TestSTTVocabularyRejectsEmptyEntries(t *testing.T) {
	cfg := Default()
	cfg.STT.Vocabulary = []string{"Hyprland", "  "}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "stt.vocabulary") {
		t.Errorf("an empty vocabulary entry was accepted: %v", err)
	}
}

// The shipped aliases are the mishearings whisper's English models actually
// produce for "jarvix" (observed with base.en). The session tests mirror this
// list rather than import it, so it is pinned here: a change on either side
// must be made deliberately, in both places.
func TestDefaultWakeAliasesAreTheKnownMishearings(t *testing.T) {
	want := []string{"jarvis", "javax", "jarvic", "jarvicks", "jarvex"}
	if got := Default().Activation.WakeAliases; !reflect.DeepEqual(got, want) {
		t.Errorf("default wake aliases = %v, want %v (update internal/session's mirror too)", got, want)
	}
}

// Alias validation runs in every activation mode: a broken entry must not sit
// unnoticed until the day wake_word mode is switched on.
func TestWakeAliasesAreValidatedInEveryMode(t *testing.T) {
	cfg := Default() // push_to_talk
	cfg.Activation.WakeAliases = []string{"jarvis", ""}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "wake_aliases") {
		t.Errorf("an empty alias was accepted in push-to-talk mode: %v", err)
	}

	cfg.Activation.WakeAliases = []string{"the jarvis"}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("a multi-word alias was accepted: %v", err)
	}

	// Clearing the aliases entirely is a choice, and a valid one.
	cfg.Activation.WakeAliases = nil
	if err := cfg.Validate(); err != nil {
		t.Errorf("empty aliases were rejected: %v", err)
	}
}
