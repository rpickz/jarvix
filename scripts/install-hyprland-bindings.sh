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

  # Verify no other binding (Omarchy default or personal) shares a Jarvix
  # chord. Same modmask+key+phase firing two actions means one of them is
  # effectively broken.
  if command -v jq >/dev/null 2>&1; then
    conflicts=$(hyprctl binds -j | jq -r '
      group_by({modmask, key: (.key | ascii_downcase), release}) | .[]
      | select(any(.[]; (.description + .arg) | test("[Jj]arvix")))
      | map(select((.description + .arg) | test("[Jj]arvix") | not))
      | .[] | "  conflicts with: \(.description // .arg)"')
    if [[ -n "$conflicts" ]]; then
      echo ""
      echo "WARNING: a Jarvix key chord is also bound elsewhere:" >&2
      echo "$conflicts" >&2
      echo "Edit the managed block in $TARGET to pick a free chord, then: hyprctl reload" >&2
      exit 1
    fi
    echo "Verified: Jarvix chords conflict with no other bindings."
  fi
else
  echo "Reload Hyprland to activate them (hyprctl reload)."
fi
