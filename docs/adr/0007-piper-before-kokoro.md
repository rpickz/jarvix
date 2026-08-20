# ADR 0007 — Piper first, Kokoro next

**Status:** accepted

## Context

The brief prefers Kokoro for the initial local voice, with Piper allowed as
fallback. On Arch today: Piper is packaged (`piper-tts-bin` +
`piper-voices-*` in AUR), runs as a simple stdin→stdout native binary, and
starts in tens of milliseconds. Kokoro has no system package and needs a
Python/ONNX runtime or a separate server process — a materially heavier
install for every user of the first milestone.

## Decision

Ship Piper as the V1 synthesizer. Keep the `tts.Synthesizer` interface
(streaming PCM chunks + up-front format) engine-neutral, and add Kokoro as a
second implementation in the follow-up milestone (likely as a managed local
server, e.g. kokoro-fastapi or a kokoro-onnx wrapper), selected by
`tts.provider = "kokoro"`.

## Consequences

- The first milestone installs cleanly from packages; voice quality is
  Piper-medium rather than Kokoro (a real, audible trade-off).
- Nothing outside `internal/tts/piper` knows Piper exists; the Kokoro
  adapter is additive.
