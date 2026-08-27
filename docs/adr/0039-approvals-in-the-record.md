# ADR 0039 — Approvals are part of the conversation record

**Status:** accepted (implements issue #118; extends ADR 0014's gate and
ADR 0027's archive schema)

## Context

The permission gate (ADR 0014) is the trust surface of the whole tool story:
nothing in the ask tier runs until the user answers a card showing the exact
command, verbatim. But the exchange itself was ephemeral window state. The
card and its outcome lived only in the open window's ListModel; the daemon's
history (`conversation.get`) carried committed user/assistant turns and
nothing else. Close the window and reopen it — or lose it to a compositor
kill and the #108 rebuild — and every confirmation exchange vanished from the
transcript, leaving invisible gaps exactly where the most consequential
events happened: the moments the user authorised Jarvix to act. "What did I
let it do" was answerable only while a particular window stayed open.

## Decision

**A resolved confirmation is a turn of the conversation record: persisted in
the archive at its position between the turns of its exchange, served by the
daemon on every history surface, and rendered by clients as the static
resolved form of the live card.**

Concretely:

1. **The archive schema grows additively** (the #117/#125 discipline). A
   record is a `Turn` with `role: "confirmation"`, its `text` the spoken
   question, and a `confirmation` payload: tool, the verbatim command
   (ADR 0014's no-rewording rule extends to the record), the deciding rule,
   the outcome, its source, and the timeout that applied. `omitempty` keeps
   every utterance's line byte-identical, an old archive without such turns
   loads clean, and `SchemaVersion` stays 1 — a reader that ignores the role
   still reads every utterance correctly.
2. **Outcomes are never conflated.** `approved`, `declined`, and `timed_out`
   are distinct on disk and on the wire, for the same reason the engine keeps
   them distinct for the model: "the user said yes", "the user said no", and
   "the user said nothing" are three different facts, and this is the one
   record whose entire point is which of them happened. An abandoned
   confirmation (interruption, stage failure) records as declined with its
   source, keeping the audit promise that no question stands unanswered.
3. **The engine anchors records by position, not by slice index.** Each
   record is kept under the engine lock the moment it resolves — before the
   `tool.confirmed`/`tool.declined` event publishes, so a resolution a client
   has seen acknowledged is visible to an immediate `conversation.get` — and
   anchored to a monotonic committed-message counter, so the retention cap
   trimming the head never shifts a record onto the wrong exchange. When the
   exchange commits, the record is staged into the archive between its two
   halves and rides the same tail flush, behind the same `SyncArchive` read
   barrier, as the turns themselves (#116). A turn that dies without
   committing still archives its records standalone: an approved command may
   already have run, and the failure of the turn around it must not unsay it.
4. **Pending stays live, and singular.** A still-pending confirmation is
   deliberately *not* a turn: it rides the snapshot's `confirmation` field
   (issue #76) and renders as the live card, so a reopen mid-wait shows
   exactly one interactive card and no history duplicate. It becomes a
   record — and therefore a turn — only at resolution.
5. **Clients render, the daemon decides** (ADR 0013). `conversation.get` and
   `conversation.read` serve the structured record; the window and the CLI
   word the outcome and draw the card's static form — same question, same
   monospace verbatim command, outcome instead of buttons. Reopening an
   archived conversation (`conversation.open`) restores its records beside
   the turns they sat between, within the same context-budget cut.

## Consequences

- Window close/reopen, the #108 kill-rebuild, `jarvix conversations show`,
  and `conversation.open` all show every confirmation exchange in place;
  the trust record survives anything short of deleting the conversation.
- The archive gains a third role. Readers that iterate roles must treat
  `confirmation` as display-only: `adoptableMessages` never lets it into the
  model context (the model was told the outcome in the turn it happened),
  and search indexes the question text like any other line.
- A conversation's `turns` count now includes its confirmation records —
  they are turns of the record, and the count says what the transcript holds.
- The head display after a daemon restart shows turns without records until
  the conversation is next opened: `history.json` is the model-context head
  and carries none. The archive keeps them all; this is a display gap on one
  path, not a loss of record, and closing it is not worth teaching the head
  store a second schema.
