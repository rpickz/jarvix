# ADR 0020 — Jarvix's permanent presence is an Omarchy bar widget, not a tray icon

**Status:** accepted

## Context

Between interactions Jarvix left no trace on screen. The overlay appears only
during a session and fades; the conversation window has to be summoned by a
keybinding, a CLI call, or a notification click — each of which requires
already knowing it exists. That is a discoverability problem now and a
**trust** problem shortly: background wake-word listening (issue #4) makes "is
the microphone open right now?" a question the user must be able to answer at
a glance, and that ticket already names a persistent indicator as a privacy
requirement (issue #31).

Something has to hold that permanent spot. The candidates:

1. **An XDG StatusNotifierItem** (freedesktop system-tray icon), picked up by
   Omarchy's `omarchy.tray` widget like any other tray application.
2. **An Omarchy `bar-widget`** — the first-class plugin kind the shell's own
   audio, network, bluetooth, and agents widgets are built on.
3. **A separate always-on window** parked somewhere on screen — dismissed out
   of hand; that is a panel, and the compositor already has one.

## Decision

Ship a `bar-widget`. `plugin/omarchy/manifest.json` declares both `panel`
(the existing overlay and conversation window) and `bar-widget`, with
`entryPoints.barWidget` pointing at `JarvixBar.qml` and a `barWidget` block
carrying the display name, category, and the one user-tunable setting. It
lands in the bar's `right` section beside the tray, network, and audio
widgets.

The widget is display-only like every other Jarvix surface (ADR 0013). Its
decisions — which glyph and words each daemon state gets, which actions the
panel offers, which icon an artifact kind takes — live in
`internal/desktop/barstatus.go` and are compiled into
`plugin/omarchy/BarState.js` by `go generate ./internal/desktop`.

## Rationale

- **A StatusNotifierItem would cost a D-Bus dependency.** Jarvix reaches the
  desktop by running binaries — `notify-send` for notifications,
  `omarchy-shell` for the window — precisely so it takes no bus library
  (ADR 0002, `internal/desktop`). Owning a tray item is not a fire-and-forget
  invocation: it means exporting an object, serving property reads, and
  answering menu activations for the life of the process. That is a bus client
  in the daemon, in Go, forever.
- **A tray icon cannot be themed correctly.** SNI carries pixmaps or icon
  names; it knows nothing of the user's Omarchy theme, foreground colour,
  urgent colour, or font scale. A `bar-widget` is handed `bar.foreground`,
  `bar.urgent`, `Color.*`, and `Style.*`, so it follows a theme switch with no
  code at all — and the accessibility requirement to respect the shell's font
  scale comes free with it.
- **The plugin already speaks the protocol.** The overlay's socket, JSON-RPC
  parsing, reconnect loop, and event vocabulary are exactly what the widget
  needs. A tray item in the daemon would duplicate the state machine on the
  other side of the process boundary and give the daemon a UI concern it has
  never had.
- **Placement stays the user's.** `omarchy plugin enable jarvix right`, or
  `--before omarchy.tray`, or nowhere at all. A tray icon appears wherever the
  tray is, in whatever order the tray decides.
- **It is the surface #4 needs.** A microphone indicator has to be somewhere
  the user already looks and already trusts. Sitting in the bar alongside the
  shell's own microphone widget is that place.

## Consequences

- The widget is only available under the Omarchy shell, like the overlay and
  the window. That is the accepted trade from ADR 0005: users without the
  shell keep the whole voice pipeline and the CLI. A portable tray icon for
  other desktops stays out of scope; if it is ever wanted it can be added
  beside this, reading the same Go table.
- The plugin now declares two kinds, and Omarchy records an enabled bar widget
  in `shell.json`'s `bar.layout` rather than in `plugins`. An installation
  that predates the widget is listed in `plugins` and `omarchy plugin enable`
  will not move it — it is already "enabled". `scripts/install-plugin.sh`
  detects that and migrates it (disable, then enable with a placement); the
  README documents the two-command manual equivalent.
- The widget holds its socket open continuously, unlike the window's
  open-only-while-visible connection: an icon that is only right during a
  session is not an at-a-glance indicator. It costs the daemon one more slow
  client it is already allowed to drop events for (docs/ipc.md).
- The state vocabulary is now a generated artifact. `BarState.js` is checked
  in so a plain `git clone` of the plugin works without a Go toolchain, and
  two tests keep it honest: byte equality with the generator, and — where
  `node` is available — running the generated JavaScript over every case and
  comparing its answers with the Go.
