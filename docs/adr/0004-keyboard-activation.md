# ADR 0004 — Push-to-talk via Hyprland release bindings; Super+Alt+V default

**Status:** accepted

## Context

The brief targets hold-`Super+V`-to-talk with Escape cancelling. Two
realities on current Omarchy:

1. `Super+V` is Omarchy's **universal paste** binding — stealing it would
   break a daily-use shortcut.
2. A bare-`Escape` cancel binding would require either grabbing keyboard
   focus in the overlay (the overlay would steal input from the focused
   window — unacceptable) or a Hyprland submap entered for the session's
   lifetime (Escape would be swallowed globally if the daemon ever failed to
   reset the submap — a bad failure mode).

Omarchy's Lua binding API (`o.bind(..., { release = true })`) provides real
press/release pairs — voxtype's F9 push-to-talk uses exactly this, so the
mechanism is proven on this platform.

## Decision

- Hold **`Super+Alt+V`** → `jarvix ptt start` (press) / `jarvix ptt stop`
  (release). Genuine push-to-talk, no faked semantics.
- **`Super+Alt+Escape`** → `jarvix cancel`. (`Super+Escape` is Omarchy's
  system menu.)
- Interruption needs no extra binding: pressing the talk chord while Jarvix
  is speaking cancels the active session and starts listening (engine
  contract).
- The bindings live in a marked, managed block in
  `~/.config/hypr/bindings.lua`; rebinding (e.g. to a dedicated `F10`) is a
  documented one-line edit.

## Consequences

- Deviates from the brief's `Super+V`/`Escape` literal keys, preserving its
  intent (hold-to-talk, instant cancel) without breaking Omarchy defaults.
- The overlay stays input-transparent (empty input region) and never takes
  keyboard focus.
- A submap-based bare-Escape cancel can be revisited later as an opt-in once
  the daemon manages submap lifecycle robustly.
