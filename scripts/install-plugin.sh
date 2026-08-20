#!/bin/bash
# Install the Jarvix overlay plugin into Omarchy's shell.
#
# Symlinks the plugin directory from this checkout so `git pull` updates it,
# then asks the running shell to rescan and enable it. Safe to re-run.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$REPO_DIR/plugin/omarchy"
DEST="$HOME/.config/omarchy/plugins/jarvix"

if [[ ! -f "$SRC/manifest.json" ]]; then
  echo "install-plugin: $SRC/manifest.json not found" >&2
  exit 1
fi

mkdir -p "$(dirname "$DEST")"

if [[ -e "$DEST" && ! -L "$DEST" ]]; then
  echo "install-plugin: $DEST exists and is not a symlink; refusing to replace it" >&2
  exit 1
fi
ln -sfnT "$SRC" "$DEST"
echo "Linked $DEST -> $SRC"

if command -v omarchy-shell >/dev/null 2>&1 && omarchy-shell shell ping >/dev/null 2>&1; then
  omarchy-shell shell rescanPlugins >/dev/null || true
  # Give the manifest scan a moment before enabling.
  sleep 1
  if omarchy-shell shell enablePlugin jarvix 'true' >/dev/null 2>&1 \
     || omarchy-shell shell setPluginEnabled jarvix 'true' >/dev/null 2>&1; then
    echo "Plugin enabled in the running shell."
  else
    echo "Plugin installed. Enable it with: omarchy plugin enable jarvix"
  fi
else
  echo "Omarchy shell not running; the plugin will be discovered on next shell start."
  echo "Enable it with: omarchy plugin enable jarvix"
fi
