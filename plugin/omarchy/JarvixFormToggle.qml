import QtQuick
import qs.Commons
import qs.Ui

// One labelled on/off switch of a form dialog (issue #99), the sibling of
// JarvixFormField: the state is written in words next to the control (the
// settings screen's rule — colour never carries a state alone), and an
// optional problem line sits underneath for daemon messages that key to this
// field (announce without a schedule).
//
// Contract:
//   label    — what the switch controls; also its accessible name.
//   checked  — the state; the caller owns it and updates it from toggled().
//   detail   — an optional dimmed explainer line ("" hides it).
//   problem  — the daemon's message for this field ("" hides the line).
//   toggled(checked) — fires on click/Enter/Space with the requested state.
//
// Display-only (ADR 0013): the toggle renders and signals; the caller's
// draft and the daemon decide everything.
Column {
  id: toggle

  property string label: ""
  property bool checked: false
  property string detail: ""
  property string problem: ""

  signal toggled(bool checked)

  spacing: Style.space(4)

  Rectangle {
    id: box
    width: parent.width
    height: row.height + Style.space(12)
    radius: Style.cornerRadius
    color: Util.alpha(Color.popups.text, box.activeFocus ? 0.12 : 0.06)
    border.color: box.activeFocus ? Color.accent : Util.alpha(Color.popups.text, 0.4)
    border.width: box.activeFocus ? 2 : 1
    activeFocusOnTab: true
    Accessible.role: Accessible.CheckBox
    Accessible.name: toggle.label
    Keys.onReturnPressed: toggle.toggled(!toggle.checked)
    Keys.onSpacePressed: toggle.toggled(!toggle.checked)

    Row {
      id: row
      anchors.verticalCenter: parent.verticalCenter
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.margins: Style.space(8)
      spacing: Style.space(8)

      Text {
        width: parent.width - stateText.width - Style.space(8)
        text: toggle.label
        wrapMode: Text.Wrap
        font.family: Style.font.family
        font.pixelSize: Style.font.subtitle
        color: Color.popups.text
      }
      Text {
        id: stateText
        text: toggle.checked ? "on" : "off"
        font.family: Style.font.family
        font.bold: true
        font.pixelSize: Style.font.subtitle
        color: Color.popups.text
      }
    }
    MouseArea { anchors.fill: parent; onClicked: toggle.toggled(!toggle.checked) }
  }

  Text {
    visible: toggle.detail !== "" && toggle.problem === ""
    text: toggle.detail
    width: parent.width
    wrapMode: Text.Wrap
    font.family: Style.font.family
    font.pixelSize: Style.font.subtitle
    color: Util.alpha(Color.popups.text, 0.7)
  }

  Text {
    visible: toggle.problem !== ""
    text: "Problem: " + toggle.problem
    width: parent.width
    wrapMode: Text.Wrap
    font.family: Style.font.family
    font.pixelSize: Style.font.subtitle
    color: Color.urgent
  }
}
