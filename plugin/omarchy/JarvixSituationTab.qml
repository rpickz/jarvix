import QtQuick
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Ui

// The Situation tab (#196, ADR 0061): the one answer to "where are we?",
// rendered in full — the headline the daemon composed, its up-front admission
// when it cannot cover the whole stretch, and one section per rank in the
// order the daemon put them in.
//
// Display-only like every Jarvix surface (ADR 0013): the ordering, the section
// headings, every sentence, and the label on every button were all decided
// daemon-side. This file lays out what it was handed and calls verbs.
//
// **Each line links to the thing it describes, through the provenance
// navigation (#168, ADR 0055) rather than a second mechanism.** The report
// arrives with a flat `sources` array of provenance references and a `link`
// index on every line that has one; the tab hands `sources` straight to
// `provenance.resolve` and reads each line's resolved item back at its own
// index — so it does no arithmetic and cannot pair a line with somebody else's
// subject. Following one is the same split the conversation window's panel
// makes: an action carrying a `tab` is this window's own navigation and is
// emitted as `navigate`, and anything else is the daemon's and goes to
// `provenance.open`.
//
// Like the Focus tab and the settings screen, the tab owns its own daemon
// socket, gated by `active` so a closed tab costs the daemon nothing. Its
// request ids live in the 600–699 range — reserved for this tab, so its
// traffic can never be mistaken for the Focus tab's (500–599) or the window's
// own (which allocates from 100 upwards).
Item {
  id: situationTab

  // The window sets this while the tab is shown; the socket only lives while
  // it is true.
  property bool active: false

  // The composed report, exactly as situation.get returned it, and the
  // resolved provenance items for its `sources`, in the same order.
  property var report: null
  property var links: []
  property string banner: ""

  // A tab action belongs to the window, which owns the tabs; the tab that
  // raised it has no way to reach another one. The window connects this to
  // its own revealIn, which is the same function the conversation panel's
  // links go through.
  signal navigate(string tab, string ref)

  readonly property int getRequestId: 600
  readonly property int resolveRequestId: 601
  readonly property int openRequestId: 602

  onActiveChanged: {
    if (active) {
      if (bridge.connected) refresh(false)
      else bridge.connected = true
    } else {
      bridge.connected = false
      banner = ""
      // Nothing is kept: the report is transient by design (ADR 0061), and a
      // tab reopened later must describe the machine then rather than replay
      // a remembered moment. The daemon's own cache decides whether that
      // costs a fresh read, which is where that decision belongs.
      report = null
      links = []
    }
  }

  // refresh asks for the report. fresh=true is the Refresh button and the only
  // thing that bypasses the daemon's cache — the ordinary open takes whatever
  // reading is current, which is what makes asking twice cheap (ADR 0061).
  function refresh(fresh) {
    bridge.write(JSON.stringify({ jsonrpc: "2.0", id: getRequestId,
      method: "situation.get", params: { fresh: fresh === true } }) + "\n")
  }

  function resolveLinks() {
    var sources = (situationTab.report && situationTab.report.sources) || []
    if (sources.length === 0) {
      situationTab.links = []
      return
    }
    bridge.write(JSON.stringify({ jsonrpc: "2.0", id: resolveRequestId,
      method: "provenance.resolve", params: { sources: sources } }) + "\n")
  }

  // linkFor returns the resolved item for one line, or null while the resolve
  // is still in flight or the line points at nothing. Lookup only — every word
  // and every button in the returned item is the daemon's.
  function linkFor(line) {
    if (!line || line.link === undefined) return null
    if (line.link < 0 || line.link >= situationTab.links.length) return null
    return situationTab.links[line.link]
  }

  function actionAt(line, index) {
    var item = situationTab.linkFor(line)
    if (!item) return null
    var actions = item.actions || []
    return index < actions.length ? actions[index] : null
  }

  function actionLabelAt(line, index) {
    var action = situationTab.actionAt(line, index)
    return action ? String(action.label || "") : ""
  }

  // runAction follows one line's link. A tab action is this window's own
  // navigation; anything else is the daemon's, because it leaves this process.
  // The same split the conversation window's provenance panel makes.
  function runAction(line, index) {
    var item = situationTab.linkFor(line)
    var action = situationTab.actionAt(line, index)
    if (!item || !action) return
    var tab = String(action.tab || "")
    if (tab !== "") {
      situationTab.navigate(tab, String(action.ref || ""))
      return
    }
    if (!bridge.connected) return
    situationTab.banner = ""
    bridge.write(JSON.stringify({ jsonrpc: "2.0", id: openRequestId,
      method: "provenance.open",
      params: { kind: String(item.kind || ""), ref: String(item.ref || ""),
        action: String(action.id || "") } }) + "\n")
  }

  Socket {
    id: bridge
    path: Quickshell.env("XDG_RUNTIME_DIR") + "/jarvix.sock"

    parser: SplitParser {
      onRead: function(line) {
        var frame
        try { frame = JSON.parse(line) } catch (e) { return }
        if (frame.method) return
        if (frame.id === undefined) return
        if (frame.id === situationTab.getRequestId) {
          if (frame.error) {
            situationTab.banner = String(frame.error.message || "the report could not be read")
          } else {
            situationTab.banner = ""
            situationTab.report = frame.result || null
            situationTab.links = []
            situationTab.resolveLinks()
          }
          return
        }
        if (frame.id === situationTab.resolveRequestId) {
          // A resolve that fails costs the links and not the report: the
          // lines are already worded, and a tab that blanked itself because
          // it could not draw a button would be hiding the answer.
          if (!frame.error) situationTab.links = frame.result.items || []
          return
        }
        if (frame.id === situationTab.openRequestId) {
          if (frame.error) {
            situationTab.banner = String(frame.error.message || "that could not be opened")
          }
        }
      }
    }

    onConnectionStateChanged: {
      if (connected) situationTab.refresh(false)
      else if (situationTab.active) retrySituation.start()
    }
  }

  Timer {
    id: retrySituation
    interval: 2000
    repeat: false
    onTriggered: { if (situationTab.active && !bridge.connected) bridge.connected = true }
  }

  // A refused action, in words (never colour alone).
  Text {
    id: situationBanner
    visible: situationTab.banner !== ""
    text: situationTab.banner
    anchors.top: parent.top
    anchors.left: parent.left
    anchors.right: parent.right
    wrapMode: Text.Wrap
    font.family: Style.font.family
    font.pixelSize: Style.font.subtitle
    color: Color.urgent
  }

  Row {
    id: situationBar
    anchors.top: situationBanner.visible ? situationBanner.bottom : parent.top
    anchors.topMargin: situationBanner.visible ? Style.space(8) : 0
    anchors.left: parent.left
    anchors.right: parent.right
    height: situationRefresh.height
    spacing: Style.space(8)

    Text {
      width: parent.width - situationRefresh.width - Style.space(8)
      anchors.verticalCenter: parent.verticalCenter
      // The age arrives pre-worded on the shared spoken scale, so this does
      // no clock arithmetic (ADR 0013).
      text: situationTab.report
        ? "Read " + String(situationTab.report.age_spoken || "") : ""
      wrapMode: Text.Wrap
      font.family: Style.font.family
      font.pixelSize: Style.font.subtitle
      color: Util.alpha(Color.popups.text, 0.7)
    }

    JarvixFormButton {
      id: situationRefresh
      label: "Refresh"
      name: "Read the machine again"
      onClicked: situationTab.refresh(true)
    }
  }

  JarvixEmptyState {
    visible: situationTab.report === null
    anchors.centerIn: parent
    width: parent.width
    text: "Reading the machine…"
  }

  Flickable {
    visible: situationTab.report !== null
    anchors.top: situationBar.bottom
    anchors.topMargin: Style.space(12)
    anchors.left: parent.left
    anchors.right: parent.right
    anchors.bottom: parent.bottom
    clip: true
    contentWidth: width
    contentHeight: situationColumn.height + Style.space(12)

    Column {
      id: situationColumn
      width: parent.width
      spacing: Style.space(12)

      Text {
        width: parent.width
        visible: text !== ""
        text: situationTab.report ? String(situationTab.report.headline || "") : ""
        wrapMode: Text.Wrap
        font.family: Style.font.family
        font.bold: true
        font.pixelSize: Style.font.subtitle
        color: Color.popups.text
      }

      // The daemon's admission that it cannot account for the whole stretch
      // since the user last looked (#190's discipline, ADR 0061). Directly
      // under the headline and above every section, which is the whole point
      // of it: read after the sections it would be qualifying an account the
      // reader has already believed. Worded daemon-side like everything here.
      Text {
        width: parent.width
        visible: text !== ""
        text: situationTab.report ? String(situationTab.report.caveat || "") : ""
        wrapMode: Text.Wrap
        font.family: Style.font.family
        font.italic: true
        font.pixelSize: Style.font.subtitle
        color: Util.alpha(Color.popups.text, 0.7)
      }

      Repeater {
        model: situationTab.report && situationTab.report.sections
          ? situationTab.report.sections : []

        Column {
          required property var modelData
          width: situationColumn.width
          spacing: Style.space(6)

          Text {
            text: String(modelData.title || "")
            font.family: Style.font.family
            font.bold: true
            font.pixelSize: Style.font.subtitle
            color: Util.alpha(Color.popups.text, 0.7)
          }

          // One row per line, on the shared collection row, so a situation
          // line reads exactly like a fact or a feed does in its own tab —
          // and gets the row's keyboard reachability and accessible naming
          // for free rather than reimplementing them.
          Repeater {
            model: modelData.lines || []
            delegate: JarvixCollectionRow {
              required property var modelData
              width: situationColumn.width
              title: String(modelData.text || "")
              // The liveness of what a line points at is the daemon's answer
              // too: it looks in the live stores, and this renders `gone` and
              // `note`. A line whose subject has vanished says so and offers
              // no button — never a dead affordance, never a silent no-op.
              meta: {
                var item = situationTab.linkFor(modelData)
                return item ? String(item.note || "") : ""
              }
              flagged: {
                var item = situationTab.linkFor(modelData)
                return item ? Boolean(item.gone) : false
              }
              actionLabel: situationTab.actionLabelAt(modelData, 0)
              actionName: actionLabel + " for " + title
              onActionTriggered: situationTab.runAction(modelData, 0)
              action2Label: situationTab.actionLabelAt(modelData, 1)
              action2Name: action2Label + " for " + title
              onAction2Triggered: situationTab.runAction(modelData, 1)
            }
          }
        }
      }
    }
  }
}
