# ADR 0066 — The account in the window: what a listing has to be told before it can offer to undo

**Status:** accepted
**Date:** 2026-08-29
**Issue:** #210 (the window surface ADR 0064 deferred)

## Context

ADR 0064 built the account of what Jarvix did in the user's name and the
machinery for putting it back, and shipped it on `undo.list` / `undo.apply`
with `jarvix actions` / `jarvix undo` in front. One of #201's acceptance
criteria was deliberately left out — the window — because
`plugin/omarchy/JarvixWindow.qml` was being repaired under #203 at the time.

The ticket for the remainder says, correctly, that everything the surface needs
is already on the wire, and then adds the clause this decision is about:

> If this surface wants something the wire does not carry, that is a change to
> `undo.list`, argued on its own terms.

It wanted four things, and each one is the same argument in a different
costume: **ADR 0013 says the daemon owns every word and every decision, and a
listing that offers to undo something is making a decision.**

## Decision

### `undo.list` gains what a surface cannot work out for itself

Additively — the flat `actions` list and every field the CLI reads are
untouched, so `jarvix actions` did not change shape.

**`can_undo`, beside `reversible`, because they are two facts.** `reversible`
is the record's own property: this action left something behind that would
restore it. `can_undo` is whether the offer stands *right now*, and the
permission gate can withhold it from a record that is perfectly reversible — an
undo is judged under the identity of the action it reverses (ADR 0064), so a
user who has turned `config.write_entry` off gets it off here too. A client
that drew its button from `reversible` would draw one that refuses when
pressed, which is the dead affordance ADR 0055 already refused once for
provenance sources. The value comes from `Undoer.Offer`, which is a new method
on the reverser rather than a re-derivation in the daemon's report, and which
lives *beside* `Apply` so that it answers with Apply's own two checks and its
own wording.

`Offer` deliberately does **not** predict the clobber guard. Whether the file
still hashes to what the action left behind is a fact about the disk at the
moment of the press; a listing that had checked it a minute ago would be making
a promise it had no way to keep. So that refusal stays where it is — at
`Apply`, in words, with the account left unchanged so the offer still stands
once the person has looked at the file.

**`when` and `state`, because a window has no clock and no vocabulary.** The
window reads the account over a socket. Subtracting its own machine's idea of
the time from the daemon's is not a formatting choice, it is arithmetic nobody
asked for — and the difference between "just now" and "yesterday" is exactly
the fact somebody deciding whether to reverse something is weighing. `when` is
a phrase, from `undo.Ago`, which is a screen scale in digits rather than
`knowledge.SpokenAge`'s speech scale in words: a reader is comparing numbers
between rows, a listener is not. `state` is the row's whole standing in one
sentence — already put back and by what, can be put back, or cannot and why —
so that no surface has to supply the lead-in, because a lead-in is wording.

**`sources`, because an encoding is the daemon's.** The account stores a
provenance reference as one `"kind:ref"` string, which is what fits a
hand-editable TOML line. It is split daemon-side into the `{kind, ref}` shape
`provenance.resolve` already takes, so the window hands the result straight over
and reaches an action's sources through the verbs the answer panel has used
since #168. A client that learned to parse the stored form would keep parsing
it after the daemon stopped writing it.

**`empty`, for symmetry with `disclosure`.** The bound already discloses itself
in one daemon-composed sentence. An account holding nothing is the same
promise at the other end of the range, and it was a literal in the CLI; it is
now one sentence in one place and the CLI prints it.

### The arrangement is on the wire too: `groups`

"Grouped by job where a job exists, chronological otherwise" is a decision. So
the daemon makes it and sends the result: one group per job, placed where that
job's **newest** action falls and holding its steps newest-first, and every
action that belonged to no job standing alone in a group of one with an empty
heading.

Placed by its newest action rather than collected into a separate section
because the page then has one reading order — nothing jumps backwards in time
between groups — while a job's twelve steps still sit under one heading instead
of scattering through the eleven other things that happened while it ran. Two
lists, grouped and ungrouped, would have forced a reader to merge them by eye to
answer "what happened last", which is the question the account exists to answer.

A group carries its own `can_undo` and `why`, on exactly the row's argument one
level up: the whole-job control is withheld where `undo.apply` would refuse it,
in the same sentence, because `undoJobBusy` is one function read by both. It
also carries `note`, which states what pressing it will *also* do — undoing a
parked job stops it (ADR 0065), and a manager who learns that afterwards has
learned something useless. That is the confirmation card's own argument applied
to a control that has no card.

**`actions` and `groups` carry the same rows twice, on purpose.** They are two
readers with two different questions: `actions` is the flat chronological
account `jarvix actions` prints, `groups` is the same account arranged as work.
They cannot drift, because both are built by the same row function over the
same slice in the same pass, and the report is a pure function of a `View`, an
offer seam and a job lookup — so every sentence on the surface is exercised by
a unit test rather than only through a socket.

### The window is a placement and nothing else

