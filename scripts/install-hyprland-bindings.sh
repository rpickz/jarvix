#!/bin/bash
# Install Jarvix push-to-talk keybindings into ~/.config/hypr/bindings.lua.
#
# The bindings are appended between marker comments so re-running replaces
# them cleanly instead of duplicating. Reloads Hyprland when possible.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SNIPPET="$REPO_DIR/plugin/hyprland/jarvix-bindings.lua"
TARGET="$HOME/.config/hypr/bindings.lua"
BEGIN="-- >>> JARVIX BINDINGS (managed by jarvix; do not edit inside) >>>"
END="-- <<< JARVIX BINDINGS <<<"

if [[ ! -f "$SNIPPET" ]]; then
  echo "install-hyprland-bindings: $SNIPPET not found" >&2
  exit 1
fi
touch "$TARGET"

tmp="$(mktemp)"
# Strip any previous managed block.
awk -v begin="$BEGIN" -v end="$END" '
  $0 == begin { skipping = 1; next }
  $0 == end   { skipping = 0; next }
  !skipping   { print }
' "$TARGET" >"$tmp"

{
  cat "$tmp"
  echo ""
  echo "$BEGIN"
  cat "$SNIPPET"
  echo "$END"
} >"$TARGET"
rm -f "$tmp"

echo "Installed Jarvix bindings into $TARGET"
if command -v hyprctl >/dev/null 2>&1 && hyprctl version >/dev/null 2>&1; then
  hyprctl reload >/dev/null && echo "Hyprland config reloaded."
else
  echo "Reload Hyprland to activate them (hyprctl reload)."
fi
