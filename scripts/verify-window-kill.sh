#!/bin/bash
# Manual verification for compositor-kill recovery (issue #106), on a real
# Omarchy session.
#
# NEEDS A LIVE COMPOSITOR. This script opens the Jarvix conversation window,
# kills its toplevel the way super+W does, and reopens it — repeatedly. It
# must never run in CI (there is no compositor there) and should not run
# while you are mid-conversation in the window: a kill resets in-window
# presentation state (tab, scroll, composer draft) by design. Daemon-side
# state is untouched.
#
# Every "open" is verified against `hyprctl clients -j`, never against the
# IPC reply string — the #24 lesson: "open" replies and `visible` readbacks
# lie, a mapped toplevel does not.
#
# The kill command is dialect-sensitive. Hyprland >= 0.55 with a Lua config
# rejects the hyprlang spelling every script on the internet uses
# (`hyprctl dispatch closewindow title:^Jarvix$` fails with a Lua parse
# error — the #106 reproduction hit exactly that). On a Lua compositor the
# whole dispatch is ONE argument holding a single Lua call:
#
#   hyprctl dispatch 'hl.dsp.window.close({ window = "address:0x..." })'
#
# The dialect is discovered, never assumed, by dispatching a Lua no-op —
# the same probe `internal/desktop/compositor.go` uses for daemon dispatches.
#
#   scripts/verify-window-kill.sh
set -uo pipefail

passes=0
failures=0

pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; passes=$((passes + 1)); }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; failures=$((failures + 1)); }
info() { printf '       %s\n' "$1"; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# jarvix_addresses prints the address of every mapped Jarvix toplevel,
# one per line. Zero lines: no window. Two or more: a leak.
jarvix_addresses() {
  hyprctl clients -j 2>/dev/null \
    | jq -r '.[] | select(.title == "Jarvix" and .mapped == true) | .address'
}

# wait_mapped polls until exactly one Jarvix toplevel is mapped (echoes its
# address) or gives up. The window is created asynchronously after the IPC
# reply, so a single immediate read would race it.
wait_mapped() {
  local addr=""
  for _ in 1 2 3 4 5 6 7 8; do
    addr=$(jarvix_addresses)
    if [[ -n $addr && $(wc -l <<<"$addr") -eq 1 ]]; then
      printf '%s' "$addr"
      return 0
    fi
    sleep 0.5
  done
  printf '%s' "$addr"
  return 1
}

# wait_unmapped polls until no Jarvix toplevel remains.
wait_unmapped() {
  for _ in 1 2 3 4 5 6 7 8; do
    [[ -z $(jarvix_addresses) ]] && return 0
    sleep 0.5
  done
  return 1
}

step "0. Prerequisites"
missing=0
for tool in hyprctl jq omarchy-shell; do
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

step "1. Discover the dispatch dialect (the compositor.go probe)"
# A Lua no-op succeeds on a Lua-configured Hyprland and moves nothing; on a
# hyprlang one it is an unrecognised dispatcher, which is itself the answer.
if hyprctl dispatch 'hl.dsp.no_op()' 2>/dev/null | grep -qi '^ok'; then
  dialect="lua"
else
  dialect="legacy"
fi
pass "dispatch dialect: $dialect"

# kill_window closes the toplevel at the given address the way super+W's
# killactive does: an xdg close request, spelled in the discovered dialect.
kill_window() {
  local addr=$1
  if [[ $dialect == lua ]]; then
    hyprctl dispatch "hl.dsp.window.close({ window = \"address:$addr\" })" >/dev/null 2>&1
  else
    hyprctl dispatch closewindow "address:$addr" >/dev/null 2>&1
  fi
}

step "2. Baseline: the window opens and maps"
omarchy-shell jarvix openWindow >/dev/null 2>&1
if addr=$(wait_mapped); then
  pass "openWindow produced a mapped toplevel at $addr"
else
  fail "no mapped Jarvix toplevel after openWindow — nothing below can pass"
fi

step "3. Kill, then every entry point reopens (>= 3 cycles, no leaks)"
declare -a entry_points=("toggleWindow" "openWindow" "openSettings")
for entry in "${entry_points[@]}"; do
  addr=$(jarvix_addresses | head -1)
  if [[ -z $addr ]]; then
    fail "cycle '$entry': no window to kill (previous cycle failed?)"
    continue
  fi
  kill_window "$addr"
  if wait_unmapped; then
    pass "cycle '$entry': compositor kill unmapped the toplevel"
  else
    fail "cycle '$entry': the toplevel survived the close dispatch"
    continue
  fi
  # The wedge this guards against: after a kill, the plugin's visible flag
  # desyncs and open requests are swallowed forever (issue #106). toggleWindow
  # must treat the killed window as closed — i.e. reopen it.
  reply=$(omarchy-shell jarvix "$entry" 2>/dev/null)
  if new_addr=$(wait_mapped); then
    pass "cycle '$entry': reopened as a fresh mapped toplevel at $new_addr (reply: ${reply:-<none>})"
  else
    count=$(jarvix_addresses | grep -c . || true)
    fail "cycle '$entry': reply was '${reply:-<none>}' but mapped Jarvix windows = $count (want 1)"
  fi
done
info "openSettings by eye: the reopened window shows the Settings tab"

step "4. Toggle state resyncs after a kill"
# With the window open (from step 3), toggling twice must land back at open —
# proving the flag tracks the real toplevel, not a stale readback.
omarchy-shell jarvix closeWindow >/dev/null 2>&1
if wait_unmapped; then
  pass "IPC closeWindow hides the window (no kill involved)"
else
  fail "IPC closeWindow left a mapped toplevel"
fi
reply=$(omarchy-shell jarvix toggleWindow 2>/dev/null)
if wait_mapped >/dev/null; then
  pass "toggle after IPC close reopens (fast path, instance retained; reply: ${reply:-<none>})"
else
  fail "toggle after IPC close did not map a window (reply: ${reply:-<none>})"
fi

step "5. Leave things tidy"
omarchy-shell jarvix closeWindow >/dev/null 2>&1
if wait_unmapped; then
  pass "window closed"
else
  fail "window still mapped after closeWindow"
fi

step "By eye — the parts a script cannot see"
cat <<'CHECKS'
  [ ] After a kill + reopen, the Chat tab shows and the conversation history
      is intact (it lives in the daemon; only tab/scroll/draft reset).
  [ ] The bar widget's "Conversation window" action works after a kill —
      one click, one window.
  [ ] Ask something, super+W mid-stream, reopen: the finished answer is in
      the window (daemon state untouched by the kill).
CHECKS

step "Result"
printf '  %d passed, %d failed\n' "$passes" "$failures"
[[ $failures -eq 0 ]]
