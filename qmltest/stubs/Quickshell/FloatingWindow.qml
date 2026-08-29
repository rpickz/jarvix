import QtQuick

// FloatingWindow, stubbed as a plain Item.
//
// The real type is a Wayland toplevel; under the offscreen platform there is
// no compositor to give it one, and a test that needed a mapped surface would
// be testing Quickshell rather than Jarvix (explicitly out of scope for #174).
// What the window's *logic* needs from its root is only that it be a visual
// item with a size, so children that `anchors.fill: parent` lay out and
// `activeFocusOnTab` chains resolve.
//
// Sizing is the one subtlety worth writing down: the real FloatingWindow
// derives its geometry from `implicitWidth`/`implicitHeight`, and an Item does
// not. Without the two bindings below every child anchored to the parent
// collapses to 0x0, `Keys` handlers still fire but nothing is ever visible,
// and a test asserting "the primary control is reachable" would pass against
// a window of no size. Binding them here makes the stub's layout behave the
// way the real one does.
Item {
  id: root

  width: implicitWidth
  height: implicitHeight

  // The real type paints its own background from `color`. Declared so the
  // window's `color: Color.background` assignment resolves; nothing reads it.
  property color color: "transparent"

  // The toplevel's title. The window sets it and the live verification
  // scripts match on it; here it only needs to exist.
  property string title: ""

  // FloatingWindow is not visible until something opens it, and the window's
  // own `visible: false` plus the overlay's open/close path depend on that
  // default surviving. Item.visible already carries the semantics.
}
