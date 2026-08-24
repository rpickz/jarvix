# ADR 0035 — The assistant's identity is one config table; the wake word becomes a detector-only knob

**Status:** accepted

## Context

Issue #103 asks for `[assistant] name` / `aliases` as the single source of
truth the STT bias sentence, the wake-transcript matching/stripping, and the
prompt's self-reference all derive from — "no second copy of the name
anywhere". But by the time it was implemented, the name was no longer
hard-coded at those call-sites: issue #86 had already moved it into
`activation.wake_word` and `activation.wake_aliases`, and `wake_word` had
grown a second, documented meaning — it may be a path to a self-trained
`.onnx` model, because the acoustic detector's vocabulary is whatever models
exist, not whatever names people choose. Three decisions therefore went
beyond the issue's letter and are recorded here.

## Decisions

**1. `activation.wake_word` is repurposed, not removed: a detector-only
override, empty by default.** The name and the detector's model word are
different things that happened to coincide for "jarvix". A user naming their
assistant "Hal" has no Hal acoustic model; the working setup is
`assistant.name = "Hal"` with the detector still pointed at `hey_jarvis` (or
a trained model path). So `wake_word` keeps its detector role, loses every
other one (bias, strip, prompt — those follow `[assistant]`), and its default
becomes empty, meaning "the assistant's name, lowercased"
(`Config.WakeDetectorWord`) — byte-identical to the old default. An old
config still setting `wake_word = "jarvix"` keeps working unchanged.

**2. A config still setting `activation.wake_aliases` is refused at load,
with directions.** The alternatives were silently ignoring the key (a tuned
alias list evaporates and the mishearing bug it fixed returns, undiagnosably)
or silently honouring it (a second copy of the identity, exactly what the
issue forbids). A validation error naming the new key is a one-time,
self-explaining migration; the field survives in the struct decode-only so
the refusal can see the stale key at all.

**3. The shipped alias list is coupled to the shipped name, derived at read
time rather than stored.** `Default()` stores no aliases;
`Assistant.EffectiveAliases` resolves unset aliases to the tuned
jarvis/javax/… list only while the name is (case-insensitively) the default.
Storing the list as a plain default would make `name = "Hal"` inherit another
name's mishearings — a strip that removes "jarvis" from Hal's transcripts —
and would also make "custom name with zero aliases", the state `jarvix
doctor` must warn about, undetectable. An explicitly set list (including an
explicitly empty one) always wins.

One deliberate tightening of the issue's example: an alias equal to the name
(`name = "Hal"`, alias `"hal"`) is refused. Matching is case-insensitive
everywhere, so the entry adds nothing the name does not already provide, and
its presence signals a misunderstanding of what aliases are for — the
validation message says so.

## Consequences

- One rename flows everywhere: bias sentence, strip, matcher, detector word,
  default prompt self-reference, doctor's report. A grep-guard test
  (`TestDerivedCallSitesCarryNoCopyOfTheDefaultName`) pins the literal out of
  the derived call-sites; the default-value definitions in `config.go` and
  product branding keep it.
- `assistant.name` is restart-class (the detector is construction-wired);
  `assistant.aliases` is idle-class (they live only in the engine's
  transcript strip). Both render in the window's Settings tab through the
  generic registry widgets — aliases as the string_list comma-separated
  field, parsed daemon-side.
- The strip now matches word *sequences*, so multi-word names ("Mister
  Smith") and multi-word aliases strip under the same leading-whole-word
  discipline; longest target wins so a prefix alias cannot strip half a
  summons.
