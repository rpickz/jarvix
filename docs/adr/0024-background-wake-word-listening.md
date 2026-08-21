# ADR 0024 — Background wake-word listening: engine choice and audio-data lifecycle

**Status:** accepted

## Context

Every Jarvix interaction so far needs a keyboard: hold the chord, speak,
release ([ADR 0008](0008-daemon-side-push-to-talk.md)). That is right at a
desk and it caps the assistant model — you cannot summon Jarvix while talking
to someone, while cooking, or from across the room. The roadmap's Phase 6
names the missing half: "Computer." — always-on summoning.

Building it means asking the user to leave a microphone open. That is a
categorically different request from anything Jarvix has made before, and the
trust story is as much the product as the feature. Two decisions have to be
recorded, and they are of different kinds:

1. **Which detection engine**, which is a licensing and accuracy trade-off.
2. **What happens to the audio**, which is a design commitment with a
   mechanism behind every clause.

## Decision 1 — the detector is an external process behind an interface

Detection runs in a helper process speaking a two-verb protocol over stdio
(`internal/wake/detector.go`): Jarvix writes 80 ms frames of 16 kHz mono PCM,
the helper answers `SCORE <0..1>` per frame. Thresholding, the
consecutive-frame rule, and the refractory period stay in Go.

The Go side talks to a `wake.Detector` interface, so the engine is a
substitution rather than a dependency, and every test in the repository drives
a fake.

### Why a process and not a library

The same reason every other Jarvix engine is a process
([ADR 0002](0002-external-engine-processes.md)). A wake-word model means an
ONNX or TensorFlow-Lite runtime, which means cgo, a C++ toolchain in the
build, and a segfault in third-party native code taking jarvixd down — and
with it push-to-talk, the conversation, and the daemon's own supervision. A
pipe costs one round trip per 80 ms frame (measured overhead below: ~5 µs of
Go per frame) and buys complete isolation.

### Why openWakeWord rather than Porcupine

| | openWakeWord | Porcupine (Picovoice) |
|---|---|---|
| Licence | Apache 2.0, models included | Proprietary SDK; free tier for personal use, commercial licence otherwise |
| Activation | none | requires an AccessKey, validated against Picovoice's servers |
| Accuracy | good; the published benchmarks are the project's own | generally reported as better, particularly in noise |
| Custom words | trainable locally, free | trained through Picovoice Console, gated by plan |
| "Jarvix" | no such model; `hey jarvis` is the nearest bundled one | `jarvis` is a built-in keyword |

Porcupine is the better detector and the wrong dependency. An assistant whose
headline claim is that it listens locally cannot have a wake word that stops
working when a licence key expires or when the vendor's activation endpoint is
unreachable, and it cannot ask a user to accept a proprietary binary as the
component that decides when the microphone matters. openWakeWord is Apache
2.0, runs on ONNX Runtime on the CPU, and its models ship with it.

`scripts/setup-wake.sh` installs openWakeWord and `wake/wake_detect.py`, which
is the reference implementation of the protocol above.
`activation.wake_command` points at anything else that speaks it — a Porcupine
wrapper included, if that is a trade a particular user wants to make.

### What we could not measure, stated plainly

**Jarvix has not measured a false-activation rate for any model.** The
acceptance criterion asks for ≤1 per hour at default sensitivity; that number
is a property of a model, a microphone, and a room, and measuring it honestly
needs recorded ambient audio from real rooms, which cannot be shipped in a Go
test.

What is measured, and what `TestActivationPolicyHoldsTheFalseActivationBudget`
pins down, is the half Jarvix owns: given a score stream from an imperfect
model, how many activations does the gating produce? Over 8 simulated hours of
scores in which **1 frame in 500 crosses the threshold** — 90 spurious spikes
an hour, far worse than any shipped model — the shipped policy produces
**0.12 activations per hour**, and a companion test proves an ungated
threshold on the same corpus does not (it is two orders of magnitude worse).
The consecutive-frame rule costs 160 ms, which is shorter than any way of
saying a wake word.

