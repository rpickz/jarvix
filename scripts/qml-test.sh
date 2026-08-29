#!/bin/bash
# Run the QML behaviour suite headless (issue #174).
#
# Used by .github/workflows/ci.yml and by `make qml-test` locally — same code
# path, so the local answer is the gate's answer.
#
#   scripts/qml-test.sh                      # every tst_*.qml
#   scripts/qml-test.sh tst_pendingturn.qml  # one file, while you chase something
#
# What this runs is the *real* plugin QML — JarvixWindow.qml and its
# components, unmodified — against stub Quickshell and theme modules and a fake
# daemon (qmltest/stubs). The window's own socket announces itself to the fake
# on construction, so nothing in the production files had to change to become
# testable. See qmltest/stubs/JarvixTest/FakeDaemon.qml.
#
# The harness lives here rather than under plugin/omarchy on purpose. That
# directory is copied verbatim into the user's shell by install-plugin.sh and
# into the release tarball by package-release.sh, so a `tests/` subdirectory
# inside it would ship — and the stubs carry a qmldir declaring a module named
# `Quickshell`, which is the last thing that should be sitting on a live
# shell's import path.
#
# The suite is deliberately fast enough for every push: the whole thing is
# about two seconds, because there is not a single sleep in it — every test
# drives an event and asserts on the next line. If that ever stops being true,
# the fix is the test that started waiting, not a nightly schedule.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TESTS_DIR="$REPO_DIR/qmltest"
STUBS_DIR="$TESTS_DIR/stubs"

# No compositor, no display, no window manager. `offscreen` is a real platform
# plugin that lays out and renders into memory, which is what the tests need —
# `minimal` skips too much of the scene graph for waitForRendering to mean
# anything.
export QT_QPA_PLATFORM=offscreen

# Qt routes qWarning/qCritical to the journal when stderr is not a terminal and
# the build has journald support. In CI stderr is a pipe, so without this a QML
# error message — the thing that tells you *why* a test failed — is written to
# a journal nobody will read and the log shows a bare FAIL. Cost of setting it
# where it was not needed: nothing.
export QT_FORCE_STDERR_LOGGING=1

# The software scene-graph backend, so the suite never depends on the GPU stack
# a machine happens to have. A CI container with no libGL, a laptop on Wayland
# and a laptop on X11 would otherwise be three different renderers and three
# chances for "it passes locally". Nothing here tests pixels (#174 puts that
# out of scope), so there is nothing to lose by rendering in software and one
# whole class of flakiness to avoid.
export QT_QUICK_BACKEND=software

# --- find a Qt 6 qmltestrunner ----------------------------------------------
# Its absence is a failure, never a skip. A gate that quietly skips when its
# tool is missing is a gate that reports green on the exact machine where it
# stopped running — which is the failure #174 names by name.
#
# The version check is not paranoia. On a distribution that still ships Qt 5,
# `qmltestrunner` on PATH is the Qt 5 one, and Qt 5 rejects this plugin's
# unversioned `import QtQuick` with "Library import requires a version" — a
# parse error, reported per test file, that reads nothing like "you ran the
# wrong binary". Resolving by qtpaths6 first makes the common case correct and
# the confusing case impossible.
find_runner() {
  local candidate

  if [ -n "${QMLTESTRUNNER:-}" ]; then
    echo "$QMLTESTRUNNER"
    return 0
  fi

  # The authoritative answer: ask Qt 6 itself where its tools live.
  for qtpaths in qtpaths6 qtpaths-qt6; do
    if command -v "$qtpaths" >/dev/null 2>&1; then
      candidate="$("$qtpaths" --query QT_INSTALL_BINS 2>/dev/null)/qmltestrunner"
      if [ -x "$candidate" ]; then
        echo "$candidate"
        return 0
      fi
    fi
  done

  # Distribution layouts that do not ship qtpaths6, plus the PATH entry, which
  # is checked last because it is the one that might be Qt 5.
  for candidate in qmltestrunner-qt6 /usr/lib/qt6/bin/qmltestrunner \
                   /usr/lib/x86_64-linux-gnu/qt6/bin/qmltestrunner \
                   /usr/lib64/qt6/bin/qmltestrunner qmltestrunner; do
    if command -v "$candidate" >/dev/null 2>&1; then
      command -v "$candidate"
      return 0
    fi
  done

  return 1
}

if ! RUNNER="$(find_runner)"; then
  cat >&2 <<'MISSING'
qml-test: no qmltestrunner found.

