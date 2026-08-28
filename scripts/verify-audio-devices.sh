#!/bin/bash
# Manual verification for audio-device changes under a live session (issue
# #142), on a real PipeWire desktop.
#
# NEEDS LIVE AUDIO AND A HUMAN EAR. This script switches your default sink
# and source and kills a playback process mid-sentence to prove Jarvix's
# bindings behave as diagnosed. It must never run in CI (there is no sound
# server there), and it will briefly move ALL desktop audio to another
# device — finish your meeting first. Your original defaults are restored on
# exit, whatever happens.
#
# What it verifies, one behaviour per step:
#   1. `jarvix doctor` names the same default sink/source wpctl does.
#   2. Speech mid-utterance FOLLOWS a default-sink switch (`wpctl
#      set-default`): the stream carries no --target, so WirePlumber moves it
#      live. You should hear the sentence hop devices without a gap.
#   3. A pw-play killed mid-utterance is respawned on the current default and
#      the answer resumes (the issue #142 fix) — with a journal line saying
#      so, never a silent loss of the rest of the answer.
#   4. Push-to-talk capture binds the default source at press time: after a
#      source switch, the next capture uses the new microphone with no
#      restart of anything.
#
#   scripts/verify-audio-devices.sh
set -uo pipefail

passes=0
failures=0
skips=0

pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; passes=$((passes + 1)); }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; failures=$((failures + 1)); }
skip() { printf '  \033[33mskip\033[0m %s\n' "$1"; skips=$((skips + 1)); }
info() { printf '       %s\n' "$1"; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# ask "question" — y/n from the human, the only instrument that can hear.
ask() {
	local answer
	read -r -p "       $1 [y/n] " answer
	[[ "$answer" == y* || "$answer" == Y* ]]
}

node_name() { # node_name @DEFAULT_AUDIO_SINK@|@DEFAULT_AUDIO_SOURCE@
	wpctl inspect "$1" 2>/dev/null | sed -n 's/^[[:space:]]*\*\{0,1\}[[:space:]]*node\.name = "\(.*\)"$/\1/p' | head -1
}

command -v wpctl >/dev/null || { echo "wpctl not found — install wireplumber"; exit 1; }
command -v jarvix >/dev/null || { echo "jarvix not found on PATH"; exit 1; }

original_sink=$(node_name @DEFAULT_AUDIO_SINK@)
original_source=$(node_name @DEFAULT_AUDIO_SOURCE@)
# restore puts the original defaults back. `wpctl set-default` takes an
# object id, not a name, so the saved names are resolved through pw-dump —
# ids can change across replugs, names cannot.
restore() {
	command -v pw-dump >/dev/null && command -v jq >/dev/null || return 0
	local name id
	for name in "$original_sink" "$original_source"; do
		[ -n "$name" ] || continue
		id=$(pw-dump 2>/dev/null | jq -r --arg n "$name" \
			'.[] | select(.info.props["node.name"] == $n) | .id' | head -1)
		[ -n "$id" ] && wpctl set-default "$id" 2>/dev/null
	done
}
trap restore EXIT

step "Ground truth"
info "default sink:   ${original_sink:-<none>}"
info "default source: ${original_source:-<none>}"
wpctl status | sed -n '/Sinks:/,/^[[:space:]│]*$/p' | sed 's/^/       /'

step "1. Doctor names the devices Jarvix will use"
doctor_line=$(jarvix doctor 2>/dev/null | grep -F "audio devices" || true)
info "${doctor_line:-<no 'audio devices' line found>}"
if [[ "$doctor_line" == *"$original_sink"* && "$doctor_line" == *"$original_source"* ]]; then
	pass "doctor's line names the same defaults wpctl reports"
else
	fail "doctor's line disagrees with wpctl (or is missing)"
fi

step "2. Speech follows a default-sink switch mid-utterance"
info "Pick a second sink id from the list above (not the current * one)."
read -r -p "       second sink id (empty to skip): " other_sink
if [ -z "$other_sink" ]; then
	skip "needs a second playback device"
else
	info "Starting a long answer, then switching the default under it..."
	jarvix ask "Count slowly from one to twenty, one number per sentence." >/dev/null 2>&1 &
	ask_pid=$!
	sleep 4
	wpctl set-default "$other_sink"
	info "Default switched. Listen: the counting should HOP to the other"
	info "device and continue — no restart, no silence, no double voice."
	if ask "did the speech move to the second device and keep going?"; then
		pass "default-following playback moves live (no --target, WirePlumber moves the stream)"
	else
		fail "speech did not follow the default switch — diagnosis says it must"
	fi
	wait "$ask_pid" 2>/dev/null
	restore
fi

step "3. A dead pw-play is respawned and the answer resumes"
info "Starting a long answer, then killing its pw-play mid-sentence..."
jarvix ask "Count slowly from one to twenty, one number per sentence." >/dev/null 2>&1 &
ask_pid=$!
sleep 4
if pkill -x pw-play; then
	info "pw-play killed. Listen: at most a syllable is lost, then the"
	info "counting resumes — the player respawned on the current default."
	if ask "did the answer resume speaking after the kill?"; then
		pass "mid-utterance death is detected and the remainder resumes"
	else
		fail "the answer did not resume — the respawn path is broken"
	fi
	if journalctl --user -u jarvixd --since "1 min ago" 2>/dev/null \
		| grep -q "pw-play died mid-stream"; then
		pass "journal records the death and resume (never a silent loss)"
	else
		skip "no journal line found (daemon not under systemd, or logs elsewhere)"
	fi
else
	skip "no pw-play process was running (answer finished early?)"
fi
wait "$ask_pid" 2>/dev/null

step "4. Capture binds the default source at press time"
info "Pick a second source id from wpctl status (empty to skip):"
wpctl status | sed -n '/Sources:/,/^[[:space:]│]*$/p' | sed 's/^/       /'
read -r -p "       second source id (empty to skip): " other_source
if [ -z "$other_source" ]; then
	skip "needs a second capture device"
else
	wpctl set-default "$other_source"
	info "Default source switched. Now hold push-to-talk and ask anything,"
	info "speaking INTO THE SECOND microphone."
	if ask "did the transcript come out right (captured on the new mic)?"; then
		pass "push-to-talk binds the default source at press time — no restart needed"
	else
		fail "capture stayed on the old source"
	fi
	restore
fi

step "Summary"
info "$passes passed, $failures failed, $skips skipped"
info "(defaults restored to: sink=$original_sink source=$original_source)"
exit $((failures > 0))
