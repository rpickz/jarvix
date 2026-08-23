# ADR 0030 — Knowledge feeds: changing facts, kept fetched, aged out loud

**Status:** accepted

## Context

The memory book (ADR 0025) deliberately holds *static* user facts: things
the user stated once and expects kept. It has no answer for facts that
*change* — a stock price, the weather, a build status. Today such a question
gets either a stale guess from the model's training data or a slow tool
round trip per ask, and issue #84 asks for the moving counterpart: values a
user-written command keeps current, cached in the daemon so the answer is
instant, with its age spoken honestly.

The second-order goal matters as much as the first: feeds are the general
seam for *anything periodic*. Weather, calendar summaries, CI status — each
becomes one TOML table and one user-written script, not bespoke daemon code.

## Decision

### `[[knowledge.feeds]]`: a fixed command, a cadence, a freshness horizon

A feed is a name, a description (the model's steering), a fixed argv that
prints the value on stdout, a mode, an interval, and a ttl. Two modes:
**eager** refreshes on schedule so the value is ready before it is asked
for; **lazy** fetches on first use and serves cached until the ttl lapses.
The ttl is the honesty line either way: a value past it still serves, but
is disclosed as stale.

The model's reach is one read verb, `knowledge.get(feed)`. It cannot write
the argv, the environment, or the schedule — the advisor rule (ADR 0016)
applied verbatim: the model chooses *which*, configuration decides
*everything else*. Fetches run with the same subprocess discipline, as its
own copy in `internal/knowledge` (tools imports knowledge for the tool, so
sharing would cycle): no shell, scrubbed environment, process-group kill on
timeout, capped output. A no-such-feed ask returns the configured list, so
the model can say what it *can* watch instead of a bare refusal.

### The scheduler is a supervised component from day one

The lesson of #74, applied in advance rather than in a bug fix: every
scheduler goroutine registers with one tracked `quiesce.Group` at spawn,
never a bare `go`. Shutdown drains the group as its own stage under the
daemon's grace deadline — cancellation reaches every loop *and* every
in-flight fetch (the process group dies with the context), so a stopping
daemon never abandons a values-file write. A reload swaps the feed set
through `Reconfigure`: the old generation of loops is cancelled *into the
same group*, so a rebuilt schedule can never orphan a goroutine either. The
service itself is construction-wired and lives for the daemon's life; only
its feed set and timers rebuild.

Everything is clock-driven through injected seams (`Now`, `Timer`, `Runner`
— the warm supervisor's pattern, ADR 0018): the tests fire the exact timer
the scheduler armed and never sleep.

### Failure serves the last good value, and backs off

A failed fetch neither errors a session nor discards anything: the last
good value serves with its (older) age disclosed, the failure is logged
(one Warn per streak, values never) and visible in `jarvix doctor` and
`knowledge.status` as "failing since …". Retries double from the feed's own
cadence (interval for eager, ttl for lazy), capped at an hour — the first
retry is never later than a normal refresh, and a broken command is never
hammered.

### Persistence: a 0600 cache that boots the daemon warm

Values persist in `$XDG_STATE_HOME/jarvix/feeds.toml`, written atomically
with the ADR 0011 fsync-and-rename discipline, 0600 in a 0700 directory —
feed values may be sensitive, so they also never appear in logs or bus
events (counts only; the values travel over the user's own socket via
`knowledge.status`, the `memory.list` precedent). Unlike the memory store
this file is a machine-written *cache*: every byte is reproducible by
running the commands again, so an unparseable file means a cold boot, never
a preserved corpse. At boot, eager schedules resume from the persisted
timestamps — a half-elapsed interval waits out its remainder; a value past
its ttl serves as stale and refreshes immediately.

### Freshness is spoken, in words

Every surface that hands a value onward carries its age as speech —
"as of four minutes ago" — via `knowledge.SpokenAge`, the sub-day
counterpart of conversation search's spoken-when (ADR 0028), which starts
at calendar days because a conversation's age is a calendar question and a
feed value's is not. The tool result states the age and instructs the model
to say it; the system prompt (appended only when feeds exist) makes
freshness-stating a standing rule.

### The gate: one identity, `knowledge.refresh`, default allow

Reads and refreshes are judged under one identity — reading a feed is what
triggers fetching it, so a policy entry cannot half-disable the feature.
It defaults to allow for routine.run's reason: authorship. The user wrote
the command; asking "may I check your AMD feed?" after "what's the AMD
price?" asks them to confirm their own sentence. `deny` disables feeds
outright; `ask` confirms each tool read (naming the feed, daemon-side) and
also stops the background schedules, because a scheduled fetch has no way
to ask a question. The gate decision for schedules is taken once at
construction — the tools section is restart-class.

### Injection: the memory budget discipline, opt-in per feed

A feed with `inject = true` rides its cached value into every model turn
under `knowledge.max_injected_tokens`, measured with the memory block's
token estimate, trimmed from the end of declaration order, trims disclosed
in the block itself and counts (never values) on a `knowledge.injected`
event. The block sits beside the desktop capture, adjacent to the question
— a reading describes "right now" — and is rebuilt fresh each turn, never
committed to history, so an answer cannot quote last turn's price with this
turn's confidence. Injection never fetches: a model turn must not wait on a
feed command.

## Consequences

- "What's the AMD price?" is answered from a value already in the daemon,
  with its age in the sentence; a cold lazy feed pays one fetch on first
  use, inside the tool call, with the overlay saying so.
- Feed tables are hand-edited TOML like `[[routines]]`: outside
  `config.set`, listed read-only (`knowledge.status`), applied on idle
  reload — except the first feed, which registers the tool and takes a
  restart.
- The user owns a new file (`feeds.toml`) they may freely delete, and a new
  doctor line tells them per feed whether the command exists and, from the
  live daemon, how many values are fresh, stale, or failing since when.
- Anything periodic the user can script is now a feed; the daemon gains no
  bespoke fetch code, ships no network calls of its own for this, and the
  worked stock-price example in docs/configuration.md is a curl one-liner
  the user writes.
