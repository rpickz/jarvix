import QtQuick
import qs.Commons
import qs.Ui

// One record of a collection, opened from its listing — the shared detail
// scaffold (issue #91): a Back button, an optional primary action, a short
// note, and the caller's content filling the rest. The Library's read-only
// record view uses it today; the sibling management tickets fill it with
// routine/script/feed/fact detail so every drill-down reads the same way.
//
// Contract:
//   backRequested   — the Back button (always present).
//   actionLabel     — labels the accent action ("" hides it); actionName is
//                     its accessible name; actionTriggered fires on
//                     click/Enter/Space.
//   note            — a short status word beside the buttons ("Read-only").
//   content (default property) — items parented into the area below the
//                     header, which fills the remaining space.
//
// Display-only (ADR 0013): the pane renders what it is given and signals
// what was pressed.
Item {
  id: pane

  property string backName: "Back to the list"
  property string actionLabel: ""
  property string actionName: actionLabel
  property string note: ""

  signal backRequested()
  signal actionTriggered()

  default property alias content: contentArea.data

  Row {
    id: detailHeader
    anchors.top: parent.top
    anchors.left: parent.left
    anchors.right: parent.right
    spacing: Style.space(8)

    Rectangle {
      id: detailBackButton
      width: detailBackLabel.width + Style.space(20)
      height: detailBackLabel.height + Style.space(8)
      radius: Style.cornerRadius
      color: Util.alpha(Color.popups.text, detailBackButton.activeFocus ? 0.18 : 0.08)
      border.color: Util.alpha(Color.popups.text, 0.5)
      border.width: detailBackButton.activeFocus ? 2 : 1
      activeFocusOnTab: true
      Accessible.role: Accessible.Button
      Accessible.name: pane.backName
      Keys.onReturnPressed: pane.backRequested()
      Keys.onSpacePressed: pane.backRequested()
      Text {
        id: detailBackLabel
        anchors.centerIn: parent
        text: "Back"
        font.family: Style.font.family
        font.pixelSize: Style.font.subtitle
        color: Color.popups.text
      }
      MouseArea { anchors.fill: parent; onClicked: pane.backRequested() }
    }

    Rectangle {
      id: detailActionButton
      visible: pane.actionLabel !== ""
      width: detailActionLabel.width + Style.space(20)
      height: detailActionLabel.height + Style.space(8)
      radius: Style.cornerRadius
      color: Util.alpha(Color.accent, detailActionButton.activeFocus ? 0.3 : 0.15)
      border.color: Color.accent
      border.width: detailActionButton.activeFocus ? 2 : 1
      // Constant for the life of the pane, for the reason JarvixCollectionRow
      // spells out at length (issues #203/#208): Qt refuses to clear
      // `activeFocusOnTab` on the item that holds focus, so a binding here
      // described the tab chain by focus history rather than by state.
      //
      // This is the site the suite was actually shouting about. `visible` on a
      // child is the *effective* value, and this pane is a form that a whole
      // tab hides when it closes — so pressing Save and then leaving the form
      // flipped it while the keyboard was still on the button, four times a
      // run. Reachability is unchanged: Qt's focus chain already skips an
      // invisible item, so a pane with no action was never a tab stop.
      activeFocusOnTab: true
      Accessible.role: Accessible.Button
      Accessible.name: pane.actionName
      Keys.onReturnPressed: pane.actionTriggered()
      Keys.onSpacePressed: pane.actionTriggered()
      Text {
        id: detailActionLabel
        anchors.centerIn: parent
        text: pane.actionLabel
        font.family: Style.font.family
        font.pixelSize: Style.font.subtitle
        color: Color.popups.text
      }
      MouseArea { anchors.fill: parent; onClicked: pane.actionTriggered() }
    }

    Text {
      visible: pane.note !== ""
      text: pane.note
      anchors.verticalCenter: parent.verticalCenter
      font.family: Style.font.family
      font.pixelSize: Style.font.subtitle
      color: Util.alpha(Color.popups.text, 0.6)
    }
  }

  Item {
    id: contentArea
    anchors.top: detailHeader.bottom
    anchors.topMargin: Style.space(8)
    anchors.left: parent.left
    anchors.right: parent.right
    anchors.bottom: parent.bottom
  }
}
