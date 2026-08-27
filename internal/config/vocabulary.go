package config

import "fmt"

// Vocabulary configures the taught vocabulary (issue #129): the words and
// phrases the user explicitly teaches Jarvix — "when I say quid I mean
// pounds" — stored in one hand-editable file under the XDG state dir and
// offered to the model on every turn, beside the remembered facts.
//
// On by default for the memory book's reason: nothing enters the store
// without the user teaching it explicitly, so the trust decision is made per
// phrase, not per install. Switching the feature off only decides whether
// taught words are *consulted*; the store survives, and deletion is always
// an explicit act.
type Vocabulary struct {
	// Enabled turns the feature on: the vocabulary tools and voice phrases
	// are registered and the taught words are injected each turn. Off, the
	// tools do not exist and nothing is injected — but the store file is
	// left alone.
	Enabled bool `toml:"enabled"`
	// MaxEntries caps how many taught phrases the store holds. Teaching
	// warns as the store approaches the cap and refuses at it, with the fix
	// named.
	MaxEntries int `toml:"max_entries"`
	// MaxInjectedTokens caps (in estimated tokens, ~4 chars each) what the
	// vocabulary block may add to one model turn. Entries that do not fit
	// are dropped from the block only — never from the store — least
	// recently taught first, and the model is told the list is incomplete.
	MaxInjectedTokens int `toml:"max_injected_tokens"`
	// SpeakBack lets Jarvix use the taught words in its own replies.
	// Default false, deliberately: the vocabulary exists so Jarvix
	// *understands* the user, and mirrored slang from a machine reads as
	// mockery more often than rapport — an in-joke is the user's to make,
	// not the assistant's to perform. A user who wants their words back
	// says so here.
	SpeakBack bool `toml:"speak_back"`
}

// vocabularyProblems validates the [vocabulary] table.
func (c Config) vocabularyProblems() []string {
	var problems []string
	if c.Vocabulary.MaxEntries <= 0 {
		problems = append(problems,
			"vocabulary.max_entries must be positive (how many taught phrases the store may hold)")
	}
	if c.Vocabulary.MaxInjectedTokens < MinVocabularyInjectedTokens {
		problems = append(problems, fmt.Sprintf(
			"vocabulary.max_injected_tokens is %d; it must be at least %d — below that not even "+
				"one taught phrase fits and the vocabulary would be silently useless while looking enabled",
			c.Vocabulary.MaxInjectedTokens, MinVocabularyInjectedTokens))
	}
	return problems
}

// MinVocabularyInjectedTokens is the floor on the injection budget; see
// vocabulary.MinInjectedTokens for the reasoning. Mirrored here because
// config deliberately does not import internal/vocabulary (the memory
// arrangement, ADR 0025).
const MinVocabularyInjectedTokens = 150
