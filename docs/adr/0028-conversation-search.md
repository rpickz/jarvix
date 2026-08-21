# ADR 0028 — Conversation search: one streaming scan behind the RAG seam

**Status:** accepted

## Context

The archive exists (ADR 0027); issue #59 asks for the question people
actually put to it: "what did we say about X?". Three surfaces want the same
answer — the conversation window's search box, `jarvix conversations
search`, and Jarvix itself, mid-answer, through a `conversations.search`
tool — and the user has RAG in mind eventually. The ticket's design
constraint is that embeddings must be addable *behind* whatever ships now,
so plain search has to be built as the interface a vector index would later
implement, not as a dead end it would replace.

The performance bound: ≤200ms over a 200-conversation archive, without
loading every file wholesale, and nothing may ever block a session.

## Decision

### The interface is the seam: `conversations.Searcher`

One interface in `internal/conversations`:

    Search(Query) ([]Match, SearchStats, error)

Query in (text, result cap, passage cap); ranked passages out, each carrying
conversation id, 1-based turn reference, role, timestamp, and a clipped
passage. Every caller — the `conversation.search` IPC method (window + CLI),
the model's tool, the CLI's daemon-down file fallback — talks to this
interface and nothing below it.

**This is deliberately the RAG seam.** An embedding index is a different
*implementation* of the same contract: it would embed the query, rank by
similarity instead of phrase/word tiers, and return the same
`[]Match` — ids, turn refs, bounded passages. Callers, wire shapes, the tool
result format, and the anti-confabulation wording would not move. Building
the index incrementally would hang off archive writes exactly where the
engine already flushes turns (`internal/session/archive.go`), off the
session lock — the hook point exists today. That is how "we may consider RAG
later" is kept cheap.

### Implementation: a streaming scan, not an index

The first implementation streams each transcript once per search: a bufio
pass over `<id>.jsonl`, one decoded turn in memory at a time, never a whole
file slurped or a whole archive resident. Measured on the ticket's corpus
(200 conversations × 30 turns, `BenchmarkSearch200Conversations`): **~12ms
per search** — seventeen times inside the 200ms budget, so an index would
buy nothing today but would cost invalidation correctness (the CLI writes to
the same directory when the daemon is down, deletions must vanish from
results instantly, and a persistent index is a second copy of private
transcripts to secure and to delete). The moment the corpus or the ranking
outgrows the scan, the seam above is where an index goes; nothing else
changes.

The scan takes no lock. Holding the store mutex for a scan would stall the
engine's post-session archive write behind a search — search must never
block a session. The files make the race safe by construction: appends land
whole lines, so the worst a concurrent write shows the scanner is a torn
final line, tolerated exactly as `Read` tolerates it.

### Ranking: deterministic tiers, recency inside a tier

Exact contiguous phrase beats scattered words; within a tier, newer turns
beat older; conversation id and turn number break remaining ties so the
order is total and a repeated search is byte-identical. All matching is
case-folded; every query word must appear in a turn for it to match.
Table-tested, including the tie-breaks.

### The live head is part of the corpus

The engine flushes each completed exchange to the active conversation's
transcript (ADR 0027), so searching the files covers the in-flight
conversation up to its last completed exchange — the current half-spoken
turn is already in the model's context and needs no search. Results carry
the conversation id; each surface compares it to the engine's active id and
says "earlier in this conversation" (tool), marks `current` (wire), or
prints `*` (CLI) — the distinction is drawn at the surface, not baked into
the corpus.

### The tool must not enable confabulation

The tool caps passages (5 per search, 280 runes each) so a search cannot
flood the context window, and every no-result shape is steering text pinned
verbatim by tests: no match says "do not guess, and do not invent a
recollection"; an empty archive and retention-off each say plainly that
there is nothing to search and why. Citations are spoken phrases ("last
Tuesday", "three weeks ago") composed daemon-side — raw timestamps never
reach the model, because #30's speech normalisation handles numbers, not
calendars.

### Privacy

Queries and passages never appear in logs at any level; the audit trail is
"a search happened, over N conversations, with M results". The unreadable
contract from ADR 0027 carries over: a record that cannot be searched is
skipped and reported, never fatal, and never quoted.

## Consequences

- Search works identically with the daemon down (`jarvix conversations
  search` falls back to the files), because the implementation lives on the
  store, not the daemon.
- Doctor and `jarvix status` report search as a state — active with a count,
  or inactive when retention is off and nothing is archived — never as a
  failure.
- A future embedding index must implement `Searcher` and nothing else; if it
  needs persistence, its update hook is the archive write path, and its
  deletion story must be as absolute as the archive's.
