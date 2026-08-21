# ADR 0017 — The deterministic intent router: a grammar table in front of the model

**Status:** accepted (implements roadmap Phase 3)

## Context

Every utterance takes the full STT → LLM → TTS round trip. "Mute" therefore
costs a model call and a couple of seconds — for a decision with exactly one
possible outcome. The cost is not only latency and tokens: a language model
is a *probabilistic* component, and "volume thirty" occasionally comes back
as prose about volume instead of a changed volume.

Roadmap Phase 3 specified the fix, and specified its shape emphatically: an
explicit grammar/pattern table, **not** a machine-learning system. This
matters twice over once wake-word activation lands (Phase 6) — "Jarvix, stop
talking" must be reflexive, not reasoned.

## Decision

**A compiled pattern table between the final transcript and `think()`.**
`internal/intent` owns the grammar; the seam is `maybeThinkLocked` in
`internal/session/engine.go`, the exact point where a transcript would
otherwise become a provider request. A hit executes a local action and
finishes the session with no provider call at all; a miss falls through to
`think()` unchanged, one map lookup later.

**Matching is strict and whole-utterance.** Every pattern is a literal word
sequence with at most one bounded integer slot, and it must consume the
entire normalized transcript. "Turn it up" is an intent; "turn it up a bit"
is not, and goes to the model. There is no prefix matching, no stemming, no
synonym folding, no edit distance, and no threshold to tune. The design rule
is: **ambiguity always belongs to the AI.** The cost of being conservative is
one model call; the cost of being liberal is Jarvix doing something the user
did not ask for.

Normalization is the one liberty taken — case, punctuation, and hyphens are
folded — because STT emits the same spoken phrase several ways. Number words
and digits are both parsed ("volume thirty" ≡ "volume 30") over 0–150, since
which one Whisper writes is not something the user controls.

**Built-ins map to a fixed argv; the transcript contributes only a validated
integer.** `volume.set` runs
`wpctl set-volume -l 1.5 @DEFAULT_AUDIO_SINK@ <n>%` where `<n>` came from
`strconv.Itoa` of a parsed int in 0–150. No shell, no word splitting, no
interpolation of spoken text into a command line — there is no path by which
a transcript becomes an argument. An out-of-range slot is a **miss, not a
clamp**: "volume five hundred" is a sentence for the model, not a silently
truncated command.

**User-defined intents are gated.** `[[intents.custom]]` entries run a real
shell command, so they pass the ADR 0014 permission gate — the same
classifier, the same spoken confirmation, the same audit events — under
their own tool identity `intent.run`. A separate identity (rather than
borrowing `shell.run`'s) means a user can allow their own hand-written
intents without also unleashing the model, and disabling `shell.run` does not
silently break phrases they configured themselves. Deny rules win either way.
Slots are rejected in user patterns outright: a slot would have to be
substituted into a shell command, which is the one thing this design refuses.

**A matched intent is a state, not a shortcut.** The state machine gains
`Acting` (Idle/Transcribing → Acting → Speaking/Idle). It exists so that a
matched intent provably never passes through `Thinking` or `Responding` —
those states mean "a provider request is open", and here none ever is. The
transitions are exhaustively tested in both directions, including the
illegal ones (`Acting → Thinking` is not a legal transition, which is the
guarantee stated as code).

**Two intents act on Jarvix rather than the desktop**, and neither is a
command:

- `speech.stop` ("stop", "stop talking") goes through the existing
  `CancelSpeech` path and acknowledges with **silence** — speaking after
  "stop talking" would be its own joke.
- `conversation.new` performs the same reset as `jarvix new`.

**A hit is still a conversation turn.** The utterance and its acknowledgement
are committed to history exactly as an AI exchange would be, so the follow-up
that *does* reach the model ("a bit louder") knows what just happened. The
two exceptions are the two where recording is actively wrong:
`conversation.new`, whose whole purpose is an empty history, and
`speech.stop`, which has no reply to pair the utterance with.

## Consequences

- **Common commands are effectively free.** Measured (`make bench`): a miss
  costs ~230ns and 2 allocations (the normalized words), a near-miss that
  reaches the pattern list ~350ns, a hit ~320ns; transcript-final to
  acknowledgement is tens of microseconds in-process, against budgets of 1ms and 300ms
  respectively. Zero provider calls on a hit is asserted with the fake
  provider. The miss path is not allocation-free as the ticket hoped —
  normalizing an utterance costs one slice and one buffer — and at ~4 orders
  of magnitude inside the budget, pooling them would be complexity bought
  with nothing.
- **The table is a code change, not a config threshold.** Adding a phrase
  means adding a string and a test. That is deliberate: a table anyone can
  read is the only kind whose behaviour can be reasoned about.
- **A command can fail** (no `wpctl`). Jarvix speaks one line and returns to
  Idle — never a stuck session — and `jarvix doctor` reports the missing
  binaries before the user finds them by saying "mute".
- **The router is the natural home for future voice control of Jarvix
  itself** ("say that again", "open settings"), and for wake-word reflexes.
- **The confirmation mechanism is now shared.** `awaitConfirmation` serves
  both the model's tool rounds and user-defined intents, with the resume
  state as a parameter. There is deliberately no second permission path.
- **The fixed-argv guarantee is fuzzed, not just asserted.** `FuzzRouterMatch`
  throws arbitrary text at the router — which is what an STT engine does —
  and fails if any argv element is ever something other than a table constant
  or the bounded slot integer.

## Alternatives considered

- **Embedding/classifier matching.** Ruled out by Phase 3 explicitly, and on
  merit: it reintroduces the probabilistic component the router exists to
  remove, and a false positive means executing something the user did not
  say.
- **Fuzzy/prefix matching for robustness.** Every near-miss it would rescue
  is an utterance the model handles correctly anyway. The asymmetry of costs
  makes strictness the only defensible default.
- **Slot-filling dialogues** ("which workspace?"). Out of scope: an intent
  that needs a follow-up question is a conversation, and conversations are
  what the model is for.
- **Running built-ins through the permission gate too.** They are a fixed
  argv table shipped in the binary with no user or model input beyond a
  bounded integer; there is nothing for a gate to decide, and asking to
  confirm "mute" would destroy the feature.
