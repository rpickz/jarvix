package config

import (
	"github.com/rpickz/jarvix/internal/intent"
)

// Intents configures the deterministic intent router (ADR 0017): the pattern
// table that answers "volume thirty", "mute", and "workspace four" locally,
// in milliseconds, without a model call. Everything it does not recognise
// verbatim goes to the assistant unchanged.
type Intents struct {
	// Enabled turns the router on. Default true — the intents it handles are
	// ones nobody wants to wait on a model for.
	Enabled bool `toml:"enabled"`
	// Terminal is the program "open terminal" launches. It is executed
	// directly, not through a shell, so it must be a single executable name
	// or absolute path.
	Terminal string `toml:"terminal"`
	// Custom holds user-defined intents ([[intents.custom]]). Their commands
	// run through the tool permission gate exactly like the assistant's.
	Custom []CustomIntent `toml:"custom"`
}

// CustomIntent is one user-defined intent.
type CustomIntent struct {
	// Match is the literal phrase to recognise, e.g. "lock the screen".
	// Matching is whole-utterance and exact; placeholders are not accepted
	// because a slot would have to be interpolated into Run.
	Match string `toml:"match"`
	// Run is the shell command to execute, subject to [tools.policy].
	Run string `toml:"run"`
	// Say is the spoken acknowledgement; empty says "Done."
	Say string `toml:"say"`
}

// intentProblems validates the intent table, naming any offending entry.
// Compiling the real router is the check: there is no second, weaker set of
// rules that configuration could pass and the daemon then reject. Routine
// phrases compile here too, so a routine phrase colliding with a built-in or
// custom intent is caught by the same rules that would route it.
func (c Config) intentProblems() []string {
	if !c.Intents.Enabled {
		return nil
	}
	if _, err := intent.New(c.IntentOptions()); err != nil {
		return []string{err.Error()}
	}
	return nil
}

// IntentOptions builds the router options from configuration: the terminal,
// the custom intents, and the routines' trigger phrases (ADR 0026).
func (c Config) IntentOptions() intent.Options {
	custom := make([]intent.Custom, 0, len(c.Intents.Custom))
	for _, e := range c.Intents.Custom {
		custom = append(custom, intent.Custom{Match: e.Match, Run: e.Run, Say: e.Say})
	}
	routines := make([]intent.RoutinePhrases, 0, len(c.Routines))
	for _, r := range c.Routines {
		routines = append(routines, intent.RoutinePhrases{
			Name: r.Name, Phrases: append([]string(nil), r.Phrases...),
		})
	}
	return intent.Options{Terminal: c.Intents.Terminal, Custom: custom, Routines: routines}
}