To measure the real number on a real machine:

```bash
jarvix status          # reports `activations` since the daemon started
systemctl --user show -p ActiveEnterTimestamp jarvixd
```

Leave it running for a working day without saying the wake word, then divide.
`activation.wake_sensitivity` is the knob; every activation is also a log line
carrying a timestamp and a confidence.

Two further limits worth stating:

- **`hey jarvis` is not `Jarvix`.** openWakeWord ships no model for the
  product's own name, and training one is out of scope for this change. The
  helper loads the nearest bundled model and reports *what it actually
  loaded*, which is what `jarvix status` and `jarvix doctor` print — so the
  gap is visible rather than papered over. Users who want the real word can
  train a model and point `activation.wake_word` at the file.
- **No echo cancellation.** While Jarvix is speaking, its own voice reaches
  the microphone, so a spoken "Jarvix" in an answer can self-trigger. This is
  accepted rather than solved: the wake word must work *while* Jarvix is
  talking (that is the interruption criterion), and the fix is PipeWire's
  `module-echo-cancel`, which belongs to the user's audio setup rather than to
  Jarvix.

## Decision 2 — the audio-data lifecycle

This is the part that makes the feature acceptable, so each clause below names
the mechanism that keeps it rather than the intention behind it.

```text
microphone ──► pw-record ──► 80 ms frame ──► ring (≤3 s, RAM) ──► detector
                (child)          │                                    │
                                 │                              score 0..1
                                 │                                    │
                                 └── on a wake word ──► utterance ──► WAV (tmpfs)
                                                            │             │
                                                    endpoint (silence)    │
                                                                          ▼
                                                              whisper ──► deleted
```

1. **Detection is local.** Frames go to a process on this machine. There is no
   network path in `internal/wake`.
2. **Pre-wake audio exists only in a fixed-size RAM ring**, allocated once in
   `NewRing` and never grown. The window is `activation.wake_ring_ms`,
   defaulting to **1200 ms** and hard-capped at **3000 ms** in two independent
   places: configuration validation rejects a larger value, and `NewRing`
   clamps it regardless — a privacy ceiling that depends on validation having
   run is not a ceiling.
3. **The default is deliberately well under the cap.** The pre-roll is the
   only ambient audio that can ever reach a transcript, so it is sized to what
   recognising a wake word needs, not to what the budget permits.
4. **Nothing before a wake word is ever written to disk, logged, or
   published.** A wake event carries a timestamp and a confidence. The
   `wake.detected` IPC event carries a confidence and nothing else, and a test
   asserts it carries no text field.
5. **Only post-wake audio is materialised**, by `audio.SaveClip`, into the
   tmpfs runtime directory at mode 0600, and the session engine deletes it as
   soon as transcription finishes — exactly what a push-to-talk capture does.
6. **Every buffer is wiped, not rewound.** `Ring.Reset` and the utterance
   buffer zero their samples. This happens when an utterance is taken, when a
   capture ends, when the listener is muted, and at shutdown.
7. **`jarvix mute` kills the capture process.** It is not a flag that makes
   Jarvix ignore what it hears: `Listener.Mute` closes the stream with
   `SIGKILL` to the process group and returns only once it has been reaped.
   The claim is therefore checkable in the process table rather than in a
   comment — `jarvix status` prints the pid, and `ps` either finds it or does
   not.
8. **A false activation costs nothing.** If nobody speaks within
   `Lead` (2.5 s) of a wake word, the capture is abandoned without
   transcription: no file, no provider call, no conversation entry.

### The indicator

Whenever a capture process is running, the Omarchy bar widget shows a hollow
microphone; muted, a struck-through one; and the panel behind it offers
`jarvix mute` / `jarvix unmute` as one click. This is
[ADR 0020](0020-bar-widget-not-tray-icon.md) being used for what it was built
for — issue #31 says #4 should consume the widget rather than invent its own
indicator, and this does: the vocabulary is two more rows in
`internal/desktop/barstatus.go`, generated into `BarState.js` like the rest.

