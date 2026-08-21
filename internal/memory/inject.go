package memory

import (
	"fmt"
	"strings"
)

// This file builds the block of remembered facts a model turn receives. The
// budget is enforced here, in code, not left to prompt discipline: the block
// is measured, facts that do not fit are dropped from the block (never from
// storage), and the model is told the list is incomplete so it can reach for
// memory.recall instead of concluding a fact does not exist.

// injectionPreamble introduces the block with its provenance: these are
// things the *user* chose to store, not something Jarvix gathered — the
// trust distinction the whole feature rests on — and having them is not an
// invitation to recite them.
const injectionPreamble = "Remembered facts: things the user asked you to remember in earlier " +
	"conversations, kept in your long-term memory. Use one only when it helps answer what the " +
	"user actually asked, and never recite this list unprompted. Entries are dated, so you can " +
	"say when something was stored or last corrected."

// EstimateTokens estimates the token cost of text as bytes/4, the usual
// rough heuristic for English prose. An estimate is the honest tool here:
// the daemon has no tokenizer for the configured model, and a cap enforced
// on a stated estimate beats a precise number nobody can compute.
func EstimateTokens(text string) int {
	return (len(text) + 3) / 4
}

// buildInjection assembles the block from facts already in injection order
// (most recently confirmed first). It keeps facts while the whole message —
// preamble, entries, and the trim disclosure it may need — fits capTokens,
// then drops from the end of the list: the least recently confirmed facts
// leave the block first.
func buildInjection(facts []Fact, capTokens int) Injection {
	inj := Injection{Total: len(facts)}
	if len(facts) == 0 {
		return inj
	}
	lines := make([]string, len(facts))
	for i, f := range facts {
		lines[i] = factLine(f)
	}
	kept := len(lines)
	for kept > 0 {
		msg := assemble(lines[:kept], len(facts)-kept)
		if EstimateTokens(msg) <= capTokens {
			inj.Message = msg
			inj.EstTokens = EstimateTokens(msg)
			inj.Facts = facts[:kept]
			inj.Trimmed = len(facts) - kept
			return inj
		}
		kept--
	}
	// Not even one fact fits — a pathological cap against a pathological
	// fact. Disclose that memory exists but could not be carried, so the
	// model knows to recall rather than deny.
	inj.Message = assemble(nil, len(facts))
	inj.EstTokens = EstimateTokens(inj.Message)
	inj.Trimmed = len(facts)
	return inj
}

// factLine renders one fact for the model: id (so it can supersede or forget
// precisely), the date it was last confirmed, and the content.
func factLine(f Fact) string {
	verb := "stored"
	if f.Updated.After(f.Stored) {
		verb = "updated"
	}
	return fmt.Sprintf("- [%s, %s %s] %s", f.ID, verb, f.Updated.Format("2006-01-02"), f.Content)
}

// assemble renders the final message: preamble, entries, and — only when the
// cap trimmed something — the disclosure that the list is incomplete.
func assemble(lines []string, trimmed int) string {
	var b strings.Builder
	b.WriteString(injectionPreamble)
	if len(lines) > 0 {
		b.WriteString("\n\n")
		b.WriteString(strings.Join(lines, "\n"))
	}
	if trimmed > 0 {
		fmt.Fprintf(&b, "\n\n(%d more remembered %s left out to save space; search with the memory.recall tool.)",
			trimmed, plural(trimmed, "fact was", "facts were"))
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
