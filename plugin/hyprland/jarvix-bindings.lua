-- Jarvix keybindings for Omarchy's Lua Hyprland config.
-- Installed by scripts/install-hyprland-bindings.sh into
-- ~/.config/hypr/bindings.lua (between the JARVIX markers).
--
-- Primary activation is daemon-side hold-to-talk: jarvixd watches the
-- SUPER+ALT+V chord via evdev (see `jarvix setup input` / ADR 0008), in
-- which case the toggle binding below no-ops automatically.
--
-- These bindings are the fallback when keyboard devices are not readable:
-- tap SUPER+ALT+V to start listening, tap again to submit (Hyprland
-- release-binds only fire reliably for bare keys — ADR 0004), and F10 as a
-- bare-key hold-to-talk. Rebind freely; plain SUPER+V stays Omarchy's paste.
if o.cmd_present("jarvix") then
  o.bind("SUPER + ALT + V", "Talk to Jarvix (tap to start/stop)", "jarvix ptt toggle")
  o.bind("F10", "Talk to Jarvix (hold)", "jarvix ptt start")
  o.bind("F10", "Submit to Jarvix", "jarvix ptt stop", { release = true })
  -- SUPER+ESCAPE is Omarchy's system menu; keep the same modifiers as the
  -- talk chord for cancel instead.
  o.bind("SUPER + ALT + ESCAPE", "Cancel Jarvix", "jarvix cancel")
end
