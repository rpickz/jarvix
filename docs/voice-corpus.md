# The voice corpus

A set of recordings of the user's own voice, run through the real speech
pipeline, asserted on what the pipeline *did* with them. Issue #143.

Everything else in this repository that touches speech is tested with faked
transcripts: the tests pin what happens after whisper, never that whisper and
the bias prompt turn real speech into those transcripts. The corpus closes
that loop, in the accent, microphone and room that this deployment actually
has — the only ground truth that matters for one machine.

```
recording.wav ─► whisper-cli (cold, live bias prompt) ─► transcript
                                                            │
                                        session.StripWakeWord│
                                                            ▼
                              intent.Router / session.IsAffirmative
                                                            │
                                                        the assertion
```

---

## Running it

```bash
make voice-corpus            # go test -tags voicecorpus ./internal/voicecorpus -v
```

Without the tag nothing runs — that is how the corpus stays out of CI. It is a
build tag rather than a skip on purpose: the recordings are personal data that
has no business on a shared runner, whisper is heavy, and a test that "skips"
everywhere is a test nobody notices has stopped running.

`jarvix doctor` prints a one-line summary in every install, with no engine and
no source tree behind it — how many phrases are defined and what the committed
baseline says about them. While nothing is recorded it says so plainly, because
"speech recognition is proven only against faked transcripts" is a fact about
the installation, not a detail about a test fixture.

---

## Recording the phrases

The list lives in [`internal/voicecorpus/phrases.toml`](../internal/voicecorpus/phrases.toml),
one entry per phrase, each with an id like `10-workspace-four`.

1. **One WAV per phrase**, named after its id: `10-workspace-four.wav`.
2. **16 kHz mono, signed 16-bit** — the only thing `whisper-cli` reads. Any
   recorder is fine as long as you convert:
   ```bash
   ffmpeg -i take.m4a -ar 16000 -ac 1 -c:a pcm_s16le 10-workspace-four.wav
   ```
   The harness names this command at you if it finds a file it cannot use.
3. **Natural pace, real microphone, quiet room.** Not your telephone voice: the
   corpus is only worth having if it records how you actually talk.
4. **Drop them in `testdata/voicecorpus/`.**
5. Phrases marked `noisy_take = true` are worth a second take in a noisy room,
   saved as `10-workspace-four-noisy.wav`. The two takes are scored and tracked
   separately — "does it work with the kitchen on" is a different question from
   "does it work".
6. Record `31-how-many-quid-did-i-spend` **after** teaching "quid" as a
   hard-to-hear word ("listen for the word quid"), so the taught term is in the
   live bias prompt. That phrase exists to measure exactly that.
7. Then baseline the run:
   ```bash
   go test -tags voicecorpus ./internal/voicecorpus -v -voicecorpus.update-baseline
   git diff internal/voicecorpus/baseline.toml   # read it before committing
   ```

Recordings never leave the machine. Nothing in this package uploads, copies or
transmits anything; if the repository ever stops being private, set
`JARVIX_VOICE_CORPUS` to a directory outside the working tree and the harness
reads them from there instead.

---

## How a phrase declares its expected outcome

The rule the whole design turns on: **assert the outcome, never the
transcript.** Whether whisper wrote "Workspace four." or "workspace 4" is not
the corpus's business — that it reached the router as `workspace.switch` with
slot 4 is.

```toml
[[phrase]]
id = "10-workspace-four"
say = "workspace four"
note = "the number word must survive as a slot"
noisy_take = true
expect = { intent = "workspace.switch", slot = 4 }
```

`say` is the script for the recording session and the yardstick a score is
measured against. It is never asserted.

| key in `expect` | what it requires |
| --- | --- |
| `wake = "name"` | the transcript opens with something the strip accepts as the assistant's name — its own spelling or a configured alias |
| `wake = "strip"` | that, **and** removing the summons leaves a real utterance behind |
| `intent = "x.y"` | the router matches that intent, on the transcript with the wake word already stripped |
| `slot = 4` | …and parsed that integer out of it |
| `no_intent = true` | the router deliberately does **not** claim it, so it reaches the assistant untouched |
| `words = ["quid"]` | those words survived into the transcript, whole and folded for case and punctuation |
| `affirmative = true/false` | the confirmation gate reads it as approval / refusal |

Every key that is set must hold; a phrase with no expectation at all is
refused, because a recording nobody asserts anything about can only ever pass.

`words` is the one to be careful with. It is for the one or two words that
carry the meaning — a taught term, a window nickname, the scale word in "nine
point two **million**" — and it must never be allowed to grow into a list of
every word in the sentence. That would be an exact-transcript assertion wearing
a different hat, and it would be deleted within a month of failing on a comma.

