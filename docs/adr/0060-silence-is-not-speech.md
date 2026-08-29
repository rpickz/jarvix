# ADR 0060 — Silence is not speech: a capture that produced nothing produces nothing

**Status:** accepted

## Context

A user reported that Jarvix sometimes "randomly thinks I've said *The assistant
is called Jarvix*" when it cannot pick up input. That sentence is not a
misrecognition. It is Jarvix's own bias prompt, handed back.

Reproduced deterministically on the machine, against two seconds of digital
silence (16 kHz mono s16le) through the real model and the real prompt:

```
$ whisper-cli -m ggml-base.en.bin -f silence.wav --prompt "The assistant is called Jarvix." --no-timestamps
 The assistant is called Jarvix.

$ whisper-cli -m ggml-base.en.bin -f silence.wav --no-timestamps
 you
```

Whisper conditions its decoder on `--prompt`. With no speech to transcribe,
the likeliest continuation of the prompt is the prompt. The sentence added so
Jarvix would *hear* its name (issue #83, generalised over the configured name
in #107, extended by the taught hard-to-hear phrases in #129) is the sentence
it invents when it hears nothing. The second command shows the same failure
without a prompt — whisper's familiar `" you"`, and its siblings `"Thank
you."` and `"Thanks for watching!"` — so the hazard is **"an empty capture
produces text"**, and the prompt merely decides *which* text.

This is not cosmetic. A hallucinated transcript starts a real exchange: it
reaches the intent router and the model, counts as a user sighting and moves
the return briefing's watermark (#188), lands in the conversation archive as
something the user said, and could in principle match a phrase or provoke a
tool call. A phantom utterance is a phantom instruction.

## Decision

**An empty capture produces no exchange.** Two rules, both in the STT seam —
the one place that knows what was sent — so no caller has to remember, and
both applied identically on the cold `whisper-cli` path and the warm
`whisper-server` one.

### 1. Do not ask when the capture has no voiced audio

`internal/audio.MeasureWAV` reads the recording, computes root-mean-square
energy over 20 ms frames and reports the loudest one. Below the floor, whisper
is never invoked. This removes the whole hallucination family rather than one
instance of it, and it is the cheaper answer in every sense: one sequential
read of a file the daemon wrote seconds ago onto tmpfs, against several hundred
milliseconds of decoding.

Energy rather than a neural VAD, for the reasons `internal/wake`'s endpointer
already gives: a second model is a second thing to install, to go missing, and
to license, for a decision that only has to separate "the microphone produced a
signal" from "the microphone produced nothing".

**The floor is `SilenceFloorRMS = 8.0` raw s16 units, which is -72 dBFS.**
The peak frame, not the mean — a half-second question inside a ten-second press
has a mean indistinguishable from silence. Chosen from the shape of the
problem:

| | peak frame RMS | dBFS |
|---|---|---|
| digital silence (muted source, wrong device) | 0 | -∞ |
| ±1 LSB dither on a live but silent chain | ≈ 0.8 | -92 |
| **the floor** | **8.0** | **-72** |
| a quiet room through PipeWire at default gain | 33 – 104 | -60 to -50 |
| `internal/wake`'s `roomTone` fixture (±60 uniform) | ≈ 35 | -59 |
| `internal/wake`'s `minNoiseFloor` | 40 | -58 |

So the floor sits about 20 dB above a dead line and about 12 dB below the
quietest room anyone actually records in — and five times below the number the
wake endpointer already treats as the bottom of the world.

### Why a quiet speaker is safe

Measured, not assumed. Whisper mean-normalises its mel spectrogram, so absolute
level tells it nothing: `ggml-base.en` on this machine transcribed *"What is
the weather like today in London?"* perfectly from clips attenuated by 30, 40,
52 and 70 dB — the last of which has a peak frame RMS of **1.7** (-86 dBFS),
below the floor. Taken alone that says no absolute gate can be *proved* safe.

But those clips were synthesised and then scaled: noise-free, which no capture
from a microphone ever is. A real quiet talker arrives with their room attached
— preamp hiss, the fan, the keyboard — all of which are above this floor before
they open their mouth. What falls below 8.0 is not a quiet person; it is an
input that produced no signal at all.

The gate therefore errs towards transcribing in **every** ambiguous case: a
clip that cannot be read, cannot be parsed, or is shorter than one analysis
frame is transcribed. Missing a real question is the worse failure, because
transcribing silence has a second line of defence behind it and a missed
question has none. A stricter, relative test — speech is 15–25 dB above the
room, which is what the wake endpointer's `NoiseRatio` measures — was
deliberately *not* used here: it would catch the room-tone-only case too, and
it would be the thing that silences somebody speaking quietly across a room.

### 2. Discard a transcript that is wholly the injected prompt

`stt.IsPromptEcho(transcript, prompt)` compares the transcript against **what
the daemon actually sent**, normalising case, punctuation and surrounding
space. Never a literal: the bias set is composed per call from the configured
assistant name (which the user can change) and the taught hard-to-hear phrases
(which they can add mid-session), so a hard-coded sentence would silently stop
covering the words it was written for.

Whole prompt or any one of its sentences, because
`config.STTBiasPromptWith` composes up to two independent sentences and whisper
is as likely to echo one as both. Equality after normalisation, never
containment: *"Jarvix, what is the assistant called?"* contains the name,
shares most of the bias sentence's words, and is an ordinary thing to say. A
test pins it.

Rule 2 is not redundant given rule 1. A microphone picking up a quiet room
delivers real signal, passes the energy gate, and whisper will still echo the
prompt at it — rule 1 only covers the input that produced nothing at all. Nor
is rule 1 redundant given rule 2: without it, silence with no prompt configured
still produces `" you"`.

### 3. A capture that produced nothing is not an error

The engine ends such a turn through `nothingHeardLocked`, publishing
`session.nothing_heard {reason}` — not `error`, not `StateError`. The previous
behaviour lit the urgent chip on the bar, the red banner in the conversation
window and a "Jarvix hit a problem" notification, and held them until the next
session; a user with an unplugged microphone got one fault report per press. A
microphone that heard nothing is the system working.

It is not silent, though, and that is what makes the discard defensible:

- The **activity feed** carries a microphone row — "I didn't catch that" — with
  the measurement as its detail, which is where a user debugging a microphone
  looks.
- **`session.timings`** carries `nothing_heard`, its second non-duration key,
  which is also what keeps the report non-empty on a turn that never reached
  the model, so `jarvix status --last` says why rather than showing nothing.
- The **overlay** and the **conversation window** say "I didn't catch that", in
  wording that comes from Go through the generated `BarState.js` (ADR 0013), in
  the ordinary non-urgent styling.
- **No `transcript.final`** is published. A blank transcript is still a claim
  about what was said, and this whole ADR is about not making claims about
  captures that carried no speech.
- **No notification.** Cancellations are already silent for the same reason:
  the user is standing at the keyboard having just pressed a key.

`session.nothing_heard` is published **before** the return to idle, unlike
`session.cancelled` which follows it. A cancelled turn commits an interrupted
exchange a client can re-read; a turn that heard nothing commits nothing at
all, so its one word has to arrive while the client still has a pending turn to
put it in.

## Whisper's own decoding flags — measured, and rejected as the primary fix

Evaluated on whisper.cpp 1.9.1 with `ggml-base.en`, against the same two
seconds of digital silence and the same bias prompt:

| flag | result |
|---|---|
| `--no-speech-thold 0.6` (default) | `The assistant is called Jarvix.` |
| `--no-speech-thold 0.3` / `0.9` / `0.99` | `The assistant is called Jarvix.` — **no effect at any value** |
| `--entropy-thold 0.1` | `The assistant is called Jarvix.` |
| `--no-fallback` | `The assistant is called Jarvix.` |
| `--logprob-thold 0.0` | ` you` |
| `--no-fallback --logprob-thold 0.0 --no-speech-thold 0.1` | `The assistant is called Jarvix.` |

Two findings. `--no-speech-thold` — the flag whose name promises exactly this
fix — does nothing on this build at any value. And `--logprob-thold 0.0`, the
one flag that changed the answer, did not remove the hallucination: it swapped
one invented sentence for another. It would also be actively harmful, since
rejecting low-probability decodes is precisely how a genuinely quiet or
accented speaker gets dropped.

whisper.cpp's CLI exposes no `--suppress-blank` at all, so the option named in
the issue does not exist to try here.

These flags are model- and version-dependent by nature: a threshold tuned
against `base.en` on 1.9.1 says nothing about `large-v3-turbo` on the next
release, and Jarvix lets the user pick both. They are therefore not adopted,
not even as a supplement — a decoding flag that silently stops working is worse
than no flag, because it is a defence nobody would think to re-test. The two
rules above are engine-independent and testable without whisper.cpp installed.

## Consequences

- `stt.TranscriptEvent` gains a `Reason`, set on a final event that
  deliberately carries no text. Additive; every other transcriber is unchanged
  and reports the default reason.
- `internal/audio` gains `MeasureWAV`, `CaptureLevel` and `SilenceFloorRMS` —
  the first energy measurement outside `internal/wake`. The duplication of a
  ten-line `rms` between the two packages is accepted: the wake endpointer
  measures a live frame stream against an adapting floor, this measures a
  finished file against a fixed one, and merging them would couple a hot
  streaming path to a file parser for the sake of one loop.
- A new bus event, `session.nothing_heard`. Older clients ignore it; a client
  that ignores it sees a session that finished without an answer, which is what
  happened. Documented in `docs/ipc.md`.
- The room-tone-only case survives: a live microphone in a quiet room passes
  the energy gate, and whisper may still return `"Thank you."` — real audio,
  real decode, no prompt to compare it against. Deliberately not addressed by a
  denylist of known hallucinations, which would be a literal by another name
  and would eventually eat a real utterance.
