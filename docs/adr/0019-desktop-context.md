# ADR 0019 — Desktop context: opt-in eyes, gathered on the model path only

**Status:** accepted (implements roadmap Phase 2)

## Context

Jarvix answers every question blind. "What does this error mean?" asked with a
terminal full of stack trace in focus gets a generic reply about errors,
because the model never sees the screen. Roadmap Phase 2 names the fix: the
conversation gains eyes — active window, selected text, clipboard — gathered
by the daemon and offered to the model.

The feature is easy to build and easy to get wrong. Reading the clipboard on
every utterance is ambient surveillance with a helpful face on it, and paying
for three subprocesses before every answer would undo the latency work of
ADR 0017. Both concerns are structural, so both are answered structurally.

## Decision

**Per-source opt-in, in configuration, with the riskiest source off.**
`[context] window/selection/clipboard`, defaulting to on/on/**off**. A
disabled source has no *gatherer object* at all: `desktop.NewCollector` builds
the list from the enabled flags, so there is no code path on which a disabled
source's content could come to exist. It is not filtered out at the end — it
is never read. With every source off, `NewCollector` returns `nil` and the
engine holds no collector, which is the zero-cost case (1.6ns, no allocation).

The clipboard defaults off because it is the one source whose contents the
user put there for somewhere else. A window title is already on screen and a
selection is what they are pointing at as they speak; the clipboard may be the
password they copied out of their vault ninety seconds ago.

**Gathered inside `think()`, after the intent router.** This is the ordering
decision the feature turns on. ADR 0017 put a deterministic router in front of
the model: "volume thirty" executes in microseconds with no provider call.
Gathering context at session start — the obvious place, and what the ticket
first suggested — would make every matched intent wait on `hyprctl` and
`wl-paste` for a capture it never uses, handing back exactly what the router
bought. So gathering happens on the one path that actually opens a provider
request, at the top of `Engine.think`. A matched intent pays nothing; a
transcript that reaches the model pays milliseconds.

**Bounded in parallel, degrading to silence.** Sources are gathered
concurrently, each under its own timeout, all inside one budget — and the two
numbers are the same number (`context.timeout_ms`, default and maximum 300ms),
because with parallel gathering they coincide by construction: adding a fourth
source can never extend what context costs. `Config.Validate` refuses a value
above 300ms, so the budget can be lowered but never raised. A source that
fails, hangs, is not installed, or has nothing to say contributes nothing, and
the turn proceeds exactly as it would with context switched off. There is
deliberately no way for a gatherer to report a *problem* upwards: to a session,
a missing compositor and an empty clipboard are the same outcome.

**Subprocesses, per ADR 0002/0003.** `hyprctl activewindow -j` and
`wl-paste [--primary] --no-newline --type text`, each in its own process group,
killed as a group at the deadline, output capped at 256 KB, stderr discarded.
No Wayland client library, no hyprland-ipc dependency. Unlike advisor
delegation (ADR 0016) the environment is *not* scrubbed: these are the user's
own compositor tools and they need `WAYLAND_DISPLAY` and
`HYPRLAND_INSTANCE_SIGNATURE` to work at all, which is a different trust
relationship from handing a question to a third-party CLI.

**Secrets are redacted before the model, and redaction takes the whole
value.** Text that matches a private-key header, a vendor credential prefix
(`sk-`, `ghp_`, `AKIA…`, `glpat-`, `xox[abprs]-`, `AIza`, …), a labelled
assignment (`api_key = "…"`), or the shape of a random high-entropy token is
replaced entirely by `[looks like a secret — not shared]`. Partial redaction
was rejected: a key spread over twenty lines cannot be blanked safely
token-by-token, and the heuristic that tries will one day leave the last eight
characters of one in place. The high-entropy rule requires *all* of length ≥32,
all three character classes, a character-class transition rate ≥0.35, and
Shannon entropy ≥3.5 — the transition rate is what keeps
`AbstractAutowireCapableBeanFactory2` and `/home/user2/Projects/…` out of the
redactor while `wJalrXUtnFEMI/K7MDENG/bPxRfiCYzEXAMPLEKEY` stays in it. The
table is tested in both directions, because a false positive silently blinds
the assistant on ordinary work and the user has no way to know why.

**A system message immediately before the question.** The capture is one
`system` message — the model should read it as fact about the machine, not as
something the user typed — placed after the carried-over history and directly
before the new user message, so "this error" and the selection are adjacent
and "right now" is unambiguous. Each source is a delimited block
(`--- selected text --- … --- end selected text ---`) so content can never be
mistaken for instruction, with a per-source cap (`context.max_chars`, default
2000) and the truncation marker written *inside* the text, so it travels to
every surface. Context is never committed to history: a capture lives exactly
one turn.

**Gathering is charged to Jarvix, not to the model.** ADR 0018 made every
session report its latency budget, and it subtracts the transcript→first-token
span from the total as "the user's choice of model" to get `jarvix_ms`.
Context gathering happens inside exactly that span, so without its own mark it
would inflate the number that is excused and deflate the number this codebase
is accountable for. It therefore has its own stage, `context_ms`, and the
model's clock starts where gathering stops. A cost that hides inside the
figure it inflates is the one kind of measurement worse than none.

**Disclosure is a feature, not a log line.** `context.last` (IPC) returns the
exact text that reached the model — already truncated, already redacted — and
`jarvix status --last` prints it — beside that interaction's latency budget,
because "what did that cost?" and "what did it see?" are the same question
asked of the same turn, and one flag should answer both. A
`context.captured` event carries sizes and
flags only, because events fan out to every connected client. Captured content
is never logged at any level; the debug line records which sources contributed
and how many characters each held. `jarvix doctor` lists the enabled sources
and whether their binaries exist, which is also the one place a user who never
read the config finds out that Jarvix looks at anything at all.

## Consequences

- **"What does this error mean?" works.** With a stack trace selected, the
  provider request carries it, verified with the fake provider.
- **Context is effectively free.** Measured on a live Hyprland session: ~2.0ms
  for the default pair (window + selection) and ~2.0–3.5ms with the clipboard
  as well — under 1.5% of the 300ms budget, and roughly the cost of two
  process spawns. Disabled costs 1.6ns and zero allocations. A matched intent
  costs nothing at all, by construction. Against ADR 0018's release-to-first-
  audio budget of 1.5s, eyes cost about 0.1% of it, and `context_ms` says so
  on every session rather than asking anyone to take that on trust.
- **The gatherer seam is the template for Phase 2's remaining surface.** A
  screen region (with its own consent UX) and later `desktop.*` tools
  implement `Gatherer` and appear in the enabled list; the budget, the
  redaction, the message shape, and the audit surfaces come for free.
- **The heuristics will miss things.** An unlabelled password that looks like
  a word passes through, and that is inherent to pattern matching. Redaction
  is the last line of defence, not the only one — the first is that the
  clipboard is off until the user turns it on.
- **A capture outlives its session, in memory.** `jarvix status --last` is
  asked *after* the answer, so the last snapshot is retained until the next
  one replaces it. It is never written to disk and never survives a daemon
  restart.
- **The daemon test suite had to be made explicit.** With window and selection
  on by default, any daemon test that runs a session would have executed
  `hyprctl` and read the developer's clipboard. Daemon tests now boot with
  context off and say why.

## Alternatives considered

- **Gathering at session start (as the ticket suggested).** Rejected on the
  ordering interaction with ADR 0017: it charges every deterministic intent —
  the fast path — for context only the slow path can use. Gathering during
  transcription would hide the latency, but transcription happens before the
  router too, so it has the same defect.
- **A Wayland client / hyprland-ipc library.** More faithful and more
  dependencies, with a worse failure mode: a protocol error becomes a daemon
  problem, where a missing binary is just "no context". Same trade as ADR 0002.
- **Redacting only the matching token.** Rejected: unsafe for multi-line keys
  and one heuristic bug away from a partial leak. Whole-value redaction is
  honest and legible to the model.
- **A `desktop.get_context` tool the model calls when it wants context.**
  Attractive — it costs nothing until needed — but the model cannot know
  whether the screen is relevant before it has seen it, and it doubles the
  round trips for the exact question the feature exists to answer. Revisit
  when a screen-region capture (expensive, and needing consent per use) lands.
- **Persisting captures for later inspection.** Rejected: an audit log of
  everything the user ever had on screen is a worse artefact than the problem
  it solves. The last capture, in memory, answers "what did it just see?" —
  which is the question people actually ask.
