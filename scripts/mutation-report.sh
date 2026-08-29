#!/bin/bash
# Run mutation testing over the security-critical packages and write a report
# of the mutants that SURVIVED (issue #172).
#
# The job existed before this script and produced a wall of scrolling output
# that nobody read, on a manual trigger nobody pulled. A signal nobody reads is
# not a signal, so three things changed together: the package set is named here
# rather than being one path in a Makefile line, the run is scheduled, and the
# survivors are written to a file the workflow keeps as an artefact.
#
#   scripts/mutation-report.sh                    # the defined package set
#   scripts/mutation-report.sh ./internal/tools   # one package, while you work
#
# Environment:
#   GREMLINS_VERSION  pinned tool version (default below)
#   MUTATION_OUT      directory for the JSON and the report (default mutation-out)
#
# Reading the report: every line is a mutant the suite did not notice. Each one
# is either a test worth writing or an equivalent mutant worth recording as
# such, WITH ITS REASON, in docs/mutation.md. There is no third option, because
# "we looked at it and moved on" is how the previous job stopped being read.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_DIR"

# The defined package set: the classifiers where a wrong answer is a security
# or a scheduling failure rather than a cosmetic one.
#
#   internal/tools    the shell classifier and the remembered-approval matrix —
#                     a wrong answer here runs a command nobody approved.
#   internal/intent   the spoken-time parser and the fixed-argv router — a wrong
#                     answer schedules a reminder at the wrong hour, or puts a
#                     transcript on a command line.
#   internal/session  the streaming state machine and the sentencer.
#
# Deliberately not "./..." — a mutation run over the whole module takes hours
# and produces a report too long to act on, which is the failure mode this
# script exists to fix.
DEFAULT_PACKAGES=(./internal/tools ./internal/intent ./internal/session)

GREMLINS_VERSION="${GREMLINS_VERSION:-v0.6.0}"
OUT_DIR="${MUTATION_OUT:-$REPO_DIR/mutation-out}"
REPORT="$OUT_DIR/mutation-report.md"

packages=("$@")
if [ ${#packages[@]} -eq 0 ]; then
  packages=("${DEFAULT_PACKAGES[@]}")
fi

mkdir -p "$OUT_DIR"
: >"$REPORT"
{
  echo "# Surviving mutants"
  echo
  echo "Commit: \`$(git rev-parse HEAD 2>/dev/null || echo unknown)\`"
  echo "Tool: \`gremlins $GREMLINS_VERSION\`"
  echo
} >>"$REPORT"

total_lived=0
failed=0

for pkg in "${packages[@]}"; do
  name="$(echo "$pkg" | tr '/.' '__')"
  json="$OUT_DIR/gremlins$name.json"
  log="$OUT_DIR/gremlins$name.log"

  echo "── mutating $pkg"
  # GOFLAGS=-count=1 keeps the baseline honest: a cached test run makes the
  # derived per-mutant timeout near zero and everything "times out".
  # --timeout-coefficient 3 is the same value the manual job used, kept so the
  # numbers stay comparable across the change that introduced this script.
  # -S l asks gremlins itself for the LIVED lines, so nothing here has to
  # parse a JSON schema that a version bump could move. The full machine
  # readable report goes to $json beside it and is kept as an artefact, which
  # is what a later run diffs against.
  if ! GOFLAGS=-count=1 go run "github.com/go-gremlins/gremlins/cmd/gremlins@$GREMLINS_VERSION" \
    unleash --timeout-coefficient 3 --output-statuses l --output "$json" "$pkg" >"$log" 2>&1; then
    # A nonzero exit is expected whenever mutants survive; only a missing
    # report means the run itself failed, and that must be loud rather than
    # silently reported as "no survivors".
    if [ ! -s "$json" ]; then
      echo "gremlins failed on $pkg and wrote no report; output follows" >&2
      cat "$log" >&2
      failed=1
      continue
    fi
  fi

  # gremlins indents its per-mutant lines, so the anchor has to allow it — a
  # `^LIVED` that silently matched nothing would report every package as clean,
  # which is the most expensive way for this script to be wrong.
  lived="$(grep -E '^[[:space:]]*LIVED' "$log" | sed 's/^[[:space:]]*//' || true)"
  count="$(printf '%s' "$lived" | grep -c . || true)"
  total_lived=$((total_lived + count))
  {
    echo "## $pkg — $count surviving"
    echo
    if [ "$count" -eq 0 ]; then
      echo "None."
    else
      echo '```'
      echo "$lived"
      echo '```'
    fi
    echo
    # The efficacy and coverage lines are gremlins' own summary. They are the
    # trend: a report with the same survivor count but a lower efficacy means
    # the suite grew and the mutants grew faster.
    grep -E 'Mutator coverage|Mutation testing completed|efficacy|coverage' "$log" || true
    echo
  } >>"$REPORT"
done

{
  echo "## Total"
  echo
  echo "$total_lived surviving mutants across ${#packages[@]} package(s)."
} >>"$REPORT"

cat "$REPORT"
if [ "$failed" -ne 0 ]; then
  exit 1
fi
