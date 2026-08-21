#!/bin/bash
# Manual verification for typing to Jarvix (issue #35), on a real Omarchy
# session.
#
# The decision logic behind Enter lives in Go and is covered by
# `go test ./internal/session ./internal/daemon`. What no Go test can see is
# the other half: that the window is a *real mapped Wayland toplevel* holding
# keyboard focus, with a field in it. An IPC call that returns "open" proves
# nothing at all — a FloatingWindow can report itself visible while never
# mapping (see the import warning at the top of JarvixWindow.qml) — so the
# check here is `hyprctl clients`, not the plugin's own word for it.
#
# The IPC half is driven with the same `session.text` request the composer
# sends, so a failure separates cleanly into "the daemon" or "the window".
#
#   scripts/verify-typed-input.sh
set -uo pipefail

SOCKET="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/jarvix.sock"

passes=0
failures=0

pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; passes=$((passes + 1)); }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; failures=$((failures + 1)); }
info() { printf '       %s\n' "$1"; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# rpc sends one JSON-RPC request and prints the first frame carrying our id.
# socat keeps the connection open long enough for the reply; without it the
# daemon's response races the socket closing.
rpc() {
  local method=$1 params=${2:-null}
  printf '{"jsonrpc":"2.0","id":99,"method":"%s","params":%s}\n' "$method" "$params" \
    | socat -t2 - "UNIX-CONNECT:$SOCKET" 2>/dev/null \
    | grep -m1 '"id":99'
}

daemon_state() { jarvix status 2>/dev/null | awk '/^state:/ { print $2 }'; }

step "0. Prerequisites"
for tool in socat jq hyprctl omarchy-shell; do
  command -v "$tool" >/dev/null 2>&1 || info "missing: $tool (some checks will be skipped)"
done
if [[ -S $SOCKET ]]; then
  pass "jarvixd socket is present at $SOCKET"
else
  fail "no daemon socket; start it with: systemctl --user start jarvixd"
fi

step "1. The daemon accepts a typed turn"
if command -v socat >/dev/null 2>&1 && [[ -S $SOCKET ]]; then
  reply=$(rpc session.text '{"text":"say the word typed and nothing else"}')
  if [[ $reply == *'"session_id"'* ]]; then
    pass "session.text started a turn: $reply"
  else
    fail "session.text was refused: ${reply:-<no reply>}"
  fi
  # Let the turn run so the next checks are not fighting a live session.
  for _ in $(seq 1 40); do
    [[ $(daemon_state) == "idle" ]] && break
    sleep 0.25
  done
else
  info "skipped: needs socat and a running daemon"
fi

step "2. An empty submit starts nothing"
if command -v socat >/dev/null 2>&1 && [[ -S $SOCKET ]]; then
  reply=$(rpc session.text '{"text":"   "}')
  if [[ $reply == *'-32602'* ]]; then
    pass "whitespace is rejected as invalid params, no session started"
  else
    fail "whitespace was not rejected: ${reply:-<no reply>}"
  fi
  if [[ $(daemon_state) == "idle" ]]; then
    pass "the daemon is still idle after the empty submit"
  else
    fail "the daemon moved to '$(daemon_state)' on an empty submit"
  fi
else
  info "skipped: needs socat and a running daemon"
fi

step "3. The typed turn is in the conversation, not off to one side"
if command -v socat >/dev/null 2>&1 && command -v jq >/dev/null 2>&1 && [[ -S $SOCKET ]]; then
  turns=$(rpc conversation.get | jq -r '.result.turns[]? | "\(.role): \(.text)"' 2>/dev/null)
  if grep -qi "say the word typed" <<<"$turns"; then
    pass "conversation.get carries the typed question"
    info "a spoken follow-up now has it as context — that is the same-conversation AC"
  else
    fail "the typed question is not in the conversation snapshot"
  fi
else
  info "skipped: needs socat, jq, and a running daemon"
fi

step "4. The window is a real, mapped toplevel (not just 'visible')"
if command -v omarchy-shell >/dev/null 2>&1 && command -v hyprctl >/dev/null 2>&1; then
  omarchy-shell jarvix openWindow >/dev/null 2>&1
  mapped=""
  for _ in 1 2 3 4 5; do
    mapped=$(hyprctl clients -j 2>/dev/null | jq -r '.[] | select(.title == "Jarvix") | .address' | head -1)
    [[ -n $mapped ]] && break
    sleep 0.5
  done
  if [[ -n $mapped ]]; then
    pass "hyprctl sees a mapped Jarvix toplevel at $mapped"
  else
    fail "no Jarvix toplevel in hyprctl clients — the window never mapped"
    info "check JarvixWindow.qml does not import Quickshell.Wayland; that alone causes this"
  fi
else
  info "skipped: needs omarchy-shell and hyprctl"
fi

step "By eye — the parts a script cannot see"
cat <<'CHECKS'
  [ ] The window has a labelled field at the bottom ("Ask Jarvix").
  [ ] The caret is already in it when the window opens — type without clicking.
  [ ] Tab reaches the field and the focus ring is visible (accent border, thicker).
  [ ] Type a question, press Enter: it appears as your turn and is answered aloud.
  [ ] Shift+Enter does NOT send (nothing happens — multi-line is not built yet).
  [ ] Enter on an empty field does nothing at all.
  [ ] While Jarvix is speaking, type and press Enter: it stops and takes the new turn.
  [ ] Ask for something risky ("delete the build directory"); when it asks,
      type "yes" — it runs that command instead of starting a new question.
      Repeat with "no" and it declines.
  [ ] `systemctl --user stop jarvixd`: the field greys out and says the daemon
      is not running; typing into it does nothing. Start it again and it comes back.
  [ ] Bump the font scale in Omarchy's settings — the field and its label grow with
      the rest of the window.
CHECKS

step "Result"
printf '  %d passed, %d failed\n' "$passes" "$failures"
[[ $failures -eq 0 ]]
