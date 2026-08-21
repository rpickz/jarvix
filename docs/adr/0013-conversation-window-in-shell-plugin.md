# ADR 0013 — Conversation window lives in the Omarchy shell plugin

**Status:** accepted

## Context

Clicking a Jarvix notification (or running `jarvix window`) must open a
window showing the full current conversation — every user/assistant turn,
live streaming state, and any error (issue #1). Something has to own that
window. The candidates:

1. **Grow the existing Quickshell plugin** (`plugin/omarchy/`) with a second,
   toggleable surface alongside the overlay card.
2. **A separate GUI process** owned by Jarvix (standalone Quickshell
   instance, GTK, or a Go-native toolkit), spawned on demand.
3. **Render in the daemon** — rejected out of hand: jarvixd is a headless
   session engine and must stay one (docs/architecture.md).

ADR 0005 already committed the display surface to a third-party Omarchy
shell plugin and recorded the assumptions that make that safe: manifest
schema 1, `qs.Commons`/`qs.Ui` imports for theme-correct styling, Quickshell
≥ 0.3 with `Socket` + `SplitParser`, and hot reload from the symlinked
plugin directory.

## Decision

Grow the plugin. `JarvixWindow.qml` is a new component instantiated by the
existing entry point (`JarvixOverlay.qml`); it is a normal toplevel
(`FloatingWindow`) that Hyprland manages like any other window. It opens and
closes via the plugin's existing `IpcHandler` (`omarchy-shell jarvix
openWindow|closeWindow|toggleWindow`), which is what `jarvix window` and the
notification click action call. The window owns its own daemon socket
connection, held only while the window is visible.

The manifest does not change shape: one `panel` entry point may declare as
many windows as it likes — the same pattern the overlay's `PanelWindow`
already uses.

## Rationale

- **The plugin already speaks the protocol.** The overlay's socket
  connection, JSON-RPC parsing, reconnect loop, and event vocabulary are
  exactly what the window needs. A second process would duplicate all of it
  in a second codebase and a second lifecycle.
- **ADR 0005's constraints all carry over unchanged.** Theme, fonts, and
  corner radii come from `qs.Commons`/`qs.Ui` for free — including the
  user's font scale, which the accessibility requirement demands. A separate
  GTK/Qt process would need its own theming bridge.
- **No new process management.** A separate process needs spawn/reap logic,
  a single-instance guard, and its own crash story. The shell plugin is
  already `keepLoaded` and hot-reloaded; a broken window fails visibly in
  `omarchy plugin list` while the daemon and CLI keep working — the failure
  mode ADR 0005 chose deliberately.
- **The architecture rule stays enforceable.** All intelligence lives in
  the daemon; the window renders IPC events and calls IPC methods, exactly
  like the overlay. Keeping both surfaces in one QML directory keeps the
  "display-only" review boundary in one place.

## Consequences

- The window is only available under the Omarchy shell, like the overlay.
  That is the accepted trade from ADR 0005: users without the shell keep the
  full voice pipeline and can read conversations via the CLI (`jarvix ask`
  streams to the terminal); the window is an enhancement, not a dependency.
- `jarvix window` shells out to `omarchy-shell <target> <function>` — the
  same CLI `scripts/install-plugin.sh` already drives — rather than adding
  any GUI code to the Go binaries.
- The window connects to the daemon socket only while visible, so a closed
  window costs the daemon nothing, and a stalled one is just another slow
  IPC client whose events the bus drops (docs/ipc.md).
- QML cannot be integration-tested here, so the window must stay thin:
  anything with logic worth testing (notification content, truncation,
  privacy switches, the conversation snapshot) lives in Go behind the
  `conversation.get` method and the notification dispatcher, both covered by
  hermetic tests.
