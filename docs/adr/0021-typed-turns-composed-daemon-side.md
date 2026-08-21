# ADR 0021 — Typed turns are composed daemon-side (`session.text`)

**Status:** accepted

## Context

The conversation window gains a text field (issue #35): type a question,
press Enter, and it joins the conversation as if it had been spoken. Speech
is the wrong input for a URL, a file path, a command flag, an API key, or an
unusual proper noun — exactly what whisper transcribes worst — and it is no
input at all when someone else is on the call.

The turn itself needs nothing new. `jarvix ask` has always submitted text
without audio: `session.start`, then `session.submit {text}`. What is new is
that the *client* pressing Enter is a text field, and those two calls hide a
decision:

- **Idle, or Jarvix mid-answer** → start a session (which cancels whatever is
  running — that is what makes typing interrupt speech) and submit the text.
- **`awaiting_confirmation`** (ADR 0014) → the text is the answer to a tool
  call waiting on the user. `session.start` here would cancel the session
  that asked, silently abandoning the very command being approved.

`jarvix ask` never had to make that decision — it is not the surface anyone
answers a confirmation from. The push-to-talk CLI does make it, and does it
by reading `status.get` first (`cmd/jarvix/commands.go`).

## Decision

Add one IPC method, `session.text {text}`, that composes the existing two and
takes that decision in the daemon, inside a single hold of the session lock
(`Engine.SubmitText`). It starts nothing new downstream: with a confirmation
pending it goes through the same `Submit` branch a typed `session.submit`
already went through, resolved by the one affirmative/negative parser in
`internal/session/confirm.go`. Empty or whitespace text is rejected before
anything is started or interrupted.

The conversation window's composer calls this and nothing else. It sends one
request per Enter and never sequences a session itself.

## Rationale

- **QML is display-only (ADR 0013).** "Which call does Enter make?" is a
  decision worth testing, and QML is where this project cannot test. In Go it
  is covered from both ends: unit tests on the engine and hermetic tests
  through the daemon's real socket.
- **A client-side sequence races the state it read.** Read
  `awaiting_confirmation`, then call `session.start` a round trip later, and
  the confirmation may have timed out in between — the CLI's `status.get`
  dance has the same hole, it just has a human's reflexes in front of it
  rather than a keystroke. Under the engine lock there is no gap.
- **One parser, one reading of "yes".** A window that decided for itself
  would need the affirmative vocabulary in QML, and the day the two disagreed
  is the day a misread "no" runs `rm -rf`.
- **Additive, so no protocol bump.** New methods do not change the protocol
  version (docs/ipc.md); `session.start` and `session.submit` are untouched
  and every existing client keeps working.

## Consequences

- Issue #35 asked for the window to call `session.start` + `session.submit`
  directly. It does so transitively — same engine path, same events, same
  history — but through one method rather than two, which is the deviation
  this ADR records and why.
- There is now a second way into a turn from text. `jarvix ask` deliberately
  keeps its two explicit calls: it streams the answer to a terminal and is
  the documented protocol example, and it is not a surface where a
  confirmation is ever pending.
- A future multi-line composer, or any other client with a text field, gets
  the interruption and confirmation semantics for free by calling the same
  method.
