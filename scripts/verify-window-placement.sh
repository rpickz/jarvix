#!/bin/bash
# Manual verification for the window-placement vocabulary (issues #176, #177),
# on a real Omarchy session.
#
# NEEDS A LIVE COMPOSITOR, AND IT OPENS AND CLOSES WINDOWS. It must never run
# in CI (there is no compositor there), and it should not run while you are
# working: part two focuses a scratch workspace, opens three terminals on it,
# arranges them, measures them, and closes them again. Nothing outside that
# workspace is touched, and no daemon is restarted.
#
# Why it exists. The hermetic tests assert the argv the daemon builds against a
# fixture of the same spelling — a self-consistent pair with no external truth
# in it, which is exactly how a resize verb that had never worked survived two
# years of green tests (issue #177). `hyprctl` exits 0 for a dispatch the
# compositor refused, so nothing short of a live compositor can tell you that a
# verb is real. This script is that check, in two halves:
#
#   Part one  — every verb the seam can emit is probed with a deliberately
#               BOGUS window address. Argument parsing happens before the
#               window lookup, so the reply distinguishes a wrong argument
#               shape from a missing window, and nothing real is touched.
#   Part two  — the user's own fixture, tiled for real: two thirds on the left,
#               two stacked in the remaining third. Geometry is read back from
#               `hyprctl clients`, never from the dispatch's reply.
#
#   scripts/verify-window-placement.sh            # both parts
#   scripts/verify-window-placement.sh --probe    # part one only (touches nothing)
#
set -uo pipefail

WS=${WS:-42}                       # a scratch workspace, far from anything real
TERM_BIN=${TERM_BIN:-alacritty}    # any windowed program with a stable class
BOGUS='address:0xdeadbeef'

passes=0
failures=0

pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; passes=$((passes + 1)); }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; failures=$((failures + 1)); }
info() { printf '       %s\n' "$1"; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

command -v hyprctl >/dev/null || { echo "hyprctl is not on PATH — this needs a live Hyprland" >&2; exit 1; }
command -v python3 >/dev/null || { echo "python3 is needed to read hyprctl's JSON" >&2; exit 1; }

step "Compositor"
version=$(hyprctl version -j | python3 -c 'import json,sys; print(json.load(sys.stdin).get("version",""))')
layout=$(hyprctl getoption general:layout -j | python3 -c 'import json,sys; print(json.load(sys.stdin).get("str",""))')
info "Hyprland $version, layout: $layout"

# dialect: the same probe the seam makes (hl.dsp.no_op()). "ok" means Lua.
if [[ $(hyprctl dispatch 'hl.dsp.no_op()' 2>&1) == "ok" ]]; then
  dialect=lua
else
  dialect=legacy
fi
info "dispatch dialect: $dialect"
if [[ $dialect == legacy ]]; then
  info "NOTE: the legacy spellings could not be probed on the machine this was written on."
  info "      You are the first check of them. A failure below is a real finding."
fi

# probe runs one dispatch against the bogus address and asserts the reply is
# NOT an argument-shape complaint. A missing window ("window not found", "no
# target") is the EXPECTED outcome and counts as a pass: it means the arguments
# parsed and the compositor got as far as looking for the window.
probe() {
  local what=$1 dispatch=$2
  local out
  out=$(hyprctl dispatch "$dispatch" 2>&1 | head -1)
  case "$out" in
    *"unrecognized arguments"* | *"expected"* | *"invalid "* | *"bad argument"* | \
    *"attempt to call a nil value"* | *"attempt to index"* | *"required"* | *"Unknown "*)
      fail "$what — the compositor rejected the argument shape: $out"
      info "  sent: $dispatch"
      ;;
    ok | *"not found"* | *"no target"* | *"No floating window"*)
      pass "$what"
      ;;
    *)
      # Anything else is unclassified rather than wrong; show it so a human
      # decides. Silence here would be the failure this script exists to end.
      pass "$what (reply: $out)"
      ;;
  esac
}

