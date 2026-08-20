# ADR 0005 — Overlay as an Omarchy shell plugin (assumptions recorded)

**Status:** accepted

## Context

Omarchy 4.x hosts the entire desktop in one Quickshell instance
(`omarchy-shell`) with a manifest-based plugin registry. Third-party plugins
are directories under `~/.config/omarchy/plugins/<id>/` with a
`manifest.json` (`schemaVersion: 1`), validated by
`services/PluginRegistry.qml`, hot-reloaded on file save. The alternative was
a standalone Quickshell (or GTK layer-shell) process owned by Jarvix.

## Decision

Ship the overlay as a third-party plugin: id `jarvix`, kind `panel`,
`keepLoaded: true`, entry point `JarvixOverlay.qml`. The plugin connects to
the daemon socket itself (Quickshell `Socket`) and derives all visibility
from daemon state — it needs no summon IPC, no shell-side logic, and no
second Quickshell environment.

## Assumptions about the installed Omarchy (verified against 4.0.0)

- Plugin manifest schema version **1**; required fields
  `id/name/version/kinds/entryPoints`; kind `panel` with `keepLoaded: true`
  is loaded persistently (the first-party OSD uses exactly this shape).
- Third-party plugin QML may import the shell's `qs.Commons` / `qs.Ui`
  modules (cloned built-ins rely on this), giving theme-correct
  `Style`/`Color`/`BorderSurface`.
- Enabled state for non-bar plugins = presence in `shell.json`'s `plugins[]`,
  toggled via `omarchy plugin enable` / `omarchy-shell shell setPluginEnabled`.
- `~/.config/omarchy/plugins/` is watched by inotify; a symlinked plugin
  directory is discovered by the rescan (`for sub in "$dir"/*/` follows
  symlinks).
- Quickshell ≥ 0.3 provides `Quickshell.Io.Socket` with `SplitParser` and
  `Quickshell.Wayland.WlrLayershell` for the overlay layer.

If a future Omarchy bumps the manifest schema or moves the Commons imports,
the plugin fails to validate/load — visibly, in `omarchy plugin list` — while
the daemon and CLI keep working.

## Consequences

- The overlay inherits the user's theme, fonts, and corner radii with zero
  Jarvix-side theming code.
- Plugin code runs unsandboxed inside the user's shell (Omarchy's model);
  keeping the QML display-only keeps the risk surface minimal.
- Users without Omarchy's shell still get the complete voice pipeline via the
  CLI; the plugin is an enhancement, not a dependency (doctor reports it as a
  warning, not a failure).
