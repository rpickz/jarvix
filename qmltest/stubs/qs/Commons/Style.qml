pragma Singleton

import QtQuick

// Style — Omarchy's metrics singleton, stubbed.
//
// The real one lives in the user's shell configuration, which a CI runner
// does not have and never will: the plugin is installed *into* a shell, so
// its theme is by definition someone else's file. The numbers below are the
// shipped defaults' shape rather than their exact values, and no test asserts
// on them — a test that cared what `Style.space(4)` returns would be testing
// Omarchy's theme, which #174 puts out of scope.
//
// What the numbers must be is *non-zero and sane*, because layout is not
// cosmetic here: a font size of 0 makes every Text item zero-high, and a
// keyboard-reachability test on a window whose controls all have no size
// proves nothing.
QtObject {
  id: root

  readonly property int cornerRadius: 8

  // The theme's spacing ramp. The real one scales by the user's density
  // setting; identity keeps the arithmetic out of the stub, which is the same
  // reason the production QML never does spacing arithmetic of its own.
  function space(n) {
    return n
  }

  readonly property QtObject spacing: QtObject {
    readonly property int rowPaddingX: 12
  }

  readonly property QtObject font: QtObject {
    // A family that certainly resolves under the offscreen platform. Naming a
    // real face would make the tests depend on the runner's font set.
    readonly property string family: "sans-serif"
    readonly property int caption: 10
    readonly property int bodySmall: 12
    readonly property int body: 14
    readonly property int subtitle: 16
    readonly property int title: 20
    readonly property int display: 28
    readonly property int icon: 14
  }
}
