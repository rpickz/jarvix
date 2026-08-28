#!/bin/bash
# Measure total statement coverage and compare it against the floor committed
# in ./coverage.floor (issue #171).
#
# Used by .github/workflows/ci.yml and by `make coverage-ratchet` locally —
# same code path, so the local answer is the gate's answer.
#
#   scripts/coverage-ratchet.sh              # measure and compare
#   scripts/coverage-ratchet.sh --raise      # print what to put in the floor
#
# The floor is a ratchet: it fails when coverage drops more than TOLERANCE
# below the committed number, and it never writes the file itself. Raising it
# is a human editing coverage.floor in the change that earned the increase, so
# the number moving is something a reviewer sees rather than something that
# happened. See coverage.floor for the argument.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_DIR"

FLOOR_FILE="$REPO_DIR/coverage.floor"
PROFILE="${COVERAGE_PROFILE:-$REPO_DIR/coverage.out}"
# 0.5 percentage points. Unrelated changes move the total by a tenth or two,
# and a gate that reddens on noise is a gate people learn to re-run.
TOLERANCE="${COVERAGE_TOLERANCE:-0.5}"

raise=false
if [ "${1:-}" = "--raise" ]; then
  raise=true
fi

# The floor file is commented, so the number is the last non-comment,
# non-blank line. Keeping the reasoning in the file rather than in a wiki is
# the point: whoever changes the number is reading the argument for it.
floor="$(grep -v '^[[:space:]]*#' "$FLOOR_FILE" | grep -v '^[[:space:]]*$' | tail -1 | tr -d '[:space:]')"
if [ -z "$floor" ]; then
  echo "coverage.floor holds no number" >&2
  exit 1
fi

# COVERAGE_TOTAL short-circuits the measurement. It exists so the comparison —
# the part with the judgement in it — can be tested without a five-minute test
# run behind it, which is what internal/build/gate_test.go does: a ratchet
# nobody has watched fail is a ratchet nobody knows the shape of.
if [ -n "${COVERAGE_TOTAL:-}" ]; then
  total="$COVERAGE_TOTAL"
else
  # No -race here. This measures which statements ran, not how they
  # interleaved, and the race detector doubles the runtime for an answer it
  # cannot change. .github/workflows/soak.yml is where interleaving is
  # measured.
  echo "── measuring total statement coverage"
  go test -coverprofile="$PROFILE" ./... >/dev/null
  total="$(go tool cover -func="$PROFILE" | tail -1 | awk '{print $NF}' | tr -d '%')"
fi

if [ -z "$total" ]; then
  echo "could not read a total out of $PROFILE" >&2
  exit 1
fi

printf 'coverage: %s%%   floor: %s%%   tolerance: %spp\n' "$total" "$floor" "$TOLERANCE"

if $raise; then
  # Deliberately prints rather than writes. The floor moves in a commit whose
  # diff shows it moving, next to the tests that earned it.
  if awk -v t="$total" -v f="$floor" 'BEGIN { exit !(t > f) }'; then
    echo
    echo "Coverage is above the floor. To raise it, replace the last line of"
    echo "coverage.floor with:"
    echo
    echo "    $total"
    echo
    echo "and commit that with the tests that earned it."
  else
    echo "Coverage is not above the floor; there is nothing to raise."
  fi
  exit 0
fi

if awk -v t="$total" -v f="$floor" -v tol="$TOLERANCE" 'BEGIN { exit !(t < f - tol) }'; then
  cat >&2 <<EOF

FAIL: total statement coverage $total% is more than ${TOLERANCE}pp below the
committed floor of $floor%.

This is a ratchet, so the fix is to cover what this change added, not to lower
the number. If the drop is genuinely correct — covered code was deleted, or a
package moved out of the module — say so in the commit message and edit
coverage.floor in the same change, so the reviewer sees the floor move.

If you are running this locally and CI disagrees: it will, by about a point.
The gate's runner has none of the external engines installed, so the tests that
probe for them skip and their code goes uncovered. The floor is the runner's
number. See the note in coverage.floor.

Which package moved:

    go tool cover -func=$PROFILE | sort -k3 -n | head -30
EOF
  exit 1
fi

# Above the floor by a wide margin is worth saying out loud: the floor is only
# a ratchet if somebody occasionally winds it on.
if awk -v t="$total" -v f="$floor" 'BEGIN { exit !(t > f + 1.0) }'; then
  echo "coverage is more than 1pp above the floor — consider \`make coverage-ratchet-raise\`"
fi
echo "OK"