A tab beside Activity and Situation, on the Approvals tab's argument restated:
the three answer three different questions about the same machine (every event
as it happened, where things stand now, and the subset that *changed*
something), and an action you cannot find is an action you cannot reverse.

Built out of the established furniture — `JarvixCollectionRow`,
`JarvixFormButton`, `JarvixEmptyState` — so a reversible action reads exactly
like a routine or a remembered fact does in its own tab, and inherits their
keyboard reachability and accessible naming rather than reimplementing them.
A row that cannot go back has **no button at all** rather than a dimmed one:
an empty `actionLabel` is already what the shared row skips in the focus chain,
so withholding the offer withholds the tab stop too.

State is carried in the daemon's sentence rather than in colour, which matters
more here than it does elsewhere in this window: "this has already been put
back" and "this can be put back" must be distinguishable by somebody who cannot
tell two greys apart, and by a screen reader, which reads the row's accessible
name and never sees the fill.

Reading comfort (`ui.line_spacing` / `text_size` / `letter_spacing`, #121/#134)
deliberately does **not** scale this tab, and that is the existing rule rather
than an omission: those settings govern the transcript's message body, and
chrome, tabs and cards keep the design system's scale. A listing that scaled
with them would reflow every row of a hundred-row account against a setting
written for a paragraph of prose.

### One provenance pointer, not two

Two surfaces now carry sources — an answer in the transcript and an action in
the account — and `provenanceItems` is a single resolved list. Two independent
"which one is open" flags would let both panels claim the same list and show
one row's sources under another's, so the window's pointer became one
namespaced key (`"turn:3"`, `"action:a17"`) and the asking moved into one
function. The `provenance.resolve` and `provenance.open` call sites stay at one
each, which is what `provenanceqml_test.go` has pinned since #168 and is worth
keeping: a second would be a second chance for a panel to show a list nobody
re-checked.

## Consequences

- `undo.list` grows six fields and one array. Nothing is removed, so `jarvix
  actions` is unaffected except that it now prints the daemon's empty sentence
  instead of its own.
- `undo.Offer` is a new method on `Undoer`, and the gate's refusal clause moved
  into a function both it and `Apply` read. One sentence, two callers.
- `undo.View` carries `Now`, the store's own injected clock, so the phrasing of
  a record's age is hermetic in tests and measured against the clock the
  records were written with in production.
- `undo.Ago` is a second relative-time scale in this repository, beside
  `knowledge.SpokenAge`. That is deliberate and the two are not merged: one is
  read, one is heard, and the difference between "4 minutes ago" and "four
  minutes ago" is the whole reason each exists.
- The window gains a twelfth tab. The strip is a `Flow` and already wraps.
- `internal/desktop/accountqml_test.go` bans the tab from wording a row's
  standing, phrasing an elapsed time, reading `reversible` as the offer,
  comparing job ids, dimming a control instead of withholding it, or asking
  about provenance outside the window's single call site.

## Alternatives considered

- **A new verb — `undo.account` — serving the window its own view.** Rejected:
  two verbs over one file is two places for the account's meaning to drift, and
  the second one would inevitably grow a second idea of what "reversible" means.
  The issue's own instruction was to change `undo.list` and argue it.
- **Grouping in QML, from the `job` field the rows already carry.** Rejected on
  ADR 0013: when grouping applies, where a group sits in the order, and what a
  group is called are three decisions, and the third one needs the job store,
  which the window cannot reach.
- **A per-row `group` heading with the window drawing a heading whenever it
  changes.** Considered — it avoids sending the rows twice — and rejected
  because it only works if `actions` is reordered so a job's rows are adjacent,
  and reordering `actions` changes what `jarvix actions` prints. The flat list
  belongs to the CLI; the arrangement belongs to the window; giving each its own
  field costs a few kilobytes on a hundred-row cap.
- **Formatting `at` in the window, as the Approvals tab formats its dates with
  `.slice(0, 10)`.** Rejected here even though the precedent exists: a grant's
  date is a calendar fact that survives being sliced, and an action's age is a
  measurement against a clock the window does not have. The existing slice is
  not a licence, and it is not extended.
- **A confirmation card in front of the reversal.** Rejected: the card exists
  for something the *model* asked to do (ADR 0053). This is the manager's own
  instruction, given by hand, on a row that names what it will do — and the gate
  still applies underneath it, under the identity of the action being reversed,
  which is where the standing instruction lives.
- **Dimming an unreversible row's control rather than removing it, so the
  layout does not move.** Rejected: a control that is present and cannot act is
  a control somebody will press, and the row already says why in words where
  the button would have been.
- **Marking the row reversed in the window after a successful `undo.apply`.**
  Rejected: whether a row is now reversed, and by what, is the account's answer.
  The window re-reads — on the reply as well as on `undo.changed`, because the
  event is published when the reversal's own row is written, an instant before
  the row it reversed is marked, so the reply is the moment both halves are
  certainly on disk.
