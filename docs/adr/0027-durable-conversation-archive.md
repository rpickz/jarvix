# ADR 0027 — Durable conversations: the archive behind the live head

**Status:** accepted

## Context

Jarvix kept exactly one conversation and destroyed it on `jarvix new`.
Continuity was built (ADR 0011) but history was not: the live thread is
capped at `history_turns` and expires after the follow-up window, so even
what the user is *in* quietly loses its head, and everything discussed —
decisions, answers, figures Jarvix looked up — evaporated. Issue #57 asks
for the opposite: conversations as durable records, listed, reopenable, and
deletable, as the foundation of the search/RAG arc. Two prior stores set the
precedents this decision has to place itself against: the history file
(ADR 0011 — atomic JSON, fsync+rename, capped) and the memory book
(ADR 0025 — TOML, chosen *because the user hand-edits it*).

## Decision

### The archive sits beside the live head, not under it

`internal/history` stays exactly what it is: the small, capped, load-fast
live head the engine restores at boot. The archive (`internal/conversations`)
is a separate, unbounded record behind it. The alternative — making the
archive the engine's one store and deriving the head from its tail — was
rejected because it rewires every load/save/reset path of ADR 0011 for no
user-visible gain, and because the two files answer different questions with
different lifetimes: the head is working memory (bounded, expiring,
deletable as a side effect of `jarvix new`), the archive is the record
(unbounded, expiring never, deletable only on explicit command). The price
is that the live thread's turns exist in both files; a few kilobytes of
duplication is cheaper than coupling crash-recovery of the record to the
semantics of working memory.

What ties them together is one pointer file, `conversations/active`: the id
of the conversation the live head belongs to. A restarted daemon that loads
a non-empty head reattaches through it, so a reboot mid-conversation keeps
appending to the same record instead of forking a new one per boot. The
pointer is a convenience, never a record — every failure reading it degrades
to "start a fresh conversation".

### Two files per conversation: append-only JSONL plus a small metadata doc

    $XDG_STATE_HOME/jarvix/conversations/
      <id>.jsonl    header line {"schema":1,"id":…}, then one line per turn:
                    {"role":"user","text":"…","ts":RFC3339}
      <id>.json     {"schema":1,"id","started","last_active","turns","preview"}
      active        the live head's conversation id

**JSONL, not the memory book's TOML** — ADR 0025's argument cuts the other
way here. Memory is TOML because the *user* edits it; an archive is an
append-mostly machine record nobody is invited to edit. JSONL gives three
things TOML cannot: an append is one written line (a crash tears at most the
line in flight, and the reader drops a torn *final* line instead of losing
the conversation — anything torn earlier is real corruption and is reported
as unreadable); the golden-file schema is per-turn records with role, text,
timestamp and a document version, which is exactly what the search ticket
(#59) will index line-by-line without a migration; and the transcript never
needs rewriting, so a 100-turn conversation costs one line per turn, not a
100-turn rewrite. The metadata file is rewritten atomically per append with
ADR 0011's exact temp+rename+fsync discipline — it is small by construction
(one preview line, capped), which is what makes listing hundreds of
conversations read only metadata and never a transcript.

Ids are `20060102-150405-xxxx`: humane to type, time-sortable, never reused
(the random suffix), and validated at every entry point because they arrive
over IPC and become file names.

### Append before the cap, flush after the session

`history_turns` governs what the model is sent, **never** what is archived.
The hook is `commitTurn`: the completed exchange is staged for the archive
*before* the cap trims the in-memory head — and even when `history_turns`
is 0 — so a 100-turn conversation archives 100 turns. The staged batch is
flushed on the session tail, after `session.finished` and off the engine
lock, exactly where the history write lives: zero latency added to the
spoken exchange, and — because the tail runs inside the engine's tracked
group — the shutdown drain (#29) waits for it. `jarvix new` therefore
archives by *detaching*: the turns are already on disk; the reset flushes
whatever the last exchange left staged and ends the attachment.

### Retention, reopening, deletion

`conversation.retention` ("on"/"off", `[conversation]`, settings registry,
idle-class) gates only writing: off hands the engine no archive at all, and
removes nothing retroactively — the daemon keeps its store handle so
listing, reading, and deleting what is already kept always work. Reopening
(`conversation.open`) is an explicit action: the engine adopts the record as
the live thread, keeps the most recent `history_turns` exchanges for the
model (the archive keeps everything the prompt cannot), restarts the
follow-up clock, moves the `active` pointer, and appends follow-ups to the
same record. Deletion removes both files and syncs the directory — no
tombstones — and deleting the *active* conversation also resets the live
thread, or the next turn would rebuild the record from working memory.
Files are 0600 in a 0700 directory; contents never appear in logs, events,
or error messages; unreadable conversations are listed as unreadable rather
than hidden, so one bad file never hides the library — or itself.

## Consequences

- Search (#59) indexes `*.jsonl` directly: versioned, line-oriented,
  timestamped records; the listing metadata is already the result surface.
- Every conversation costs disk forever until deleted — accepted; text is
  small and the user holds first-class delete/off controls.
- The head/archive duplication means `conversation.get` (the window's live
  view) and `conversation.read` (the archive view) can briefly disagree
  while a flush is in flight; both converge on the session tail.
- A crash between the transcript append and the metadata rewrite leaves a
  turn count one short; the next full read reports the transcript's truth.
