# ADR 0025 — A curated knowledge base: remember, consult, correct, forget

**Status:** accepted

## Context

Jarvix forgets everything that is not in the current conversation. Tell it
the staging server is called atlas, and the moment the follow-up window
lapses the fact is gone. Conversation history (ADR 0011) is not the answer:
it is a rolling record of what was *said*, dropped oldest-first, and a fact
mentioned once three weeks ago has no chance of surviving in it.

Issue #60 asks for the other kind of memory: a **curated knowledge base** —
small, structured, current — of facts the user explicitly asked Jarvix to
keep, consulted on every turn, audible ("what do you know about my setup?"),
correctable ("actually it's helios"), and deletable ("forget that"). The
trust model is the heart of it: Jarvix writes to its own memory **only on
the user's explicit word**. Automatic curation — Jarvix deciding unprompted
what is worth keeping — is deliberately out of scope; it changes the trust
model and gets its own ticket once explicit memory has proven the store.

## Decision

### One hand-editable TOML file, owned by the user

The store is a single file, `$XDG_STATE_HOME/jarvix/memory.toml` (0600 in a
0700 directory), written atomically with the fsync-and-rename discipline of
ADR 0011. **TOML, not JSONL**, and the owner is the reason: the contract is
that the *user* edits this file — correcting a fact in an editor is a
first-class operation, not a recovery procedure — and TOML is the dialect
this project already asks users to hand-edit (`config.toml`), with readable
multi-line structure, native datetimes, and no escaping puzzles. JSONL wins
for append-only machine logs, which this store is exactly not: it is small,
curated, and rewritten whole on every change. The file opens with a header
documenting its own format, so the format is discoverable without reading
Go.

Hand-edits are picked up **without a restart**: every operation — including
the per-turn injection — begins with one `stat(2)`, and a changed
mtime/size triggers a re-read. A hand-added `[[fact]]` needs only a
`content`; ids and timestamps are repaired at load (timestamps to *now*, so
a fresh hand-add is not the first thing the injection trim drops). A file
that does not parse — including an unknown key, which is how a typo like
`contnet` shows up — degrades to a warning plus an empty memory, never a
crash, and is **moved aside** (`memory.toml.corrupt`) before Jarvix will
write again: a typo must never cost the user their facts. Ids carry a
persisted high-water mark (`next_id`) and are never reused, so a trail or an
old conversation naming `m2` can never come to describe a different fact.

### Three tools; the gate splits them by reversibility

`memory.remember` and `memory.recall` are built-in **allow** (like
`artifact.create`, ADR 0014): recall is a read, and remember's blast radius
is bounded by construction — it writes only into the user's own memory
file, the model confirms in one sentence what was stored, and a wrong fact
is undone with "forget that". Asking would turn every "remember X" — an
instruction the user just gave out loud — into a question about itself.
`memory.forget` is the one irreversible verb, so it takes the policy
default (**ask**) and, via `Confirmable`, the question names the exact fact
about to go, resolved from the store — never from the model's description.

### Supersede: the daemon detects, the model decides, the store remembers

"Actually the staging server is helios" must *update*, not accumulate a
contradiction. A remember whose content resembles a stored fact (a
deterministic significant-word matcher — no embeddings, testable in both
directions) stores nothing and hands the candidates back as the tool
result, ids and dates included; the model then calls again with `update_id`
to supersede or `force_new` for a genuinely separate fact. The judgement
call is made deliberately, by the model, with the evidence in front of it —
a false positive costs one extra tool round, a false negative a duplicate
the user can still correct. An update keeps the fact's id and `stored`
date, moves `updated`, and pushes the old value onto a `[[fact.previous]]`
trail with both of its timestamps — so "when did that change" stays
answerable from the file alone.

### Injection: capped in code, trimmed from the block, disclosed twice

Facts reach the model as **one system message directly after the system
prompt**, before the carried-over history — standing knowledge precedes the
thread, while a desktop capture (ADR 0019), which describes one moment,
stays adjacent to the question it belongs to. The block opens with its
provenance ("things the user asked you to remember") and closes under a
token budget enforced in code: `memory.max_injected_tokens` (default 500,
floor 100), measured as bytes/4 — an honest estimate, since the daemon has
no tokenizer for an arbitrary configured model. Facts that do not fit are
dropped from the **block only, never from storage**, least recently
confirmed first, and the trim is disclosed twice: to the model ("N more
facts were left out; search with memory.recall") so absence is never read
as nonexistence, and to the user through the audit surfaces. Storage has
its own cap (`memory.max_facts`, default 200) that warns from nine-tenths
full and refuses at the limit with the fix named.

Like desktop context, injection happens inside `think()`, after the
deterministic intent router (ADR 0017): a matched intent pays nothing, and
a turn that reaches the model pays one stat of a file already in memory.

### Reliability, privacy, disclosure

A remember writes **synchronously inside the tool round** — the fact is on
disk before the model can claim it is remembered, which is strictly
stronger than the post-session history write of #29: the shutdown drain
(`Engine.Shutdown` waiting on the session goroutines) covers it for free.
Fact content never appears in logs or bus events at any level; the
`memory.injected` event carries counts and estimates only. What the model
was given is always answerable after the fact: the engine retains the last
injection for the `memory.last` IPC method, and `jarvix status --last`
prints the injected facts themselves, beside desktop context and the typing
audit. `jarvix memory list|forget` drive the store directly over IPC —
hearing and correcting a memory never requires talking a model into it.

### Disabled means absent — and never deletes

`memory.enabled = false` registers no tools, injects nothing, appends no
system-prompt section, and builds no store object (nil, structurally, like
a disabled context source). It does **not** delete the file: unlike
conversation history, whose disable path clears the disk, the store holds
facts the user deliberately curated. Deletion is always an explicit act —
"forget", `jarvix memory forget`, or deleting the file. The feature
defaults **on**, because the explicit-write trust model means nothing enters
the store without the user asking, per fact.

## Consequences

- The user owns a plain file: `cat`, edit, back up, or delete it, and
  Jarvix follows along on the next turn without being told.
- Memory cannot crowd out the conversation: the injection cost has a hard,
  configured ceiling, and the overflow behaviour is legible to model and
  user alike.
- The two-round supersede flow costs one extra provider round when a
  conflict is detected — accepted as the price of the model deciding
  update-versus-new deliberately instead of a heuristic guessing.
- The mtime check can miss an edit that lands within timestamp granularity
  *and* leaves the byte size unchanged — vanishingly rare for hand-edits,
  self-healing on the next real change.
- Restart-class configuration: the store and tools are wired at daemon
  construction, so `memory.*` changes ask for a restart rather than
  half-applying on a live reload.
- Advisors still see nothing: their environment stays scrubbed (ADR 0016),
  and sharing memory with them is a deliberate future decision.
- The follow-ups this substrate invites — automatic curation, routines
  referencing remembered preferences, a curated corpus beside the raw
  transcript for RAG — all get to build on a store whose trust model is
  already proven.
