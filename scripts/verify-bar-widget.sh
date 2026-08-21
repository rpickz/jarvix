#!/bin/bash
# Manual verification for the Jarvix bar widget (issue #31), on a real
# Omarchy session.
#
# QML cannot be integration-tested here — the decision logic lives in Go and
# is covered by `go test ./internal/desktop` — so what is left is the wiring:
# is the widget in the bar, does it hold the daemon's state, does the panel's
# IPC surface answer. This script checks those without a human squinting at
# the screen, then lists the handful of things only eyes can confirm.
#
# Nothing here changes the user's shell configuration. The one step that
# touches the daemon (stop/start, to prove the "not running" state) is opt-in.
#
#   scripts/verify-bar-widget.sh              # checks + a live session walk
#   scripts/verify-bar-widget.sh --restart-daemon  # also proves "not running"
set -uo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PLUGIN_DIR="$REPO_DIR/plugin/omarchy"
SHELL_JSON="${XDG_CONFIG_HOME:-$HOME/.config}/omarchy/shell.json"
RESTART_DAEMON=0
[[ ${1:-} == "--restart-daemon" ]] && RESTART_DAEMON=1

passes=0
failures=0

pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; passes=$((passes + 1)); }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; failures=$((failures + 1)); }
info() { printf '       %s\n' "$1"; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# widget_state asks the bar widget what it is showing. The answer is the key
# from the Go table (idle, listening, not-running, error, wake-armed, …).
widget_state() { omarchy-shell jarvix.bar state 2>/dev/null | tr -d '\r\n'; }

# resting_state is what the widget should show between sessions. With
# background listening on that is a microphone rather than the idle mark, and
# both are correct — the point of the check is that the widget reconnected,
# not which resting state it landed in.
is_resting() {
  case "$1" in
    idle | wake-armed | wake-muted) return 0 ;;
    *) return 1 ;;
  esac
}

# daemon_state asks jarvixd directly, so the two can be compared.
daemon_state() {
  jarvix status 2>/dev/null | awk '/^state:/ { print $2 }'
}

step "1. The manifest is one the shell will accept"
if omarchy plugin validate "$PLUGIN_DIR"; then
  pass "omarchy plugin validate $PLUGIN_DIR"
else
  fail "omarchy plugin validate rejected the plugin"
fi

step "2. The widget is in the bar"
if [[ -r $SHELL_JSON ]] && command -v jq >/dev/null 2>&1; then
  section=$(jq -r '
    (.bar.layout // {}) | to_entries[]
    | .key as $section | .value[]?
    | (if type == "object" then .id else . end)
    | select(. == "jarvix") | $section
  ' "$SHELL_JSON" 2>/dev/null | head -1)
  if [[ -n $section ]]; then
    pass "jarvix sits in the bar's '$section' section"
    [[ $section == right ]] || info "the ticket asks for 'right'; move it with: omarchy plugin enable jarvix right"
  else
    fail "jarvix is not in the bar layout — run: scripts/install-plugin.sh"
  fi
else
  info "skipped: need jq and $SHELL_JSON to read the layout"
fi

step "3. The widget's IPC surface answers"
if ! command -v omarchy-shell >/dev/null 2>&1 || ! omarchy-shell shell ping >/dev/null 2>&1; then
  fail "the Omarchy shell is not running; start it and re-run"
else
  state=$(widget_state)
  if [[ -n $state ]]; then
    pass "omarchy-shell jarvix.bar state -> $state"
  else
    fail "the widget did not answer; is the plugin enabled? (omarchy plugin list)"
  fi
  if omarchy-shell jarvix ping >/dev/null 2>&1; then
    pass "the overlay/window IPC target still answers (both kinds loaded)"
  else
    fail "omarchy-shell jarvix ping failed — the panel kind is not loaded"
  fi
fi

if [[ $RESTART_DAEMON == 1 ]]; then
  step "4. A stopped daemon reads as 'not running', not as a missing icon"
  systemctl --user stop jarvixd >/dev/null 2>&1
  # The widget notices on its socket closing; give it a moment to repaint.
  for _ in 1 2 3 4 5; do
    [[ $(widget_state) == "not-running" ]] && break
    sleep 1
  done
  if [[ $(widget_state) == "not-running" ]]; then
    pass "the widget shows 'not running' with jarvixd stopped"
    info "look at the bar: the icon must still be there, dimmed — never absent"
  else
    fail "with jarvixd stopped the widget shows '$(widget_state)'"
  fi
  systemctl --user start jarvixd >/dev/null 2>&1
  for _ in 1 2 3 4 5; do
    is_resting "$(widget_state)" && break
    sleep 1
  done
  if is_resting "$(widget_state)"; then
    pass "the widget reconnects on its own once jarvixd is back"
  else
    fail "after restarting jarvixd the widget shows '$(widget_state)'"
  fi
else
  step "4. Skipped: 'not running' state (pass --restart-daemon to check it)"
fi

step "5. The widget follows a live session"
if [[ -z $(daemon_state) ]]; then
  info "jarvixd is not running; skipping the session walk"
else
  jarvix ask "say hello in five words" >/dev/null 2>&1 &
  ask_pid=$!
  seen=""
  # Sample often enough to catch the short states. This is a witness, not a
  # timing assertion: what matters is that the widget moved with the daemon.
  for _ in $(seq 1 120); do
    current=$(widget_state)
    if [[ -n $current && $seen != *"|$current|"* ]]; then
      seen="$seen|$current|"
      info "widget: $current"
    fi
    kill -0 "$ask_pid" 2>/dev/null || break
    sleep 0.25
  done
  wait "$ask_pid" 2>/dev/null
  if [[ $seen == *"|thinking|"* || $seen == *"|responding|"* || $seen == *"|speaking|"* ]]; then
    pass "the widget tracked the session through the states above"
  else
    fail "the widget never left '$seen' during a session"
  fi
fi

step "By eye — the parts a script cannot see"
cat <<'CHECKS'
  [ ] The icon's shape changes with the state, not just its colour.
  [ ] Hovering shows the state in words ("Listening — The microphone is open").
  [ ] Left-clicking toggles the conversation window — one window, not two.
  [ ] Right-clicking opens the panel: window, new conversation, settings, and
      recent artifacts (plus "Start Jarvix" when the daemon is stopped).
  [ ] Tab into the panel, arrow through the rows, Enter activates, Esc closes.
  [ ] `omarchy theme next` restyles icon, panel, and font with the bar.
CHECKS

step "Result"
printf '  %d passed, %d failed\n' "$passes" "$failures"
[[ $failures -eq 0 ]]
