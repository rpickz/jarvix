# ADR 0004 — Tap-to-talk on the chord, hold-to-talk on a bare key

**Status:** accepted (v2 — supersedes the chord-hold design after live testing)

## Context

The brief targets hold-`Super+V`-to-talk with Escape cancelling. Three
realities on current Omarchy/Hyprland:

1. `Super+V` is Omarchy's **universal paste** binding — stealing it would
   break a daily-use shortcut.
2. A bare-`Escape` cancel binding would require either grabbing keyboard
   focus in the overlay (the overlay would steal input from the focused
   window — unacceptable) or a Hyprland submap entered for the session's
   lifetime (Escape would be swallowed globally if the daemon ever failed to
   reset the submap — a bad failure mode).
3. **Hyprland release-binds (`{ release = true }`) are not reliable for
   modifier chords.** Verified live on Hyprland 0.56: holding
   `Super+Alt+V`, the release bind fired 60–250 ms into the hold on a real
   keyboard, and depending on the order the three keys were released, often
   not at all. Release binds are dependable only for **bare keys** — which
   is precisely why voxtype's hold-to-talk key is plain `F9` while its
   chord (`Super+Ctrl+X`) is a *toggle*. The brief anticipated this (§11):
   use the platform's real mechanism rather than faking hold semantics.

## Decision

Follow the platform's proven pattern (voxtype's):

- **Tap `Super+Alt+V`** → `jarvix ptt toggle`: idle → start listening;
  listening → submit; any other active state (thinking/speaking) →
  interrupt and start listening. Press binds fire reliably for chords.
- **Hold `F10`** → `jarvix ptt start` (press) / `jarvix ptt stop`
  (release): genuine hold-to-talk on a bare key, where release binds are
  dependable.
- **`Super+Alt+Escape`** → `jarvix cancel`. (`Super+Escape` is Omarchy's
  system menu.)
- The toggle decision lives in the CLI (one `status.get` round-trip), not
  in the binding layer, so any future activation source gets the same
  semantics.
- The bindings live in a marked, managed block in
  `~/.config/hypr/bindings.lua`; both keys are documented one-line edits.
  The installer and `jarvix doctor` verify no other binding shares a Jarvix
  chord.

## Consequences

- Deviates from the brief's literal `Super+V`-hold/`Escape` keys while
  preserving its intent: instant summon, instant cancel, instant interrupt,
  no faked semantics, no broken Omarchy defaults.
- Tap-to-toggle has no stuck-session failure mode from lost release events;
  hold mode on F10 keeps the original UX for those who prefer it.
- A recording that hits the safety cap in toggle mode waits for the second
  tap (or auto-submit, tracked for M2) rather than being lost.
- The overlay stays input-transparent and never takes keyboard focus.
