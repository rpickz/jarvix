import QtQuick
import qs.Commons
import qs.Ui

// One labelled text field of a form dialog (issue #99): a label, an input
// box, the daemon's field-level problem underneath, and an optional hint line
// (the schedule field's daemon-computed next-fire preview). This is the
// shared field of the window's entry forms — the knowledge/memory forms
// (#100) reuse it — so every form field reads the same way.
//
// Contract:
//   label       — above the input; also its accessible name.
//   text        — the field's value (alias onto the input).
//   placeholder — dimmed sample text while empty ("08:30 mon-fri").
//   problem     — the daemon's message for this field ("" hides the line).
//                 Shown as text with a wording prefix, never colour alone.
//   hint        — a dimmed informational line ("" hides it), suppressed
//                 while a problem is showing so the two never contradict.
//   monospace   — for values that must not be reworded (a script's path).
//   edited(text)     — every keystroke; the caller updates its draft.
//   committed()      — focus left the field / Enter; the caller revalidates.
//
// Display-only (ADR 0013): the field renders what it is given and signals
// what was typed — every rule about the value lives in the daemon.
Column {
  id: field

  property string label: ""
  property alias text: input.text
  property string placeholder: ""
  property string problem: ""
  property string hint: ""
  property bool monospace: false

  signal edited(string text)
  signal committed()

  spacing: Style.space(4)

  Text {
    text: field.label
    width: parent.width
    wrapMode: Text.Wrap
    font.family: Style.font.family
    font.pixelSize: Style.font.subtitle
    color: Color.popups.text
  }

  Rectangle {
    width: parent.width
    height: input.height + Style.space(12)
    radius: Style.cornerRadius
    color: Util.alpha(Color.popups.text, 0.06)
    border.color: input.activeFocus ? Color.accent : Util.alpha(Color.popups.text, 0.4)
    border.width: input.activeFocus ? 2 : 1

    TextInput {
      id: input
      anchors.verticalCenter: parent.verticalCenter
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.margins: Style.space(8)
      activeFocusOnTab: true
      font.family: field.monospace ? "monospace" : Style.font.family
      font.pixelSize: Style.font.subtitle
      color: Color.popups.text
      clip: true
      Accessible.role: Accessible.EditableText
      Accessible.name: field.label
      onTextEdited: field.edited(text)
      onEditingFinished: field.committed()
    }

    Text {
      visible: input.text === "" && field.placeholder !== ""
      anchors.verticalCenter: parent.verticalCenter
      anchors.left: parent.left
      anchors.margins: Style.space(8)
      text: field.placeholder
      font.family: field.monospace ? "monospace" : Style.font.family
      font.pixelSize: Style.font.subtitle
      color: Util.alpha(Color.popups.text, 0.4)
    }
  }

  // The problem is the daemon's own sentence, verbatim, under the field it
  // names — words carry the state, the urgent colour only underlines it.
  Text {
    visible: field.problem !== ""
    text: "Problem: " + field.problem
    width: parent.width
    wrapMode: Text.Wrap
    font.family: Style.font.family
    font.pixelSize: Style.font.subtitle
    color: Color.urgent
  }

  Text {
    visible: field.hint !== "" && field.problem === ""
    text: field.hint
    width: parent.width
    wrapMode: Text.Wrap
    font.family: Style.font.family
    font.pixelSize: Style.font.subtitle
    color: Util.alpha(Color.popups.text, 0.7)
  }
}
