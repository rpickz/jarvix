import QtQuick
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Ui

// Jarvix conversation window: the persistent, reviewable surface for the
// current conversation. Like the overlay it is a thin view over jarvixd —
// it renders the `conversation.get` snapshot plus live IPC events, and holds
// no session logic of its own (docs/architecture.md, ADR 0013).
//
// It is a normal toplevel window, opened from a notification click or
// `jarvix window` via the IpcHandler in JarvixOverlay.qml. Its socket is
// connected only while the window is visible, so a closed window costs the
// daemon nothing and a stalled one is just another slow client whose events
// the bus drops.
FloatingWindow {
  id: win
  visible: false
  title: "Jarvix"
  implicitWidth: 520
  implicitHeight: 640
  color: Color.background

  // The settings screen replaces the conversation while open (issue #9);
  // Escape closes it before closing the window.
  property bool settingsOpen: false

  // --- daemon state -------------------------------------------------------
  property bool socketReady: false
  property string sessionState: "idle"
  property string errorStage: ""
  property string errorMessage: ""
  // True while assistant.delta events are building the newest turn.
  property bool assistantStreaming: false

  ListModel { id: turns } // { role: "user"|"assistant", text: string }

  function openWindow() { visible = true }
  function closeWindow() { visible = false }
  function toggleWindow() { visible = !visible }

  onVisibleChanged: {
    if (visible) {
      if (daemon.connected) requestConversation()
      else daemon.connected = true
    } else {
      daemon.connected = false
    }
  }

  function requestConversation() {
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: 1, method: "conversation.get" }) + "\n")
  }

  // loadSnapshot replaces the model with the daemon's authoritative view;
  // events append incrementally from here on.
  function loadSnapshot(result) {
    turns.clear()
    var list = result.turns || []
    for (var i = 0; i < list.length; i++) {
      turns.append({ role: String(list[i].role), text: String(list[i].text) })
    }
    sessionState = String(result.state || "idle")
    assistantStreaming = false
  }

  function handleEvent(method, params) {
    switch (method) {
    case "state.changed":
      var next = String(params.state || "idle")
      if (sessionState === "idle" && next !== "idle") {
        // A new session begins: the previous error is history now.
        errorStage = ""
        errorMessage = ""
      }
      sessionState = next
      break
    case "transcript.final":
      // Fires once per session; a snapshot taken after it already contains
      // the turn and the event will not repeat, so appending cannot double.
      turns.append({ role: "user", text: String(params.text || "") })
      break
    case "assistant.delta":
      if (!assistantStreaming) {
        turns.append({ role: "assistant", text: "" })
        assistantStreaming = true
      }
      var chunk = String(params.content || "")
      if (chunk !== "") {
        turns.setProperty(turns.count - 1, "text", turns.get(turns.count - 1).text + chunk)
      }
      break
    case "assistant.finished":
      // The full text is authoritative — it heals any deltas the bus
      // dropped while we were a slow client.
      var full = String(params.content || "")
      if (assistantStreaming) {
        if (full === "" && turns.get(turns.count - 1).text === "") {
          turns.remove(turns.count - 1) // empty answer: the error event explains
        } else if (full !== "") {
          turns.setProperty(turns.count - 1, "text", full)
        }
      } else if (full !== "") {
        turns.append({ role: "assistant", text: full })
      }
      assistantStreaming = false
      break
    case "error":
      errorStage = String(params.stage || "")
      errorMessage = String(params.message || "something went wrong")
      assistantStreaming = false
      break
    case "session.finished":
    case "session.cancelled":
      assistantStreaming = false
      break
    }
  }

  // --- daemon connection --------------------------------------------------
  Socket {
    id: daemon
    path: Quickshell.env("XDG_RUNTIME_DIR") + "/jarvix.sock"

    parser: SplitParser {
      onRead: function(line) {
        var frame
        try { frame = JSON.parse(line) } catch (e) { return }
        if (frame.method) {
          win.handleEvent(frame.method, frame.params || {})
        } else if (frame.id === 1 && frame.result) {
          win.loadSnapshot(frame.result)
        }
      }
    }

    onConnectionStateChanged: {
      win.socketReady = connected
      if (connected) {
        win.requestConversation()
      } else {
        win.sessionState = "idle"
        win.assistantStreaming = false
        if (win.visible) retry.start()
      }
    }
  }

  Timer {
    id: retry
    interval: 2000
    repeat: false
    onTriggered: { if (win.visible && !daemon.connected) daemon.connected = true }
  }

  // --- presentation -------------------------------------------------------
  // State is conveyed as text, never colour alone; all sizes come from the
  // shell's Style tokens so the user's font scale is respected.
  readonly property string stateLabel: {
    switch (sessionState) {
    case "listening":    return "Listening"
    case "transcribing": return "Transcribing"
    case "thinking":     return "Thinking"
    case "responding":   return "Responding"
    case "speaking":     return "Speaking"
    case "cancelling":   return "Cancelling"
    case "error":        return "Error"
    default:             return "Idle"
    }
  }

  Item {
    anchors.fill: parent
    anchors.margins: Style.space(16)

    Row {
      id: header
      spacing: Style.space(8)
      anchors.top: parent.top
      anchors.left: parent.left
      anchors.right: parent.right

      Text {
        text: "Jarvix"
        font.family: Style.font.family
        font.bold: true
        font.pixelSize: Style.font.title
        color: Color.popups.text
      }
      Text {
        visible: win.socketReady
        text: "— " + win.stateLabel
        anchors.verticalCenter: parent.verticalCenter
        font.family: Style.font.family
        font.pixelSize: Style.font.subtitle
        color: Util.alpha(Color.popups.text, 0.7)
      }

      // Settings toggle: keyboard-reachable, state as text. The screen is a
      // thin client of the daemon, so it is only offered while connected.
      Rectangle {
        id: settingsButton
        visible: win.socketReady
        width: settingsButtonText.width + Style.space(20)
        height: settingsButtonText.height + Style.space(8)
        anchors.verticalCenter: parent.verticalCenter
        radius: Style.cornerRadius
        color: Util.alpha(Color.popups.text, settingsButton.activeFocus ? 0.18 : 0.08)
        border.color: Util.alpha(Color.popups.text, 0.5)
        border.width: settingsButton.activeFocus ? 2 : 1
        activeFocusOnTab: true
        Accessible.role: Accessible.Button
        Accessible.name: win.settingsOpen ? "Back to conversation" : "Open settings"
        Keys.onReturnPressed: win.settingsOpen = !win.settingsOpen
        Keys.onSpacePressed: win.settingsOpen = !win.settingsOpen
        Text {
          id: settingsButtonText
          anchors.centerIn: parent
          text: win.settingsOpen ? "Conversation" : "Settings"
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Color.popups.text
        }
        MouseArea { anchors.fill: parent; onClicked: win.settingsOpen = !win.settingsOpen }
      }
    }

    // Daemon unreachable: say so instead of hanging on an empty view.
    Column {
      visible: !win.socketReady
      anchors.centerIn: parent
      spacing: Style.space(8)
      width: parent.width

      Text {
        text: "Jarvix daemon is not running"
        anchors.horizontalCenter: parent.horizontalCenter
        font.family: Style.font.family
        font.bold: true
        font.pixelSize: Style.font.title
        color: Color.popups.text
      }
      Text {
        text: "Start it with: systemctl --user start jarvixd"
        anchors.horizontalCenter: parent.horizontalCenter
        font.family: Style.font.family
        font.pixelSize: Style.font.subtitle
        color: Util.alpha(Color.popups.text, 0.7)
      }
    }

    Text {
      visible: win.socketReady && !win.settingsOpen && turns.count === 0
      anchors.centerIn: parent
      text: "No conversation yet — hold Super+Alt+V and speak."
      font.family: Style.font.family
      font.pixelSize: Style.font.subtitle
      color: Util.alpha(Color.popups.text, 0.7)
    }

    // The settings screen shares the conversation's content area.
    JarvixSettings {
      id: settingsScreen
      visible: win.settingsOpen
      active: win.visible && win.settingsOpen && win.socketReady
      anchors.top: header.bottom
      anchors.topMargin: Style.space(12)
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.bottom: errorBanner.visible ? errorBanner.top : parent.bottom
      anchors.bottomMargin: errorBanner.visible ? Style.space(12) : 0
    }

    ListView {
      id: list
      visible: win.socketReady && !win.settingsOpen
      anchors.top: header.bottom
      anchors.topMargin: Style.space(12)
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.bottom: errorBanner.visible ? errorBanner.top : parent.bottom
      anchors.bottomMargin: errorBanner.visible ? Style.space(12) : 0
      clip: true
      spacing: Style.space(14)
      model: turns

      // Keep the newest turn visible while it streams, but stop following
      // the moment the user scrolls back to reread something.
      property bool followTail: true
      onMovementEnded: followTail = atYEnd
      onContentHeightChanged: { if (followTail) positionViewAtEnd() }
      onCountChanged: { if (followTail) positionViewAtEnd() }

      delegate: Column {
        width: list.width
        spacing: Style.space(4)

        Text {
          text: model.role === "user" ? "You" : "Jarvix"
          font.family: Style.font.family
          font.bold: true
          font.pixelSize: Style.font.subtitle
          color: model.role === "user"
            ? Util.alpha(Color.popups.text, 0.7)
            : Color.accent
        }
        Text {
          text: model.text
          width: parent.width
          wrapMode: Text.Wrap
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Color.popups.text
        }
      }
    }

    // Failures are stated in words — stage and message — not colour alone.
    Rectangle {
      id: errorBanner
      visible: win.socketReady && win.errorMessage !== ""
      anchors.bottom: parent.bottom
      anchors.left: parent.left
      anchors.right: parent.right
      height: errorText.height + Style.space(20)
      radius: Style.cornerRadius
      color: Util.alpha(Color.urgent, 0.12)
      border.color: Color.urgent
      border.width: 1

      Column {
        id: errorText
        anchors.verticalCenter: parent.verticalCenter
        anchors.left: parent.left
        anchors.right: parent.right
        anchors.margins: Style.space(10)
        spacing: Style.space(2)

        Text {
          text: win.errorStage !== "" ? "Failed at " + win.errorStage : "Failed"
          font.family: Style.font.family
          font.bold: true
          font.pixelSize: Style.font.subtitle
          color: Color.popups.text
        }
        Text {
          text: win.errorMessage
          width: parent.width
          wrapMode: Text.Wrap
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Color.popups.text
        }
      }
    }
  }

  Shortcut {
    sequences: ["Escape"]
    onActivated: {
      if (win.settingsOpen) win.settingsOpen = false
      else win.closeWindow()
    }
  }
}
