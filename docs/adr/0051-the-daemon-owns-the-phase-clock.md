# ADR 0051 — The daemon owns the phase clock: elapsed time is a fact on the wire, not a client's stopwatch

**Status:** accepted

## Context

Jarvix's surfaces say how long it has been busy. The bar widget's tooltip has
said "Thinking — 12s · Consulting claude" since issue #70, and the conversation
window's pending assistant turn now says the same thing in the message list
(issue #158).

The bar computed that figure itself. It recorded `Date.now()` when a
`state.changed` arrived and ticked a counter off it. For a widget that has been
running since login that is right, and it cost nothing to add.

It is wrong for anything that can start watching in the middle. The pending
turn's whole point is that a window opened five seconds into a six-second think
must show what a window that was already open shows — and a client-side
stopwatch shows "0s", then "1s", while the daemon has been working for five.
Not a rounding error: a fabricated number, presented as a measurement, in the
one place a user is looking to find out whether waiting is worth it. The same
hole opens whenever the window is rebuilt after a compositor kill (#108), and
whenever a client reconnects.

The honesty rules (#71) already say a surface may only claim what is actually
happening. A duration is a claim.

## Decision

**The session engine records when the current state began, and that instant
travels with the state.** It is set in `setStateLocked` — the one choke point
every transition passes through — from the engine's injectable clock, and it
rides:

- `state.changed` as `since_ms`, so a client already watching counts from the
  daemon's instant rather than from its own observation of the event; and
- `conversation.get` as `state_since_ms`, so a client that has *no* events
  counts from exactly the same instant.

`Engine.Phase()` reads the state, its session, its start, and the tool call in
flight as **one** consistent snapshot. Four accessors would let a client pick a
state up from one moment and a start time from the next and quote a duration
that never happened.

Clients subtract; they do not measure. Both clocks are this machine's, so the
subtraction is honest, and a snapshot that carries no start (an older daemon)
simply shows no count rather than inventing one.

**Corollary: the tool in flight is part of the phase.** "Thinking" is true
during a two-minute advisor call and useless. Only the daemon knows which call
is open, so `Phase` carries the tool's name and its own progress label, and
`conversation.get` serves them — otherwise a window that arrives mid-round is
reduced to a generic label precisely when the specific one matters most.

## Consequences

- Any future surface that reports a duration — a CLI status line, a second
  window, a phone client — gets the same number without inventing a stopwatch,
  and cannot disagree with the surfaces that already exist.
- The additions are fields on events and results that already existed. No new
  event, no new method, no polling, and no protocol bump: a client that ignores
  them sees exactly the payload it always saw (docs/ipc.md).
- The bar widget keeps its local counter. It has no reconnect-mid-phase problem
  worth solving — it is running before any session starts — and rewriting a
  working, tested surface to prove a point would be churn. The rule binds
  anything that *can* start mid-phase, and the bar is now the only counter in
  the tree that does not read `since_ms`. If it ever grows a case where it can,
  the field is already on the event it already handles.
- The engine gained two guarded fields and one clearing rule. The clearing is
  the part worth naming: the running tool is cleared both when the call returns
  *and* on every transition to Idle, because a tool blocked inside a cancelled
  context returns late or never, and a surface still saying "Consulting claude"
  after the turn ended would be reporting work that belongs to nothing.
- The elapsed *threshold* is not a client decision either. It lives beside the
  wording in `internal/desktop/pending.go` and is compiled into the QML library
  with it, so "when does a wait start saying how long" is one tested rule
  rather than a constant each surface picks for itself.
