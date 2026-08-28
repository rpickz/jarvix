# ADR 0048 — Window overlays: one click-through panel per monitor, fed by a gentle poll

**Status:** accepted

## Context

Issue #127 asks for tiny, static, top-right overlays on enrolled windows: a
focus-thread badge (#123), an AI-session state mark (#124, classified by
#137), and the nickname tag (#126). The anti-goals are as binding as the
features — nothing animated, no timers or counts, unenrolled windows
completely clean, input passthrough, fullscreen hides, one global off
switch — and the standing commitments apply: all classification daemon-side
with QML display-only (ADR 0013), geometry from the existing compositor seam
with no new dialect code (ADR 0022), window addresses never on the wire,
Hyprland-specific and degrading to nothing elsewhere.

Three decisions needed making: what the overlay *surfaces* are, how geometry
tracking works, and what may honestly be overlaid at all.

## Decision

### One full-output panel per monitor, not a surface per window

The shell draws ONE fully click-through layer surface per monitor
(`Variants` over `Quickshell.screens`, layer `top`, empty input region), and
positions every chip as an Item inside it from the daemon's rows.

The alternative — a layer surface per overlaid window — was rejected for
churn: surfaces would be created and destroyed as windows enroll, close, and
change workspace, and repositioned through compositor round trips on every
move, all to represent state that is a plain list. Its one real advantage
would be compositor-managed stacking (a surface could in principle sit in
the window's own layer), but wlr-layer-shell offers no per-toplevel
attachment anyway — a layer surface cannot ride a window's z-order — so the
per-window design would buy the churn without buying the stacking. With that
advantage off the table, n-surfaces-that-churn loses to one-panel-that-never-
does on every axis: fewer moving parts, no lifecycle races, and window
tracking reduced to assigning x/y.

The panel sits on the `top` layer: above normal windows (chips must be
readable over what they annotate), below the `overlay` layer the mid-screen
indicator and confirmation controls use — a passive ambience must never
paint over an active question. Row geometry is global; each panel shows the
rows whose top-right corner lands on its own screen, offset by the screen's
origin (`ShellScreen.x/y`, verified against the installed Quickshell
modules).

### Geometry: poll the inventory, gently and conditionally

The daemon's feed (internal/overlay) re-reads `hyprctl clients -j` through
the shared seam every 2 seconds — but only while anything is enrolled, and
publishes only when the composed rows changed. With no anchored thread and
no nickname the loop parks entirely: no timer, no subprocess, woken only by
the bus events that can change enrolment (`focus.changed`, a nickname
assignment, a settings change). A poke also fires on those events while
enrolled, so a thread switch swaps badge fills immediately rather than up to
an interval late.

Events were considered: Hyprland has a socket2 event stream that would make
tracking instant. Nothing in this repository consumes it — every compositor
interaction is a short-lived subprocess by deliberate trade (ADR 0002/0022:
no IPC library, no protocol version to track, unavailability degrades to a
sentence) — so events would mean building and supervising a persistent
connection, a new seam, and a new failure mode, for a surface whose whole
personality is "calm and peripheral". A 2-second convergence bound is within
a glance for ambient marks; the poll costs two millisecond-scale subprocess
calls per tick (the inventory, plus the focus snapshot's own anchor-liveness
read); and stale-window pruning falls out for free, because every tick
recomputes the world from the live inventory. Revisit if a persistent
Hyprland event consumer ever earns its place for some other feature — the
feed's seams take an event source without restructuring.

### Honesty: overlay only what the inventory can prove

Two suppression rules narrow "the overlay tracks the window" deliberately:

- **Only the focused workspace is overlaid.** The inventory reports every
  mapped window but not which workspace each monitor is currently showing;
  the one workspace whose visibility is certain is the focused window's own.
  On a multi-monitor desktop the other monitors' windows go bare rather than
  risk badges pinned over whatever is actually displayed there.
- **A covered corner gets no chip.** Layer surfaces draw above every
  toplevel, so a chip on an occluded window would float over the occluder. A
  fullscreen window therefore silences its whole workspace (which also
  satisfies "fullscreen hides its own overlay"); a floating window (always
  above the tiled layer) suppresses any chip whose corner rectangle it
  intersects; and between two floating windows — whose relative stacking the
  inventory simply does not carry — only the focused one keeps its chip.
  Suppression is always the safe direction: a missing ambient mark misleads
  nobody, a wrongly floating one lies.

The occlusion test uses a fixed 280×44 corner region (`overlay.RegionWidth/
RegionHeight`); the QML clamps its chip inside the same box, so the judged
rectangle always contains the drawn one.

### The wire, and the #137 seam

`overlays.get` / `overlays.changed` carry finished rows —
`{x, y, width, height, tag?, badge?{thread, active}, ai_state?}` — and
nothing compositor-internal: addresses stay daemon-side as matching keys
(ADR 0022). `ai_state` admits exactly `working` / `needs_you` / `done`;
absent or unrecognised renders nothing, so vocabulary drift between builds
degrades to absence, never to a guessed colour. The daemon adapter
(`overlayAIState` in internal/daemon/overlays.go) is the single line #137
changes when the focus payloads carry a classification — feed, wire, and QML
already carry the field end to end.

`overlays.enabled` (settings registry, live class, default true) is the one
global off switch, voice-toggleable like any registry row; disabled clears
every chip within a moment and stops the polling.

## Consequences

- A moved, resized, killed, or workspace-switched window converges within
  one 2-second poll; a thread switch or nickname change lands immediately
  via the event poke. Nothing about the surface itself ever animates.
- Users who never anchor or name a window pay nothing: no polling, no
  events, no surfaces mapped.
- Multi-monitor support is per-panel filtering; unusual per-monitor scale
  mixes may offset chips (Hyprland layout coordinates vs Qt screen
  geometry) — accepted for v1, revisit with real hardware if reported.
- The feed's rules are hermetically tested in internal/overlay; the QML
  carries guard scans (static, click-through, display-only) like every other
  shell file, plus `scripts/verify-window-overlays.sh` for a live session.
