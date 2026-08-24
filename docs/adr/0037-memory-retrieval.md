# ADR 0037 — Memory retrieval: pinned facts stay ambient, the rest is searched on demand

**Status:** accepted (implements issue #104; amends ADR 0025's injection
section)

## Context

ADR 0025's memory is whole-book injection: every fact rides every prompt,
newest-updated first, trimmed silently at the token budget. That has a
scaling cliff — a growing book drops its oldest-updated tail and the user
never hears about it — and it makes per-fact usefulness unmeasurable:
"retrieved" means nothing when everything is always injected. Issue #104
asks for the hybrid: a small ambient core the user chooses, a long tail
behind a deterministic lookup tool with the same guaranteed-execution
contract every other Jarvix tool has (the model chooses *when*; the daemon's
code decides *how*, correctly, every time), and retrieval finally observable
per fact.

## Decision

### The retrieval policy: three states, decided in code

`Book.Inject` picks the ambient set by exactly this rule, in this order:

1. **No fact pinned, whole book fits the budget** — every fact is ambient.
   Byte-for-byte the pre-#104 block: no mention of search, no disclosures.
   A user who never touches pinning sees zero change; a test pins this.
2. **Any fact pinned** — exactly the pinned facts are ambient, and
   `memory.max_injected_tokens` governs them alone. An over-budget pinned
   set trims its least recently confirmed tail (the ADR 0025 trim order,
   unchanged), disclosed to the model in the block *and* to the user as a
   warning sentence in `memory.list` — never silently. Unpinned facts are
   not in the prompt; the block states how many exist and that
   `memory.search` finds them, and tells the model not to search for what
   it already has.
3. **No fact pinned, book over budget** — nothing is ambient. The old
   behaviour here was the silent tail-drop; the honest replacement is a
   terse block saying all N facts exist and are searchable, plus the
   user-facing warning telling them pinning is how facts get back into
   every prompt. Deliberately *not* "inject what fits": that would keep the
   cliff (which facts ride is an accident of update timestamps) and remove
   the pressure that makes the user curate. The engagement rule, compactly:
   **the split engages when any pin exists or the book exceeds the budget.**

Pinning is user-only. The model gets no pin tool: the ambient set is the
user's judgement about what must shape every answer, exactly as the store's
trust model makes writing memory the user's explicit word (ADR 0025). The
window's fact card toggles it (`memory.set_pinned`, ungated — the opposite
click undoes it exactly), the edit form carries it beside the content
(`memory.add`/`memory.update` grew a `pinned` field; the daemon compares
before writing so a pin-only save never manufactures a revision of unchanged
text), and `pinned = true` in memory.toml is the hand-edit. A pin never
touches `updated` or the supersede trail — it is presentation-of-memory
state, not content.

### memory.search: the recall tool, renamed and given a ranking

One read tool, not two: `memory.recall` **is renamed** to `memory.search`
(the issue's contract names it, and two overlapping lookup tools would make
the model guess). It keeps recall's built-in **allow** tier (a read of the
user's own facts, ADR 0025's argument verbatim), its place in the Activity
pane's per-tool argument policy ("query not shown" — queries can quote
facts), and its omit-the-query enumeration, which the forget flow's id
lookup still needs. A `[tools.policy.tool]."memory.recall"` override no
longer matches anything; the built-in default is allow, so nothing breaks
silently — rename the key to `"memory.search"`.

The ranking is pure daemon code — same query, same book, same order, pinned
by a determinism test — and deliberately shallow (embeddings are out of
scope; the stats this ADR adds are the evidence a future semantic layer
would be judged against):

- **Score** per fact, integer arithmetic only: `+2` for each query word
  found verbatim among the fact's significant words (the stopword filter the
  supersede matcher already uses), `+1` for a query word of ≥3 letters that
  prefixes a fact word ("deploy" finds "deployment"), `+3` when the whole
  trimmed query appears case-insensitively in the content (quoting a fact
  beats sharing its vocabulary). Zero-score facts are excluded — an empty
  result is the honest answer.
- **Order**: score descending, ties broken exactly like the injection order
  (updated, then stored, then id) — equal matches prefer what the user
  touched last.
- **Cap**: 10 results, so a tool result cannot become a second injection
  block. Search covers the whole book, pinned facts included — a search must
  never claim a fact does not exist; the prompt tells the model not to
  re-search its ambient facts, and if it does anyway it gets the truth.

Benchmarked at ~1.3 ms per call on a full 200-fact book *including* the
stats write below, fsync and all — two orders of magnitude inside the
issue's 50 ms budget.

### Retrieval stats: batched per search call, best-effort, honest

Each fact returned by a queried `memory.search` records `times_retrieved++`
and `last_retrieved = now` (the book's injected clock). The write strategy:

- **The batch is the search call.** One store write per search, however many
  facts matched — never one per fact. Search rate is tool-call rate, which
  is human-interaction rate, so the write amplification the issue warns
  about is bounded by the user talking; no timers, no debounce goroutine,
  no flush-on-shutdown state to lose.
- **Best-effort by design.** The facts were retrieved whether or not the
  bookkeeping reaches disk, so a failed stats write logs a warning and the
  search still answers. `saveLocked` commits in-memory state only on
  success, which is the whole mutation-safety argument: the book never
  holds stats the file does not, the trail is untouched (two scalar fields
  change), and the next successful search counts from the persisted state.
  A test injects a failing write and proves the book unharmed.
- **Only search counts.** Ambient injection is not retrieval (a pinned fact
  would drown the signal), and neither is browsing — `memory.list`, the
  CLI, the tool's empty-query enumeration all move nothing. "Retrieved N
  times" measures the model going looking and this fact answering; that is
  the usefulness signal the eventual RAG-over-conversations work builds on.

On the wire and the card, absence is absence: a never-retrieved fact writes
no stats keys to TOML (hand-edit diffs stay clean), sends none over IPC, and
shows no line — "retrieved N times · last <relative>" appears only when true,
with the relative wording reused from the knowledge feeds' `SpokenAge` (one
scale, one copy). `normalize` repairs hand-edits without fabricating: a
negative count is never-retrieved; a bare `last_retrieved` implies the one
retrieval that stamped it.

### Prompt

The block's disclosures carry the per-turn truth (what is ambient, what is
searchable, what was trimmed), so the system prompt gains only the judgement
the model makes before reading any tool description: facts shown are in
front of it, facts not shown are found only with `memory.search`, and it
must never claim to remember what it has neither been shown nor searched
for — the issue-#71 honesty rule applied to memory. Cost: the memory section
grows from ~147 to ~195 estimated tokens (**+48**), measured by the same
bytes/4 estimate the context-floor check uses.

## Consequences

- Memory now scales past the budget without silent loss: every over-budget
  state is either the user's explicit split or carries a warning in the
  Memory tab, the CLI, and the block itself.
- Existing small books behave identically, pinned by test; existing
  over-budget books change behaviour — from silently truncated to
  search-only-with-warning — which is the one deliberate regression-shaped
  choice here, made because the silent state was the bug.
- The no-lookup fast path is intact: a turn that needs no memory pays the
  same one stat(2) as before; search costs a round only when the model
  reaches for it.
- Conversations that stored "memory.recall" in their history describe a tool
  that no longer exists; the model treats it like any unknown tool name and
  the descriptions steer it to `memory.search`.
- Retrieval stats make the store rewrite-on-search: a hand-edit landing in
  the same instant as a search's write can be overwritten (the mtime/size
  check races). Accepted — the same window ADR 0025 accepted for every
  write, and stats are the least valuable line in the file.