The QML behaviour suite (issue #174) cannot run, and this is a failure rather
than a skip on purpose: a silent skip would report green on the one machine
where the tests stopped running.

Install the Qt 6 QML test runner and the QtTest QML module:

  Arch      pacman -S qt6-declarative
  Debian    apt-get install qt6-declarative-dev-tools qml6-module-qttest \
                            qml6-module-qtquick qml6-module-qtqml
  Fedora    dnf install qt6-qtdeclarative-devel

Or point QMLTESTRUNNER at the binary yourself.
MISSING
  exit 1
fi

# Prove the runner works before blaming the suite for its failures. A trivial
# TestCase exercises the three things that are missing far more often than a
# real bug is: the binary itself, the QtTest QML module, and a Qt 6 engine that
# accepts this plugin's unversioned imports. Its banner is also the only place
# qmltestrunner prints its version.
SMOKE_DIR="$(mktemp -d)"
trap 'rm -rf "$SMOKE_DIR"' EXIT
cat > "$SMOKE_DIR/tst_smoke.qml" <<'SMOKE'
import QtQuick
import QtTest

TestCase {
    name: "RunnerSmoke"
    function test_the_runner_runs() { compare(1 + 1, 2) }
}
SMOKE

if ! SMOKE_OUT="$("$RUNNER" -input "$SMOKE_DIR" 2>&1)"; then
  echo "qml-test: $RUNNER could not run a trivial TestCase:" >&2
  echo "$SMOKE_OUT" >&2
  echo "qml-test: the QtTest QML module is probably not installed" >&2
  exit 1
fi

if ! printf '%s\n' "$SMOKE_OUT" | grep -q 'Qt 6\.'; then
  echo "qml-test: $RUNNER is not a Qt 6 runner:" >&2
  printf '%s\n' "$SMOKE_OUT" | head -2 >&2
  echo "qml-test: Qt 5 rejects this plugin's unversioned imports; set QMLTESTRUNNER" >&2
  exit 1
fi

echo "qml-test: runner $RUNNER"

# --- lint the harness -------------------------------------------------------
# qmllint sits in the same bin directory as the runner, so finding one has
# found the other. It is pointed at the harness and not at the plugin: with the
# stubs on the import path the production files finally resolve their modules
# and report thousands of pre-existing findings, and turning those into a gate
# in the same change that introduces the runner would mean rewriting eight
# thousand lines of QML nobody asked to have rewritten. The harness is new
# code, so it starts clean and stays clean.
#
# qmllint exits 0 for warnings, so the check is on the output, the same shape
# as the gofmt check next door.
#
# The lint is version-gated, and this is the one compromise in the file, so it
# is worth the paragraph. Qt 6.4's qmllint — what the Ubuntu runners carry —
# types a JavaScript array literal as `void*` and then reports `push` as
# missing on it; 6.5 and later infer the type and say nothing. The finding is
# an artefact of the linter's age, not of the harness.
#
# Neither obvious escape works. Suppressing the category with
# `--missing-property disable` is rejected by 6.4, which does not know the
# flag, so the fix for the old linter needs the new one. Rewriting the harness
# to index-assign instead of `push` is worse than it looks: these are QML
# `property var` arrays, and reading one hands back a copy, so `a[a.length] = x`
# appends to something that is then thrown away — it turned eight passing tests
# red before the cause was obvious.
#
# So the lint runs where its answer can be believed, and says out loud when it
# did not run. The alternative — a check that fails on the runner and passes on
# every developer machine — is worse than no check, because it teaches people
# to scroll past a red step. The tests are what CI must run, and they run
# everywhere.
qmllint_version() {
  "$1" --version 2>&1 | head -1 | sed -nE 's/.*[^0-9.]([0-9]+)\.([0-9]+).*/\1 \2/p'
}

QMLLINT_MIN_MAJOR=6
QMLLINT_MIN_MINOR=5
QMLLINT="$(dirname "$RUNNER")/qmllint"

if [ ! -x "$QMLLINT" ]; then
  # A missing linter is a broken installation rather than an old one: qmllint
  # ships in the same package as the runner we just used, so its absence means
  # something is wrong with the environment and not with the linter's age.
  echo "qml-test: no qmllint beside $RUNNER; the installation is incomplete" >&2
  exit 1
fi

# `|| true` because read returns non-zero on an empty herestring, and under
# `set -e` that would abort before the message below could explain why.
lint_major=""
lint_minor=""
read -r lint_major lint_minor <<< "$(qmllint_version "$QMLLINT")" || true
if [ -z "${lint_major:-}" ]; then
  echo "qml-test: could not read a version from $QMLLINT" >&2
  exit 1
fi

if [ "$lint_major" -gt "$QMLLINT_MIN_MAJOR" ] ||
   { [ "$lint_major" -eq "$QMLLINT_MIN_MAJOR" ] && [ "$lint_minor" -ge "$QMLLINT_MIN_MINOR" ]; }; then
  lint_targets=("$TESTS_DIR"/*.qml "$STUBS_DIR"/*/*.qml "$STUBS_DIR"/*/*/*.qml)
  if findings="$("$QMLLINT" -I "$STUBS_DIR" "${lint_targets[@]}" 2>&1)" && [ -z "$findings" ]; then
    echo "qml-test: qmllint ${lint_major}.${lint_minor} clean"
  else
    echo "qml-test: qmllint findings in the harness:" >&2
    printf '%s\n' "$findings" >&2
    exit 1
  fi
else
  echo "qml-test: SKIPPED the harness lint — qmllint is ${lint_major}.${lint_minor}," \
       "older than ${QMLLINT_MIN_MAJOR}.${QMLLINT_MIN_MINOR}, and reports a false" \
       "'push not found' on every JavaScript array literal. The tests below still run."
fi

# --- run --------------------------------------------------------------------
# One process per file rather than one for the whole directory: qmltestrunner
# names its results after the binary, not after the file, so a single run
# reports every failure as "qmltestrunner::…" with no way to tell which file it
# came from. Per-file runs cost about 40ms each and make the log readable.
if [ "$#" -gt 0 ]; then
  files=("$@")
else
  files=()
  for f in "$TESTS_DIR"/tst_*.qml; do
    files+=("$(basename "$f")")
  done
fi

if [ "${#files[@]}" -eq 0 ]; then
  echo "qml-test: no tst_*.qml under $TESTS_DIR — the suite has vanished" >&2
  exit 1
fi

# A binding loop is a failure of this suite, wherever it comes from (issue
# #203).
#
# Qt reports one as a warning and then carries on, having broken the cycle by
# dropping a binding — so the window still renders, the tests still pass, and
# the property that stopped updating is whichever one the engine happened to
# drop. That is the worst shape a UI defect can take: it is invisible on the
# machine you are looking at and different on the machine you are not. #203
# shipped with one for months for exactly that reason, on the transcript's
# letter spacing, which meant a reading-comfort setting somebody had turned up
# on purpose could silently stop applying.
#
# So the runner reads its own output. This is general rather than a test of
# the one loop that has been fixed: every future loop, in any file the suite
# touches, production or harness, fails the run on the push that introduced it
# — which is the only moment it is cheap to fix. There is no allow-list, and
# adding one would defeat the point; a loop that is genuinely acceptable does
# not exist, because "acceptable" would have to mean "we know which binding Qt
# will drop", and that is exactly what nobody knows.
#
# The output is captured rather than streamed so it can be scanned. Each file
# takes a couple of hundred milliseconds and is printed the moment it
# finishes, so nothing is lost but the line-by-line trickle.
BINDING_LOOP='Binding loop detected'

failed=0
loops=""
for file in "${files[@]}"; do
  path="$TESTS_DIR/$(basename "$file")"
  if [ ! -f "$path" ]; then
    echo "qml-test: no such test file: $path" >&2
    exit 1
  fi
  echo "--- $(basename "$path")"
  output=""
  if ! output="$(cd "$REPO_DIR" && "$RUNNER" -input "$path" -import "$STUBS_DIR" 2>&1)"; then
    failed=1
  fi
  printf '%s\n' "$output"
  # Collected rather than reported here, and reduced to the location that
  # needs editing: a loop in a ListView delegate is re-reported once per
  # instantiation and once per test function, so the unfixed transcript
  # produced forty identical lines saying what one line says. The QTest prefix
  # naming the test function is dropped for the same reason — the loop belongs
  # to the QML file, not to whichever test happened to instantiate it — and
  # the absolute file:// URL is trimmed to a repo-relative path so the summary
  # reads like the rest of this repo's failures.
  #
  # `|| true` because `grep` reports "no match" as a failure and the file that
  # is *clean* is the common case; under `set -e` and `pipefail` the first
  # loop-free file would otherwise abort the script mid-suite, which is
  # exactly the silent green this gate exists to prevent.
  found="$(printf '%s\n' "$output" | grep -F "$BINDING_LOOP" || true)"
  if [ -n "$found" ]; then
    loops+="$(printf '%s\n' "$found" \
      | sed -E -e 's|^QWARN[[:space:]]*: [^ ]+\(\) ||' -e "s|file://$REPO_DIR/||")"$'\n'
  fi
done

# `grep .` drops the blank lines the per-file collection leaves behind, so an
# empty result really is empty and the check below is a simple one.
if loops="$(printf '%s' "$loops" | grep . | sort -u)" && [ -n "$loops" ]; then
  echo "qml-test: a binding loop was reported. Qt broke the cycle by dropping a" >&2
  echo "binding, so one of the properties below has quietly stopped updating:" >&2
  printf '%s\n' "$loops" >&2
  echo "qml-test: give the shared value a property of its own and have both" >&2
  echo "bindings read that, rather than one of them reading the other." >&2
  failed=1
fi

if [ "$failed" -ne 0 ]; then
  echo "qml-test: FAILED" >&2
  exit 1
fi

echo "qml-test: ok"
