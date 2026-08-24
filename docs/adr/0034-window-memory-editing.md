# ADR 0034 — Window memory editing: the book's own path, ungated add/edit

**Status:** accepted

## Context

Issue #100 gives the window's Memory tab Add and Edit. The sibling surface —
knowledge feeds — landed as one registry row on the generic config entry
verbs (ADR 0033), and the tempting symmetry was to do the same for memory:
declare a `fact` family and let `config.upsert_entry` write it. Two
questions had to be answered instead: which write path, and whether the
permission gate stands in front of it the way it stands in front of the
window's Forget button (`memory.forget_gated`, #92).

## Decision

**Memory is not a config entry family.** memory.toml is not config.toml: it
lives under the XDG state dir, not the config dir; it is rewritten whole on
every change rather than byte-preserved around an edit (ADR 0025 — comments
are documented as not preserved); its concurrency discipline is the Book's
stat-per-operation hand-edit pickup plus a mutex, not a fingerprint
handshake; and its ids, trails, and corrupt-file move-aside are invariants
the config editor knows nothing about. Forcing it through
`entryAdminFamilies` would have meant teaching the config pipeline a second
file, a second fingerprint, and a null byte-preservation contract — three
lies for one shared verb name. Instead the daemon gains `memory.add` and
`memory.update` (internal/daemon/memory_admin.go), thin placements over
`Book.Add` / `Book.Update` — the same calls the `memory.remember` tool
makes — so the window can never write a fact the book would not.

**What IS shared is the refusal shape.** The book's refusals arrive as
matchable sentinels (`ErrNoContent`, `ErrStoreFull`, `ErrUnknownID` in
internal/memory) and the daemon places them in the entry form's wire shape:
`-32001` with `problems: [{field, message}]` — empty content on the
`content` field, a full store as a whole-entry problem — so the QML dialog
pins them with the same code the config families use. The sentences are the
book's, verbatim; the daemon decides placement, never wording (ADR 0013's
no-second-copy rule, applied to errors).

**Add and edit are ungated.** ADR 0025 split the memory tools by
reversibility, not by direction of write: remember is built-in allow because
its blast radius is bounded and a wrong fact is undone with one forget;
forget is the one irreversible verb, so it asks. The window's verbs land on
the same line the tools do. An add is a remember with a keyboard — the fact
in the form is the user's explicit word, and asking would turn their own
instruction into a question about itself. An edit supersedes: the old value
moves onto the fact's `[[fact.previous]]` trail with both timestamps, so
nothing is destroyed and the change itself is on the record. Forget — the
verb that does destroy — keeps its confirmation card exactly as #92 built
it. If a future surface ever wants bulk rewriting or trail deletion, that is
a new reversibility question, not an extension of this one.

**Saves are announced without content.** Each success publishes
`memory.entry_changed {action, id, chars}` — the activity feed renders
"Fact added: m7" with a size, the Memory tab refreshes off it, and the
memory privacy contract (counts and ids in events, content only over the
socket on request) holds for window saves exactly as for tool calls.

## Consequences

- The window's memory form is a different two calls from the config forms,
  but the same components and the same problem-pinning code — the sharing
  #100 asked for lives in the wire shape, not in a forced family row.
- The Book remains the single writer discipline for memory.toml; a future
  surface (CLI add, an import) reuses `memory.add`/`memory.update` or the
  Book directly and inherits ids, trails, cap warnings, and the corrupt-file
  guarantee for free.
- Hand-edits keep working mid-session: the Book re-reads on its next
  operation, so a form save after a hand edit composes instead of clobbering
  — which is why these verbs need no fingerprint.
