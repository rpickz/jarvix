// Package memory is Jarvix's curated knowledge base (ADR 0025): the small,
// structured set of facts the user explicitly asked Jarvix to keep —
// "remember that the staging server is called atlas" — consulted on every
// turn that reaches the model.
//
// It is deliberately not conversation history. History (internal/history) is
// a record of what was *said*, rolled forward and eventually dropped; this is
// what Jarvix has distilled as *true and worth keeping* — current, corrected
// when the user corrects it, and deleted when the user says forget.
//
// Three properties shape everything here, each a requirement rather than a
// nicety:
//
//   - The user owns the store. It is one human-editable TOML file under the
//     XDG state dir (0600 in a 0700 directory), documented in its own header,
//     and a hand-edit is picked up without a restart. A file Jarvix cannot
//     parse degrades to a warning and an empty memory, never a crash — and is
//     moved aside, never overwritten, so a typo cannot cost the user their
//     facts.
//   - Bounded. Injection into the model is token-capped, with the facts that
//     do not fit dropped from *injection only* (never from storage) and the
//     trim disclosed to the model. Storage itself is capped with an
//     actionable warning as it fills.
//   - Content is private. Fact contents never reach the log at any level;
//     what was injected is retained for the audit surfaces (`jarvix status
//     --last`, the memory.last IPC method) instead.
package memory

import "time"

// Fact is one remembered statement. Content is a short, self-contained
// sentence — the model is steered to phrase it that way — because each fact
// must make sense injected on its own, months after the conversation that
// stored it.
type Fact struct {
	// ID is the short handle ("m3") the tools and the CLI address the fact
	// by. Stable for the fact's lifetime; hand-added facts without one are
	// assigned the next free id.
	ID string
	// Content is the fact itself, in words.
	Content string
	// Stored is when the fact was first remembered.
	Stored time.Time
	// Updated is when the content last changed — equal to Stored until the
	// fact is superseded. It is also the injection-trim priority: the facts
	// confirmed least recently are the first left out when the token cap
	// bites.
	Updated time.Time
	// Source references the turn that stored or last updated the fact (a
	// session id such as "s12"), so "where did that come from" is
	// answerable. Empty when unknown — a hand-edit, or a store operation
	// outside any session.
	Source string
	// Previous is the supersede trail, oldest first: every content this fact
	// held before, with when it was stored and when it was replaced — so
	// "when did that change" is answerable long after the correction.
	Previous []Revision
	// Pinned marks a fact as ambient: pinned facts are the ones injected
	// into every model turn once the pin/search split engages (issue #104).
	// A pin is presentation-of-memory state, not content — toggling it never
	// touches Updated or the supersede trail.
	Pinned bool
	// TimesRetrieved counts how often memory.search returned this fact — the
	// usefulness signal behind the Memory tab's "retrieved N times" line.
	// Zero means never retrieved, and the surfaces show nothing rather than
	// fabricate. Ambient injection and the user's own listings do not count:
	// retrieval means the model went looking and this fact answered.
	TimesRetrieved int
	// LastRetrieved is when memory.search last returned this fact; zero when
	// it never has.
	LastRetrieved time.Time
}

// Revision is one superseded value of a fact.
type Revision struct {
	// Content is the old value.
	Content string
	// Stored is when the old value was written.
	Stored time.Time
	// Superseded is when it was replaced.
	Superseded time.Time
}

// Injection is what one turn's memory consultation produced: the message
// block handed to the model, and the accounting the audit surfaces disclose.
type Injection struct {
	// Message is the delimited block injected as a system message, carrying
	// its provenance ("things the user asked you to remember"). Empty when
	// there is nothing to inject — an empty memory must not cost a message.
	Message string
	// Facts are the facts the block carries, in injection order (most
	// recently confirmed first). Retained so the user can see exactly what
	// the model was given, mirroring desktop context (ADR 0019).
	Facts []Fact
	// Trimmed counts facts that *should* have been in the block — the whole
	// store before the split engages, the pinned set after — but were left
	// out by the token cap. The trim is disclosed to the model inside
	// Message; this field discloses it to the user, and it is what turns
	// into the Memory tab's over-budget warning (never silent, issue #104).
	Trimmed int
	// Searchable counts facts deliberately not in the block: the unpinned
	// facts once the pin/search split engages (ADR 0037). Unlike Trimmed
	// this is not a loss — the model is told they exist and how to find
	// them with memory.search.
	Searchable int
	// Total is how many facts the store held at injection time.
	Total int
	// EstTokens is the estimated token cost of Message (see EstimateTokens).
	EstTokens int
}
