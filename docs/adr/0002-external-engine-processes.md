# ADR 0002 — Speech engines run as external processes

**Status:** accepted

## Context

whisper.cpp and Piper are native C++ inference engines. They could be linked
into jarvixd via cgo bindings, run as long-lived sidecar servers, or invoked
as short-lived processes per operation.

## Decision

V1 invokes both as short-lived subprocesses per operation, behind the
`stt.Transcriber` / `tts.Synthesizer` interfaces:

- `whisper-cli --model … --file … --no-timestamps --no-prints`
- `piper-tts --model … --output_raw` (text on stdin, raw PCM on stdout)

## Rationale

- **Crash isolation** — a native crash kills one session, not the daemon.
  The daemon-must-not-crash requirement falls out structurally.
- **Cancellation** — killing a process is immediate and reliable;
  interrupting speech mid-sentence is a core UX requirement.
- **Zero cgo** — pure-Go builds, trivial cross-compilation, no ABI coupling
  to system libraries; engines upgrade via pacman independently of Jarvix.
- **Memory** — idle jarvixd holds no models in RAM.

## Consequences

- Per-utterance process spawn + model load adds latency (~a second for
  whisper base.en; Piper starts faster than it speaks). Acceptable for V1.
- If model-load latency becomes the bottleneck, the adapters can switch to
  managed long-lived server processes (whisper.cpp server mode, a Piper
  daemon) **without interface changes** — that is the point of the seam.
- The rest of Jarvix must never know which engine runs; only the adapters and
  `jarvix doctor` reference engine names.
