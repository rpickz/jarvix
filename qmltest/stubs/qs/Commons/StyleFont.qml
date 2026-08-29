import QtQuick

// The type of `Style.font` — the theme's type ramp, stubbed.
//
// A named type rather than the inline `QtObject { … }` the real theme writes,
// and the reason is qmllint (issue #208). An inline QtObject is typed as bare
// `QObject`, so every member read off it is "not found on type QObject" and a
// harness file cannot name a size at all without the lint going red. As a
// declared type the members resolve, which means a test may say
// `Style.font.bodySmall` — and, better, that a harness file naming a token the
// theme does not have fails the lint instead of the eye.
//
// The numbers are the shipped ramp's shape rather than its exact values, and
// no test asserts on them: a test that cared what `bodySmall` returns would be
// testing Omarchy's theme, which #174 puts out of scope. What they must be is
// non-zero, sane, and ordered — a size of 0 makes every Text item zero-high,
// and a keyboard-reachability test on a window whose controls have no size
// proves nothing.
QtObject {
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
