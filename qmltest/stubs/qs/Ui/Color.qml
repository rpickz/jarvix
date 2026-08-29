pragma Singleton

import QtQuick

// Color — Omarchy's palette singleton, stubbed.
//
// Every colour here is deliberately *distinct*, and that is the whole point.
// One acceptance criterion of #174 is that state is conveyed by text and not
// by colour alone; the way to test that is to strip the colours out and check
// the words still say it. A stub palette that painted everything the same
// would make such a test pass for the wrong reason, and one that reused the
// theme's real values would make it a test of the theme. Distinct, arbitrary,
// and asserted on by nothing.
QtObject {
  readonly property color background: "#101014"
  readonly property color foreground: "#e6e6ea"
  readonly property color accent: "#4c8dff"
  readonly property color urgent: "#ff5c5c"

  readonly property QtObject popups: QtObject {
    readonly property color text: "#e6e6ea"
    readonly property color border: "#3a3a44"
    readonly property color background: "#18181f"
  }
}
