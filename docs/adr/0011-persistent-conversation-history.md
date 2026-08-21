# ADR 0011 — Persistent conversation history

**Status:** accepted (roadmap Phase 5)

## Context

Conversation memory (ADR 0010) lives in the daemon's process memory, so a
restart — crash, logout, upgrade, config reload — wipes the thread
mid-conversation: "what should I change?" after a reboot answers from
nothing. The roadmap names persistence as the next Phase 5 step, and it is
the precondition for the Jarvix window ever showing more than the current
session.

## Decision — one JSON file under the XDG state dir

History persists to `$XDG_STATE_HOME/jarvix/history.json` (fallback
`~/.local/state/jarvix/`), via the existing `config.Paths` helpers. State,
not data: it is machine-local operational memory the user may delete at
will. The format is a small versioned JSON document — the user+assistant
pairs the engine already keeps, plus the last-turn timestamp so the
follow-up window applies across restarts exactly as it does in memory. Only
final exchanges are stored: no tool traffic, and never the system prompt.

A `history.Store` seam (`internal/history`) separates the engine from the
disk; `history.File` is the real implementation, `history.Fake` the test
one.

## Decision — persistence degrades, it never breaks the conversation

- **Privacy.** The content is user speech: file mode 0600 in a 0700
  directory, capped at 1 MiB (oldest exchanges dropped first), and turn
  contents are never logged — load/save/expire log turn *counts* at debug.
- **Crash safety.** Writes go to a temp file in the same directory, fsync,
  then rename — a kill mid-write leaves the old history or the new one,
  never a torn file. A corrupt or unreadable file at startup downgrades to
  a warning plus an empty history; the daemon always boots.
- **Zero added latency.** The engine saves after `session.finished`, off
  its lock path. A failed save warns once and latches the engine into
  in-memory-only mode rather than warning on every exchange.
- **User control.** `jarvix new` clears disk as well as memory (directly
  when the daemon is not running), and `conversation.history_turns = 0`
  never writes and removes any existing file on startup.

The engine's follow-up-window clock became an injectable `func() time.Time`
so tests can lapse the window across a simulated restart deterministically.

## Consequences

- A follow-up asked after a daemon restart still has its context, inside
  the same window that governs in-memory expiry; outside it, the old thread
  stays on disk but is dropped before it reaches the provider.
- The on-disk file is the natural read source for the future Jarvix window
  and per-conversation threads (still future work, as is any off-machine
  sync — explicitly never).
- The document carries a version field, so a future shape change can warn
  and start fresh instead of guessing.