step "Part one — every verb the seam can emit, probed with a bogus address"
info "Nothing real is touched: the address below belongs to no window."
if [[ $dialect == lua ]]; then
  probe "focus a window"          "hl.dsp.focus({ window = \"$BOGUS\" })"
  probe "close a window"          "hl.dsp.window.close({ window = \"$BOGUS\" })"
  probe "send to a workspace"     "hl.dsp.window.move({ workspace = $WS, window = \"$BOGUS\", follow = false })"
  probe "resize (x/y are SIZE)"   "hl.dsp.window.resize({ window = \"$BOGUS\", x = 100, y = 100, relative = false })"
  probe "position"                "hl.dsp.window.move({ window = \"$BOGUS\", x = 100, y = 100, relative = false })"
  probe "float on"                "hl.dsp.window.float({ window = \"$BOGUS\", action = \"enable\" })"
  probe "float off"               "hl.dsp.window.float({ window = \"$BOGUS\", action = \"disable\" })"
  probe "pin on"                  "hl.dsp.window.pin({ window = \"$BOGUS\", action = \"enable\" })"
  probe "pin off"                 "hl.dsp.window.pin({ window = \"$BOGUS\", action = \"disable\" })"
  probe "fullscreen set"          "hl.dsp.window.fullscreen({ window = \"$BOGUS\", mode = \"fullscreen\", action = \"set\" })"
  probe "maximise set"            "hl.dsp.window.fullscreen({ window = \"$BOGUS\", mode = \"maximized\", action = \"set\" })"
  probe "fullscreen unset"        "hl.dsp.window.fullscreen({ window = \"$BOGUS\", mode = \"fullscreen\", action = \"unset\" })"
  probe "window to a monitor"     "hl.dsp.window.move({ monitor = \"NO-SUCH-OUTPUT\", window = \"$BOGUS\", follow = false })"
  probe "workspace to a monitor"  "hl.dsp.workspace.move({ workspace = $WS, monitor = \"NO-SUCH-OUTPUT\" })"
else
  probe "focus a window"          "focuswindow $BOGUS"
  probe "close a window"          "closewindow $BOGUS"
  probe "send to a workspace"     "movetoworkspacesilent $WS,$BOGUS"
  probe "resize"                  "resizewindowpixel exact 100 100,$BOGUS"
  probe "position"                "movewindowpixel exact 100 100,$BOGUS"
  probe "float on"                "setfloating $BOGUS"
  probe "float off"               "settiled $BOGUS"
  probe "pin"                     "pin $BOGUS"
  probe "fullscreen"              "fullscreen 1"
  probe "window to a monitor"     "movewindow mon:NO-SUCH-OUTPUT,silent,$BOGUS"
  probe "workspace to a monitor"  "moveworkspacetomonitor $WS NO-SUCH-OUTPUT"
fi

# The two layout messages are probed differently: they act on whatever holds
# focus, so a bogus address cannot protect anything. An INVALID message is sent
# instead — it exercises the same parse and changes nothing, and its reply names
# the layout, which is the fact worth knowing.
step "Part one (b) — the layout messages, probed with a nonsense message"
layout_reply=$(hyprctl dispatch "$(
  if [[ $dialect == lua ]]; then printf 'hl.dsp.layout("jarvix-probe-not-a-message")'
  else printf 'layoutmsg jarvix-probe-not-a-message'; fi)" 2>&1 | head -1)
case "$layout_reply" in
  *"Unknown ${layout} layoutmsg"*)
    pass "hl.dsp.layout reaches the $layout layout (it named the layout back)" ;;
  *"attempt to index"* | *"expected string"*)
    fail "the layout verb has the wrong shape: $layout_reply" ;;
  *)
    fail "unexpected layout reply: $layout_reply" ;;