The wake state is a **second dimension** alongside the session state rather
than another session state. A session state describes a turn; this describes
the microphone between turns, which is exactly when nothing else on screen
would say anything.

## Decision 3 — the listener is a supervised component, twice over

The wake listener owns two `warm.Supervisor` children
([ADR 0018](0018-supervised-persistent-engine-workers.md)):

- **`wake-capture`** — the `pw-record` reading the default source. When a
  headset is plugged in or unplugged the default source changes and pw-record
  exits; the listener reopens against whatever the default has become. That is
  the whole device-change story: no daemon restart, one line in the log.
- **`wake-detector`** — the model helper, with a **200 MB resident cap** so
  the memory NFR is a mechanism rather than a hope. It is re-acquired from its
  supervisor every ~30 s of audio, which is what runs the cap check without
  paying a `/proc` read twelve times a second.

Neither death touches push-to-talk, and a detector that is missing at startup
is reported once with the command that fixes it — the listener is simply not
started, because a supervisor quietly retrying a helper that was never
installed is a crash-loop with better manners.

## Decision 4 — activation is a session, started on the wake word

`Engine.StartWake` / `FinishWake` sit beside `StartVoice` / `StopVoice` and
reach the same code: interruption, transcription, the intent router, the
permission gate, history. A wake-word session must not be a second pipeline
with its own bugs.

The split between the two calls is the latency story. `StartWake` runs the
instant the detector fires — *before* the request has been captured — so
saying "Jarvix, stop" while Jarvix is talking stops it there and then rather
than a sentence and an 800 ms endpoint later. `FinishWake` arrives when the
endpointer finds silence, and submits in the same lock: with no key to
release, the silence *is* the submission.

Two consequences worth naming:

- The wake word is **inside** the transcript, because the pre-roll
  deliberately contains it. It is stripped before anything reads it
  (`stripWakeWord`), narrowly — only a leading occurrence, only as a whole
  word. Without that, "Jarvix, volume thirty" would never match the intent
  router, which matches strictly against the whole utterance
  ([ADR 0017](0017-deterministic-intent-router.md)).
- A wake word heard while a chord is held is **ignored**. Two captures of one
  utterance is not something to arbitrate, and the deliberate gesture wins.

## Measured

On the development machine (AMD Ryzen AI Max+ 395), with the detector
substituted so the number is Jarvix's own overhead:

| | |
|---|---|
| Per-frame pipeline (decode, ring, gate) | **~5 µs**, 0 allocations |
| As a fraction of one core | **~0.006%** (budget: 5%) |
| Endpointer, per frame | ~700 ns, 0 allocations |
| Wake word → session started | one frame boundary, ≤80 ms (budget: 500 ms) |

The detector's own inference cost is the installed model's and is not measured
here; openWakeWord's published figures put a single model at a few percent of
one core, which is what the 5% budget was written against. `jarvix status`
reports the detector's resident size so the memory half is observable on the
machine that matters.

## Consequences

- Background listening is **off by default** and stays off after an upgrade. A
  microphone that opens itself is a decision to be made deliberately, not one
  to inherit from a default.
- All `[activation]` wake settings are **restart class**, like the chord: the
  listener and its children are wired at daemon construction. The knob that is
  live is `jarvix mute`, which is the one a user reaches for in the moment.
  Mute is runtime state and does not persist across a restart — the
  configuration file is the durable switch.
- `jarvix doctor` gains a check that reports whether the detector is
  installed, and — when the daemon is up — whether a capture process is
  actually running and which one.
- Two new IPC methods (`wake.mute`, `wake.status`), two new events
  (`wake.detected`, `wake.changed`), and two new fields on `status.get`. No
  protocol version bump: they are additive, and a client that ignores them
  behaves exactly as before.
- The wake listener holds a second capture process while push-to-talk is also
  configured. They never capture the same utterance (the engine refuses a wake
  during a held chord), but a machine with an exclusive-mode audio device
  would see contention. PipeWire does not work that way, which is why
  [ADR 0003](0003-pipewire-direct.md) made it the only target.
