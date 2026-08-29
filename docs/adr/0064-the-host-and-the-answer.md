# ADR 0064 — The light model hosts the conversation while the heavy model answers

**Status:** accepted (implements issue #161; builds directly on ADR 0063's tiers,
inherits ADR 0018's warm-engine discipline, the one-playback-stream doctrine of
issues #52/#53, and the supersession floor of #120/#133; written against the
incident of issue #71)

## Context

ADR 0063 gave Jarvix three model tiers and a control to choose between them. It
did not resolve the thing that made tiering worth wanting. The user's diagnosis
of Jarvix as it stands is *"reply quality is quite low, but very quick"*, and the
obvious fix — a bigger model — trades one problem for a worse one. **Dead air in
a voice interface is worse than a mediocre answer.** There is no spinner, no
cursor, nothing at all; four seconds of silence is indistinguishable from a
crash, and the user asks again.

Tiering as ADR 0063 shipped it therefore offers a choice between two bad turns:
quick and shallow, or good and silent. The user's own proposal is to stop
offering the choice, by inverting what the small model is *for*:

> the lightest model is not a cheap answerer — it is the **handler for the
> user**: acknowledging, setting expectations, chatting, asking clarifying
> questions — while a heavier model produces the real answer.

Speed and quality stop competing because a different model owns each. **The host
owns presence. The answering tier owns correctness.**

## Decision

### The instant tier is the host

`[ai.tiers.instant]` keeps its meaning as a tier a user can select (Quick still
answers from it), and gains a second job: on any turn served by a *heavier* tier,
it is the voice that keeps the user company while that tier works.

The cascade, each stage reached only when the one before cannot serve:

1. **Procedural** — the intent router (ADR 0017). Deterministic, instant, no
   model. Nothing below runs if a phrase matches.
2. **Host** — the instant tier. Acknowledge, set the expectation, ask for a
   detail that is genuinely missing. It never answers.
3. **Answer** — the routed tier, started **concurrently with** the host.
4. **Deep** — on explicit ask or model escalation, exactly as ADR 0063 has it.

### The answer goes first, and both run

`think()` issues the answering tier's request on its own goroutine. The host's
goroutine **waits for that to have happened** (`sess.answerIssued`) before it
opens a request of its own. This is a gate rather than two statements in an
order, because the guarantee has to survive scheduling: the host may cost the
answer nothing, and that is the condition on which the whole feature is allowed
to exist.

Both then run at once. The host's request is in flight *during* the wait, so when
the grace expires the line already exists. The alternative — arm a timer, and
call the host only if it fires — puts the small model's own latency on top of the
grace, and a holding line that lands a second late covers a silence that has
already done its damage.

The cost is one small-model call on every slow turn. That is the price of the
feature, it is paid to the tier explicitly configured as the cheap one, and it is
not paid at all on a fast turn.

### The grace, and where it is measured from

**Default 700ms**, `[ai.tiers] host_grace_ms`, `0` switches the host off.

700ms is roughly where a person stops reading a pause as "it heard me" and starts
reading it as "did it hear me?". Shorter and the host talks over turns that were
about to answer anyway; longer and the silence has already become the user's
problem.

It is measured **from the same instant `transcript_to_first_delta_ms` is measured
from** — `timings.modelStart()`, the context-gathering mark or the transcript
mark. Deliberately, so the number a user sets the grace against is the number
`jarvix status --last` already prints for them, rather than a second,
differently-anchored measurement of the same wait.

**If the answering tier begins streaming inside the grace, the host says nothing
at all.** Not a shorter line, not a quieter one: nothing. A fast turn with
chatter on it is worse than a fast turn, and it is the failure mode that would
make people switch this off.

### The honesty guard: an allowlist, because a blocklist cannot be made reliable

This is the highest-risk feature in the programme for the honesty rules. A small
model speaking first — before anything has been checked, before any tool has run
— is precisely the shape that produced issue #71, where a model too small for
what it was holding narrated actions it had never performed.

A pinned system prompt tells the host what it may say. It is not the enforcement.
A prompt is a request, and the host is by definition the weakest model in the
house: the one least likely to honour one.

**The enforcement is a refusal check, and it is an allowlist.** The choice
matters more than any of its details. Asking "does this line assert something?"
cannot be answered reliably — there is no finite list of ways to state a fact,
and every one that is missed is spoken aloud in Jarvix's voice as though it were
the answer. So the guard asks a question that *can* be answered: **is this line
one of the two shapes a host line is allowed to have?**

- A **holding line**: one clause of at most ten words, made only of letters,
  spaces and apostrophes, beginning with one of a closed set of openers about
  waiting and thinking. No digits (a number is a fact). No comma, colon or dash
  (a second clause is where the answer hides behind a permitted opener). No verb
  of action — the host holds no tools, so "checking that" would be a claim about
  work happening nowhere.
- A **clarifying question**: four to twenty words, opened by one of a closed set
  of question words, put to the user. Negated auxiliaries are excluded by name:
  *"isn't the deploy script the one that runs on merge?"* is an assertion wearing
  a question mark.

Over both, a phrase blocklist catches what shape cannot: claimed actions
("I've", "I checked", "done"), guesses ("probably", "I think", "it looks like"),
and negations. The host's token budget is set just above one sentence, so a model
that starts writing an essay is cut off mid-sentence and then refused for having
no terminator — the cap is part of the guard, not only a latency measure.

**A refused line is discarded, never spoken and never published.** It is logged
with its text, because "the host is saying things it should not" is something an
operator must be able to see, and it is recorded as `host_outcome = "refused"`,
because a silent discard makes a working guard look like a broken feature. It is
deliberately *not* put in an event: an event carrying it is a client displaying
it, and a line this guard refused must not reach the user by any route.

**The asymmetry is what makes this defensible, and it is stated here so nobody
loosens it by accident.** A false refusal costs nothing — the turn degrades to
what it has always been, silence then the answer. A false acceptance costs a
sentence in Jarvix's voice stating something nobody checked. The guard is tuned
hard towards refusal and will refuse plenty of lines a person would have been
happy to hear. That is the trade, taken deliberately, in that direction.

There is **no fallback line**. A refused host does not get replaced by a canned
"one moment": the turn simply goes quiet, which is the behaviour of every Jarvix
before this one.

### The host holds no tools at all

Not "tools it is unlikely to use" — none. `hostRequest` carries no tool
definitions and **takes no parameter through which any could be passed**, so the
rule is structural rather than remembered. A host that produces a tool call
anyway (a misconfigured endpoint, a model hallucinating a capability) has its
whole line thrown away rather than reasoned about.

The host's prompt is its instruction and the user's question, and nothing else:
no history, no remembered facts, no taught vocabulary, no desktop capture, no
feed values. Three reasons, and they agree. **Latency** — first-token time scales
with the prompt. **Honesty** — a model that cannot see the screen or the
knowledge base cannot state anything from them; the guard refuses assertions, and
this makes most of them unavailable in the first place. And **it does not need
them**: acknowledging a question and asking which of two things it meant are both
answerable from the question alone.

### The handoff: one voice, one stream, nothing rebuilt

The holding line goes through the turn's one `streamingSpeaker`, like every other
sentence Jarvix says. There is no second playback path, and there was never going
to be one — issues #52 and #53 spent themselves removing the last.

Everything the handoff needs already existed:

- **The answer arrives mid-sentence.** The sentence finishes. `superseded` is
  checked at *dequeue*, never at the device, so a begun utterance is never cut,
  and the answer's sentences queue behind it on the same stream. Inherited.
- **The answer beats the dequeue.** The holding line is stamped for `holdTurn`
  (0), one below the first turn any answer sentence can carry, so the
  supersession floor of #120/#133 drops it unplayed the moment the answer commits
  a sentence. One field on `utterance`; the mechanism is untouched.

That second point is the whole of the integration, and it is right rather than
convenient: a line that says "let me think about that" *is* the older message
once the thinking has produced words. The floor already means "the oldest turn
still allowed to play", and the host's line honestly belongs to the turn before
the answer's.

The remaining race — the answer beginning in the instant between the host's last
check and its enqueue — is deliberately **not** closed with a lock. It is closed
at the queue, by supersession, which drops the line rather than trying to win a
race at the microphone.

### A clarifying question takes the turn

If the accepted line is a question, the host **abandons the answer attempt** and
the question becomes this turn's reply. It is spoken and recorded exactly like
any other reply — the first-delta mark, the move to `Responding`, the deltas, the
sentence to the voice — because it *is* one, and every surface that watches a
turn must see it as one.

That is also what makes the continuity work (#125): the exchange commits as
question-and-answer into ordinary conversation history, so the user's reply
arrives as a plain follow-up with the original question behind it. **A
clarification never strands the question that prompted it.**

The one exclusion is arbitrated under a lock (`sess.hostMu`), and it is the one
place in this design that needs one: a clarification cancelling an answer that
has already put a word on the screen is exactly as wrong as an answer streaming
underneath a question the host has committed to asking. `beginAnswer` and
`claimHost` are the two claims, and they are mutually exclusive in both orders.

### Standing down: every "no" is silent and costs nothing

The host does not run when tiering is off, when there is no instant tier, when
there is no voice on the turn (speech off, or a quiet scheduled session), when
the turn already owes the user a tier notice (being told twice is worse than
once), when the grace is zero, or — the one that is a judgement rather than a
mechanism — **when the user chose Quick**.

Quick standing the host down is the point: its whole purpose is covering a wait
that Quick has chosen not to have. It applies both to the turn instant actually
serves and to the turn where ADR 0063's never-instant-with-tools rule bumped it
to medium, because the choice was still made.

An **unreachable** host is the same silence, arrived at differently: the line
never comes, nothing is said, nothing is recorded, no error reaches the user, and
the answering tier's turn is untouched. A cold host is skipped rather than waited
for, because the host is never waited *for* — it is only ever raced against.

### The record names both, separately

`host_tier`, `host_model` and `host_outcome` sit beside ADR 0063's tier keys and
answer the same question from the other end: those name the model that produced
the *answer*, these name the model that produced the *holding line*.

`host_outcome` is `held`, `clarified` or `refused`. It is absent from every turn
on which the host produced nothing — a key on every fast turn saying "unused"
would be noise on exactly the turns this feature is proudest of, and its absence
is the statement that the answer arrived inside the grace. Read `held` beside
`superseded_sentences`, which says whether the answer overtook the line before it
was heard.

On a clarification the turn's `tier` keys name the **host**, with
`tier_reason = "host"`. The tier that was routed to was abandoned before it said
a word, and a record naming it would claim an answer that never happened.

An `assistant.host` event carries the line, its kind and the tier that produced
it, and the activity feed renders it as a row labelled with that tier rather than
with "Jarvix" — so a reader scrolling the feed can see that the sentence they
heard first came from a different model than the answer after it.

## Consequences

- **A slow turn is no longer silent**, and a fast turn is unchanged. The user
  never sits in dead air wondering whether the machine heard them, and never
  hears a placeholder that guessed.
- **The shipped state is unchanged.** No `[ai.tiers.instant]` means no host; no
  `[ai.tiers]` at all means no host, no event, and no record keys — ADR 0063's
  byte-identity promise re-checked now that a second model can speak.
- **The guard will be too strict, on purpose.** Expect a working host to have
  lines refused: an unusual phrasing, a comma, a number. The record says so, the
  turn is unharmed, and loosening it is a decision to be taken deliberately with
  #71 in front of you — not a bug to be fixed by widening a list.
- **The host is a second provider call on slow turns.** It goes to the tier
  explicitly configured as the cheap one, it is capped at a sentence, and it can
  never delay the answer. A cold host defeats itself: ADR 0018's warm-engine
  discipline is a prerequisite, not a nicety.
- `streamOnce` now takes a round context. Every round but the first passes the
  session's, so nothing else changed; the first passes the host's, which is what
  makes an abandoned answer distinguishable from a failed one.

## Alternatives considered

- **Call the host only after the grace expires.** Cheaper — nothing is spent on
  a fast turn — but the holding line then arrives at the grace *plus* the small
  model's latency, covering a silence that has already ended. The point of the
  line is that it is immediate.
- **Speculative parallel answers from two tiers, keeping the better.** Out of
  scope by the issue's own words, and the honesty question ("which one did you
  hear?") deserves its own decision.
- **Let the host answer trivial questions.** This is the whole thing the ADR is
  against, and ADR 0063 already refused the classifier that would decide which
  questions are trivial.
- **A canned fallback line when the guard refuses.** It would mean chatter on
  exactly the turns where the host is misbehaving, and it would hide the refusal
  behind something that sounded fine.
- **Cut the holding line short when the answer arrives.** Rejected on #120's
  existing argument: cutting pays at the device, buys at most one sentence of
  latency, and a word chopped in half is audibly broken every time it happens.
- **A blocklist of things the host may not say.** The version of this feature
  that would have shipped a lie. See the guard, above.
