# ADR 0018 — Speech engines stay warm as supervised persistent workers

**Status:** accepted

**Extends:** ADR 0002 (engines run as external processes)

## Context

ADR 0002 chose short-lived engine subprocesses, one per operation, and named
the cost precisely: "per-utterance process spawn + model load adds latency…
if model-load latency becomes the bottleneck, the adapters can switch to
managed long-lived server processes **without interface changes** — that is
the point of the seam."

Issue #12 is that moment. Measured on the development machine (Ryzen AI Max+
395, Vulkan ggml backend, whisper base.en, Kokoro v1.0), the reference
interaction — a short question, provider thinking excluded — spends its time
like this:

| stage | cold |
| --- | --- |
| release → transcript (whisper-cli) | ~175 ms |
| first token → first audio sample (Kokoro) | ~730 ms |
| **release → first audio** | **~900 ms** |

Nearly all of it is start-up. whisper-cli re-reads the ggml file per
question; the Kokoro helper boots a Python interpreter and loads a 310 MB
ONNX model per response. None of that work depends on what was said.

Nothing measured any of it, either. The product's premise is that the
computer feels present, and "feels" was the whole of the evidence.

## Decision

Two changes, and they belong together: the second is what makes the first
reviewable.

### 1. Every session publishes its latency budget

The session engine marks four stage boundaries — capture stop, transcript,
first provider delta, first PCM sample, first audio out — and publishes them
as a `session.timings` IPC event plus one structured log line.
`jarvix status --last` prints them. The stage vocabulary is identical in all
three places, so a number a user reads is greppable in the journal.

The last mark needs a seam: only the player knows when audio actually left
for the device, and putting it on the `audio.Player` interface would burden
every implementation with a measurement most do not make. It travels in the
context instead, as `audio.Trace`, modelled on `net/http/httptrace`.

`first PCM` is marked on the first *sample*, never on the `Speak` call: a
warm engine returns its channel immediately and a cold one returns it after
loading its model, so timing the call would report a warm worker as
infinitely fast.

### 2. Engines run as supervised persistent workers, behind the same interfaces

`internal/warm` owns one generic `Supervisor[Child]`: at most one live child
per engine, spawned on first use, restarted with exponential backoff, retired
when it outgrows a memory cap, reaped after an idle period, and killed on
shutdown. The adapters keep implementing `stt.Transcriber` and
`tts.Synthesizer` unchanged — ADR 0002's seam holds exactly as predicted.

Each engine keeps warm in the way its own upstream supports:

- **whisper** — `whisper-server` on a loopback port, `POST /inference` per
  clip. whisper-cli has no long-lived mode; the server is the binary
  whisper.cpp ships for this, so we use it rather than inventing a protocol
  its maintainers do not support.
- **Kokoro** — `kokoro_stream.py --serve`, a framed line protocol
  (`SPEAK <id> <text>` / `CHUNK`, `END`, `ABORTED`, `ERROR`), with the model
  loaded once. The helper already looped over its input; it now speaks a
  protocol, for the reason below.
- **Piper** — `piper-tts --output_dir`, which reads one utterance per line
  of stdin and prints the finished WAV's path per line. That is a complete
  request/response protocol with an unambiguous end-of-utterance marker,
  which the streaming `--output_raw` mode does not have.

### Cancellation

ADR 0002 made "kill the process" the cancellation mechanism. A persistent
worker cannot use it as the primary path: killing it would throw away the
model load, so interrupting one sentence would make the *next* answer slow —
exactly backwards. Each engine therefore cancels within its own protocol:

- **Kokoro** aborts per utterance. `ABORT <id>` is read on a separate thread
  and checked between PCM frames, so speech stops within one frame (tens of
  milliseconds) and the worker stays warm. This is the reason the helper
  gained a protocol rather than a loop.
- **whisper** has no per-request abort. The HTTP request is cancelled and its
  result dropped; the server finishes decoding a clip nobody is waiting for.
  Bounded (a clip is seconds long), silent, and invisible to the user, who
  has already moved on.
- **Piper** has no abort either, and does not need one: it emits nothing
  until an utterance is complete, so a cancelled sentence produces no audio
  at all — silence is immediate by construction. The abandoned result is
  drained and deleted in the background. A worker that has not produced it
  within a deadline is killed and respawned: **kill-and-respawn is the
  documented fallback here, never the normal path.**

### Failure is never the session's problem

