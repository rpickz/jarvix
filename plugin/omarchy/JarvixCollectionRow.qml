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
//   detail      — a monospace line between subtitle and meta ("" hides it):
//                 a feed's current value, verbatim — the confirmation card's
//                 command-block treatment for values that must not be
//                 reworded.
//   meta        — third line, dimmed ("" hides it): freshness, dates, paths.
//   flagged     — a failing or incomplete entry. Colours the title urgent,
//                 but never alone: the caller must already say why in words
//                 (in subtitle or meta) before setting this.
//   actionLabel — labels the accent button ("" hides it); actionName is its
//                 accessible name; actionTriggered fires on click/Enter/Space.
//   action2Label — a second, quieter button below the first ("" hides it),
//                 for rows with two operations (Refresh now / Disable);
//                 action2Name and action2Triggered mirror the first's.
//   action3Label — a third button in the same quiet style ("" hides it), for
//                 rows with three operations (a fact's Edit / Pin / Forget);
//                 action3Name and action3Triggered mirror the second's.
//   interactive — makes the row itself focusable and clickable; activated
//                 fires. The sibling tickets wire this to their detail views
//                 (JarvixDetailPane) or expanders; a plain listing leaves it
//                 off.
//
// Display-only, like every Jarvix surface (ADR 0013): the row renders what
// it is given and signals what was pressed — it decides nothing. State is
// text plus emphasis, never colour alone; all sizes come from the shell's
// Style tokens.
Rectangle {
  id: row

  property string title: ""
  property string subtitle: ""
  property string detail: ""
  property string meta: ""
  property bool flagged: false
  property string actionLabel: ""
  property string actionName: actionLabel
  property string action2Label: ""
  property string action2Name: action2Label
  property string action3Label: ""
  property string action3Name: action3Label
  property bool interactive: false

  signal actionTriggered()
  signal action2Triggered()
  signal action3Triggered()
  signal activated()

  height: body.height + Style.space(16)
  radius: Style.cornerRadius
  color: Util.alpha(Color.popups.text, 0.06)
  // The focus ring: a colour *and* a thicker border, like the composer.
  border.color: row.activeFocus ? Color.accent : "transparent"
  border.width: row.activeFocus ? 2 : 0
  // The one `activeFocusOnTab` in this file that is still a binding, and it
  // has to be: `interactive` is not a visibility or an enabled-ness, so there
  // is nothing for Qt's focus chain to skip on. Carrying it on `enabled`
  // instead — the remedy the buttons below use — would disable the row's
  // children too, and a listing whose rows do not drill down still offers Run
  // and Forget.
  //
  // It is safe as written because callers set it per delegate from data the
  // delegate was built with, so it does not move under a live row. If one ever
  // makes it move, the runner will say so: scripts/qml-test.sh fails the suite
  // on "Cannot set activeFocusOnTab to false", and the answer then is a row
  // that is always in the tab chain and announces in words that it does not
  // open — not a re-bind of this property.
  activeFocusOnTab: interactive
  Accessible.role: interactive ? Accessible.Button : Accessible.ListItem
  Accessible.name: title
    + (subtitle !== "" ? ". " + subtitle : "")
    + (detail !== "" ? ". " + detail : "")
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
      width: parent.width - (actionColumn.visible ? actionColumn.width + Style.space(8) : 0)
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
        visible: row.detail !== ""
        text: row.detail
        width: parent.width
        wrapMode: Text.WrapAnywhere
        font.family: "monospace"
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

    Column {
      id: actionColumn
      visible: row.actionLabel !== "" || row.action2Label !== "" || row.action3Label !== ""
      anchors.verticalCenter: parent.verticalCenter
      spacing: Style.space(6)

      Rectangle {
        id: actionButton
        visible: row.actionLabel !== ""
        width: actionButtonLabel.width + Style.space(20)
        height: actionButtonLabel.height + Style.space(8)
        anchors.right: parent.right
        radius: Style.cornerRadius
        color: Util.alpha(Color.accent, actionButton.activeFocus ? 0.35 : 0.18)
        border.color: Color.accent
        border.width: actionButton.activeFocus ? 2 : 1
        // A constant for the life of the delegate, and it has to be (issue
        // #208, the rule #203 established on the confirmation card).
        //
        // Qt refuses to clear `activeFocusOnTab` on the item that currently
        // holds focus — it logs "Cannot set activeFocusOnTab to false once
        // item is the active focus item" and leaves the property as it was —
        // so the old binding, `visible`, made a button's tab-reachability
        // depend on where the keyboard happened to be when the row's labels
        // changed rather than on the row's state. A button somebody was
        // standing on when its label was cleared stayed in the tab chain for
        // the rest of the session, while the identical button in the row
        // below left as intended: two rows in the same state, two different
        // tab orders. These rows are how a routine gets disabled and a fact
        // gets forgotten, so the tab order moving underneath a keyboard user
        // mid-operation is the worst moment for it.
        //
        // Nothing about what is reachable changes. `visible` is already the
        // distinction Qt supports and already honours: its focus chain skips
        // whatever is invisible or disabled, so a button with no label was
        // never a tab stop and still is not. Only the property expressing it
        // has moved to the one Qt will not fight over.
        activeFocusOnTab: true
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

      // The quieter sibling: same behaviour, no accent fill — the row's
      // second operation must not compete with its first.
      Rectangle {
        id: action2Button
        visible: row.action2Label !== ""
        width: action2ButtonLabel.width + Style.space(20)
        height: action2ButtonLabel.height + Style.space(8)
        anchors.right: parent.right
        radius: Style.cornerRadius
        color: Util.alpha(Color.popups.text, action2Button.activeFocus ? 0.16 : 0.06)
        border.color: action2Button.activeFocus ? Color.accent : Util.alpha(Color.popups.text, 0.4)
        border.width: action2Button.activeFocus ? 2 : 1
        // Constant, for the reason spelled out on the first button above.
        activeFocusOnTab: true
        Accessible.role: Accessible.Button
        Accessible.name: row.action2Name
        Keys.onReturnPressed: row.action2Triggered()
        Keys.onSpacePressed: row.action2Triggered()
        Text {
          id: action2ButtonLabel
          anchors.centerIn: parent
          text: row.action2Label
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Color.popups.text
        }
        MouseArea { anchors.fill: parent; onClicked: row.action2Triggered() }
      }

      // The third slot, visually identical to the second: a row's extra
      // operations must all read quieter than its primary one.
      Rectangle {
        id: action3Button
        visible: row.action3Label !== ""
        width: action3ButtonLabel.width + Style.space(20)
        height: action3ButtonLabel.height + Style.space(8)
        anchors.right: parent.right
        radius: Style.cornerRadius
        color: Util.alpha(Color.popups.text, action3Button.activeFocus ? 0.16 : 0.06)
        border.color: action3Button.activeFocus ? Color.accent : Util.alpha(Color.popups.text, 0.4)
        border.width: action3Button.activeFocus ? 2 : 1
        // Constant, for the reason spelled out on the first button above.
        activeFocusOnTab: true
        Accessible.role: Accessible.Button
        Accessible.name: row.action3Name
        Keys.onReturnPressed: row.action3Triggered()
        Keys.onSpacePressed: row.action3Triggered()
        Text {
          id: action3ButtonLabel
          anchors.centerIn: parent
          text: row.action3Label
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Color.popups.text
        }
        MouseArea { anchors.fill: parent; onClicked: row.action3Triggered() }
      }
    }
  }
}
