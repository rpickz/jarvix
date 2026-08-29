package stt

import (
	"strings"
	"unicode"
)

// This file holds the second line of defence against a capture that becomes
// speech (issue #191).
//
// Whisper conditions its decoder on an initial prompt. Jarvix supplies one so
// that "Jarvix" is not heard as "Jarvis" (issue #83, generalised over the
// configured name in #107) and so that taught hard-to-hear phrases are in the
// decoder's vocabulary (#129). The cost of that bias is that when there is
// nothing to transcribe, the likeliest continuation of the prompt *is* the
// prompt, and whisper returns it as a transcript. Reproduced on the machine:
//
//	$ whisper-cli -m ggml-base.en.bin -f silence.wav \
//	      --prompt "The assistant is called Jarvix." --no-timestamps
//	 The assistant is called Jarvix.
//
// The sentence added so Jarvix would hear its name is the sentence it invents
// when it hears nothing. A phantom utterance is a phantom instruction: it
// reaches the intent router and the model, counts as the user being present,
// and lands in the archive as something they said.
//
// The rule compares the transcript against **the prompt the daemon actually
// sent**, never a hard-coded sentence. The bias set is composed at call time
// from the configured assistant name and the taught phrases, both of which the
// user can change while the daemon runs; a literal here would silently stop
// covering the words it was written for.

// IsPromptEcho reports whether transcript is nothing but the bias prompt
// handed back.
//
// It is deliberately an equality test after normalisation, not a containment
// test. "Jarvix, what is the assistant called?" contains the name, shares most
// of its words with the bias sentence, and is a perfectly ordinary thing to
// say; only a transcript that is *wholly* one of the injected sentences — or
// wholly the whole prompt — is an echo. Discarding real speech would be a
// worse bug than the one this fixes.
//
// Normalisation folds case, punctuation and surrounding space, because whisper
// returns its echo with a leading space and its own idea of terminal
// punctuation (" The assistant is called Jarvix." above), and because the
// prompt's own capitalisation is a presentation choice made in
// config.STTBiasPromptWith rather than anything the decoder promises to
// reproduce.
//
// Both are compared sentence by sentence as well as whole: the prompt is
// composed of up to two independent sentences ("The assistant is called X."
// and "Conversations may mention: a, b, c.") and whisper is as likely to echo
// one of them as both.
func IsPromptEcho(transcript, prompt string) bool {
	text := normaliseForEcho(transcript)
	if text == "" {
		// An empty transcript is already nothing; it is the caller's
		// no-speech path, not an echo, and saying otherwise would report the
		// wrong reason to a user debugging their microphone.
		return false
	}
	whole := normaliseForEcho(prompt)
	if whole == "" {
		return false
	}
	if text == whole {
		return true
	}
	for _, sentence := range promptSentences(prompt) {
		if text == sentence {
			return true
		}
	}
	return false
}

// promptSentences splits the bias prompt into its normalised sentences.
//
// Splitting on the full stop is enough because this function only ever sees
// prompts this codebase composed (config.STTBiasPromptWith): two short
// declarative sentences, each ending in a period, with no abbreviations or
// decimals in them. A term the user taught could in principle contain a full
// stop, in which case the sentence splits into fragments — harmless, because a
// fragment can only ever match a transcript that is exactly that fragment, and
// the whole-prompt and whole-sentence comparisons still stand.
func promptSentences(prompt string) []string {
	var out []string
	for _, part := range strings.Split(prompt, ".") {
		if s := normaliseForEcho(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// normaliseForEcho reduces a string to its comparable core: lower case,
// letters and digits and single spaces only.
//
// Punctuation is dropped rather than mapped to a space, except that a
// punctuation mark between two words still separates them because the space
// beside it survives — so "mention: a, b" and "mention a b" agree, and
// "Jarvix's" does not silently become two words.
func normaliseForEcho(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if space && b.Len() > 0 {
				b.WriteRune(' ')
			}
			space = false
			b.WriteRune(r)
		case unicode.IsSpace(r):
			space = true
		default:
			// Punctuation: dropped, and it does not itself create a word
			// boundary — the space beside it, if any, already did.
		}
	}
	return b.String()
}
