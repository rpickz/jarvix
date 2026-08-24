package memory

import (
	"fmt"
	"strings"
)

// This file builds the block of remembered facts a model turn receives. The
// budget is enforced here, in code, not left to prompt discipline: the block
// is measured, facts that do not fit are dropped from the block (never from
// storage), and the model is told — honestly — what is in front of it and
// what is only reachable through memory.search, so it neither re-searches
// what it already has nor concludes an absent fact does not exist.

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
// preamble, entries, and the disclosures it may need — fits capTokens, then
// drops from the end of the list: the least recently confirmed facts leave
// the block first. searchable is how many further facts are deliberately
// not offered to the block at all (the unpinned rest once the split of ADR
// 0037 engages); it is disclosed but never competes for the budget's lines.
func buildInjection(facts []Fact, capTokens, searchable int) Injection {
	inj := Injection{Total: len(facts) + searchable, Searchable: searchable}
	if len(facts) == 0 && searchable == 0 {
		return inj
	}
	lines := make([]string, len(facts))
	for i, f := range facts {
		lines[i] = factLine(f)
	}
	kept := len(lines)
	for kept > 0 {
		msg := assemble(lines[:kept], len(facts)-kept, searchable)
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
	// model knows to search rather than deny.
	inj.Message = assemble(nil, len(facts), searchable)
	inj.EstTokens = EstimateTokens(inj.Message)
	inj.Trimmed = len(facts)
	return inj
}

// searchOnlyInjection is the block for a book that outgrew the budget with
// nothing pinned (ADR 0037): no fact is ambient, and instead of the old
// silent tail-drop the model is told exactly that — N facts exist, none are
// shown, memory.search is the way to them. Deliberately terse: this message
// rides every turn until the user pins.
func searchOnlyInjection(total int) Injection {
	msg := fmt.Sprintf("You have %d remembered facts — things the user asked you to remember — "+
		"in your long-term memory, but none are shown here. Find the ones you need with the "+
		"memory.search tool; never claim to remember something you have not searched for.", total)
	return Injection{
		Message:    msg,
		Total:      total,
		Searchable: total,
		EstTokens:  EstimateTokens(msg),
	}
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

// assemble renders the final message: preamble, entries, and the disclosures
// — the budget trim (a warning: these facts should have been here) and the
// searchable rest (not a loss: those facts live behind memory.search by
// design). Each appears only when it has something to say, so a small,
// unpinned, in-budget book reads exactly as it always has.
func assemble(lines []string, trimmed, searchable int) string {
	var b strings.Builder
	b.WriteString(injectionPreamble)
	if len(lines) > 0 {
		b.WriteString("\n\n")
		b.WriteString(strings.Join(lines, "\n"))
	}
	if trimmed > 0 {
		fmt.Fprintf(&b, "\n\n(%d more remembered %s left out to save space; search with the memory.search tool.)",
			trimmed, plural(trimmed, "fact was", "facts were"))
	}
	if searchable > 0 {
		fmt.Fprintf(&b, "\n\n(%d further %s not shown here by design; find them with the memory.search tool. "+
			"The facts listed above are already in front of you — do not search for those.)",
			searchable, plural(searchable, "remembered fact is", "remembered facts are"))
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