esac
case "$layout" in
  dwindle*)
    info "preselect (place_next) is available on this layout; swapwithmaster (master) is NOT" ;;
  master*)
    info "swapwithmaster (master) is available on this layout; preselect (place_next) is NOT" ;;
  *)
    info "layout \"$layout\" is neither dwindle nor master — expect both place_next and master to report that" ;;
esac

step "Monitors, and the usable area a percentage resolves against"
hyprctl monitors -j | python3 -c '
import json, sys
for m in json.load(sys.stdin):
    if m.get("disabled"):
        continue
    scale = m.get("scale") or 1
    res = m.get("reserved") or [0, 0, 0, 0]
    lw, lh = round(m["width"] / scale), round(m["height"] / scale)
    uw = lw - res[0] - res[2]
    uh = lh - res[1] - res[3]
    name, mw, mh = m["name"], m["width"], m["height"]
    ox, oy = m["x"] + res[0], m["y"] + res[1]
    print(f"       {name}: mode {mw}x{mh} @ scale {scale}"
          f" -> logical {lw}x{lh}, bars {res}, usable {uw}x{uh} at ({ox},{oy})")
    print(f"         two thirds of its usable width = {round(uw*66/100)}px")
'
info "The daemon computes these same numbers (placement.Monitor.Usable). On a"
info "SCALED output, check the logical size above against what you see: the"
info "division by scale is reasoned from Hyprland's coordinate model, not probed."

step "Screen names (#180), if the daemon is running"
# Read-only, and deliberately so: naming a screen writes to the user's state
# directory, and a verification script must not decide what their monitors are
# called. What IS worth proving live is that the daemon's list agrees with the
# compositor's — a picker offering a screen that is not there, or missing one
# that is, is the failure this section catches.
if jarvix monitors --json >/dev/null 2>&1; then
  jarvix monitors --json | python3 -c '
import json, subprocess, sys
reply = json.load(sys.stdin)
listed = sorted(m["connector"] for m in reply.get("monitors") or [])
probe = subprocess.run(["hyprctl", "monitors", "-j"], capture_output=True, text=True)
live = sorted(m["name"] for m in json.loads(probe.stdout) if not m.get("disabled"))
# Bound to locals and joined before formatting: an f-string with quotes inside
# its braces is what broke the monitors block once already (01e4761), and this
# heredoc gives python no way to say so louder than a SyntaxError.
listed_str = ", ".join(listed) or "(none)"
live_str = ", ".join(live) or "(none)"
print(f"       daemon lists: {listed_str}")
print(f"       compositor:   {live_str}")
if listed != live:
    print("       MISMATCH — the picker would offer the wrong set")
    raise SystemExit(1)
for n in reply.get("nicknames") or []:
    name, connector = n["name"], n["connector"]
    where = "plugged in" if n.get("present") else "NOT plugged in right now"
    print(f"       {name} means {connector} ({where})")
path = reply.get("path") or "(nowhere yet)"
print(f"       names are kept in {path}")
' && pass "the daemon's screen list matches the compositor's" \
    || fail "the daemon's screen list does not match the compositor's"
else
  info "SKIPPED: jarvixd is not running, so there is nothing to compare against."
fi

if [[ ${1:-} == --probe ]]; then
  step "Result (probe only)"
  printf '  %d passed, %d failed\n' "$passes" "$failures"
  [[ $failures -eq 0 ]]
  exit
fi

step "Part two — the fixture, tiled for real, on workspace $WS"
if [[ $layout != dwindle* ]]; then
  info "SKIPPED: the arrangement half needs a dwindle-family layout (this is \"$layout\")."
  info "Set general:layout = dwindle and re-run, or accept that place_next reports"
  info "\"this workspace's layout cannot arrange windows that way\" on this machine."
  step "Result"
  printf '  %d passed, %d failed\n' "$passes" "$failures"
  [[ $failures -eq 0 ]]
  exit
fi

