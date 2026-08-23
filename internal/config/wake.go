package config

import (
	"fmt"
	"strings"
	"time"
)

// This file validates the background-listening settings (ADR 0024).
//
// The checks divide in two, and the division is deliberate. Anything that is
// nonsense in *any* mode — a sensitivity outside 0..1, a pre-roll longer than
// the privacy ceiling — is rejected whatever activation.mode says, so a value
// cannot sit unnoticed in the file waiting to take effect the day someone
// switches the mode on. Anything that only matters once the microphone is
// actually open is checked only when it is.

// wakeProblems reports configuration problems in the [activation] wake
// settings.
func (c Config) wakeProblems() []string {
	var problems []string
	a := c.Activation

	if a.WakeSensitivity < 0 || a.WakeSensitivity > 1 {
		problems = append(problems, fmt.Sprintf(
			"activation.wake_sensitivity is %.2f; it must be between 0 and 1 (higher is more eager, 0.5 is the default)",
			a.WakeSensitivity))
	}
	if a.WakeRingMs < 0 {
		problems = append(problems, "activation.wake_ring_ms must not be negative (0 keeps no audio from before the wake word)")
	} else if a.WakeRingMs > MaxWakeRingMs {
		problems = append(problems, fmt.Sprintf(
			"activation.wake_ring_ms is %d; %d is the maximum. This is the only ambient audio that can ever reach a transcript, so the limit is a privacy guarantee rather than a tuning range",
			a.WakeRingMs, MaxWakeRingMs))
	}
	if a.EndpointSilenceMs < 0 {
		problems = append(problems, "activation.endpoint_silence_ms must not be negative")
	}
	if a.MaxUtteranceSec < 0 {
		problems = append(problems, "activation.max_utterance_sec must not be negative")
	}
	// Aliases are checked in every mode: a broken entry must not sit unnoticed
	// until the day wake_word mode is switched on. The strip compares one
	// whitespace-delimited transcript word at a time, so an alias containing
	// whitespace could never match anything — reject it rather than let it
	// silently do nothing.
	for _, alias := range a.WakeAliases {
		switch {
		case strings.TrimSpace(alias) == "":
			problems = append(problems,
				"activation.wake_aliases contains an empty entry; each one must be a single word the wake word is misheard as (e.g. \"jarvis\")")
		case strings.ContainsAny(alias, " \t"):
			problems = append(problems, fmt.Sprintf(
				"activation.wake_aliases entry %q contains whitespace; the transcript strip matches single words, so a multi-word alias can never match", alias))
		}
	}

	if !a.WakeWordEnabled() {
		return problems
	}

	if strings.TrimSpace(a.WakeWord) == "" {
		problems = append(problems,
			"activation.wake_word is empty; set the word that should summon Jarvix (e.g. \"jarvix\")")
	}
	if len(a.WakeCommand) == 0 || strings.TrimSpace(a.WakeCommand[0]) == "" {
		problems = append(problems,
			"activation.wake_command is empty; background listening needs a detector helper (default: [\"jarvix-wake\"], installed by scripts/setup-wake.sh)")
	}
	for _, arg := range a.WakeCommand {
		if strings.ContainsAny(arg, " \t") {
			problems = append(problems, fmt.Sprintf(
				"activation.wake_command entry %q contains whitespace; the helper is launched directly, not through a shell, so each entry is one argument", arg))
			break
		}
	}
	if a.EndpointSilenceMs < minEndpointSilenceMs || a.EndpointSilenceMs > maxEndpointSilenceMs {
		problems = append(problems, fmt.Sprintf(
			"activation.endpoint_silence_ms is %d; use between %d and %d (default 800 — below that a mid-sentence pause submits, above it every request ends in a wait)",
			a.EndpointSilenceMs, minEndpointSilenceMs, maxEndpointSilenceMs))
	}
	if a.MaxUtteranceSec <= 0 {
		problems = append(problems, "activation.max_utterance_sec must be positive; it bounds one hands-free request")
	}
	return problems
}

// The endpoint window a person can actually work with. Below the floor a
// natural pause between clauses submits half a sentence; above the ceiling
// the wait after speaking stops feeling like a conversation.
const (
	minEndpointSilenceMs = 200
	maxEndpointSilenceMs = 5000
)

// WakeRing is the configured pre-roll as a duration.
func (a Activation) WakeRing() time.Duration {
	return time.Duration(a.WakeRingMs) * time.Millisecond
}

// EndpointSilence is the configured endpoint threshold as a duration.
func (a Activation) EndpointSilence() time.Duration {
	return time.Duration(a.EndpointSilenceMs) * time.Millisecond
}

// MaxUtterance is the configured per-request ceiling as a duration.
func (a Activation) MaxUtterance() time.Duration {
	return time.Duration(a.MaxUtteranceSec) * time.Second
}
