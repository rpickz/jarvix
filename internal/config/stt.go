package config

import (
	"strings"
	"unicode"
)

// This file is the input-vocabulary half of name recognition (issue #83):
// composing the bias prompt whisper decodes under, and validating the terms
// that feed it. The output-side counterpart is tts.lexicon — deliberately a
// separate vocabulary, because what the user says and what Jarvix says are
// different word lists.

// STTBiasPrompt composes the initial prompt both transcription paths carry:
// the assistant's name ([assistant], issue #103) plus every [stt] vocabulary
// term. Empty when there is nothing to bias toward, which switches the
// mechanism off entirely.
//
// The shape is deliberate, and was chosen against the real base.en model
// rather than on theory. whisper.cpp conditions its decoder on this text as
// if it were the preceding transcript, and two failure modes fall out of the
// obvious formats: a bare "Jarvix" prompt gets *absorbed* — audio that opens
// with the name decodes as a continuation, and the name vanishes from the
// transcript — and a lowercase name biases toward the lowercase token, which
// still absorbs a leading occurrence. A short sentence *about* the name, with
// the name capitalised as the proper noun it is, biased every tested
// mishearing back to "Jarvix" while leaving the audio's own words intact.
// Case is presentation only: everything that matches the transcript afterwards
// is case-insensitive.
func (c Config) STTBiasPrompt() string {
	return c.STTBiasPromptWith(nil)
}

// STTBiasPromptWith is STTBiasPrompt plus the taught hard-to-hear phrases
// (issue #129) — the ONE copy of the bias sentence composition, so a taught
// phrase and an [stt] vocabulary term enter whisper's conditioning in
// exactly the same shape (a full capitalised sentence; bare terms get
// absorbed, see above). Taught phrases join the same "Conversations may
// mention" sentence as the configured terms, deduplicated case-insensitively
// so a phrase present in both biases once. The caller bounds taught (the
// store's MaxHardToHear cap); this function only composes.
func (c Config) STTBiasPromptWith(taught []string) string {
	var parts []string
	if name := capitalise(strings.TrimSpace(c.Assistant.Name)); name != "" {
		parts = append(parts, "The assistant is called "+name+".")
	}
	var terms []string
	seen := make(map[string]bool, len(c.STT.Vocabulary)+len(taught))
	add := func(t string) {
		t = strings.TrimSpace(t)
		key := strings.ToLower(t)
		if t == "" || seen[key] {
			return
		}
		seen[key] = true
		terms = append(terms, t)
	}
	for _, t := range c.STT.Vocabulary {
		add(t)
	}
	for _, t := range taught {
		add(t)
	}
	if len(terms) > 0 {
		parts = append(parts, "Conversations may mention: "+strings.Join(terms, ", ")+".")
	}
	return strings.Join(parts, " ")
}

// STTBiasPromptFunc returns the closure a transcription path reads its bias
// prompt through: the composition above, evaluated per call, over whatever
// hard-to-hear phrases taught reports at that moment.
//
// It exists so there is ONE answer to "what prompt would the daemon actually
// send?", reachable from outside the daemon. The daemon builds its
// transcribers here (daemon.fillDeps); the voice-corpus harness (issue #143)
// has to bias its whisper runs identically or it would be measuring a
// pipeline nobody uses — and a harness that hard-codes "The assistant is
// called Jarvix." would keep passing after the user renames the assistant or
// teaches a word, which is exactly the regression the corpus is for.
//
// taught is nil when there is no vocabulary store to consult (the feature is
// off, or the caller has none). That is a legitimate state, not an error, and
// it must be expressed as a nil func rather than as a nil store: a typed-nil
// *vocabulary.Store behind an interface reads as present and panics on first
// use, which is the kind of mistake this signature makes impossible.
func (c Config) STTBiasPromptFunc(taught func() []string) func() string {
	if taught == nil {
		return c.STTBiasPrompt
	}
	return func() string { return c.STTBiasPromptWith(taught()) }
}

// capitalise upper-cases the first rune. The bias prompt presents the
// assistant's name as the proper noun it is, and the capitalised token is the
// one whisper renders a sentence-leading proper noun with — see STTBiasPrompt
// for the evidence.
func capitalise(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// sttProblems reports configuration problems in the [stt] vocabulary. The
// terms are spliced into a prompt, so the only thing to reject is an entry
// with no content — everything else is a legitimate word to bias toward.
func (c Config) sttProblems() []string {
	var problems []string
	for _, t := range c.STT.Vocabulary {
		if strings.TrimSpace(t) == "" {
			problems = append(problems,
				"stt.vocabulary contains an empty entry; each one must be a term to bias speech recognition toward (e.g. \"Hyprland\")")
			break
		}
	}
	return problems
}