Every warm adapter keeps its cold adapter and falls through to it: engine not
installed, worker still in backoff, worker died mid-request, or a
`kokoro_stream.py` predating the serve protocol. A session pays one cold
start and answers normally; the journal gets one warning, not one per
interaction.

### No orphans

Each child is started in its own process group and killed as a group
(`kill(-pgid)`): the Python helper spawns ONNX threads and whisper-server
survives a bare parent exit perfectly happily. The daemon kills its workers
on shutdown and on every config reload that rebuilds the adapters, so a
long-running jarvixd cannot accumulate engine processes.

### Configuration

A new `[performance]` section: `warm_engines` (default **true**),
`warm_memory_cap_mb` (default 2048, 0 disables), `warm_idle_reap_sec`
(default 600). With `warm_engines = false` the daemon behaves exactly as it
did before this ADR — the same adapters, the same per-operation processes.

## Consequences

Measured on the same machine, same reference interaction, provider thinking
excluded:

| configuration | release → transcript | first token → first sample | release → first audio |
| --- | --- | --- | --- |
| whisper + Kokoro, cold | ~175 ms | ~730 ms | **~903 ms** |
| whisper + Kokoro, warm | ~31 ms | ~256 ms | **~289 ms** |
| whisper + Piper, cold | ~177 ms | ~193 ms | **~374 ms** |
| whisper + Piper, warm | ~29 ms | ~51 ms | **~81 ms** |

The 1.5 s target is met with room to spare, and the first interaction after a
daemon start or an idle reap still pays the cold column — which is why the
cold path stays a supported, tested configuration rather than a legacy one.

- **Memory** — ADR 0002's "idle jarvixd holds no models in RAM" no longer
  holds by default. whisper-server with base.en is ~165 MB resident; Kokoro's
  helper is several hundred more. The idle reaper gives it back after ten
  minutes and `warm_engines = false` restores the old property outright.
- **Piper loses intra-utterance streaming.** At a real-time factor of ~0.03 a
  sentence renders in tens of milliseconds, far less than the voice load it
  replaces, and the numbers above are measured with the trade already made.
- **A protocol is now a compatibility surface.** `kokoro_stream.py` announces
  `READY <version> <rate>`; a mismatch degrades to the one-shot path with a
  warning naming `scripts/setup-kokoro.sh`, rather than hanging on a protocol
  the installed helper does not know.
- **Port allocation is racy by construction.** whisper-server cannot bind
  port 0, so a free port is reserved and released before the exec. If
  something claims it in between, the spawn fails, the supervisor backs off,
  and the session runs cold — the failure mode is a slow answer, not a broken
  one.
- **`make bench-engines`** (build tag `engines`) measures the table above with
  the real engines. It is deliberately not in CI: it measures the machine, not
  the code. The hermetic `make bench` still guards Jarvix's own overhead,
  which is ~25 µs of the budget.

## Amendment (issue #72): the instrument never reads negative

A live session published `jarvix_ms=-3835` on a turn that contained a tool
round and a confirmation wait. The arithmetic was "total minus thinking",
where thinking was measured to the first *text* delta: a first round that
emitted only a tool call pushed that mark past the confirmation question's
audio, so the subtraction included the user's decision time and the tool's
runtime and went negative. Two attribution decisions fix it, both following
the `context_ms` precedent (ADR 0019 / #34) — name the span, charge the
right party:

- **`first provider delta` is the first output, tool call included.** A tool
  call is the model's first product exactly as a token is; counting it keeps
  the pipeline marks in pipeline order on every turn shape.
- **Tool execution (`tool_ms`) and confirmation waits (`confirm_wait_ms`)
  are excluded spans, reported as their own stages.** How long `docker ps`
  takes is the command's runtime; how long the user deliberates over "should
  I go ahead?" is the user's time. Both are visible in the timings — the
  turn's real length stays on the record — and neither is charged to
  `jarvix_ms` or to the model. Pipeline spans are reported net of the
  excluded time that fell inside them.

`jarvix_ms` is therefore `release_to_first_audio_ms` minus the model's time
to first output minus the excluded time before the first sound — which
equals the sum of the Jarvix-owned pipeline spans, each a real elapsed
interval, so it is non-negative by construction. The invariant (every stage
≥ 0; thinking + jarvix + excluded-in-window = the wall clock) is asserted
across every turn shape in `internal/session/timings_test.go`, with the
incident's exact shape as a named regression test. A session cancelled
mid-wait settles the open span at report time: partial timings publish
consistently or not at all, never negative.
