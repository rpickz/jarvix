# ADR 0063 — Instant, medium and deep: model tiers, and honest routing between them

**Status:** accepted (implements issue #159; generalises ADR 0016's advisor
bridge, applies ADR 0037's disclosure stance to a new budget, and inherits ADR
0017's strictness argument for the spoken half)

## Context

Jarvix had one conversational brain (`[ai] provider` + `model`) and, beside it,
an advisor bridge the model could choose to consult (`advisor.ask`, ADR 0016).
That is two tiers by accident rather than by design. There was no lightweight
model for a trivial turn, "deep" could only ever be a CLI, and nothing routed:
every turn — "what time is it" included — paid the main model's first-token
latency, and every hard turn depended on the model *deciding* to delegate.

The user's own framing is what this ADR implements: an **instant** model
(small, immediate), a **medium** model (today's default), and a **deep** model
(slow, strongest — local if the hardware is there, external otherwise), each
independently configurable so the mix can be all-local, all-remote, or
anything between.

The motivation behind the motivation matters more. Today the user gets *fast
but low-quality* replies, and the point of tiering is not to spend less on
trivia: it is to stop making them choose between "quick" and "good" at all.
That is why this ADR ships a **control** as well as a mechanism.

## Decision

### The vocabulary: three tiers, two names each

Three tiers, fixed: `instant`, `medium`, `deep`. A fourth would have to earn a
name a person would say out loud, and none suggests itself.

They carry two names on purpose. **In the config file** they are named for what
they select — a tier is an endpoint and a model. **On screen and out loud**
they are named for the trade the user is making: **Quick**, **Balanced**,
**Deep**. `ai.TierLabel` is the single definition of that second name; the
window reads it through the generated `BarState.js`, the doctor's rows and
everything Jarvix says compose from it, and a QML guard fails the build if the
window ever spells one of the three words itself.

### The config shape (the contract other work builds on)

```toml
[ai.tiers]
default = "medium"               # which tier a NEW conversation starts on

[ai.tiers.instant]
provider      = "lmstudio"       # a name from [ai.<name>], exactly like ai.provider
model         = "qwen3-1.7b"
history_turns = 4                # optional tighter context budget

[ai.tiers.medium]
provider = "fireworks"
model    = "accounts/fireworks/models/qwen3p8-max"

[ai.tiers.deep]
advisor = "claude"               # xor provider/model
```

A tier **points at** an endpoint; it never carries a base URL or a credential
of its own. There is one place an endpoint is described and one place a
credential is named, and "add a tier" must not come to mean "add a second copy
of the provider configuration". `"tiers"` is a reserved `[ai]` key so an
endpoint can never be called `tiers` and make `[ai.tiers.instant]` ambiguous.

**An absent tier is not a configured tier**, with exactly one exception:

- **medium** with no table of its own binds to the `[ai]` brain, because that
  is precisely what medium means — the model this configuration has always
  used. This is the whole backwards-compatibility promise: a config that adds
  only `[ai.tiers.instant]` keeps answering ordinary turns from yesterday's
  model.
- **instant** and **deep** with no table do not exist. Asking for one is
  answered by *saying so*, never by serving the same model under a stronger
  name.

**No `[ai.tiers]` table at all switches tiering off entirely**: one brain, one
code path, no extra event keys, no record keys, no control in the window.
`TestNoTiersConfiguredIsTodaysTurnExactly` compares the whole provider request
and the whole `assistant.started` payload against what they were before this
change, because "mostly the same" is not the promise that was made.

### The routing table

`ai.Decide` is a pure function of five inputs, in `internal/ai` beside the
`Provider` interface, and it is exhaustively tested with no engine anywhere
near it. It costs no model call and no network — a pre-classification round
trip to decide which model answers would spend the very latency the instant
tier exists to save.

Precedence, most specific last:

1. the configured **default** (medium when unset or unservable),
2. overridden by the conversation's **pin** (the control, or a spoken phrase),
3. overridden by this turn's **explicit ask** ("think hard about this…").

Then two corrections, in this order:

4. **A tier with no binding cannot serve.** The default takes the turn and the
   decision records what was refused, so the caller can say it out loud.
5. **A turn that may call a tool is never served by the instant tier.**

Rule 5 is the hard rule. It is applied last so that no path — a default of
instant, a pin, an explicit "quick answer" — can get round it, it is enforced
inside the table rather than at a call site, and it has its own exhaustive
test over every combination of inputs.

**It exists because this project has already lived through the failure.** In
issue #71 a model too small for the prompt it was given narrated actions it had
never performed; a small model *holding tools* is that same failure with the
safety catch off. Jarvix would say it had opened the file, moved the window,
sent the message. One second of latency is not worth a sentence like that, so
the trade is not offered.

**There is deliberately no triviality classifier.** Nothing decides that a
question "looks small" and can go to the weak model. ADR 0017 settled that
argument for the intent router — ambiguity belongs to the model, and the cost
of being liberal is Jarvix doing something the user did not ask for — and it
applies here with more force, because the cost of being wrong is a worse answer
whose cause the user cannot see. Instant is reached when somebody *chooses* it.

### The control, and its spoken equivalents

**Quick / Balanced / Deep, beside the composer**, because it is a decision
about the question about to be asked rather than a preference buried in a
settings screen. It applies from the next turn, persists for the conversation,
and a new conversation returns to the configured default — a pin is a decision
about the thread being had, exactly like a remembered approval (ADR 0053),
which is why it is cleared in the same three places those are.

**The level lives in the engine and nowhere else.** The control, the spoken
phrases, `thinking.get`/`thinking.set` and the conversation snapshot are all
views of it; `thinking.changed` is what keeps an open window right when the
change came from the microphone. The current level is stated **as text**
("Thinking: Deep") beside the buttons as well as marked on one of them —
colour alone is not a legible setting.

Levels this machine cannot serve are **shown and marked unavailable**, not
hidden: a control that silently dropped "Deep" would leave the user wondering
whether the feature exists. Pressing one is refused *in the control*, in the
daemon's own sentence — which is the point of asking before the turn rather
than discovering it during one.

The spoken half is two mechanisms, and the difference between them is the
difference between a setting and a request:

- **Pins** are whole utterances and go through the pattern table like every
  other built-in ("switch to deep", "quick answers from now on"). They are
  owned phrases, so a routine or custom intent claiming one is a config error
  naming this owner — two things able to move a setting by voice is one too
  many.
- **Escalations** are prefixes of a question that still has to be answered
  ("think hard about this, what should I do…"). The pattern table cannot
  express those and must not try: ADR 0017 made whole-utterance matching the
  router's central guarantee, and loosening it to prefixes would put every
  sentence beginning with a table phrase at risk of being claimed. So an
  escalation is a separate, additive scan that **claims nothing** — the
  utterance goes to the model exactly as it would have, one map lookup later,
  and it goes **unstripped**, because "think hard" is a legitimate instruction
  to a model as well as to the router and the archive should hold what the user
  actually said.

"Ask claude" is deliberately not in either table. It names an advisor, not a
tier; the two coincide only when the deep tier happens to be that advisor, and
the model already has `advisor.ask` for exactly that request.

### Honesty: three things earn a sentence, and nothing else does

- **A tier that was asked for and is not configured.** "I have no deep model
  configured, so this is the balanced one's answer."
- **A tier that was tried and could not be reached** — no key, nothing
  listening, an advisor that is not installed. "I couldn't reach the deep
  model, so this is the balanced one's answer." From where the user sits these
  are one disappointment with two causes, and both have to be *said*.
- **A deliberate trip to the deep tier.** One spoken cue before the answer —
  once, not a countdown. ADR 0016 settled the same question for advisor
  consultations: the point is that the wait was chosen.

Not on the list: the tool rule turning instant into medium. That fires on a
large share of ordinary turns, and a speed control that apologised every time
the user asked a question needing a tool would be noise within an afternoon. It
is recorded, and the pending turn names the tier that actually answered, which
is the disclosure that matters.

Each sentence is spoken **before** the answer streams and prepended to the
published text, so the window, the overlay and the archive show the sentence
the user heard. It is **transient** — not committed to conversation history —
on the return briefing's terms (ADR 0050): it describes how one turn was
served, and carrying "I couldn't reach the deep model" into the model's own
context as something it said would be noise the next answer has to reason
around.

### Failover: once, at the start, or not at all

A tier that produces *nothing at all* — `Chat` refuses before streaming, or the
stream errors before a single token or tool call — may be failed over to the
fallback tier, exactly once, and only on the first round. Everything else is an
ordinary failure reported as it always was.

The two refusals are deliberate. A stream that broke half-way has already put
words on the screen, and finishing them from a different model is a splice, not
a failover. And once a turn has run tools, its history belongs to the model
that asked for them. A chain of downgrades is refused for a third reason: it is
how a user ends up several models away from what they asked for, having been
told once.

The failover target is never instant. A failover happens on a turn already in
flight, whose tools were decided before it started, and rule 5 has no exception
at any point in a turn.

### The record: the tier that answered, never the tier that was asked for

Every turn a tier decided carries `tier`, `tier_model`, `tier_reason` and —
when something was refused — `tier_wanted` in `session.timings`, which is what
`jarvix status --last` prints and what the activity feed's Timings row leads
with. `tier_model` is the model name for an endpoint tier and `advisor <name>`
for an advisor one. A turn from a configuration with no tiers carries none of
them: a key that said "medium" on every turn would claim a routing decision
nobody made, and the key's *presence* is the statement that routing happened.

`tier_reason` distinguishes `unavailable` (not configured) from `unreachable`
(configured, tried, silent), because they have different fixes.

The pending turn (#158) shows the serving tier for the whole of the wait —
"Thinking · 6s · Deep" — so the speed/quality trade is visible while it is
being paid for, not only afterwards.

### Shared conversation, one tighter budget, disclosed

Every tier gets the same conversation, the same memory, the same taught
vocabulary, the same desktop context and the same feed values. A tier may carry
a smaller `history_turns`, and that budget takes from **conversation history
only**: the standing knowledge is what makes an answer Jarvix's rather than a
stranger's, it is already individually budgeted, and dropping it to save tokens
is how a fast tier becomes a tier that does not know the user's name.

When the budget actually removes something, the prompt says so *inside itself*
and the record carries `tier_context_dropped`. That is ADR 0037's stance
applied to a new budget: a trimmed prompt that does not admit it produces an
answer whose confidence outruns its material.

### An advisor as a tier

`tools.AdvisorProvider` presents one configured advisor as an `ai.Provider`, so
a tier is an endpoint or a CLI with no difference at any call site. Delegation
already existed as a *tool the model chooses*; a tier is the same bridge
reached from the other end — the user, or the router, deciding that this turn
is answered by the strong assistant. Both go through one `Advisor.Consult`, so
there is exactly one copy of the no-shell, own-process-group,
scrubbed-environment discipline of ADR 0016.

An advisor-backed tier is offered **no tools**, and its prompt says plainly
that it cannot act on this computer. It could not call one anyway, and a tier
holding tools it cannot use is the #71 shape a third time.

### The model's own escalation

`thinking.ask_deep` is ADR 0016's argument generalised: the routing table
cannot tell "what time is it" from "plan my week around these constraints", and
the one party that can is the model already holding the question. It is
registered only when a deep tier exists — a tool advertising a deeper answer
this machine cannot give would invite the model to promise one — and it runs
silently by default for `advisor.ask`'s exact reason: it reads and replies and
nothing else, which is no more authority than the model turn Jarvix was already
making.

It answers; it does not act. Exactly one model in a turn holds the machine's
capabilities, and it is the one whose tool round is open.

### Doctor probes every tier for real

One row per configured tier, by name and by endpoint, with #113/#114's
discipline: an endpoint tier is probed with the same `GET /models` request and
the same ten-second budget the provider check uses, and the tri-state answer is
kept — reachable, unauthorised (the address is right, the key is not),
unreachable. **A tier that cannot answer fails there, not mid-conversation.**

An advisor tier is checked with `exec.LookPath` and nothing else, which is
`advisorChecks`' existing exception and its existing argument: invoking an
assistant CLI to see whether it works would spend the user's own budget every
time they ran `jarvix doctor`.

Nothing is printed at all when no tiers are configured. `jarvix doctor`'s value
is that every line is worth reading.

## Consequences

- A user with a big GPU can run instant and medium locally and deep
  externally; a user with none can point all three at APIs; a user offline
  keeps whatever is local and is told plainly what is unreachable.
- **Which model answered is now a fact on the record.** It was not before,
  including for the single-brain case — but the keys are absent there
  deliberately, so nothing about an untouched configuration changed.
- The instant tier's latency is measured by the instrument that already exists:
  `transcript_to_first_delta_ms`, now qualified by the tier beside it.
- **This change picks no models.** It makes tiers possible; the shipped state
  is one brain, exactly as before. (Two facts worth recording for whoever does
  choose: `kimi-k3` leaks its reasoning scratchpad into `content`, which a
  speech engine would then read aloud, so it must not become a default
  anywhere; and `deepseek-v4-pro` 404s on chat completions.)
- Three surfaces now show a level that lives in one place. The cost is a
  `thinking.changed` event and a snapshot key; the alternative — each surface
  keeping its own idea of the setting — is the drift this project has avoided
  everywhere else.
- The escalation scan runs on every transcript the intent router did not claim.
  It is a bounded loop over a ten-entry literal table on an already-normalised
  word slice, in the same order of magnitude as the router's own miss path.

## Alternatives considered

- **Route on a classifier's opinion of the question.** Ruled out on ADR 0017's
  argument, and on cost asymmetry: a false "this is trivial" is a worse answer
  the user cannot diagnose.
- **Ask a small model which tier to use.** A pre-classification round trip
  spends exactly the latency the instant tier exists to save, and adds a second
  probabilistic component to a decision that has to be predictable.
- **Fall an absent instant or deep back to the `[ai]` brain**, the way medium
  does. It would make "deep" quietly mean "medium" and the control would move
  without anything changing — the silent downgrade this whole ADR is written
  against.
- **Speculative parallel execution** (answer on two tiers, keep the better).
  Out of scope by the issue's own words, and the honesty and cost implications
  deserve their own decision.
- **Per-tier pricing and cost accounting.** No billing model exists here.
