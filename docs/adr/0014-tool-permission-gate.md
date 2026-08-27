# ADR 0014 — The tool permission gate: risky commands require spoken confirmation

**Status:** accepted (implements the permission boundary reserved by ADR 0006);
amended 2026-08 — the audio/display split below (issue #119)

## Context

`shell.run` (ADR 0009) is arbitrary code execution guarded by a config
boolean and a system-prompt plea — a hope, not a control. Before the tool
surface widens (advisors, artifacts, `desktop.*`, `files.*`) and before
wake-word activation lets a session start from across the room, execution
needs a real gate: the user must be able to hear what is about to happen
and stop it.

## Decision

**Three tiers, classified daemon-side.** Every tool call passes an
allow / ask / deny policy (`internal/tools/policy.go`) before
`Registry.Execute`:

- **allow** — a shipped read-only allow list (word-prefix patterns:
  `docker ps`, `df -h`, `git status`, `journalctl`, …) runs silently,
  preserving pre-gate behaviour for the commands that made the tool useful.
- **ask** — everything unmatched, plus explicit risk patterns (`rm`, `dd`,
  `mkfs*`, `sudo`, output redirection `>`), pauses for confirmation.
- **deny** — catastrophic patterns (`rm` targeting `/`, `dd of=/dev/…`,
  redirection onto a block device, fork bombs) never run, with or without
  confirmation.

Ordering is the security argument: **deny > risk > allow > default-ask**.
A compound command (`;`, `&&`, `||`, pipes, `$()`, backticks) is split and
judged by its riskiest part; deny rules also run against the unsplit command
so splitting can never defeat them. Quoting is deliberately not honoured —
over-splitting can only escalate towards ask. Obfuscation (`$IFS`, env
prefixes) cannot reach the allow tier because allow matching is exact
word-prefix; anything unrecognisable simply asks. The allow list is the
precision instrument; the deny list is defence in depth, not the gate.

**Nothing from the model is trusted.** Classification reads the parsed
`command` argument only, and the spoken summary is generated from that
command — a model cannot describe `rm -rf ~` as "tidying up". The exact
command is published verbatim (`tool.confirmation_required`) for the
overlay. Unknown tools default to ask, so future tools ship gated by
construction; `artifact.create` carries a built-in allow default because
its writes are confined to the artifact directory (ADR 0012). The policy
lives in config, which the model cannot write.

**Confirmation is a session state.** The state machine gains
`AwaitingConfirmation` (Thinking/Responding → AwaitingConfirmation →
Thinking), because "Jarvix asked and is listening for an answer" is
authoritative session state, not a boolean. The user answers by:

- voice — a push-to-talk press while awaiting flows into the pending
  confirmation (AwaitingConfirmation → Listening → Transcribing → resolve);
  the transcript is parsed strictly: negation anywhere declines, only a
  clear yes-word or known phrase ("go ahead", "do it") approves, anything
  ambiguous declines;
- text — `session.submit {text}` while awaiting is the answer;
- CLI — `jarvix confirm` / `jarvix deny` (`session.confirm`).

A 30-second timeout (configurable) declines; interruption and cancellation
decline and tear down cleanly. Decline in every form returns a
"declined by user" tool result to the model — the tool loop continues, so
the assistant answers gracefully instead of the session dying, and nothing
has executed.

**Approvals are conversation-scoped.** `remember_for_conversation = true`
re-runs an approved (tool, exact command) pair silently until the
conversation ends — `jarvix new`, the follow-up window, or a daemon restart.
Approvals are never persisted; the risk of a stale yes outliving its context
outweighs the convenience.

**Observable.** Every ask/deny decision is logged (command, decision, rule,
source) and published as bus events (`tool.confirmation_required`,
`tool.confirmed`, `tool.declined`, `tool.denied`) so the window can show an
audit trail. `tool.started`/`tool.finished` now bracket real executions
only. `jarvix status` prints the effective policy.

## Consequences

- All patterns compile once at construction; a Decide call is a few string
  scans (microseconds — the ≤10ms budget for allow-listed calls holds by
  orders of magnitude, pinned by a benchmark).
- The confirmation exchange is reusable: any future clarifying question can
  ride the same AwaitingConfirmation state and reply routing.
- `[tools.policy] tool."shell.run" = "allow"` restores pre-gate trust for
  users who want it — deny patterns still win even then.
- The conservative allow list means some harmless commands (`find`, `sed`,
  `env`, bare `ip`) ask; the cost is one spoken question, and `shell_allow`
  lets users extend the list. The alternative — an allow list with
  writable corners — would quietly hollow out the gate.
- Residual risk is acknowledged, not hidden: an approved command runs with
  full user authority, and the deny list is best-effort pattern matching,
  not a sandbox. Namespaces/seccomp remain out of scope (per the ticket).

## Amendment (issue #119): verbatim display is mandatory, verbatim audio is optional

The original decision conflated two guarantees under "the user must be able
to hear what is about to happen": that the ground truth is *available* to the
user before they answer, and that it is *read aloud*. Living with the gate
separated them — the verbatim text is already on the confirmation card (and
the ask is identified on the mid-screen overlay), so the read-out repeats
what is in front of the user and makes every ask slow. The amendment splits
the doctrine explicitly:

- **Verbatim display is mandatory and not configurable.** Every visual
  surface that renders a pending confirmation shows the daemon's exact
  command (the window card), or identifies the ask and defers to a surface
  that does (the overlay's compact request). The events
  (`tool.confirmation_required`), the mid-wait snapshots
  (`conversation.get`, `status.get`), and the conversation record always
  carry the full generated question and the verbatim command, whatever is
  spoken. Nothing about "generated daemon-side from the parsed command —
  never from the model's description" changes.
- **Verbatim audio is optional, default off.** By default the spoken prompt
  names the action class and points at the screen ("May I run a shell
  command? The details are on screen.") — a sentence that is honest exactly
  because the display half is mandatory. `confirmations.speak_details =
  true` restores the full verbatim read-out. The knob is audio-only and
  lives in the settings registry; it never widens what the assistant may do,
  so it is editable from the Settings tab and by voice (ADR 0036).

Unchanged and deliberately so: the ask/deny tiers and their ordering, the
one-confirmation mechanism (one state, one timeout, one `session.confirm`
resolution verb shared by every surface, one audit trail), `script.run`'s
always-ask floor, and the eyes-free path — a user who wants to decide without
looking turns `speak_details` on and hears everything, as before.

A resolution from any surface also stops whatever remains of the question's
own read-out: the answer has been given, and the conversation's speech
resumes immediately (the "moves onto the next message" requirement).
