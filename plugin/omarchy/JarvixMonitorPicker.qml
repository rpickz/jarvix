import QtQuick
import qs.Commons
import qs.Ui

// The monitor picker (#180): one labelled control that offers the screens
// plugged in right now — each with its size and the name the user gave it —
// plus "the current monitor", the reference that means "wherever I am".
//
// It is a cycler rather than a dropdown, which is this window's house style
// for a closed set (JarvixSettings' enum rows): the list is short, a cycler
// is one keyboard idiom with no popup to trap focus, and the selected option
// is always readable without opening anything.
//
// Contract:
//   label    — what is being chosen; also the accessible name.
//   options  — [{value, label}], in the order to cycle. value is what the
//              daemon is sent: a connector name, or "" for the current
//              monitor. The caller builds it from monitors.list, because
//              which screens exist is the daemon's answer, never this file's.
//   value    — the selected option's value; the caller owns it and updates
//              it from chosen().
//   problem  — the daemon's message for this field ("" hides the line).
//   hint     — an optional dimmed explainer ("" hides it).
//   chosen(value) — fires with the newly selected value.
//
// Display-only (ADR 0013): no screen name, no size and no reserved word is
// composed here. An option whose label this file invented would be an option
// that could disagree with what a routine resolves.
Column {
  id: picker

  property string label: ""
  property var options: []
  property string value: ""
  property string problem: ""
  property string hint: ""

  signal chosen(string value)

  spacing: Style.space(4)

  // indexOfValue is where the current selection sits, or 0 when the value is
  // not among the options — which happens honestly: a nickname pointing at a
  // screen that has just been unplugged is a value with no option, and
  // landing on the first one is better than rendering an empty control.
  function indexOfValue() {
    for (var i = 0; i < picker.options.length; i++) {
      if (String(picker.options[i].value) === picker.value) return i
    }
    return 0
  }

  function currentLabel() {
    if (picker.options.length === 0) return "no screens reported"
    var i = picker.indexOfValue()
    return String(picker.options[i].label || picker.options[i].value)
  }

  function step(delta) {
    if (picker.options.length === 0) return
    var next = (picker.indexOfValue() + delta + picker.options.length) % picker.options.length
    picker.chosen(String(picker.options[next].value))
  }

  Text {
    text: picker.label
    width: parent.width
    wrapMode: Text.Wrap
    font.family: Style.font.family
    font.pixelSize: Style.font.subtitle
    color: Util.alpha(Color.popups.text, 0.7)
  }

  Rectangle {
    id: box
    width: parent.width
    height: row.height + Style.space(12)
    radius: Style.cornerRadius
    color: Util.alpha(Color.popups.text, box.activeFocus ? 0.12 : 0.06)
    border.color: box.activeFocus ? Color.accent : Util.alpha(Color.popups.text, 0.4)
    border.width: box.activeFocus ? 2 : 1
    activeFocusOnTab: true
    Accessible.role: Accessible.ComboBox
    Accessible.name: picker.label
    Accessible.description: picker.currentLabel()
    Keys.onSpacePressed: picker.step(1)
    Keys.onReturnPressed: picker.step(1)
    Keys.onLeftPressed: picker.step(-1)
    Keys.onRightPressed: picker.step(1)

    Row {
      id: row
      anchors.verticalCenter: parent.verticalCenter
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.margins: Style.space(8)
      spacing: Style.space(8)

      Text {
        width: parent.width - arrows.width - Style.space(8)
        text: picker.currentLabel()
        wrapMode: Text.Wrap
        font.family: Style.font.family
        font.bold: true
        font.pixelSize: Style.font.subtitle
        color: Color.popups.text
      }
      Text {
        id: arrows
        // The affordance is a character, not a colour: the control has to
        // read as steppable in a screenshot and to a reader alike.
        text: "↔"
        font.family: Style.font.family
        font.pixelSize: Style.font.subtitle
        color: Util.alpha(Color.popups.text, 0.7)
      }
    }
    MouseArea { anchors.fill: parent; onClicked: picker.step(1) }
  }

  Text {
    visible: picker.hint !== "" && picker.problem === ""
    text: picker.hint
    width: parent.width
    wrapMode: Text.Wrap
    font.family: Style.font.family
    font.pixelSize: Style.font.subtitle
    color: Util.alpha(Color.popups.text, 0.7)
  }

  Text {
    visible: picker.problem !== ""
    text: "Problem: " + picker.problem
    width: parent.width
    wrapMode: Text.Wrap
    font.family: Style.font.family
    font.pixelSize: Style.font.subtitle
    color: Color.urgent
  }
}
