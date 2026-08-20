-- Jarvix push-to-talk bindings for Omarchy's Lua Hyprland config.
-- Installed by scripts/install-hyprland-bindings.sh into
-- ~/.config/hypr/bindings.lua (between the JARVIX markers).
--
-- Default chord: hold SUPER+ALT+V to talk, release to submit.
-- (Plain SUPER+V is Omarchy's universal paste; Jarvix will not steal it.
--  To use another key, edit these lines — e.g. F10 works well as a
--  dedicated talk key, mirroring voxtype's F9.)
if o.cmd_present("jarvix") then
  o.bind("SUPER + ALT + V", "Talk to Jarvix (hold)", "jarvix ptt start")
  o.bind("SUPER + ALT + V", "Submit to Jarvix", "jarvix ptt stop", { release = true })
  -- SUPER+ESCAPE is Omarchy's system menu; keep the same modifiers as the
  -- talk chord for cancel instead.
  o.bind("SUPER + ALT + ESCAPE", "Cancel Jarvix", "jarvix cancel")
end
