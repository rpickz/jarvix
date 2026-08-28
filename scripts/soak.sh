#!/bin/bash
# Soak one package: run its tests enough times, under enough pressure, to make
# a probabilistic ordering fault likely rather than lucky (issue #171).
#
# Used by .github/workflows/soak.yml on a schedule and by `make soak` locally —
# same code path, so reproducing a nightly failure is copying one command, not
# reconstructing it. docs/soak.md is the human-facing version of this comment.
#
#   scripts/soak.sh                                   # every mode, every package
#   scripts/soak.sh repeat ./internal/session         # one mode, one package
#   SOAK_LOG_DIR=/tmp/soak scripts/soak.sh constrained ./internal/focus
#
# Why these modes, in the order they earned their place:
#
#   repeat       -race -count=50, whole package. Whole-package matters: the
#                fault this found (#170, 2 failures in 100) needed the rest of
#                the package running alongside to perturb the schedule. A
#                -run=TheOneTest soak would have found nothing.
#   constrained  GOMAXPROCS=2 -race -count=25. Fewer processors means goroutines
#                queue behind each other instead of running side by side, which
#                is what surfaces a loop that stops reading its own store
#                (#155, and the same defect again in internal/reminders, #166).
#                Neither was ever caught by an unconstrained run.
#   unraced      -count=50, no race detector. The detector changes timing, and
#                a family of speech tests once passed ONLY because it did
#                (#156): they were green under -race and red without it, which
#                a race-only soak cannot see.
#
# Nothing is piped through head or tail. The first sighting of #170 was lost
# because its output was truncated to the last few lines, and the failure did
# not reproduce for another day. The full log goes to a file; on failure the
# whole file is printed. That is affordable because -v is deliberately NOT
# passed: `go test` is silent on success, so a passing soak's log is four lines
# and a failing one is exactly the evidence.
set -uo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_DIR"

# The concurrency-prone packages. Everything here either runs a scheduler loop,
# owns a socket with a watcher behind it, or flushes on a goroutine the caller
# does not join. Adding a package is cheap; the soak is scheduled, not on the
# PR path.
DEFAULT_PACKAGES=(
  ./internal/session
  ./internal/daemon
  ./internal/focus
  ./internal/reminders
  ./internal/conversations
  ./internal/automation
)

# Bounded by construction. `go test -timeout` is the important one: on expiry
# Go panics and dumps every goroutine's stack, which for a park-forever fault
# IS the diagnosis (#155 was a parked scheduler). A wall-clock kill from the
# runner would take that away and leave "the job was cancelled" — so this
# number must stay BELOW every step timeout in .github/workflows/soak.yml,
# and those are set from it rather than the other way round.
#
# The default is generous against measured times. On a 32-core box the slowest
# combination (internal/daemon, -race -count=50) takes about five and a half
# minutes; a 2-core hosted runner is several times slower, and 40 minutes still
# leaves the timeout meaning "hung" rather than "busy".
TEST_TIMEOUT="${SOAK_TEST_TIMEOUT:-40m}"
LOG_DIR="${SOAK_LOG_DIR:-$REPO_DIR/soak-logs}"

mode="${1:-all}"
shift || true
packages=("$@")
if [ ${#packages[@]} -eq 0 ]; then
  packages=("${DEFAULT_PACKAGES[@]}")
fi

mkdir -p "$LOG_DIR"

# soak_one runs a single (mode, package) pair and returns its exit status.
soak_one() {
  local mode="$1" pkg="$2"
  local count race procs slug log
  slug="${mode}-$(echo "$pkg" | tr '/.' '__')"
  log="$LOG_DIR/${slug}.log"

  case "$mode" in
    repeat)
      count="${SOAK_COUNT:-50}"
      race="-race"
      procs=""
      ;;
    constrained)
      count="${SOAK_CONSTRAINED_COUNT:-25}"
      race="-race"
      procs="2"
      ;;
    unraced)
      count="${SOAK_UNRACED_COUNT:-50}"
      race=""
      procs=""
      ;;
    *)
      echo "unknown soak mode: $mode (want repeat, constrained, unraced or all)" >&2
      return 2
      ;;
  esac

  local -a cmd=(go test)
  [ -n "$race" ] && cmd+=("$race")
  cmd+=("-count=$count" "-timeout=$TEST_TIMEOUT" "$pkg")

  local reproduce="scripts/soak.sh $mode $pkg"
  local literal=""
  [ -n "$procs" ] && literal="GOMAXPROCS=$procs "
  literal+="${cmd[*]}"

  # The header is the reproduce context, written into the log itself so an
  # artefact downloaded weeks later still says what produced it. `go test` has
  # no seed to record — the variable is the schedule — so what a human needs is
  # the exact command, the processor count, the toolchain and the commit.
  {
    echo "=== jarvix soak ==============================================="
    echo "mode        : $mode"
    echo "package     : $pkg"
    echo "command     : $literal"
    echo "reproduce   : $reproduce"
    echo "GOMAXPROCS  : ${procs:-default (nproc = $(getconf _NPROCESSORS_ONLN 2>/dev/null || echo '?'))}"
    echo "go          : $(go version)"
    echo "commit      : $(git rev-parse HEAD 2>/dev/null || echo unknown)"
    echo "started     : $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "==============================================================="
  } >"$log"

  echo "── soak $mode $pkg  ($literal)"
  local status=0
  if [ -n "$procs" ]; then
    GOMAXPROCS="$procs" "${cmd[@]}" 2>&1 | tee -a "$log"
  else
    "${cmd[@]}" 2>&1 | tee -a "$log"
  fi
  status="${PIPESTATUS[0]}"

  {
    echo "finished    : $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "exit status : $status"
  } >>"$log"

  if [ "$status" -ne 0 ]; then
    # Print the whole log, not an excerpt. See the note at the top of the file:
    # `go test` without -v prints nothing for a passing run, so "the whole log"
    # is the failure and its context and no more.
    echo
    echo "!! SOAK FAILURE: $mode $pkg (exit $status)"
    echo "!! full output follows, and is retained as $log"
    echo "----------------------------------------------------------------"
    cat "$log"
    echo "----------------------------------------------------------------"
    echo "!! reproduce locally with: $reproduce"
    echo
  fi
  return "$status"
}

modes=()
if [ "$mode" = "all" ]; then
  modes=(repeat constrained unraced)
else
  modes=("$mode")
fi

failures=()
for m in "${modes[@]}"; do
  for pkg in "${packages[@]}"; do
    if ! soak_one "$m" "$pkg"; then
      failures+=("$m $pkg")
    fi
  done
done

if [ ${#failures[@]} -ne 0 ]; then
  echo
  echo "soak failed:"
  for f in "${failures[@]}"; do
    echo "  - $f"
  done
  exit 1
fi

echo "soak clean: ${modes[*]} over ${#packages[@]} package(s); logs in $LOG_DIR"
