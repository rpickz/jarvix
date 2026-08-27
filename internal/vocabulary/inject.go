package vocabulary

import (
	"fmt"
	"strings"
)

// This file builds the vocabulary block a model turn receives, beside the
// remembered-facts block. The budget is enforced here, in code: the block is
// measured, entries that do not fit are dropped from the block (never from
// storage) least recently taught first, and the trim is stated inside the
// block itself — a cap is never silent (the ADR 0037 stance). With nothing
// taught the block is the empty string, which is what keeps a zero-entry
// prompt byte-identical to one before this feature existed.

// injectionPreamble introduces the block with its provenance — these are
// words the *user* chose to teach, the same trust distinction the memory
// preamble draws — and pins the two behaviours that must hold before any
// entry is read: a taught phrase in the user's words carries its taught
// meaning, and the list is never recited or echoed back. The wording is
// test-pinned (TestInjectionWordingIsPinned): "understand, don't parrot" is
// the acceptance criterion of #129, not prose to drift.
const injectionPreamble = "Taught vocabulary: words and phrases the user explicitly taught you, " +
	"with what each one means when the user says it. When the user uses a taught phrase, " +
	"understand it as its meaning — no need to ask, and never echo the phrase back to explain " +
	"you understood. Never recite this list unprompted."

// speakBackOff is the default stance (vocabulary.speak_back = false): Jarvix
// understands the user's words but answers in plain language. Default-off
// because mirrored slang from a machine reads as mockery more often than
// rapport — the reasoning recorded in the ADR — and a user who wants it back
// says so with one setting.
const speakBackOff = " Do not use these words yourself: answer in plain words, and keep the " +
	"user's phrasing to the user."

// speakBackOn is the opted-in stance (vocabulary.speak_back = true).
const speakBackOn = " You may use these words in your own replies where they fit naturally."

// estimateTokens estimates the token cost of text as bytes/4 — the memory
// book's stated heuristic, reused so the two blocks are budgeted on one
// scale.
func estimateTokens(text string) int {
	return (len(text) + 3) / 4
}

// Inject builds the vocabulary block for one model turn: every entry while
// the whole message fits the token budget, then dropping from the end of the
// injection order — the least recently taught words leave the block first,
// and the block says how many did. speakBack selects the closing stance
// sentence (vocabulary.speak_back).
func (s *Store) Inject(speakBack bool) Injection {
	entries := s.List("")
	inj := Injection{Total: len(entries)}
	if len(entries) == 0 {
		return inj // no message: an empty vocabulary must not cost a byte
	}
	lines := make([]string, len(entries))
	for i, e := range entries {
		lines[i] = entryLine(e)
	}
	kept := len(lines)
	for kept > 0 {
		msg := assemble(lines[:kept], len(entries)-kept, speakBack)
		if estimateTokens(msg) <= s.maxInjectedTokens {
			inj.Message = msg
			inj.EstTokens = estimateTokens(msg)
			inj.Entries = entries[:kept]
			inj.Trimmed = len(entries) - kept
			return inj
		}
		kept--
	}
	// Not even one entry fits — a pathological budget against a pathological
	// entry. Disclose that a vocabulary exists but could not be carried, so
	// the model asks rather than guesses.
	inj.Message = assemble(nil, len(entries), speakBack)
	inj.EstTokens = estimateTokens(inj.Message)
	inj.Trimmed = len(entries)
	return inj
}

// entryLine renders one entry for the model: id (so the forget tool can be
// precise), the date it was last taught, the phrase, its meaning, and the
// note when one exists.
func entryLine(e Entry) string {
	verb := "taught"
	if e.Updated.After(e.Taught) {
		verb = "re-taught"
	}
	line := fmt.Sprintf("- [%s, %s %s] %q means: %s", e.ID, verb,
		e.Updated.Format("2006-01-02"), e.Phrase, e.Meaning)
	if e.Note != "" {
		line += " (" + e.Note + ")"
	}
	return line
}

// assemble renders the final message: preamble, stance, entries, and the
// trim disclosure when the budget bit. The disclosure names the loss plainly
// — there is no search tool over vocabulary, so a trimmed word is genuinely
// out of reach this turn and the model must not pretend otherwise.
func assemble(lines []string, trimmed int, speakBack bool) string {
	var b strings.Builder
	b.WriteString(injectionPreamble)
	if speakBack {
		b.WriteString(speakBackOn)
	} else {
		b.WriteString(speakBackOff)
	}
	if len(lines) > 0 {
		b.WriteString("\n\n")
		b.WriteString(strings.Join(lines, "\n"))
	}
	if trimmed > 0 {
		fmt.Fprintf(&b, "\n\n(%d more taught %s left out to save space; if the user's words seem "+
			"to carry a meaning you were not shown, ask rather than guess.)",
			trimmed, plural(trimmed, "phrase was", "phrases were"))
	}
	return b.String()
}

// plural picks the grammatical form for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
