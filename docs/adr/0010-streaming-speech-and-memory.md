# ADR 0010 — Streaming speech and conversation memory

**Status:** accepted (roadmap M2 + M3)

## Context

Two gaps made Jarvix feel less like a working partner: it spoke only after
the entire answer was generated (a long silence on longer replies), and it
forgot everything between questions (no follow-ups). Both are essential for
long, serious sessions.

## Decision — speak while streaming

`think` now speaks sentence-by-sentence as the model generates, over one
continuous playback stream:

- A `sentencer` splits the token feed into complete sentences (boundary =
  terminal punctuation or newline followed by whitespace; decimals and
  `http://` are not split; an unpunctuated run flushes at 240 chars).
- A `streamingSpeaker` owns a single `player.Play` for the whole turn and
  synthesizes each sentence as it arrives, forwarding PCM into that one
  stream. Playback starts (and the session enters `Speaking`) on the first
  audio; `tts.started` fires then, `tts.finished` when the stream drains.
- One speaker serves all tool rounds of a `think`, so any narration and the
  final answer play in order without gaps.

Result, measured live: on a four-sentence answer speech began at +2.15s and
generation finished at +6.49s — **4.3s of speaking that used to be silence.**

Consequence: `tts.started` can now precede `assistant.finished` (audio starts
before text generation ends). The event contract already forbids assuming
cross-event ordering except the documented guarantees; tests assert event
presence, not order.

## Decision — conversation memory

The engine keeps a rolling history of prior user+assistant exchanges:

- `conversationMessages` prepends the system prompt and the retained history
  to each new user turn; `commitTurn` appends the completed exchange, capped
  to `conversation.history_turns` pairs (default 16).
- Only the user question and the assistant's **final** answer are stored —
  not intermediate tool calls/results — keeping context clean and avoiding
  dangling `tool_call_id` references across turns.
- `conversation.follow_up_window_sec` (default 900) resets the thread after
  an idle gap, so a new question does not inherit a stale one. `jarvix new`
  / `conversation.reset` clears it on demand.
- Only successfully completed turns are committed; cancelled or interrupted
  turns leave history untouched.

History lives in memory in the daemon (per its lifetime). Persistence across
restarts is deliberately out of scope here — see the roadmap's persistent
conversational mode.

## Consequences

- Follow-ups work: "why is my build failing?" → "what should I change?" keeps
  context, verified live.
- The two features compose with tools: a tool-using turn streams its final
  spoken answer and is remembered by its final text.
- Memory is bounded (turn cap + idle window) so context cannot grow without
  limit or leak an old thread into a new task.