### Adding a phrase

1. Add a `[[phrase]]` block with the next number.
2. `go test ./internal/voicecorpus` — with no tag and no engine. The hermetic
   `TestShippedManifestExpectationsHoldOnIdealTranscripts` feeds your phrase's
   own words through the real router and requires the outcome you declared. If
   that fails, the manifest is wrong, not the recording you have not made yet:
   a typo'd intent name, or a phrasing the router does not actually claim.
3. Record it, run the harness, update the baseline.

---

## The live bias prompt

The harness biases whisper with the prompt **the daemon would send**, composed
by `config.STTBiasPromptFunc` — the same function `daemon.fillDeps` builds its
transcribers with — over the live configuration and the live vocabulary store.
That means the assistant's configured name (#107) and the taught hard-to-hear
phrases (#129), read at transcription time rather than frozen into the test.

A hard-coded prompt would keep passing after you renamed the assistant or
taught a word, which is precisely the regression the corpus exists to catch.
The run prints the prompt it used, and the baseline records a short hash of it
(a hash, not the text: the prompt carries words you had trouble being
understood saying, and a committed file has no business holding that list).

---

## Scores and the baseline

Each recording gets a **score**: the fraction of the phrase's own words that
survived into the transcript, ignoring order, case and punctuation. It is a
tracking number, never an assertion — nothing requires a score of 1, and a
phrase can score 0.5 and still route perfectly (whisper writing "workspace 4"
for "workspace four" costs half the score and none of the meaning). What it is
for is drift: a bias that got worse, a model swap, a microphone that has
started clipping. Those move a number before they flip an outcome.

`internal/voicecorpus/baseline.toml` is the committed record of what works
today. A run fails when:

- something the baseline says **passed** now fails;
- a score falls more than **0.05** below its baseline;
- a recording is present that the baseline has **never agreed to**.

A run does **not** fail when something improved, when the bias prompt changed,
or when a baselined recording is missing from this run — all three are printed
as notes. `pass = false` in the baseline is legitimate and expected: some
phrases in this corpus are meant to be hard (a deliberate mispronunciation,
your fastest and most slurred register), and recording honestly that they do
not work today is what keeps the run green for a known weakness while still
failing the moment something that *did* work stops working.

The baseline is only ever rewritten by `-voicecorpus.update-baseline`, typed by
a person. Nothing updates it automatically: a baseline that updated itself
would agree with every regression it exists to catch.

---

## What an empty or broken corpus does

**Empty** — no directory, or a directory with no recordings — is a **skip**
with a note saying how many phrases are waiting and what is unproven until they
exist. That is the state this feature shipped in, and it is not an error.

**Broken is never a skip.** If the directory exists and holds something wrong,
the run fails and names every defect at once:

- a stem that matches no phrase (`99-typo.wav`) — otherwise it would vanish
  from the run silently;
- a format whisper cannot read, with the `ffmpeg` line to fix it;
- a WAV that will not parse;
- a recording with **no voiced audio in it**.

That last one matters more than it looks. Handed silence, whisper does not
return nothing — it returns its most likely continuation of whatever
conditioned the decoder, which with a bias prompt is the bias prompt. Measured
on this machine:

```
$ whisper-cli -m ggml-base.en.bin -f silence.wav \
      --prompt "The assistant is called Jarvix." --no-timestamps
 The assistant is called Jarvix.
```

The daemon's answer to that is a voice-activity gate in `internal/audio` plus
`stt.IsPromptEcho` (issue #191, ADR 0060), both inside the adapter this harness
calls — so the harness inherits them rather than working around them. But a
silent file in the corpus is a *recording that failed*, and letting it through
would mean grading the hallucination-suppression path while reporting on speech
recognition. So it is rejected at load time, by name, with the level it
measured.

---

## Design notes

- **No parallel pipeline.** The harness runs the real cold `whisper-cli`
  adapter with the real gates, and asks the engine's own functions what the
  transcript meant — `session.StripWakeWord`, `session.WakeWordLeads`,
  `session.IsAffirmative`, a router compiled from `config.IntentOptions`. A
  harness with private copies would only prove that the copies still work.
- **Cold, not warm.** The cold path is the one every install has and the one
  the warm path falls back to. Grading through a persistent server would be
  grading an optimisation.
- **The judgement is hermetic.** Scoring, baseline comparison, manifest
  validation, the loud-failure rules and the skip all live in untagged files
  and are tested by `go test ./...` with no engine anywhere near them. Only the
  wiring needs the tag.
