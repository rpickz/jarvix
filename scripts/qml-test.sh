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
QMLLINT="$(dirname "$RUNNER")/qmllint"
if [ -x "$QMLLINT" ]; then
  lint_targets=("$TESTS_DIR"/*.qml "$STUBS_DIR"/*/*.qml "$STUBS_DIR"/*/*/*.qml)
  if findings="$("$QMLLINT" -I "$STUBS_DIR" "${lint_targets[@]}" 2>&1)" && [ -z "$findings" ]; then
    echo "qml-test: qmllint clean"
  else
    echo "qml-test: qmllint findings in the harness:" >&2
    printf '%s\n' "$findings" >&2
    exit 1
  fi
else
  echo "qml-test: no qmllint beside $RUNNER; the harness was not linted" >&2
  exit 1
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

failed=0
for file in "${files[@]}"; do
  path="$TESTS_DIR/$(basename "$file")"
  if [ ! -f "$path" ]; then
    echo "qml-test: no such test file: $path" >&2
    exit 1
  fi
  echo "--- $(basename "$path")"
  if ! (cd "$REPO_DIR" && "$RUNNER" -input "$path" -import "$STUBS_DIR"); then
    failed=1
  fi
done

if [ "$failed" -ne 0 ]; then
  echo "qml-test: FAILED" >&2
  exit 1
fi

echo "qml-test: ok"
