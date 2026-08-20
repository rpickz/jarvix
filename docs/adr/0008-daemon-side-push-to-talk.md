# ADR 0008 — Daemon-side push-to-talk via evdev

**Status:** accepted

## Context

ADR 0004 v2 moved the chord to tap-to-toggle because Hyprland release-binds
misfire for modifier chords. But push-to-talk — hold, speak, release — is
the intuitive model, and Jarvix is meant for long working sessions where the
activation gesture must be effortless and reliable. Established Linux
push-to-talk software (Mumble, Discord) does not use compositor bindings at
all: it reads key state from the kernel's input devices.

## Decision

jarvixd watches keyboard event devices (`/dev/input/…-event-kbd`) directly
and tracks the configured chord (`activation.ptt_chord`, default
`leftmeta+leftalt+v`):

- all chord keys down → `session.start` + `voice.start` (listening begins
  the instant the chord lands)
- **any** chord key released → `voice.stop` + `session.submit` (whichever
  finger lifts first ends the hold — no release-ordering sensitivity)
- the minimum-recording guard discards accidental blips

The watcher is plain Go (no cgo): `input_event` structs are parsed from
reads; keyboards are discovered via `by-path`/`by-id` globs and rescanned
for hotplug. No compositor cooperation is needed, so the mechanism is
compositor-agnostic.

**Graceful degradation:** input devices are root:input by default. When the
daemon cannot read them, it logs a warning and the Hyprland tap-to-toggle
binding remains the activation path; `jarvix doctor` explains the fix
(`jarvix setup input` installs a udev `uaccess` rule scoped to keyboards).
`status.get` reports `ptt: "daemon" | "external"`, and the toggle CLI
no-ops when the daemon owns the chord so the two paths never fight.

## Privacy

Reading input devices means the daemon process receives every key event —
keylogger-shaped capability, handled explicitly:

- Non-chord events are discarded at the earliest possible point
  (`ChordTracker.Handle` returns before any processing) — enforced by the
  code structure and covered by tests.
- No key event is ever stored, buffered beyond the read batch, logged, or
  transmitted. The logger records only device open/close and chord
  press/release actions.
- The udev rule grants access to the *user's* session (uaccess ACL for the
  active seat), not to the world, and is scoped to `ID_INPUT_KEYBOARD`
  devices — not mice or other inputs.
- `jarvix setup input` states the trade-off before printing the commands;
  the rule file is labelled and removable to revoke.
- This is the same access model every Linux push-to-talk app requires.

## Consequences

- Hold-to-talk works exactly as a user expects, on any chord, regardless of
  compositor binding quirks; release latency is one evdev event.
- The Hyprland layer shrinks to fallback toggle + cancel + optional F10.
- Wake-word work (M9) will reuse nothing from this path (it is audio-side),
  but a future "hold to whisper a command while a session is active"
  interaction gets its input mechanism for free.
