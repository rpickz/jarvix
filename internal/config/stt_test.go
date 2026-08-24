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

// The sentence form survives a rename (issue #103): whatever the user calls
// their assistant, the bias stays a capitalised full sentence *about* the
// name — the shape that stopped whisper absorbing it (issue #83). A custom
// name must never regress to a bare-word prompt.
func TestSTTBiasPromptFollowsAConfiguredName(t *testing.T) {
	for _, c := range []struct{ name, want string }{
		{"Hal", "The assistant is called Hal."},
		{"hal", "The assistant is called Hal."},
		{"Mister Smith", "The assistant is called Mister Smith."},
		{"  Hal  ", "The assistant is called Hal."},
	} {
		cfg := Default()
		cfg.Assistant.Name = c.name
		if got := cfg.STTBiasPrompt(); got != c.want {
			t.Errorf("name %q: bias prompt = %q, want %q", c.name, got, c.want)
		}
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

// No assistant name and no vocabulary means no prompt at all: an empty bias
// must switch the flag/field off entirely, not send whisper an empty string.
// (A blank name no longer survives validation, but the composition must not
// depend on validation having been run.)
func TestSTTBiasPromptIsEmptyWithNothingToBiasToward(t *testing.T) {
	cfg := Default()
	cfg.Assistant.Name = "  "
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
	if got := Default().Assistant.EffectiveAliases(); !reflect.DeepEqual(got, want) {
		t.Errorf("default name aliases = %v, want %v (update internal/session's mirror too)", got, want)
	}
}

// The old [activation] wake_aliases key is refused with directions, in every
// mode: silently reverting a tuned list to the shipped one would look exactly
// like the mishearing bug the aliases exist to fix (issue #103).
func TestLegacyWakeAliasesKeyIsRefusedWithDirections(t *testing.T) {
	cfg := Default() // push_to_talk
	cfg.Activation.WakeAliases = []string{"jarvis"}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "activation.wake_aliases has moved") {
		t.Errorf("a stale wake_aliases key was not refused with directions: %v", err)
	}
	if !strings.Contains(err.Error(), "[assistant]") {
		t.Errorf("the refusal does not say where the key went: %v", err)
	}
}
