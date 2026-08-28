#!/bin/bash
# Manual verification for the window overlays (issue #127), on a real
# Omarchy session.
#
# NEEDS A LIVE COMPOSITOR AND A RUNNING jarvixd. This script drives the
# daemon side of the feature — enrolment via focus threads and nicknames,
# the overlays.get snapshot, the off switch — and asserts what a script can
# assert: the feed's rows, the layer surfaces' presence, and the wire's
# hygiene. It must never run in CI (no compositor there), and it mutates
# state visibly: it creates and ends a focus thread called
# "overlay verification" and toggles overlays.enabled (restoring it).
#
# The dialect note from verify-window-kill.sh applies: nothing here
# dispatches, so no dialect probe is needed — `hyprctl clients -j` and
# `hyprctl layers` read the same on both dialects.
#
#   scripts/verify-window-overlays.sh
set -uo pipefail

passes=0
failures=0

pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; passes=$((passes + 1)); }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; failures=$((failures + 1)); }
info() { printf '       %s\n' "$1"; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

SOCK="${XDG_RUNTIME_DIR:-/run/user/$UID}/jarvix.sock"

# rpc sends one JSON-RPC request to jarvixd and prints the first reply line.
# Ids sit in the 900 range: the QML surfaces own 600-649 and 800-849, and a
# probe script should be recognisable in a daemon log as neither.
rpc() {
  local method=$1 params=${2:-null}
  printf '{"jsonrpc":"2.0","id":900,"method":"%s","params":%s}\n' "$method" "$params" \
    | timeout 5 nc -U -q1 "$SOCK" 2>/dev/null | head -1
}

overlays_get() { rpc "overlays.get"; }

step "0. Prerequisites"
missing=0
for tool in hyprctl jq nc; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    fail "missing: $tool"
    missing=1
  fi
done
if [[ $missing -ne 0 ]]; then
  info "this script needs a live Omarchy/Hyprland session; it cannot run in CI"
  exit 1
fi
if ! hyprctl clients -j >/dev/null 2>&1; then
  fail "hyprctl cannot reach a compositor — run this inside a live session, never CI"
  exit 1
fi
pass "live compositor reachable"
if [[ ! -S $SOCK ]] || ! overlays_get | jq -e '.result' >/dev/null 2>&1; then
  fail "jarvixd is not answering overlays.get on $SOCK"
  info "start it: systemctl --user start jarvixd"
  exit 1
fi
pass "jarvixd answers overlays.get"

step "1. Clean by default"
rows=$(overlays_get | jq '.result.rows | length')
info "current rows: $rows (0 unless you already have anchored/named windows)"
enabled=$(overlays_get | jq '.result.enabled')
if [[ $enabled == true ]]; then
  pass "overlays.enabled is on"
else
  info "overlays.enabled is off; the script will still exercise the feed shape"
fi

step "2. Enrolment: anchoring the focused window produces a row"
before=$(overlays_get | jq '.result.rows | length')
if rpc "focus.create" '{"name":"overlay verification","windows":1}' | jq -e '.result' >/dev/null; then
  pass "focus.create anchored the focused window"
else
  fail "focus.create refused (a thread of that name may already exist)"
fi
sleep 1  # the poke lands via the bus; one second is far past it
after=$(overlays_get | jq '.result.rows | length')
if [[ $after -gt $before ]]; then
  pass "rows went $before -> $after after anchoring"
else
  fail "rows went $before -> $after; expected the anchored window to appear"
fi

step "3. The row is a badge with geometry and nothing identifying"
row=$(overlays_get | jq -c '.result.rows[0] // empty')
if [[ -n $row ]]; then
  if jq -e '.width > 0 and .height > 0' <<<"$row" >/dev/null; then
    pass "row carries real geometry: $(jq -c '{x,y,width,height}' <<<"$row")"
  else
    fail "row geometry is empty: $row"
  fi
  if jq -e '.badge.active == true' <<<"$row" >/dev/null; then
    pass "badge is filled (the thread just created is active)"
  else
    info "badge: $(jq -c '.badge' <<<"$row") — filled expected if no other thread took over"
  fi
  if jq -e 'has("ai_state") | not' <<<"$row" >/dev/null; then
    pass "no ai_state before #137's classifier — absent means absent"
  else
    info "ai_state present: $(jq -c '.ai_state' <<<"$row") (#137 has landed)"
  fi
  if grep -qiE '0x[0-9a-f]{4,}|address' <<<"$row"; then
    fail "row leaks a window address: $row"
  else
    pass "no window address on the wire (ADR 0022)"
  fi
else
  fail "no row to inspect"
fi

step "4. The layer surfaces exist and sit on the top layer"
if hyprctl layers 2>/dev/null | grep -q "jarvix-window-overlays"; then
  pass "jarvix-window-overlays layer surface is mapped"
  if hyprctl layers -j >/dev/null 2>&1; then
    lvl=$(hyprctl layers -j | jq -r '..|objects|select(.namespace? == "jarvix-window-overlays")|.level' | sort -u | head -1)
    info "layer level: ${lvl:-unknown} (2 = top; the mid-screen overlay is 3)"
  fi
else
  info "no jarvix-window-overlays layer mapped — is the shell plugin loaded and rows > 0?"
fi

step "5. The off switch clears everything, live"
fp=$(rpc "config.get" | jq -r '.result.fingerprint')
if rpc "config.set" "{\"changes\":{\"overlays.enabled\":false},\"fingerprint\":\"$fp\"}" | jq -e '.result.applied' >/dev/null; then
  sleep 1
  if [[ $(overlays_get | jq '.result.rows | length') -eq 0 ]]; then
    pass "overlays.enabled=false: rows empty"
  else
    fail "rows survived the off switch"
  fi
  fp=$(rpc "config.get" | jq -r '.result.fingerprint')
  rpc "config.set" "{\"changes\":{\"overlays.enabled\":true},\"fingerprint\":\"$fp\"}" >/dev/null
  sleep 1
  if [[ $(overlays_get | jq '.result.rows | length') -gt 0 ]]; then
    pass "re-enabled: rows returned"
  else
    fail "rows did not return after re-enabling"
  fi
else
  fail "config.set could not toggle overlays.enabled"
fi

step "6. Leave things tidy"
if rpc "focus.end" '{"thread":"overlay verification"}' | jq -e '.result' >/dev/null; then
  pass "verification thread ended"
else
  fail "could not end the verification thread — run: jarvix (voice) 'end thread overlay verification'"
fi
sleep 1
info "rows now: $(overlays_get | jq '.result.rows | length')"

step "By eye — the parts a script cannot see"
cat <<'CHECKS'
  [ ] The chip sits inside the window's TOP-RIGHT corner and moves with the
      window (drag it; converges within ~2s — the poll, not an animation).
  [ ] Filled vs hollow badge follows "switch to <thread>" / a second thread.
  [ ] "call this window builds" adds the tag beside the badge; unenrolled
      windows show nothing at all.
  [ ] Clicks pass straight through the chip to the window under it.
  [ ] Fullscreen (super+F) hides every overlay on that workspace; leaving
      fullscreen brings them back.
  [ ] A floating window dragged over a chip's corner makes that chip vanish
      rather than float above the floater.
  [ ] The bar shows the active thread's name in a static chip beside the
      Jarvix icon; it clears when the daemon stops.
  [ ] Nothing anywhere pulses, counts, or animates.
CHECKS

step "Result"
printf '  %d passed, %d failed\n' "$passes" "$failures"
[[ $failures -eq 0 ]]
