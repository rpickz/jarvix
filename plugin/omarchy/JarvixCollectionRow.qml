import QtQuick
import qs.Commons
import qs.Ui

// One row of a managed collection — a routine, a script, a knowledge feed, a
// remembered fact. This is the shared visual system for the window's
// collection tabs (issue #91): the sibling management tickets build their
// listings out of this row rather than inventing their own, so every
// collection reads the same way.
//
// Contract:
//   title       — first line, bold, wrapped (a name, or a fact's content).
//   subtitle    — second line, wrapped ("" hides it): phrases, modes.
//   meta        — third line, dimmed ("" hides it): freshness, dates, paths.
//   flagged     — a failing or incomplete entry. Colours the title urgent,
//                 but never alone: the caller must already say why in words
//                 (in subtitle or meta) before setting this.
//   actionLabel — labels the accent button ("" hides it); actionName is its
//                 accessible name; actionTriggered fires on click/Enter/Space.
//   interactive — makes the row itself focusable and clickable; activated
//                 fires. The sibling tickets wire this to their detail views
//                 (JarvixDetailPane); a plain listing leaves it off.
//
// Display-only, like every Jarvix surface (ADR 0013): the row renders what
// it is given and signals what was pressed — it decides nothing. State is
// text plus emphasis, never colour alone; all sizes come from the shell's
// Style tokens.
Rectangle {
  id: row

  property string title: ""
  property string subtitle: ""
  property string meta: ""
  property bool flagged: false
  property string actionLabel: ""
  property string actionName: actionLabel
  property bool interactive: false

  signal actionTriggered()
  signal activated()

  height: body.height + Style.space(16)
  radius: Style.cornerRadius
  color: Util.alpha(Color.popups.text, 0.06)
  // The focus ring: a colour *and* a thicker border, like the composer.
  border.color: row.activeFocus ? Color.accent : "transparent"
  border.width: row.activeFocus ? 2 : 0
  activeFocusOnTab: interactive
  Accessible.role: interactive ? Accessible.Button : Accessible.ListItem
  Accessible.name: title
    + (subtitle !== "" ? ". " + subtitle : "")
    + (meta !== "" ? ". " + meta : "")
  Keys.onReturnPressed: { if (interactive) row.activated() }
  Keys.onSpacePressed: { if (interactive) row.activated() }

  MouseArea {
    anchors.fill: parent
    enabled: row.interactive
    onClicked: row.activated()
  }

  Row {
    id: body
    anchors.verticalCenter: parent.verticalCenter
    anchors.left: parent.left
    anchors.right: parent.right
    anchors.margins: Style.space(10)
    spacing: Style.space(8)

    Column {
      width: parent.width - (actionButton.visible ? actionButton.width + Style.space(8) : 0)
      spacing: Style.space(2)

      Text {
        text: row.title
        width: parent.width
        wrapMode: Text.Wrap
        font.family: Style.font.family
        font.bold: true
        font.pixelSize: Style.font.subtitle
        // Urgent flags the entry but never carries it alone: the caller's
        // subtitle or meta says "failing", "incomplete" in words.
        color: row.flagged ? Color.urgent : Color.popups.text
      }
      Text {
        visible: row.subtitle !== ""
        text: row.subtitle
        width: parent.width
        wrapMode: Text.Wrap
        font.family: Style.font.family
        font.pixelSize: Style.font.subtitle
        color: Color.popups.text
      }
      Text {
        visible: row.meta !== ""
        text: row.meta
        width: parent.width
        wrapMode: Text.Wrap
        font.family: Style.font.family
        font.pixelSize: Style.font.subtitle
        color: Util.alpha(Color.popups.text, 0.7)
      }
    }

    Rectangle {
      id: actionButton
      visible: row.actionLabel !== ""
      width: actionButtonLabel.width + Style.space(20)
      height: actionButtonLabel.height + Style.space(8)
      anchors.verticalCenter: parent.verticalCenter
      radius: Style.cornerRadius
      color: Util.alpha(Color.accent, actionButton.activeFocus ? 0.35 : 0.18)
      border.color: Color.accent
      border.width: actionButton.activeFocus ? 2 : 1
      activeFocusOnTab: visible
      Accessible.role: Accessible.Button
      Accessible.name: row.actionName
      Keys.onReturnPressed: row.actionTriggered()
      Keys.onSpacePressed: row.actionTriggered()
      Text {
        id: actionButtonLabel
        anchors.centerIn: parent
        text: row.actionLabel
        font.family: Style.font.family
        font.pixelSize: Style.font.subtitle
        color: Color.popups.text
      }
      MouseArea { anchors.fill: parent; onClicked: row.actionTriggered() }
    }
  }
}
