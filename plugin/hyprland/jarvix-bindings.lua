-- Jarvix keybindings for Omarchy's Lua Hyprland config.
-- Installed by scripts/install-hyprland-bindings.sh into
-- ~/.config/hypr/bindings.lua (between the JARVIX markers).
--
-- Tap SUPER+ALT+V to start listening, tap again to submit. Tapping while
-- Jarvix is speaking interrupts it and listens again. Modifier chords get
-- tap semantics because Hyprland release-binds only fire reliably for bare
-- keys (the same reason voxtype's hold key is F9) — see ADR 0004.
--
-- F10 is genuine hold-to-talk: press and hold while speaking, release to
-- submit. Rebind either freely; plain SUPER+V stays Omarchy's paste.
if o.cmd_present("jarvix") then
  o.bind("SUPER + ALT + V", "Talk to Jarvix (tap to start/stop)", "jarvix ptt toggle")
  o.bind("F10", "Talk to Jarvix (hold)", "jarvix ptt start")
  o.bind("F10", "Submit to Jarvix", "jarvix ptt stop", { release = true })
  -- SUPER+ESCAPE is Omarchy's system menu; keep the same modifiers as the
  -- talk chord for cancel instead.
  o.bind("SUPER + ALT + ESCAPE", "Cancel Jarvix", "jarvix cancel")
end
