# ADR 0064 — Review and undo: what is reversible, what is not, and when the difference is said

**Status:** accepted
**Date:** 2026-08-29
**Issue:** #201 (the third slice of the operator direction, #195)

## Context

The permission gate (ADR 0014) answers *may I*. The activity feed answers
*what happened*, one row at a time, in the past tense, with no handle on it.
Nothing answered *what did you do* as a piece of work, and nothing answered
*put it back*.

#195 makes the user the manager of the machine and Jarvix the operator of it.
Delegation without accountability is not delegation: if Jarvix acts in the
user's name they have to be able to see what it did, judge it, and reverse it.
That is three separate capabilities and only the first is obvious.

The trap under this feature is not the recording. It is **candour**. An undo
that quietly does nothing, an undo that overwrites work the user did in an
editor, a card that lets someone approve a one-way decision without saying it
was one-way — each of those is worse than not having the feature, because each
one substitutes a false sense of reversibility for an honest absence of it.
This project already has scar tissue from a model narrating actions it never
performed (#71); an undo that narrates a restoration it did not perform is the
same failure with the user's files underneath it.

## Decision

### The record is written where the action happens

Not inferred from the activity feed, not reconstructed from a log line. The
tool that changed something is the only code that knows what it changed and
what would put it back, and it says so through a context seam:

```go
prevFile := undo.Snapshot(ctx, path)   // the bytes before
… the mutation …
prevFile.Note(ctx, undo.Action{Tool: …, Summary: …})
```

A `context.Context` is the transport for **internal/provenance's exact
reason** (ADR 0055), restated: the things with something to say are reached
through interfaces whose signatures belong to what they do, and threading a
recorder through every one of them would put this feature in every tool's
contract. A context lets the tools that change the machine say so and costs
every other tool nothing. The recorder is installed once, beside the
provenance sink, at `Engine.executeTool`.

A nil recorder makes every call a no-op, so a tool exercised by a unit test or
run outside a turn behaves byte-for-byte as it did before this existed.

**The boundary is Jarvix's own actions.** The account records the assistant's
tools. A person typing `jarvix config set` is the manager's own hand, and a
manager does not need an account of themselves; #201 puts "undoing what the
user did" out of scope explicitly. One consequence is worth stating rather
than discovering: the assistant **structurally cannot** write `[tools.policy]`
(#109's exclusion wall, ADR 0053), so "approvals" — named in the ticket's list
of reversible kinds — has nothing to record. Every approval rule is added by a
person: the card's third button, `jarvix approvals add`, or the window. That
is not a gap in the account; it is the account correctly saying that Jarvix
never did it.

### The record's shape — and why it is #200's contract

```
id          a17, minted by the store, never reused
at          when it happened, UTC
job         the piece of work it belonged to (#200)
tool        the gate identity that acted — config.write_entry, shell.run
summary     one plain sentence, past tense: `saved the routine "morning"`
target      what it touched: a path, a window's description
provenance  #168 references, never content
restore     how to put it back, or the honest refusal to promise
undone_by   the id of the record that reversed this one
```

`job` is a field the record **already carries**, on every row, from the first
day. It is empty today because jobs do not exist. That is deliberate: when
#200 lands, grouping is a query (`Store.Job(id)`) rather than a migration, and
the reversal machinery is the machinery that already exists. See "What remains
for #200" below for the precise remainder.

### Three kinds of restore, and no more

- **`file`** — the previous bytes of one file, plus the digest of what the
  action left behind.
- **`window`** — one window's workspace, layer, fullscreen state and geometry.
- **`none`** — it genuinely cannot be undone, with a stated reason.

**The file kind is deliberately generic, and that is the design's main
economy.** Every store Jarvix writes by voice — configuration, the memory
book, the taught vocabulary, the reminders, an artifact — is one document
written whole. "The previous bytes" is therefore a complete and exact answer
for all of them, with no per-store reversal logic to write, get wrong, and
drift out of step with the store it reverses. Adding a reversible kind of
store is nothing: snapshot the file, note the action.

**Byte-exact, and not "re-serialise the parsed document".** `internal/config`'s
rewriters are byte-preserving on purpose — comments, key order and spacing
survive an edit — and re-serialising would silently eat the user's own notes.
Reversing a delete through `UpsertEntryTOML` would also put the entry back at
the *end* of the file rather than where it was. An undo that lost a user's
comments or moved their entry is a second edit wearing a reversal's name.

**The window kind is the one bespoke reversal**, because the state lives in
the compositor and nowhere else. It is dispatched through the same seam the
window tools use (ADR 0022 allows exactly one), in an order that is not
arbitrary: fullscreen off first (a fullscreen window ignores geometry), then
workspace (so it is on the right screen before it is sized for it), then
floating (position and size mean different things in and out of the tiling
layer), then size, then position. Each is a set rather than a toggle, so a
reversal applied twice converges instead of undoing itself — `SetFloating`'s
own rule (ADR 0026), borrowed.

**Focus is not recorded at all.** Moving the user's attention changes nothing
about the machine, and a row per "switch to my browser" would bury the account
in the one action nobody would ever want back.

### What is not reversible, and why the distinction is stated at approval time

A shell command the user approved is **recorded verbatim and described, never
falsely promised as undoable**. This is the ticket's spine. Jarvix has no idea
what a command did; an offer it could not keep would be worse than no offer.
The same holds for a script, a routine, a typed keystroke, a closed window, a
launched program, and a refetched feed — each with its own one-clause reason
rather than a shared "this is irreversible", because a warning that does not
say what it is warning about is a warning people learn to click past.

**The warning is on the confirmation card, before approval.** This is the half
that changes anything. A manager who learns a decision was one-way when they
read the account afterwards has learned something useless.

It rides the card's **existing summary sentence** rather than a new field, and
that is a decision rather than an expedient. The daemon owns the wording (ADR
0013); the summary is the one string every surface already renders — the
window's card, the overlay, the spoken question under
`confirmations.speak_details`, the pending snapshot a window opening mid-question
reads, and the record kept in the conversation archive (ADR 0039). A new field
would have reached whichever surface was updated for it and left every other
one quietly silent about the thing that matters most. A structured
`irreversible: true` rides the same event beside it, set *only* when the
summary actually carries the clause, so the flag and the sentence cannot
disagree about whether the user was told.

The abbreviated spoken prompt (#119) carries the **short** form — "This can't
be undone." — and not the reason. A screen can afford the reason and is better
for having it; audio cannot, and `shell.run` is the most-asked question there
is, so a full clause on every one of them would be a sentence users learn to
talk over. What is never abbreviated away is the fact itself, because it is
the one part of the question a person cannot recover by looking at the screen:
by then they have already answered.

**An unclassified tool says nothing.** Silence is the answer for a capability
the table does not know, because the failure mode of guessing "reversible" is
a user approving a one-way change believing otherwise. The table lists
read-only tools explicitly for the same reason: "we decided this needs no
warning" must be distinguishable from "nobody looked", and an external test
pins every literal in it to its `internal/tools` constant so a rename cannot
silently demote `shell.run` to unclassified.

### An undo is itself a consequential action

**It is judged under the identity of the action it reverses.** Putting a
config write back *is* a config write, so it faces the tier `config.write_entry`
faces; a user who turned config writes off gets them off, including under
another name. No new tool identity is invented, so nothing about this feature
needs a line in the shipped policy and no user has to configure a capability
they did not ask for.

**When it cannot tell, it refuses.** Every file reversal carries the SHA-256
of the bytes the action left behind. If the file no longer hashes to that,
something changed it since — the user in an editor, another action, a sync
from another machine — and there is no way from here to know whether that
change matters. So: no write, a sentence saying what was found and where the
file is, and the account **unchanged**, so the offer still stands once the
person has looked. Refusing is not consuming. The same shape covers a file
that has since been deleted (Jarvix does not know what removed it) and a
window whose four identity facts no longer agree (an address is a handle the
compositor recycles, ADR 0062).

**A reversal earns its own row**, and the row it reversed is marked rather
than removed. The account is what happened, and putting something back is a
thing that happened. Undoing an undo is deliberately not offered: ask for the
change again if you want it back.

### The store, and a bound that discloses itself

`~/.local/state/jarvix/undo.toml`, on ADR 0011's discipline: hand-editable
TOML, 0600 in a 0700 directory, atomic fsync-and-rename writes, stat-based
hand-edit pickup, a corrupt document warned about and moved aside rather than
overwritten, ids never reused. Registered with **internal/storefault**'s
shared suite (#173) on its first day rather than its second, which is what
that suite exists for: a new store inherits the promises by construction
instead of re-arguing them.

State rather than `config.toml`, and for a reason the other state stores do
not have: this file is not something the user configures, it is something
Jarvix writes about itself continuously without asking. Putting it in
configuration would mean every action Jarvix took also edited the file the
permission gate reads.

**The bound is 100 actions, and it is the one bounded store here that cannot
refuse at its cap.** Every sibling refuses and names the fix; this one evicts,
because refusing to record would leave an action that happened with nothing in
the account — the single outcome the feature exists to prevent. So it drops
the oldest, **counts what it dropped**, and every surface prints one
daemon-composed sentence: *"I keep the last 100 actions; 37 older ones have
dropped off."* The count is persisted, so the arithmetic survives a restart; a
bound whose numbers reset is a bound that lies after the first reboot. The
file's own header says it too, because the file is one of the places a person
goes looking for an action that is no longer there.

A second cap, 64 KiB, limits the previous bytes one record may keep. An action
whose file exceeds it is recorded as **irreversible with that as the stated
reason, at the time it happens** — visible in the account rather than
discovered when somebody says "undo that". A half-kept copy would be worse
than none: it would restore a truncated file over a whole one.

**`Forget` deletes for real.** A shell command is recorded verbatim and a user
may have dictated a secret into one, so a row can be dropped — from the file
by hand, or through the store's verb — and it deletes rather than tombstones,
exactly as the conversation archive does (ADR 0027). The id it held never
names anything again.

**The restore payload never leaves the file.** `undo.list` reports that a row
can be put back; the previous bytes stay on disk, because a listing carrying
the contents of `config.toml` would put the user's api keys on every connected
socket — the same rule `typing.audit` keeps about typed text (ADR 0023).

## Consequences

- One more state file on the shared discipline, validated by `jarvix restore`
  with the daemon's own loader (ADR 0045) and gated by the backup write
  barrier like every other store.
- Two new IPC verbs (`undo.list`, `undo.apply`), one new bus event
  (`undo.changed`), and two CLI commands (`jarvix actions`, `jarvix undo`).
- Confirmation cards for `shell.run`, `script.run`, `routine.run`,
  `intent.run`, the typing tools, `desktop.close_window`,
  `desktop.launch_app` and `knowledge.refresh` gain a clause. Users will
  notice their shell cards got a sentence longer. That is the feature.
- `tools.ConfigAdmin` gains `Path()`. It is the only interface change this
  needed, and it exists so the account can snapshot the real file rather than
  a guess.
- Eight tool `Execute` methods took their context instead of discarding it.

## What remains for #200 (jobs)

Job-scoped undo is in #201's acceptance criteria and is **implemented and
tested here**, against a job id set by hand. What is missing is only the code
that sets it:

- **Nothing populates `Action.Job`.** A job needs to install its id on the
  tool context alongside the recorder — one more context value, read in
  `undo.Note` — or set it on each `Action` it drives. Either is a small change
  confined to the job runner.
- **`Store.Job(id)` and `Undoer.JobActions(ctx, id)` exist**, return the job's
  records oldest-first, reverse them **newest step first** (an earlier step
  may be what a later one depended on, so restoring in the order they happened
  would produce a state that never existed), and report both halves — which
  were reversed and which could not be. `TestAJobIsReversedNewestStepFirst`
  pins all of it.
- **The `undo.apply` verb already takes `{job}`** and `jarvix undo --job <id>`
  already routes to it. With no job ids in the account it answers "I have
  nothing in the account for that job", which is the honest reply.
- What #200 must decide, and this ADR deliberately does not: whether a job's
  reversal is one confirmation or one per step, and whether a job that is
  still running may be undone or must be interrupted first. Both are questions
  about jobs, not about reversal.

## Deferred to a follow-up

The window surface — the account rendered as a reviewable list of work, with
the #168 provenance links — is **not in this change**. Everything it needs is
on the wire (`undo.list` carries the rows, the disclosure and the provenance
references; `undo.changed` says when to re-read), but `plugin/omarchy/JarvixWindow.qml`
is being repaired under #203 concurrently and editing it here would have put a
new feature into a file somebody else is fixing defects in. It is filed as
**#210**, which lists the wire it already has to build on.

## Alternatives considered

- **Deriving the account from the activity feed.** Rejected by the ticket's
  own NFR and on the merits: a feed row says a tool ran, not what it changed
  or what would restore it, and a recorder that inferred a restore payload
  from a past-tense sentence would be exactly the confident reconstruction
  this project distrusts.
- **A per-store reversal — "unremember this fact", "un-teach this phrase".**
  Rejected: each would be a second implementation of a store's own logic,
  each would drift, and none would be byte-exact. One file kind covers all of
  them and cannot disagree with the store it reverses because it does not know
  anything about it.
- **A new `undo.apply` tool identity in the shipped policy.** Rejected: it
  would be a capability every user had to configure, and it would let the
  reversal of a denied action run under a tier that had never been denied.
  Judging an undo as the thing it performs needs no new configuration and
  cannot be looser than what it reverses.
- **A new `irreversible` field on the confirmation event, and nothing in the
  summary.** Rejected: it would have reached whichever surface was updated for
  it. The clause rides the sentence every surface already shows; the flag
  rides beside it for a surface that later wants to style it.
- **Storing the previous bytes in a sidecar file per record.** Considered —
  it would keep `undo.toml` short and readable — and rejected as two files to
  keep in step for a store whose whole promise is that it does not lose track
  of things. The cost is an escaped, long `previous` line, which is the price
  of byte-exactness and is documented in the file's own header.
- **Recording at `Registry.Execute`, the single choke point.** Rejected: it
  cannot see the before-state, which is the entire payload. It would produce
  an account that knew a tool ran and nothing about what to do next — which is
  the activity feed, again.
- **Refusing to record at the cap, like every other store here.** Rejected on
  the argument stated above: the failure mode is an action that happened with
  no record of it, which is the one thing this feature must never produce.
