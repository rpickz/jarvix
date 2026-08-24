import QtQuick
import qs.Commons
import qs.Ui

// One small button of a form dialog (issue #99): the window's standard
// button rectangle — accent for a primary action, quiet for the rest — as a
// component, because the entry forms need a dozen of them (add/remove
// phrase, step reorder, delete confirm) and twelve hand-rolled rectangles
// would drift. Same behaviour as the buttons in JarvixCollectionRow: focus
// ring, Enter/Space, an accessible name.
Rectangle {
  id: button

  property string label: ""
  property string name: label
  property bool accent: false

  signal clicked()

  width: buttonLabel.width + Style.space(20)
  height: buttonLabel.height + Style.space(8)
  radius: Style.cornerRadius
  color: button.accent
    ? Util.alpha(Color.accent, button.activeFocus ? 0.35 : 0.18)
    : Util.alpha(Color.popups.text, button.activeFocus ? 0.16 : 0.06)
  border.color: button.accent || button.activeFocus
    ? Color.accent : Util.alpha(Color.popups.text, 0.4)
  border.width: button.activeFocus ? 2 : 1
  activeFocusOnTab: true
  Accessible.role: Accessible.Button
  Accessible.name: button.name
  Keys.onReturnPressed: button.clicked()
  Keys.onSpacePressed: button.clicked()

  Text {
    id: buttonLabel
    anchors.centerIn: parent
    text: button.label
    font.family: Style.font.family
    font.pixelSize: Style.font.subtitle
    color: Color.popups.text
  }

  MouseArea { anchors.fill: parent; onClicked: button.clicked() }
}
