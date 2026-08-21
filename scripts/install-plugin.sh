#!/bin/bash
# Install the Jarvix plugin into Omarchy's shell.
#
# Symlinks the plugin directory from this checkout so `git pull` updates it,
# then asks the running shell to rescan and puts the Jarvix widget in the bar.
# Safe to re-run: an already-placed widget is left where the user put it.
#
# The plugin declares two kinds. `panel` is the overlay and the conversation
# window; `bar-widget` is the permanent icon in the bar. Omarchy records an
# enabled plugin in one of two places in shell.json — bar widgets go in
# `bar.layout.<section>`, everything else in `plugins` — and a plugin already
# listed in `plugins` is "enabled" as far as `omarchy plugin enable` is
# concerned, so enabling it again would not place the widget. Moving it takes
# a disable first; that is what the migration below is for. Both kinds load
# either way, so the overlay and window keep working from the bar entry.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$REPO_DIR/plugin/omarchy"
DEST="$HOME/.config/omarchy/plugins/jarvix"
SHELL_JSON="${XDG_CONFIG_HOME:-$HOME/.config}/omarchy/shell.json"
PLUGIN_ID="jarvix"
# Where the widget belongs: alongside the tray, agents, bluetooth, network,
# and audio widgets. Override by passing a section (left|center|right).
SECTION="${1:-right}"

if [[ ! -f "$SRC/manifest.json" ]]; then
  echo "install-plugin: $SRC/manifest.json not found" >&2
  exit 1
fi

if [[ ! $SECTION =~ ^(left|center|right)$ ]]; then
  echo "install-plugin: section must be left, center, or right (got '$SECTION')" >&2
  exit 1
fi

mkdir -p "$(dirname "$DEST")"

if [[ -e "$DEST" && ! -L "$DEST" ]]; then
  echo "install-plugin: $DEST exists and is not a symlink; refusing to replace it" >&2
  exit 1
fi
ln -sfnT "$SRC" "$DEST"
echo "Linked $DEST -> $SRC"

enable_command="omarchy plugin enable $PLUGIN_ID $SECTION"

# Where does shell.json currently list the plugin? Reading the file is the
# only way to tell a bar placement from a plain plugin entry: `omarchy plugin
# list` reports both as "enabled".
placement() {
  [[ -r "$SHELL_JSON" ]] || return 0
  command -v jq >/dev/null 2>&1 || return 0
  local id="$PLUGIN_ID"
  if jq -e --arg id "$id" '
    [(.bar.layout // {}) | to_entries[] | .value[]?
     | (if type == "object" then .id else . end)] | index($id) != null
  ' "$SHELL_JSON" >/dev/null 2>&1; then
    echo bar
  elif jq -e --arg id "$id" '
    [(.plugins // [])[]? | (if type == "object" then .id else . end)] | index($id) != null
  ' "$SHELL_JSON" >/dev/null 2>&1; then
    echo plugins
  fi
}

if ! command -v omarchy-shell >/dev/null 2>&1 || ! omarchy-shell shell ping >/dev/null 2>&1; then
  echo "Omarchy shell not running; the plugin will be discovered on next shell start."
  echo "Put Jarvix in the bar with: $enable_command"
  exit 0
fi

# The manifest scan is a subprocess, and the IPC call returns before it
# finishes — a plugin it has not reached yet answers "unknown". Retry rather
# than sleeping a guessed amount and hoping.
omarchy-shell shell rescanPlugins >/dev/null || true

case "$(placement)" in
  bar)
    echo "Jarvix is already in the bar; leaving it where it is."
    exit 0
    ;;
  plugins)
    echo "Moving Jarvix from the plugin list into the bar (it now ships a bar widget)."
    omarchy plugin disable "$PLUGIN_ID" >/dev/null || true
    ;;
esac

for attempt in 1 2 3 4 5; do
  if omarchy plugin enable "$PLUGIN_ID" "$SECTION" >/dev/null 2>&1; then
    echo "Jarvix is in the bar's $SECTION section."
    exit 0
  fi
  # Not `[[ ... ]] && sleep 1`: on the last pass that list fails, and under
  # `set -e` a failing list at the end of the loop body kills the script
  # before it can print the fallback.
  if [[ $attempt -lt 5 ]]; then sleep 1; fi
done

echo "Plugin installed, but the shell would not place the widget."
echo "Put Jarvix in the bar with: $enable_command"
