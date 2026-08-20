# ADR 0003 — PipeWire directly, via pw-cat, no cross-platform audio layer

**Status:** accepted

## Context

Jarvix targets modern Omarchy, where PipeWire is the only audio system.
Options for capture/playback: a portable audio library (PortAudio/miniaudio
via cgo), native PipeWire bindings, or PipeWire's own CLI tools.

## Decision

Use `pw-record` and `pw-play` (pw-cat) as managed subprocesses behind the
narrow `audio.Recorder` / `audio.Player` interfaces. Capture writes 16 kHz
mono s16 WAV to `$XDG_RUNTIME_DIR/jarvix/` (tmpfs, deleted after use);
playback pipes raw PCM into `pw-play --raw`. No cross-platform abstraction.

## Rationale

- pw-cat ships with PipeWire on every Omarchy machine; it handles format
  negotiation, resampling, and routing exactly as the rest of the desktop
  does.
- Same benefits as ADR 0002: no cgo, kill = instant stop (SIGINT first on
  capture so the WAV header is finalised), crash isolation.
- Jarvix is Linux-first by design; portability layers would be speculative
  generality.

## Consequences

- Latency of spawning pw-play (~tens of ms) is imperceptible next to
  synthesis; capture start is bounded by process spawn, also fine.
- Future needs (application/system audio capture for meetings) are still
  reachable through PipeWire targets without changing the interfaces.
- If a future feature needs sample-level streaming (wake word, VAD), that
  becomes a new implementation of the same interfaces using native bindings —
  measured, not assumed.
