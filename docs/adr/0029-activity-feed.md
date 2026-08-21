# 0029. The activity feed: a daemon-side ring of rendered rows

Date: 2026-08-21

## Status

Accepted. Implements issue #70.

## Context

The user asked to see what Jarvix is thinking about and doing — a small
indicator when the window is closed, detailed and extensive inside it. The
need was proven the day it was asked for: Jarvix claimed in prose to have
launched an app and focused windows while its logs showed `tool_calls=0`,
and the only instrument that could show the difference was `journalctl`.

The raw material already existed. The bus publishes nearly everything the
daemon does — tool bounds, gate decisions, injections, routine steps,
timings, errors — and every surface then discards it. Refusals were worse
than discarded: a refused launch ("firefox is not installed") reached the
journal and the model's tool result, and no user surface at all.

## Decision

**One subscriber, one ring, rendered daemon-side.** A bus subscriber in the
daemon (`internal/daemon/activity.go`, the same pattern as the notification
watcher) renders each event into zero or more *rows* — kind, label, detail,
failed — using a table in `internal/desktop/activity.go`, and keeps them in
a bounded in-memory ring (`ui.activity_rows`, live-reload). `activity.get`
returns the ring; each append is also pushed as an `activity.row` event.
The window renders both verbatim and merges them by the row's monotonic
`seq`. Nothing touches disk: conversations are the durable record, activity
is operational and dies with the daemon.

**Rows arrive worded, not raw.** The alternative — teaching the window to
render raw events — would have meant compiling the entire wording table to
JavaScript and keeping two renderers in agreement forever. Rendering once,
daemon-side, keeps ADR 0013's rule at its strongest: the window's only
activity vocabulary is a generated glyph-per-kind table
(`plugin/omarchy/ActivityState.js`, `go generate ./internal/desktop`), and
every sentence it shows was composed where it is tested. The bar's richer
tooltip follows the BarState pattern exactly: `desktop.LiveTooltip` decides
what the hover text says — elapsed time in the phase, the tool or advisor
in flight, the confirmation question while one is pending — and is
generated into `BarState.js`, with a node-run mirror test proving the two
agree.

**The bus learns what the feed needed.** Three additive changes:
`tool.finished` gains `duration_ms` and `outcome` (ok, or `error` for the
registry's own failures); `assistant.finished` gains `tool_calls`, so a
turn that answered without requesting a single tool is a stated fact — the
feed renders it as an explicit text-only marker rather than an absence to
be noticed; and the desktop tools gain a `desktop.refusal` event (verb,
target, reason) published from the places that until now only logged —
which is precisely the row the incident needed.

**Privacy is enforced in the vocabulary, and tested by mutation.** Rows
carry counts for memory and desktop context (ADR 0019/0025), lengths for
typed text (ADR 0023) and search queries (ADR 0028), and per-tool argument
summaries that default to *nothing* for tools the table does not know —
model-authored arguments can carry anything, so unknown means silence. The
tests feed events salted with forbidden content and fail if any row
repeats it; an edit that starts copying content into a row cannot pass.

**Slow-client discipline is inherited, and reconciliation is explicit.**
The subscriber is an ordinary bus client: it may drop events (missing rows,
honestly missing) and can never wedge the engine. `activity.row` pushes may
likewise be dropped, which is why `activity.get` — what the daemon actually
holds — is the reconciliation path, and why rows carry `seq`. One ordering
caveat is documented in docs/ipc.md: rows are derived, so a session's last
rows can trail `session.finished` on the wire; clients that treat that
event as terminal lose nothing.

## Consequences

- "Did it actually do it?" is answered by looking: tool rows with argument
  summaries, outcomes and durations; gate rows for ask/decline/deny with
  the daemon's reason; the text-only marker for the claimed-but-never-called
  case; refusal rows for what the desktop would not do.
- The ring costs a bounded few hundred kilobytes (rows × per-row caps) and
  one subscriber channel when no window is open.
- A new bus event needs a vocabulary entry before it appears in the feed —
  a deliberate allowlist: events are born invisible to the feed, and become
  visible with words that were chosen and tested, never by default.
- `activity.row` slightly widens bus traffic (one derived event per row).
  Clients that do not care ignore it, per the protocol's additive-change
  rule.
