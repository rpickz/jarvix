package config

import "fmt"

// Memory configures the knowledge base (ADR 0025): the facts the user
// explicitly asks Jarvix to remember, stored in one hand-editable file under
// the XDG state dir and offered to the model on every turn.
//
// It is on by default, unlike shell access or typing, because nothing enters
// the store without the user saying "remember ..." out loud — the trust
// model is explicit-write, and switching the feature off only decides
// whether the stored facts are *consulted*, never whether they survive:
// disabling memory does not delete the store. Deletion is always an explicit
// act ("forget ...", `jarvix memory forget`, or deleting the file).
type Memory struct {
	// Enabled turns the feature on: the memory tools are registered and the
	// remembered facts are injected each turn. Off, the tools do not exist
	// and nothing is stored or injected — but the store file is left alone.
	Enabled bool `toml:"enabled"`
	// MaxFacts caps how many facts the store holds. Remembering warns as the
	// store approaches the cap and refuses at it, with the fix named.
	MaxFacts int `toml:"max_facts"`
	// MaxInjectedTokens caps (in estimated tokens, ~4 chars each) what the
	// remembered-facts block may add to one model turn. Facts that do not
	// fit are dropped from the block only — never from the store — least
	// recently confirmed first, and the model is told the list is incomplete.
	MaxInjectedTokens int `toml:"max_injected_tokens"`
}

// memoryProblems validates the [memory] table.
func (c Config) memoryProblems() []string {
	var problems []string
	if c.Memory.MaxFacts <= 0 {
		problems = append(problems,
			"memory.max_facts must be positive (how many remembered facts the store may hold)")
	}
	if c.Memory.MaxInjectedTokens < MinMemoryInjectedTokens {
		problems = append(problems, fmt.Sprintf(
			"memory.max_injected_tokens is %d; it must be at least %d — below that not even "+
				"one fact fits and memory would be silently useless while looking enabled",
			c.Memory.MaxInjectedTokens, MinMemoryInjectedTokens))
	}
	return problems
}

// MinMemoryInjectedTokens is the floor on the injection budget; see
// memory.MinInjectedTokens for the reasoning. Mirrored here because config
// deliberately does not import internal/memory.
const MinMemoryInjectedTokens = 100
