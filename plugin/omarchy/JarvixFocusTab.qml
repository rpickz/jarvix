import QtQuick
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Ui

// The Focus tab (#123, ADR 0041): the focus threads — each with its anchors,
// parked thoughts, check-in interval, and last activity — plus the live
// timeboxed session, rendered from focus.list and refreshed by focus.changed
// pushes. Display-only like every Jarvix surface (ADR 0013): the ordering
// (active first), the spoken-style ages, the anchor-gone verdicts, and every
// sentence were decided daemon-side; this file renders fields and calls
// verbs.
//
// Like the settings screen, the tab owns its own daemon socket, gated by
// `active` so a closed tab costs the daemon nothing. Its request ids live in
// the 500–599 range — reserved for this tab, so its traffic can never be
// mistaken for the window's own (which allocates from 100 upwards) even if
// the two surfaces ever share a connection.
Item {
  id: focusTab

  // The window sets this while the tab is shown; the socket only lives
  // while it is true.
  property bool active: false

  property var threads: []
  property var session: null
  // "" shows the listing; a thread id shows that thread's parked thoughts.
  property string detailId: ""
  property string banner: ""

  readonly property int listRequestId: 500
  readonly property int switchRequestId: 501
  readonly property int endRequestId: 502
  readonly property int sessionEndRequestId: 503

  onActiveChanged: {
    if (active) {
      if (bridge.connected) refresh()
      else bridge.connected = true
    } else {
      bridge.connected = false
      detailId = ""
      banner = ""
    }
  }

  function refresh() {
    bridge.write(JSON.stringify({ jsonrpc: "2.0", id: listRequestId,
      method: "focus.list" }) + "\n")
  }

  function switchTo(id) {
    bridge.write(JSON.stringify({ jsonrpc: "2.0", id: switchRequestId,
      method: "focus.switch", params: { thread: id } }) + "\n")
  }

  function endThread(id) {
    bridge.write(JSON.stringify({ jsonrpc: "2.0", id: endRequestId,
      method: "focus.end", params: { thread: id } }) + "\n")
  }

  function endSession() {
    bridge.write(JSON.stringify({ jsonrpc: "2.0", id: sessionEndRequestId,
      method: "focus.session.end" }) + "\n")
  }

  function detailThread() {
    for (var i = 0; i < threads.length; i++) {
      if (threads[i].id === detailId) return threads[i]
    }
    return null
  }

  // Label assembly only — every phrase in these lines arrived from the
  // daemon or is a fixed caption; nothing is derived or computed here.
  function anchorsLine(t) {
    var anchors = t.anchors || []
    if (anchors.length === 0) return ""
    var parts = []
    for (var i = 0; i < anchors.length; i++) {
      var label = String(anchors[i].app || "")
      if (anchors[i].title) label += " — " + anchors[i].title
      if (anchors[i].gone) label += " (gone)"
      parts.push(label)
    }
    return "Anchored to " + parts.join(", ")
  }

  function metaLine(t) {
    var parts = []
    var parked = Number(t.parked_count || 0)
    parts.push(parked === 1 ? "1 parked thought" : parked + " parked thoughts")
    if (t.last_activity_spoken) parts.push("touched " + t.last_activity_spoken)
    if (t.remind_every_min) parts.push("check-in every " + t.remind_every_min + " min")
    return parts.join(" · ")
  }

  function sessionLine() {
    if (!session) return ""
    if (session.phase === "closing") {
      return "Focus session on " + session.thread_name + " is over — keep focusing, or take a break?"
    }
    var mins = Math.max(1, Math.ceil(Number(session.remaining_sec || 0) / 60))
    return "Focusing on " + session.thread_name + " — " + mins + " min left of " + session.minutes
  }

  Socket {
    id: bridge
    path: Quickshell.env("XDG_RUNTIME_DIR") + "/jarvix.sock"

    parser: SplitParser {
      onRead: function(line) {
        var frame
        try { frame = JSON.parse(line) } catch (e) { return }
        if (frame.method) {
          // The daemon pushes focus.changed on every mutation — voice, verb,
          // or the clock — so the tab re-requests rather than guessing what
          // the change meant (ADR 0013).
          if (frame.method === "focus.changed") focusTab.refresh()
          return
        }
        if (frame.id === undefined) return
        if (frame.id === focusTab.listRequestId) {
          if (frame.result) {
            focusTab.threads = frame.result.threads || []
            focusTab.session = frame.result.session || null
            if (focusTab.detailId !== "" && !focusTab.detailThread()) focusTab.detailId = ""
          }
          return
        }
        if (frame.id === focusTab.switchRequestId ||
            frame.id === focusTab.endRequestId ||
            frame.id === focusTab.sessionEndRequestId) {
          if (frame.error) {
            focusTab.banner = String(frame.error.message || "the focus action failed")
          } else {
            focusTab.banner = ""
          }
          // Success needs no handling: focus.changed triggers the refresh.
        }
      }
    }

    onConnectionStateChanged: {
      if (connected) focusTab.refresh()
      else if (focusTab.active) retryFocus.start()
    }
  }

  Timer {
    id: retryFocus
    interval: 2000
    repeat: false
    onTriggered: { if (focusTab.active && !bridge.connected) bridge.connected = true }
  }

  // A refused action, in words (never colour alone).
  Text {
    id: focusBanner
    visible: focusTab.banner !== ""
    text: focusTab.banner
    anchors.top: parent.top
    anchors.left: parent.left
    anchors.right: parent.right
    wrapMode: Text.Wrap
    font.family: Style.font.family
    font.pixelSize: Style.font.subtitle
    color: Color.urgent
  }

  // The live timebox, one glance from anywhere in the tab.
  Rectangle {
    id: sessionBanner
    visible: focusTab.session !== null && focusTab.detailId === ""
    anchors.top: focusBanner.visible ? focusBanner.bottom : parent.top
    anchors.topMargin: focusBanner.visible ? Style.space(8) : 0
    anchors.left: parent.left
    anchors.right: parent.right
    height: visible ? sessionRow.height + Style.space(16) : 0
    radius: Style.cornerRadius
    color: Util.alpha(Color.accent, 0.12)
    border.color: Color.accent
    border.width: 1

    Row {
      id: sessionRow
      anchors.verticalCenter: parent.verticalCenter
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.margins: Style.space(10)
      spacing: Style.space(8)

      Text {
        width: parent.width - endSessionButton.width - Style.space(8)
        anchors.verticalCenter: parent.verticalCenter
        text: focusTab.sessionLine()
        wrapMode: Text.Wrap
        font.family: Style.font.family
        font.bold: true
        font.pixelSize: Style.font.subtitle
        color: Color.popups.text
      }
      JarvixFormButton {
        id: endSessionButton
        label: "End session"
        name: "End the focus session"
        onClicked: focusTab.endSession()
      }
    }
  }

  JarvixEmptyState {
    visible: focusTab.threads.length === 0 && focusTab.detailId === ""
    anchors.centerIn: parent
    width: parent.width
    text: "No focus threads yet — say “new thread”, then a name.\nAdd “with this window” to anchor it to what you're looking at."
  }

  ListView {
    id: threadList
    visible: focusTab.threads.length > 0 && focusTab.detailId === ""
    anchors.top: sessionBanner.visible ? sessionBanner.bottom
      : (focusBanner.visible ? focusBanner.bottom : parent.top)
    anchors.topMargin: (sessionBanner.visible || focusBanner.visible) ? Style.space(10) : 0
    anchors.left: parent.left
    anchors.right: parent.right
    anchors.bottom: parent.bottom
    clip: true
    spacing: Style.space(10)
    model: focusTab.threads

    delegate: JarvixCollectionRow {
      required property var modelData
      width: threadList.width
      title: modelData.name + (modelData.active ? " — active" : "")
      subtitle: focusTab.anchorsLine(modelData)
      meta: focusTab.metaLine(modelData)
      // The row opens the parked-thoughts detail.
      interactive: true
      onActivated: focusTab.detailId = modelData.id
      actionLabel: modelData.active ? "" : "Switch"
      actionName: "Switch to the " + modelData.name + " thread"
      onActionTriggered: focusTab.switchTo(modelData.id)
      action2Label: "End"
      action2Name: "End the " + modelData.name + " thread"
      onAction2Triggered: focusTab.endThread(modelData.id)
    }
  }

  // One thread's parked thoughts, on the shared detail scaffold.
  JarvixDetailPane {
    visible: focusTab.detailId !== ""
    anchors.fill: parent
    note: focusTab.detailThread() ? focusTab.detailThread().name : ""
    onBackRequested: focusTab.detailId = ""

    JarvixEmptyState {
      visible: {
        var t = focusTab.detailThread()
        return t === null || !t.parked || t.parked.length === 0
      }
      anchors.centerIn: parent
      width: parent.width
      text: "Nothing parked on this thread — say “later”, then the thought."
    }

    ListView {
      anchors.fill: parent
      clip: true
      spacing: Style.space(8)
      model: {
        var t = focusTab.detailThread()
        return t && t.parked ? t.parked : []
      }
      delegate: JarvixCollectionRow {
        required property var modelData
        width: parent ? parent.width : 0
        title: modelData.text
        meta: String(modelData.at || "").substring(0, 16).replace("T", " ")
      }
    }
  }
}
