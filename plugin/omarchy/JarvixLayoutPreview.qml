import QtQuick
import qs.Commons
import qs.Ui

// The routine editor's preview diagram (#181, ADR 0059): one workspace's
// arrangement, drawn to the target screen's real proportions, with a
// proportional rectangle per window labelled by what it launches and the
// share it ends up with.
//
// It is presentation and nothing else. Every number here arrives in the
// daemon's `config.validate_entry` reply — the screen's aspect ratio, the
// inset the bars reserved, and each rectangle as fractions of the glass,
// computed by placement.Arrange against the same Monitor.Usable a run
// resolves against (ADR 0013). This file multiplies a fraction by the width
// of a box and renders text it was handed. There is no percentage parsed
// here, no monitor geometry read here, and no rule about what fits: an
// arrangement the daemon says cannot happen is not drawn at all, and its own
// message is shown in its place. A drawing assembled from arithmetic of this
// file's own would be a second claim about the same routine, and on the day
// the two disagreed the picture is the one the user would believe.
//
// Contract:
//   workspace — one entry of the reply's `preview.workspaces`: heading,
//               drawable, unavailable, aspect, usable, panels, problems,
//               summaries. The caller passes it through untouched.
//
// The text under the drawing is not a caption. The arrangement has to be
// conveyed in words as well as in the picture, and those words are the only
// channel left when the target screen is in a bag and there is nothing to
// draw at all.
Column {
  id: root

  property var workspace: ({})

  // aspect is the screen's own width-over-height, with a fallback that only
  // ever applies while nothing is being drawn — the drawing is bound to
  // `drawable`, which the daemon only sets when it computed a real one.
  readonly property real aspect: Number(root.workspace.aspect) > 0
    ? Number(root.workspace.aspect) : 1
  readonly property var usable: root.workspace.usable || ({})
  readonly property var panels: root.workspace.panels || []
  readonly property var problems: root.workspace.problems || []
  readonly property var summaries: root.workspace.summaries || []

  // describeArrangement joins the daemon's sentences for a screen reader, so
  // the drawing announces the same arrangement it shows.
  function describeArrangement() {
    var out = []
    for (var i = 0; i < root.summaries.length; i++) out.push(String(root.summaries[i]))
    return out.join(" ")
  }

  spacing: Style.space(6)

  Text {
    width: parent.width
    wrapMode: Text.Wrap
    text: String(root.workspace.heading || "")
    font.family: Style.font.family
    font.bold: true
    font.pixelSize: Style.font.subtitle
    color: Color.popups.text
  }

  // The refusal, where the drawing would have been. Words first, colour only
  // underlining them — the settings screen's rule, applied to a picture that
  // is deliberately absent.
  Text {
    visible: root.workspace.drawable !== true
    width: parent.width
    wrapMode: Text.Wrap
    text: "Not drawn: " + String(root.workspace.unavailable || "")
    font.family: Style.font.family
    font.pixelSize: Style.font.subtitle
    color: Color.urgent
  }
  Repeater {
    model: root.workspace.drawable === true ? 0 : root.problems.length
    delegate: Text {
      required property int index
      width: root.width
      wrapMode: Text.Wrap
      text: "Problem: " + String((root.problems[index] || {}).message || "")
      font.family: Style.font.family
      font.pixelSize: Style.font.subtitle
      color: Color.urgent
    }
  }

  // The screen. Its height comes from the daemon's aspect ratio, so an
  // ultrawide is drawn as an ultrawide and a share read off it is the share
  // the routine asked for.
  Rectangle {
    id: glass
    visible: root.workspace.drawable === true
    width: parent.width
    height: parent.width / root.aspect
    radius: Style.cornerRadius
    color: Util.alpha(Color.popups.text, 0.06)
    border.color: Util.alpha(Color.popups.text, 0.4)
    border.width: 1
    Accessible.role: Accessible.Graphic
    Accessible.name: String(root.workspace.heading || "")
    Accessible.description: root.describeArrangement()

    // The part of the screen windows may occupy. Drawn as an inset because
    // the difference between it and the glass is what the bars took, which is
    // the reason "66% of the screen" is not 66% of what you can see.
    Rectangle {
      x: glass.width * Number(root.usable.x || 0)
      y: glass.height * Number(root.usable.y || 0)
      width: glass.width * Number(root.usable.width || 0)
      height: glass.height * Number(root.usable.height || 0)
      color: "transparent"
      border.color: Util.alpha(Color.popups.text, 0.25)
      border.width: 1
    }

    Repeater {
      model: root.panels.length
      delegate: Rectangle {
        id: panelBox
        required property int index
        readonly property var panel: root.panels[index] || ({})

        x: glass.width * Number(panelBox.panel.x || 0)
        y: glass.height * Number(panelBox.panel.y || 0)
        width: glass.width * Number(panelBox.panel.width || 0)
        height: glass.height * Number(panelBox.panel.height || 0)
        radius: Style.cornerRadius
        // A window lifted out of the layout is drawn over the tiles and shows
        // it: same shape, stronger edge, so the two never read as one thing.
        color: Util.alpha(Color.accent, panelBox.panel.kind === "tiled" ? 0.16 : 0.28)
        border.color: Util.alpha(Color.accent, 0.8)
        border.width: panelBox.panel.kind === "tiled" ? 1 : 2
        clip: true

        Column {
          anchors.left: parent.left
          anchors.right: parent.right
          anchors.top: parent.top
          anchors.margins: Style.space(6)
          spacing: Style.space(2)

          Text {
            width: parent.width
            elide: Text.ElideRight
            text: (panelBox.index + 1) + ". " + String(panelBox.panel.label || "")
            font.family: Style.font.family
            font.bold: true
            font.pixelSize: Style.font.subtitle
            color: Color.popups.text
          }
          Text {
            width: parent.width
            elide: Text.ElideRight
            text: String(panelBox.panel.share || "")
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Util.alpha(Color.popups.text, 0.75)
          }
          Text {
            width: parent.width
            elide: Text.ElideRight
            text: String(panelBox.panel.size || "")
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Util.alpha(Color.popups.text, 0.6)
          }
        }
      }
    }
  }

  // What a rectangle cannot show: a share the layout never had occasion to
  // honour, a float with nowhere named, a promotion the layout owns. The
  // daemon's sentences, under the drawing they belong to.
  Repeater {
    model: root.workspace.drawable === true ? root.panels.length : 0
    delegate: Text {
      required property int index
      readonly property string note: String((root.panels[index] || {}).note || "")
      visible: note !== ""
      width: root.width
      wrapMode: Text.Wrap
      text: String((root.panels[index] || {}).label || "") + ": " + note
      font.family: Style.font.family
      font.pixelSize: Style.font.subtitle
      color: Util.alpha(Color.popups.text, 0.7)
    }
  }

  // The arrangement in words. Always, drawing or no drawing: the picture is
  // one way of reading this and not the only one.
  Repeater {
    model: root.summaries.length
    delegate: Text {
      required property int index
      width: root.width
      wrapMode: Text.Wrap
      text: "• " + String(root.summaries[index] || "")
      font.family: Style.font.family
      font.pixelSize: Style.font.subtitle
      color: Util.alpha(Color.popups.text, 0.85)
    }
  }
}
