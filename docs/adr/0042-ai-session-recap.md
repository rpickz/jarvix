# ADR 0042 — AI-session recap: a thread anchored to an AI session recaps itself

**Status:** accepted

## Context

Focus threads (#123, ADR 0041) made switching cost one sentence — but the
recap is templated from the thread's own record, and for the hardest kind of
front that record says the least. The user's multi-tasking usually includes
an AI session (Claude Code or opencode in a terminal), and re-entering one
means scrolling back through a wall of agent output asking "where were we?".
#124 asks that question to answer itself: on "switch to X" or "where am I on
X", read what is visible in the anchored window, ask the model for a short
"where we're up to", and speak that.

Three commitments constrain the design. ADR 0041: the base recap is honest
because it is templated — a model-composed sentence must never be able to
degrade that honesty, only to add on top of it. ADR 0019: reading the screen
is opt-in, bounded, redacted, disclosed, and never persisted. And the
interaction budget: a recap that arrives after the user has re-oriented
themselves has failed at its one job (target ≤4s from ask to first audio).

## Decision

### The trigger: terminals by default, opt-in and opt-out by hand

Capture-based summarisation runs only when the switch or check lands on a
thread whose live anchor is a **terminal** (the same class list typing
escalates on, `tools.typing.terminal_classes`, shipped defaults when unset)
— terminals are where AI sessions live, and their visible content is work
product the user is actively directing. A **browser or any other anchor is
never silently read to the model**: a page can be a bank, a mailbox, a
medical record. Each thread can override by hand in `focus.toml`: `recap =
"always"` (an AI session hosted somewhere unrecognised) or `recap = "never"`.
An unrecognised value repairs to the default.

Two consents stack under that trigger. The `[context] window` opt-in gates
the whole feature — with Jarvix's eyes switched off the capture seam reports
itself unavailable and recaps stay templated, silently. And in auto mode a
non-terminal window's identity is read only far enough to learn what it is;
its content goes no further than the daemon.

The enrichment applies to the user's "switch to X" and "where am I on X"
alike, and to the scheduled check-in that replays the same phrase (ADR
0041's one-sentence-per-question invariant): a check-in is the user's own
standing question, the same gates hold, and what it reads is the window
title line the daemon already inventories for anchor liveness.

### The capture: the desktop-context seam, no new mechanism

`internal/focus` never touches the desktop or the provider. It takes two
seams, daemon-bound like the firing path: `Capture(ctx, anchor)` and
`Summarise(ctx, prompt)`. The daemon's capture reads the anchored window
from the shared compositor seam — app plus **live title**, which AI CLIs
keep updated with their state — under the ADR 0019 rules restated: the
`[context] max_chars` bound, the secret redactor before anything else, a
timeout, and honouring ctx. A richer content gatherer (should one ever
exist) slots in behind the same seam without this feature changing shape.

### The contract: three sentences, enforced tolerantly

The prompt is a pinned template (`RecapPrompt`): the capture travels between
`--- window content ---` markers declared to be content, never instructions;
the output contract is at most three short sentences, present state first,
then the apparent next step, nothing the content does not support, no lists,
no preamble. The reply is then enforced, not trusted: list markers are
stripped, whitespace collapsed, a short labelled preamble ("Summary:")
dropped, a fourth sentence truncated at the third boundary — and an empty or
run-on reply is a contract violation that falls back rather than being read
aloud mangled. On success the summary **replaces** the base recap: the
acceptance criterion caps the whole spoken recap at three sentences, and the
parked-thoughts detail is one "what did I park" away.

### The deadline: hard, with no late barge-in

Capture and summary share one context deadline (3s, inside the ≤4s target
with synthesis to spare). The switch itself has already committed before
enrichment starts — a slow model can delay the sentence, never the switch —
and a summary that misses the budget is **dropped**: the base recap speaks
behind the admission, and nothing barges in a minute later over whatever the
user moved on to. Cancellation rides the session context like any speech.

### Honesty: the record always speaks on failure

Every failure path speaks the thread's own record behind one pinned
admission — never an invented summary, never a silent downgrade:

- capture unreadable or empty: *"I couldn't read the session window just
  now, so this is from my own record."* (A window already gone is disclosed
  by the base recap's own gone-clause; an unreadable desktop stays silent,
  per ADR 0041.)
- model error, contract violation, or deadline: *"I couldn't get a fresh
  read of the session in time, so this is from my own record."*

### Transient: composed, spoken, dropped

The captured text and the summary exist in the spoken sentence and nowhere
else: not in `focus.toml`, not in conversation memory, not in any event or
log. The `focus.recap` event — and the activity ring row rendered from it —
carries the thread, the outcome, and sizes and timings only (`chars`,
`capture_ms`, `model_ms`, `total_ms`), which is also how the latency
criterion is measured. Tests pin both directions: the content markers never
appear in the store or the event, and the row still says a recap happened.

### Out of this slice

A working/needs-you/done classification of the session (the #127 overlay
wants one) is deliberately not smuggled into the prose contract. When it
lands, it is a first line the model emits and the daemon parses off before
speech — an extension of `Summarise`'s contract behind the same seam, not a
second capture or a second call.

## Consequences

- Re-entering an AI session costs seconds of listening instead of a minute
  of scrolling, and only ever costs latency when the trigger actually
  applies; every other thread recaps exactly as ADR 0041 shipped.
- What production capture can read today is the window's identity line —
  honest but thin. The fixtures and the output contract are pinned against
  richer capture, so deepening the seam later is a daemon-side change only.
- A scheduled check-in on an opted-in thread spends one bounded model call
  per firing; the do-not-nag rule and the reminder cadence bound the rate,
  and the `focus.recap` event makes each spend visible.
- The model's words now reach one focus sentence, behind three gates
  (consent, trigger, contract) and one admission-shaped fallback — the
  templated recap remains the floor the feature can never sink below.