occupied=$(hyprctl clients -j | WS=$WS python3 -c '
import json, os, sys
ws = int(os.environ["WS"])
print(sum(1 for c in json.load(sys.stdin) if (c.get("workspace") or {}).get("id") == ws))')
if [[ ${occupied:-0} -gt 0 ]]; then
  echo "workspace $WS already has $occupied window(s); choose an empty one with WS=<n>" >&2
  exit 1
fi

opened=()
launch() { # launch $TERM_BIN and echo the address of the NEW window
  local before after
  before=$(hyprctl clients -j | python3 -c 'import json,sys; print(",".join(c["address"] for c in json.load(sys.stdin)))')
  setsid -f "$TERM_BIN" >/dev/null 2>&1
  for _ in $(seq 1 40); do
    after=$(hyprctl clients -j | BEFORE="$before" python3 -c '
import json, os, sys
seen = set(filter(None, os.environ["BEFORE"].split(",")))
for c in json.load(sys.stdin):
    if c["address"] not in seen and c.get("mapped"):
        print(c["address"]); break')
    [[ -n $after ]] && { echo "$after"; return 0; }
    sleep 0.25
  done
  return 1
}

cleanup() {
  for addr in "${opened[@]}"; do
    hyprctl dispatch "hl.dsp.window.close({ window = \"address:$addr\" })" >/dev/null 2>&1
  done
}
trap cleanup EXIT

hyprctl dispatch "hl.dsp.focus({ workspace = $WS })" >/dev/null
sleep 0.3

# The usable width of whichever monitor is showing this workspace, and the two
# thirds the fixture asks for.
read -r usable_w usable_h < <(hyprctl monitors -j | WS=$WS python3 -c '
import json, os, sys
ws = int(os.environ["WS"])
mons = json.load(sys.stdin)
m = next((x for x in mons if (x.get("activeWorkspace") or {}).get("id") == ws),
         next((x for x in mons if x.get("focused")), mons[0]))
scale = m.get("scale") or 1
res = m.get("reserved") or [0, 0, 0, 0]
print(round(m["width"]/scale) - res[0] - res[2], round(m["height"]/scale) - res[1] - res[3])')
two_thirds=$(( (66 * usable_w + 50) / 100 ))
half_height=$(( (50 * usable_h + 50) / 100 ))
info "usable ${usable_w}x${usable_h}; two thirds = ${two_thirds}px, half the height = ${half_height}px"

# The sequence the runner produces (internal/routine, ADR 0056): launch,
# preselect, launch, preselect, launch, then size — in that order, because the
# layout decides where a window goes when it maps.
if ! main=$(launch); then fail "the first window never appeared"; else
  opened+=("$main")
  hyprctl dispatch "hl.dsp.focus({ window = \"address:$main\" })" >/dev/null
  hyprctl dispatch 'hl.dsp.layout("preselect r")' >/dev/null
fi
if ! right=$(launch); then fail "the second window never appeared"; else
  opened+=("$right")
  hyprctl dispatch "hl.dsp.focus({ window = \"address:$right\" })" >/dev/null
  hyprctl dispatch 'hl.dsp.layout("preselect d")' >/dev/null
fi
if ! below=$(launch); then fail "the third window never appeared"; else
  opened+=("$below")
fi
if [[ ${#opened[@]} -eq 3 ]]; then
  hyprctl dispatch "hl.dsp.focus({ window = \"address:$main\" })" >/dev/null
  hyprctl dispatch "hl.dsp.window.resize({ window = \"address:$main\", x = $two_thirds, y = $usable_h, relative = false })" >/dev/null
  sleep 0.4
fi

geom() { # $1 = address -> "x y w h", read back from the compositor
  hyprctl clients -j | ADDR="$1" python3 -c '
import json, os, sys
addr = os.environ["ADDR"]
for c in json.load(sys.stdin):
    if c["address"] == addr:
        print(*c["at"], *c["size"]); break'
}

if [[ ${#opened[@]} -eq 3 ]]; then
  read -r mx my mw mh < <(geom "$main")
  read -r rx ry rw rh < <(geom "$right")
  read -r bx by bw bh < <(geom "$below")
  info "main  ${mw}x${mh} at ${mx},${my}"
  info "right ${rw}x${rh} at ${rx},${ry}"
  info "below ${bw}x${bh} at ${bx},${by}"

  # A tolerance, because gaps and borders are the user's own configuration and
  # this script must pass on a themed desktop. What is being checked is that
  # the SPLIT moved, not that the pixels are exact.
  tol=$(( usable_w / 12 ))
  within() { local got=$1 want=$2; local d=$(( got - want )); [[ ${d#-} -le $tol ]]; }

  if within "$mw" "$two_thirds"; then
    pass "the main window took two thirds of the usable width (${mw}px, wanted ~${two_thirds}px)"
  else
    fail "the main window is ${mw}px wide, wanted ~${two_thirds}px — the tiled resize did not move the split"
  fi
  if [[ $rx -gt $mx ]]; then
    pass "the second window is to the RIGHT of the main one (preselect r)"
  else
    fail "the second window is at x=$rx, not right of the main one at x=$mx"
  fi
  if [[ $by -gt $ry ]]; then
    pass "the third window is BELOW the second (preselect d)"
  else
    fail "the third window is at y=$by, not below the second at y=$ry"
  fi
  if within "$rw" "$bw"; then
    pass "the stacked pair share the remaining third at the same width"
  else
    fail "the stacked pair are ${rw}px and ${bw}px wide"
  fi
  if within "$(( rh + bh ))" "$mh"; then
    pass "the stacked pair together fill the main window's height"
  else
    fail "the stack is $(( rh + bh ))px tall against the main window's ${mh}px"
  fi

  # Convergence (ADR 0026): the same exact resize, applied again, lands in the
  # same place. A delta would double.
  hyprctl dispatch "hl.dsp.window.resize({ window = \"address:$main\", x = $two_thirds, y = $usable_h, relative = false })" >/dev/null
  sleep 0.3
  read -r _ _ mw2 _ < <(geom "$main")
  if [[ $mw2 -eq $mw ]]; then
    pass "re-applying the placement converged (${mw2}px both times)"
  else
    fail "re-applying the placement moved it: ${mw}px then ${mw2}px"
  fi
fi

step "By eye, before the windows close"
cat <<'CHECKS'
  [ ] Three tiled windows: one wide on the left, two stacked on the right.
  [ ] None of them is floating — they share the workspace, nothing overlaps.
  [ ] The left one is visibly about twice the width of the stack.

  Then, with the daemon running and a routine written in the vocabulary:
  [ ] `jarvix routines` prints each step's mode, share and monitor.
  [ ] Running it places the same arrangement, and says "all N apps placed".
  [ ] Unplug the second monitor and run it again: the step naming that screen
      says "no monitor is called <name> right now" and the others still land.

  And for screen names (#180), on a routine whose steps say monitor = "top"
  and monitor = "bottom":
  [ ] `jarvix monitors name top` and `jarvix monitors name bottom DP-2` —
      then `jarvix monitors` shows each screen with the name you gave it.
  [ ] Running the routine places the same arrangement as it did with the
      connector names spelled out.
  [ ] `jarvix monitors name DP-2` is refused naming the screen that owns the
      word, and `jarvix monitors name current` naming the reserved word.
  [ ] Unplug the bottom screen: the step naming it says "no monitor is called
      bottom right now: it means DP-2, which is not plugged in", the other
      steps still land, and the name is still listed — marked not plugged in.
  [ ] Plug it into a different port, then `jarvix monitors repoint bottom
      <new connector>`: the routine works again with no edit to the routine.
  [ ] Set general:layout = master and run it: the arranging steps say the
      layout cannot arrange windows that way, rather than reporting placed.
CHECKS

step "Result"
printf '  %d passed, %d failed\n' "$passes" "$failures"
[[ $failures -eq 0 ]]
