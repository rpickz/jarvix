# ADR 0038 — A conversation ends only when the user says so

**Status:** accepted (implements issue #117; amends ADR 0027's lifecycle
section and the follow-up-window default)

## Context

A real exchange (2026-08-27, the daemon log in issue #117): the user asked a
question, Jarvix asked for clarification, the user pushed-to-talk to answer —
and the answering session started with no memory of the question. The log
shows why: starting a session cancels the previous one (`interrupted by new
session`), and a cancelled session committed nothing. Its exchange was in
neither working memory nor the archive; the model even ran
`conversations.search` (results=0) and then told the user it lacked context.
To the user, the conversation had simply died — as a *side effect* of the most
natural voice pattern there is, the clarification loop.

Two separate defects compound here. First, the commit ran only on the happy
path: `commitTurn` sat at the tail of a completed `think()`, so interruption,
`jarvix cancel`, and the stop word all dropped the exchange — even when, as in
the incident, the answer had fully streamed and was merely still being spoken.
Second, the lifecycle had an implicit end nobody asked for:
`follow_up_window_sec` defaulted to 900, so fifteen quiet minutes forgot the
thread as thoroughly as `jarvix new`.

## Decision

**One conversation continues across sessions, interruptions, cancellations,
idle time, and daemon restarts, until the user explicitly starts a new one.**

Concretely:

1. **Interrupted exchanges are committed, marked interrupted.** Every cancel
   path — a new push-to-talk superseding a session, `jarvix cancel`, the stop
   word (`CancelSpeech`), shutdown's cancel — commits whatever exchange the
   dying session was carrying: the user's turn and any assistant text that had
   streamed (mirrored per-delta onto the session for exactly this moment).
   The mark is carried twice, deliberately:
   - **In the text**, as a bracketed annotation on the assistant half
     (`[interrupted — the user cut this answer off here]`, or `[interrupted —
     the user cut in before any answer was given]` when nothing streamed).
     Text is the one channel every consumer already reads — the model sees
     why the last answer stops mid-thought, the window and CLI render it with
     no new plumbing, and a reopened conversation carries it back into
     context unchanged.
   - **In the archive schema**, as `interrupted: true` on both turns.
     Additive and `omitempty`: completed turns' lines — and the golden files —
     stay byte-identical, old archives without the key load as `false`, and
     `SchemaVersion` stays 1.

   The staging happens under the engine lock *before* `session.cancelled` is
   published, so the read barrier (`SyncArchive`, issue #115/#116) gives
   interrupted commits the same acknowledged-visible guarantee completed
   turns have: a client that has seen the event can already find the exchange
   by `conversation.search`. The empty-string exception in `runIntent` keeps:
   an assistant half is never empty (providers reject empty messages), which
   is why the no-answer case commits the annotation as the whole assistant
   turn rather than committing the user turn alone.

2. **No implicit end.** `follow_up_window_sec` now defaults to **0**: idle
   time is not a decision, and a conversation must only end on one. The knob
   stays — anyone who wants the old auto-forget gap can set it, and it remains
   documented in docs/configuration.md — but the shipped behaviour is that
   hours of idle keep appending to the same thread. Daemon restarts already
   preserved the thread by design and continue to: the capped live head is
   reloaded from history.json (ADR 0011) and the archive's `active` pointer
   reattaches it to the same archived conversation (ADR 0027), so a restart
   is invisible to the conversation. Failure of either store degrades to a
   fresh thread with a warning, never a crash — unchanged.

3. **One explicit-end verb, `conversation.new`, three thin clients.** The
   verb cancels any session in flight (which commits that exchange, marked
   interrupted, into the thread being ended — ordering matters and lives in
   the engine, under one lock), flushes the staged tail into the archived
   record, detaches, and clears working memory, approvals, and the persisted
   head. The entry points hold no sequencing of their own:
   - **Voice:** "start a new conversation" and its variants were already in
     the deterministic intent router (ADR 0017, `conversation.new` intent);
     the engine path they reach is the same reset core. Deterministic on
     purpose — ending a conversation must never depend on the model.
   - **Window:** the Chat tab's New chat button sends `conversation.new` and
     renders whatever `conversation.changed` says happened (ADR 0013).
   - **Bar:** the widget's right-click panel's "New conversation" item runs
     `jarvix new`, which now calls `conversation.new`.
   `conversation.reset` remains, unchanged, as the forget-only operation the
   daemon itself composes (deleting the active conversation) and older
   clients may call: it does not touch a session in flight.

## Consequences

- The incident shape is now a test
  (`TestInterruptedExchangeSurvivesIntoTheNextSession`): ask → clarifying
  question → interrupt → answer, asserting the follow-up's model context
  contains the full pending exchange.
- Working memory can now contain an exchange whose assistant half is an
  annotation. That is the point: an honest "I was cut off" beats a hole the
  model fills with "I have no context for that".
- A stopped answer ("stop", `speech.cancel`) is an interrupted exchange too,
  and is committed as one — previously it vanished exactly like the incident's
  turn.
- Conversations grow until explicitly ended. The archive was already
  unbounded by design (ADR 0027); the live head stays capped by
  `history_turns`, so prompt cost does not grow with thread age.
- Users who relied on the fifteen-minute auto-forget must now either say
  "start a new conversation" (or click/tap the same) or restore the old
  behaviour with `follow_up_window_sec`.
