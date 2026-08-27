// Package vocabulary is Jarvix's taught vocabulary (issue #129): the words
// and phrases the user explicitly taught it — "when I say quid I mean pounds"
// — consulted on every turn that reaches the model so the user never has to
// translate themselves for their own assistant.
//
// It is deliberately not the memory book (internal/memory, ADR 0025), though
// it follows the book's storage discipline to the letter. A fact is a
// statement about the world; a vocabulary entry is a statement about the
// user's *language* — phrase → meaning, with an optional note — and the two
// are curated, listed, and injected on different terms. Sharing the store
// would blur the one boundary that makes each list small enough to ride a
// prompt.
//
// Three properties are requirements here, inherited from the book:
//
//   - The user owns the store. One human-editable TOML file under the XDG
//     state dir (0600 in a 0700 directory), documented in its own header,
//     hand-edits picked up without a restart, and an unparseable file
//     degrades to a warning plus an empty vocabulary — moved aside on the
//     next write, never overwritten.
//   - Explicit teaching only. Nothing enters the store unless the user taught
//     it — by voice, by chat, or in the window's form. There is no
//     auto-learning from usage, ever: an assistant that silently decided what
//     the user "really meant" would be rewriting them.
//   - Bounded and disclosed. Injection is token-capped with the trim
//     disclosed to the model (the ADR 0037 stance: a cap is never silent),
//     the store itself is capped with an actionable warning as it fills, and
//     the hard-to-hear flag — which feeds the finite STT bias prompt — has
//     its own small cap, refused loudly at the limit.
//
// Entry content is private: phrases and meanings never reach the log at any
// level; events carry ids and sizes only.
package vocabulary

import "time"

// Entry is one taught word or phrase.
type Entry struct {
	// ID is the short handle ("w3") the tools, the window, and the forget
	// flow address the entry by. Stable for the entry's lifetime; hand-added
	// entries without one are assigned the next free id.
	ID string
	// Phrase is the user's word or phrase, as they say it ("quid"). It is
	// the entry's identity: teaching the same phrase again supersedes the
	// meaning, never adds a silent second entry.
	Phrase string
	// Meaning is what the phrase means when the user says it ("pounds").
	Meaning string
	// Note is optional context ("UK money slang"); empty means none.
	Note string
	// HardToHear marks a phrase whisper mishears, so it joins the STT bias
	// prompt (the name-alias precedent, issues #83/#107). Presentation-of-
	// recognition state, not content: toggling it never touches the
	// timestamps or the supersede trail. Flagging is capped (MaxHardToHear)
	// because the bias prompt is finite; the cap refuses loudly, never
	// silently.
	HardToHear bool
	// Taught is when the phrase was first taught.
	Taught time.Time
	// Updated is when the entry last changed — equal to Taught until it is
	// re-taught. It is also the injection-trim priority: the entries taught
	// least recently are the first left out when the token cap bites.
	Updated time.Time
	// Source references the turn that taught or last updated the entry (a
	// session id such as "s12"). Empty when unknown — a hand-edit, or the
	// window's form.
	Source string
	// Previous is the supersede trail, oldest first: every meaning this
	// phrase held before, so "quid used to mean euros?" stays answerable
	// long after the correction (the memory-book discipline).
	Previous []Revision
}

// Revision is one superseded value of an entry.
type Revision struct {
	// Phrase is the phrase as it was then — usually unchanged, but a rename
	// in the window's form keeps the old spelling here.
	Phrase string
	// Meaning is the old meaning.
	Meaning string
	// Note is the old note, "" when there was none.
	Note string
	// Taught is when the old value was written.
	Taught time.Time
	// Superseded is when it was replaced.
	Superseded time.Time
}

// Injection is what one turn's vocabulary consultation produced: the message
// block handed to the model, and the accounting the audit surfaces disclose.
type Injection struct {
	// Message is the block injected as a system message beside the
	// remembered facts. Empty when nothing is taught — an empty vocabulary
	// must not cost a message, which is what keeps a zero-entry prompt
	// byte-identical to one before this feature existed (the pinned
	// acceptance criterion of #129).
	Message string
	// Entries are the entries the block carries, in injection order (most
	// recently taught first). Retained so the user can audit exactly what
	// the model was given, mirroring memory.last (ADR 0025).
	Entries []Entry
	// Trimmed counts entries the token cap left out of the block — never out
	// of storage. Disclosed to the model inside Message and to the user
	// through the vocabulary.list warning: a trim is never silent (ADR 0037).
	Trimmed int
	// Total is how many entries the store held at injection time.
	Total int
	// EstTokens is the estimated token cost of Message (bytes/4, the
	// memory book's shared heuristic).
	EstTokens int
}
