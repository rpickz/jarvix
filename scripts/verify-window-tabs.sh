#!/bin/bash
# Manual verification for the tabbed conversation window (issue #91), on a
# real Omarchy session.
#
# Tab selection is pure presentation state (ADR 0013), so most of this
# feature is what a script cannot see: the strip, the keyboard, the badge.
# What a script *can* pin down is the plumbing the tabs stand on — that the
# window is a real mapped toplevel, that every listing IPC the collection
# tabs render actually answers, and that the bar's panel actions land where
# they say (openSettings must open the window on the Settings tab). The rest
# is the by-eye checklist at the end, one line per acceptance criterion.
#
#   scripts/verify-window-tabs.sh
set -uo pipefail

SOCKET="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/jarvix.sock"

passes=0
failures=0

pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; passes=$((passes + 1)); }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; failures=$((failures + 1)); }
info() { printf '       %s\n' "$1"; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# rpc sends one JSON-RPC request and prints the first frame carrying our id.
rpc() {
  local method=$1 params=${2:-null}
  printf '{"jsonrpc":"2.0","id":99,"method":"%s","params":%s}\n' "$method" "$params" \
    | socat -t2 - "UNIX-CONNECT:$SOCKET" 2>/dev/null \
    | grep -m1 '"id":99'
}

step "0. Prerequisites"
for tool in socat jq hyprctl omarchy-shell; do
  command -v "$tool" >/dev/null 2>&1 || info "missing: $tool (some checks will be skipped)"
done
if [[ -S $SOCKET ]]; then
  pass "jarvixd socket is present at $SOCKET"
else
  fail "no daemon socket; start it with: systemctl --user start jarvixd"
fi

step "1. Every collection tab's listing IPC answers"
if command -v socat >/dev/null 2>&1 && [[ -S $SOCKET ]]; then
  # One check per tab that renders a listing: the tab can only be as real as
  # the method behind it. An enabled:false answer still passes — the tab
  # shows the switched-off empty state, which is the contract.
  declare -A listings=(
    [activity.get]="rows"          # Activity
    [conversation.list]="conversations" # Library
    [routines.list]="routines"     # Automations (routines half)
    [scripts.list]="scripts"       # Automations (scripts half)
    [knowledge.status]="enabled"   # Knowledge
    [memory.list]="enabled"        # Memory
  )
  for method in "${!listings[@]}"; do
    reply=$(rpc "$method")
    if [[ $reply == *"\"${listings[$method]}\""* ]]; then
      pass "$method answers with ${listings[$method]}"
    else
      fail "$method did not answer as expected: ${reply:-<no reply>}"
    fi
  done
else
  info "skipped: needs socat and a running daemon"
fi

step "2. The window is a real, mapped toplevel (not just 'visible')"
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
    info "check no FloatingWindow file imports Quickshell.Wayland; that alone causes this"
  fi
else
  info "skipped: needs omarchy-shell and hyprctl"
fi

step "3. The bar's panel actions land on the right tabs"
if command -v omarchy-shell >/dev/null 2>&1; then
  # The Settings action is the one that targets a tab: openSettings must
  # answer "open" — the window, already showing the Settings tab. Whether
  # the right tab is showing is the first line of the by-eye list below.
  reply=$(omarchy-shell jarvix openSettings 2>/dev/null)
  if [[ $reply == *open* ]]; then
    pass "openSettings answered 'open' (by eye: the Settings tab is the one showing)"
  else
    fail "openSettings did not answer 'open': ${reply:-<no reply>}"
  fi
  if omarchy-shell jarvix.bar state >/dev/null 2>&1; then
    pass "the bar widget's IPC target answers"
  else
    fail "omarchy-shell jarvix.bar state failed — is the widget on the bar?"
  fi
else
  info "skipped: needs omarchy-shell"
fi

step "By eye — the parts a script cannot see"
cat <<'CHECKS'
  The strip
  [ ] One tab strip: Chat · Activity · Library · Automations · Knowledge ·
      Memory · Settings. No Activity/History/Settings toggle buttons remain.
  [ ] Chat is the tab showing when the window first opens.
  [ ] The active tab is bold with an underline — visible in greyscale, not
      colour alone.
  [ ] After step 3 above, the window is open on the Settings tab (the bar
      panel's Settings action landed there); the bar panel's "Conversation
      window" action toggles the window itself.

  Keyboard
  [ ] Tab (the key) walks the focus ring through the strip; each tab shows a
      focus ring (accent border, thicker).
  [ ] Left/Right on a focused tab move selection along the strip, wrapping.
  [ ] Enter/Space on a focused tab select it; selecting Chat puts the caret
      back in the composer.
  [ ] Ctrl+Tab / Ctrl+Shift+Tab cycle the tabs from anywhere in the window.
  [ ] Escape steps back: record → listing, search → library, any tab → Chat,
      Chat → window closed.

  Each tab
  [ ] Activity: the live feed, streaming new rows as Jarvix acts.
  [ ] Library: the archive listing, search box, one record read-only with
      Back / Continue this conversation.
  [ ] Automations: every [[routines]] and [[scripts]] entry with its phrases;
      a captured routine still needing a launch command says "incomplete" in
      words; Run starts one through the ordinary gated path.
  [ ] Knowledge: every [[knowledge.feeds]] entry with mode and freshness in
      words ("fresh — fetched 3m ago", "failing since … — <reason>").
  [ ] Memory: every remembered fact with its stored date; "remember I take
      Fridays off", reopen the tab, and the new fact is listed.
  [ ] Empty states: with nothing configured, each tab says what would appear
      there and how — never a blank pane.
  [ ] Stop the daemon: the not-running panel stands in; the tabs remain and
      keyboard navigation still works. Start it again and the tabs refill.

  Chat keeps working behind the tabs
  [ ] Ask a question, switch to Activity while it streams, switch back: the
      full answer is there — streaming never paused.
  [ ] Per-tab state holds: scroll Activity up, visit Library, return — the
      scroll position survived. A typed search survives leaving the tab.
  [ ] Ask for something risky ("delete the build directory"). While the
      confirmation is pending, switch to any other tab: the Chat tab shows a
      "!" badge and the header still says a question is waiting. Return to
      Chat: the card is on screen with its countdown running. Approve or
      decline works exactly as before.
CHECKS

step "Result"
printf '  %d passed, %d failed\n' "$passes" "$failures"
[[ $failures -eq 0 ]]
