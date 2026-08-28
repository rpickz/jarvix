import QtQuick
import Quickshell
import Quickshell.Io
import Quickshell.Wayland
import qs.Commons
import qs.Ui

// Jarvix overlay: a thin view over jarvixd's state. It connects to the
// daemon's Unix socket, renders session state and streamed events, and shows
// nothing at all while Jarvix is idle. All intelligence lives in the daemon;
// this file only displays what it is told (see docs/ipc.md for the protocol).
Item {
  id: root

  // --- daemon state -------------------------------------------------------
  property string sessionState: "idle"
  property string transcript: ""
  property string response: ""
  property string errorMessage: ""
  property bool socketReady: false

  // The overlay stays up briefly after a session ends so the user can read
  // the tail of the response (or the error) before it fades.
  property bool lingering: false
  readonly property bool active: socketReady
    && (sessionState !== "idle" || root.lingering)

  function reset() {
    transcript = ""
    response = ""
    errorMessage = ""
  }

  // --- pending tool confirmation (issue #119) -----------------------------
  // A compact approve/decline surface on the indicator the user is already
  // looking at. Display-only in the ADR 0013 sense: the question, the gist,
  // and the countdown all come off the socket, and the buttons call the SAME
  // session.confirm verb as the window card — one resolution path, so both
  // surfaces resolve together on the daemon's own events. The command is
  // deliberately elided here: the one-line gist identifies the ask, and the
  // window card carries the full verbatim command (ADR 0014) — this surface
  // points at it rather than truncating it into something misreadable.
  property bool confirmPending: false
  property string confirmTool: ""
  property string confirmSummary: ""
  property int confirmTimeoutSec: 0    // the configured window, from the daemon
  property double confirmDeadlineMs: 0 // absolute auto-decline time; 0 = clock not started
  property double confirmNowMs: 0      // ticked by confirmTick so the binding updates

  // Seconds left before auto-decline, or -1 while the daemon has not started
  // the clock (the question is still being asked aloud). Clamped at 0 like
  // the window card: only the daemon declines, so a countdown reaching zero
  // waits for tool.declined rather than resolving anything itself.
  readonly property int confirmRemainingSec: confirmDeadlineMs > 0
    ? Math.max(0, Math.ceil((confirmDeadlineMs - confirmNowMs) / 1000)) : -1

  function showConfirmation(tool, summary, timeoutSec, deadlineMs) {
    confirmTool = tool
    confirmSummary = summary
    confirmTimeoutSec = timeoutSec
    confirmDeadlineMs = deadlineMs
    confirmNowMs = Date.now()
    confirmPending = true
  }

  function clearConfirmation() {
    confirmPending = false
    confirmTool = ""
    confirmSummary = ""
    confirmDeadlineMs = 0
  }

  // JSON-RPC ids for the overlay's session.confirm calls. Its own private
  // range (600–649) so a reply is recognisable as a confirm reply; the
  // connection is the overlay's alone, but a fixed range keeps this file's
  // ids disjoint from every other surface's by construction.
  property int confirmRequestId: 0
  property int nextConfirmRequestId: 600

  // One session.confirm call with a literal boolean — exactly what the window
  // card's buttons send. Nothing resolves here: the surface clears on the
  // daemon's tool.confirmed / tool.declined event, the same signal that
  // resolves the card, so both surfaces always agree (single source of truth).
  function answerConfirmation(approved) {
    if (!daemon.connected || !confirmPending) return
    confirmRequestId = nextConfirmRequestId
    nextConfirmRequestId = nextConfirmRequestId >= 649 ? 600 : nextConfirmRequestId + 1
    daemon.write(JSON.stringify({
      jsonrpc: "2.0", id: confirmRequestId, method: "session.confirm",
      params: { approved: approved }
    }) + "\n")
  }

  function handleEvent(method, params) {
    switch (method) {
    case "state.changed":
      var next = String(params.state || "idle")
      if (sessionState === "idle" && next !== "idle") {
        lingerTimer.stop()
        lingering = false
        reset()
      }
      sessionState = next
      if (next === "idle" && (response !== "" || errorMessage !== "")) {
        lingering = true
        lingerTimer.interval = errorMessage !== "" ? 4000 : 1800
        lingerTimer.restart()
      }
      break
    case "transcript.partial":
    case "transcript.final":
      transcript = String(params.text || "")
      break
    case "assistant.delta":
      response += String(params.content || "")
      break
    case "assistant.finished":
      if (params.content) response = String(params.content)
      break
    case "tool.confirmation_required":
      // The gate asked. The deadline is unknown until the daemon says the
      // clock has started — the question may still be being spoken — so the
      // countdown opens at "up to timeout_sec", exactly like the card.
      showConfirmation(String(params.tool || ""), String(params.summary || ""),
        Number(params.timeout_sec || 0), 0)
      break
    case "tool.confirmation_deadline":
      confirmDeadlineMs = Number(params.deadline_ms || 0)
      confirmNowMs = Date.now()
      break
    case "tool.confirmed":
    case "tool.declined":
      // Resolved on any surface — this one, the card, voice, text, CLI, or
      // the timeout: the daemon's event is the single source of truth, so
      // the surface returns to its normal state the same moment the card
      // marks its outcome.
      clearConfirmation()
      break
    case "session.finished":
      // The daemon never lets a confirmation outlive its session; clearing
      // here only covers this overlay having been a slow client that missed
      // the resolution event.
      clearConfirmation()
      break
    case "session.cancelled":
      clearConfirmation()
      lingerTimer.stop()
      lingering = false
      break
    case "error":
      errorMessage = String(params.message || "something went wrong")
      break
    }
  }

  Timer {
    id: lingerTimer
    onTriggered: { root.lingering = false; root.reset() }
  }

  // --- daemon connection --------------------------------------------------
  Socket {
    id: daemon
    path: Quickshell.env("XDG_RUNTIME_DIR") + "/jarvix.sock"
    connected: true

    parser: SplitParser {
      onRead: function(line) {
        var frame
        try { frame = JSON.parse(line) } catch (e) { return }
        if (frame.method) {
          root.handleEvent(frame.method, frame.params || {})
        } else if (frame.result && frame.result.state !== undefined) {
          // Response to the status.get sent on connect.
          root.sessionState = String(frame.result.state)
          // A pending confirmation rides status.get (issue #119) for the
          // same reason it rides conversation.get for the window (issue
          // #76): attaching mid-wait must not leave this surface blind to a
          // question that is already open. Null when nothing is pending, so
          // the same read clears a stale card after a reconnect.
          if (frame.result.confirmation) {
            root.showConfirmation(
              String(frame.result.confirmation.tool || ""),
              String(frame.result.confirmation.summary || ""),
              Number(frame.result.confirmation.timeout_sec || 0),
              Number(frame.result.confirmation.deadline_ms || 0))
          } else {
            root.clearConfirmation()
          }
        }
        // A session.confirm reply that lost the race — the confirmation
        // already resolved elsewhere — comes back as an error, and is
        // deliberately ignored: the resolution event has already returned
        // the surface to normal, and alarming over "a moment late" informs
        // nobody (same stance as the window card).
      }
    }

    onConnectionStateChanged: {
      root.socketReady = connected
      if (connected) {
        // Sync state in case the daemon is mid-session when we attach.
        write(JSON.stringify({ jsonrpc: "2.0", id: 1, method: "status.get" }) + "\n")
      } else {
        root.sessionState = "idle"
        root.lingering = false
        // A question cannot be answered over a dead socket, and it may have
        // resolved while we were away; the status.get on reconnect restores
        // it if it is genuinely still open.
        root.clearConfirmation()
        reconnect.start()
      }
    }
  }

  Timer {
    id: reconnect
    interval: 2000
    repeat: false
    onTriggered: { if (!daemon.connected) daemon.connected = true }
  }

  // The tiny per-window overlays (#127): thread badge, AI-state glyph, and
  // nickname tag on enrolled windows. A sibling surface with its own daemon
  // connection (ADR 0013), hosted here because this file is the plugin's
  // panel entry point — it touches neither the conversation window's
  // LazyLoader below (#106) nor the confirmation controls above (#119).
  JarvixWindowOverlays {}

  // The conversation window: the click-through target for notifications and
  // `jarvix window`. It manages its own daemon connection (ADR 0013).
  //
  // It is owned by a LazyLoader rather than declared inline, and the instance
  // is discarded and rebuilt after the compositor destroys its toplevel
  // (super+W / killactive — issue #106). Why recreation, established from
  // Quickshell's own source (src/window/proxywindow.cpp, floatingwindow.cpp):
  //
  //   - FloatingWindow.visible has asymmetric read and write paths. A write
  //     lands in ProxyFloatingWindow's `bWantsVisible` property, whose
  //     *change* callback is the only thing that ever shows or hides the
  //     window; a read comes from the backing QWindow.
  //   - A compositor kill closes the toplevel through plain Qt (QCloseEvent →
  //     QWindow destroy(): platform window deleted, QWindow turns invisible).
  //     Quickshell resyncs its internal mVisible and emits closed(), but
  //     nothing on that path resets bWantsVisible — it is stranded at true.
  //   - Every later `visible = true` is therefore a same-value property
  //     write: no change, no callback, no toplevel — openWindow assigns true
  //     to true and toggleWindow answers "closed" forever. And reviving the
  //     half-dead object with visible false-then-true was observed live to
  //     not map a window either: it re-creates the platform window through
  //     generic Qt rather than Quickshell's own createWindow/connectWindow
  //     path. A window object whose toplevel the compositor destroyed cannot
  //     be trusted again; only a fresh object (or a shell restart) recovers.
  //
  // FloatingWindow emits closed() exactly when the window went away without
  // `visible` being set false by us (Quickshell suppresses it for ordinary
  // hides), so that signal is the kill detector. A plain IPC closeWindow only
  // flips `visible`, so the instance — current tab, scroll, composer draft —
  // survives it exactly as before; only a compositor kill pays the recreation
  // (in-window presentation state resets, which #106 accepts: the window is
  // display-only per ADR 0013, the conversation lives in the daemon).
  LazyLoader {
    id: convLoader
    active: true

    JarvixWindow {
      // Marks the instance dead and schedules the rebuild. Deferred via
      // callLater inside conversationWindowKilled: closed() is emitted from
      // within Qt's close-event delivery, and tearing the window down
      // mid-emission would destroy the object currently signalling.
      onClosed: root.conversationWindowKilled()
    }
  }

  // True from the compositor killing the toplevel until the replacement
  // exists. Held as state rather than only a deferred call so an IPC request
  // landing in that gap forces the rebuild instead of poking the corpse.
  property bool convWindowDead: false

  function conversationWindowKilled() {
    root.convWindowDead = true
    Qt.callLater(root.respawnConversationWindow)
  }

  function respawnConversationWindow() {
    if (!root.convWindowDead) return
    root.convWindowDead = false
    convLoader.active = false // deleteLater()s the dead instance
    convLoader.active = true  // incubates the replacement synchronously
  }

  // conversationWindow hands every entry point a live instance whatever came
  // before: alive → as-is, compositor-killed → rebuilt, somehow inactive →
  // activated. All window IPC converges here so no path can reach a dead
  // object.
  function conversationWindow() {
    if (root.convWindowDead) root.respawnConversationWindow()
    if (!convLoader.active) convLoader.active = true
    return convLoader.item
  }

  // Shell contract for summoned panels. The overlay itself derives its
  // visibility from daemon state, so summoning Jarvix opens the conversation
  // window — the surface a user summons *to*.
  function open() {
    var w = conversationWindow()
    if (w) w.openWindow()
  }
  function close() {
    var w = conversationWindow()
    if (w) w.closeWindow()
  }

  IpcHandler {
    target: "jarvix"
    function state(): string { return root.sessionState }
    function ping(): string { return "ok" }
    // Window controls, driven by `omarchy-shell jarvix <fn>`: the CLI's
    // `jarvix window` toggles; a notification click opens.
    function openWindow(): string {
      var w = root.conversationWindow()
      if (!w) return "error"
      w.openWindow()
      return "open"
    }
    function closeWindow(): string {
      var w = root.conversationWindow()
      if (w) w.closeWindow()
      return "closed"
    }
    function toggleWindow(): string {
      var w = root.conversationWindow()
      if (!w) return "error"
      w.toggleWindow()
      return w.visible ? "open" : "closed"
    }
    // The bar widget's Settings action. Settings are a screen inside the
    // window, so this opens the window already showing them rather than
    // inventing a second surface.
    function openSettings(): string {
      var w = root.conversationWindow()
      if (!w) return "error"
      w.openSettings()
      return "open"
    }
  }

  // --- presentation -------------------------------------------------------
  readonly property int pad: Style.space(18)
  readonly property int maxTextWidth: Style.space(420)

  readonly property string statusLabel: {
    if (!socketReady) return ""
    if (errorMessage !== "") return "Jarvix hit a problem"
    switch (sessionState) {
    case "listening":    return "Listening"
    case "transcribing": return "Transcribing"
    case "thinking":     return "Jarvix is thinking"
    case "responding":   return "Responding"
    case "speaking":     return "Speaking"
    case "awaiting_confirmation": return "Waiting for your go-ahead"
    case "cancelling":   return "Cancelled"
    default:             return ""
    }
  }

  readonly property string bodyText: {
    if (errorMessage !== "") return errorMessage
    if (response !== "") return response
    if (transcript !== "" && (sessionState === "thinking" || sessionState === "transcribing"))
      return transcript
    return ""
  }

  PanelWindow {
    id: panel
    visible: root.active
    anchors { top: true; bottom: true; left: true; right: true }
    color: "transparent"
    WlrLayershell.namespace: "jarvix-overlay"
    WlrLayershell.layer: WlrLayer.Overlay
    WlrLayershell.keyboardFocus: WlrKeyboardFocus.None
    exclusionMode: ExclusionMode.Ignore
    // Never intercept clicks meant for the desktop below — except the
    // approve/decline controls while a confirmation is pending (issue #119),
    // which are the one thing on this surface that is *for* clicking. The
    // mask is exactly their rectangle, so everything else stays click-through
    // even mid-question, and returns to fully click-through on resolution.
    mask: Region { item: root.confirmPending ? confirmControls : null }

    BorderSurface {
      id: card
      anchors.horizontalCenter: parent.horizontalCenter
      anchors.top: parent.top
      anchors.topMargin: Style.space(64)
      width: card.borderLeft + root.pad + content.width + root.pad + card.borderRight
      height: card.borderTop + root.pad + content.height + root.pad + card.borderBottom
      color: Util.alpha(Color.background, 0.97)
      borderSpec: Border.surfaceSpec("popups", "border", Color.popups.border, Math.max(1, Style.space(2)))
      radius: Style.cornerRadius
      opacity: root.active ? 1 : 0

      Behavior on opacity { NumberAnimation { duration: 140; easing.type: Easing.OutCubic } }
      Behavior on height { NumberAnimation { duration: 120; easing.type: Easing.OutCubic } }
      Behavior on width { NumberAnimation { duration: 120; easing.type: Easing.OutCubic } }

      Column {
        id: content
        x: card.borderLeft + root.pad
        y: card.borderTop + root.pad
        width: Math.max(statusRow.width, body.visible ? body.width : 0,
          confirmBlock.visible ? confirmBlock.width : 0)
        spacing: body.visible || confirmBlock.visible ? Style.space(10) : 0

        Row {
          id: statusRow
          spacing: Style.space(10)

          // Activity indicator: a restrained pulse while listening, steady
          // otherwise. Personality through responsiveness, not gimmicks.
          Rectangle {
            id: dot
            width: Style.space(10)
            height: width
            radius: width / 2
            anchors.verticalCenter: parent.verticalCenter
            color: root.errorMessage !== "" ? Color.urgent : Color.accent

            SequentialAnimation on opacity {
              running: root.sessionState === "listening" || root.sessionState === "thinking"
              loops: Animation.Infinite
              alwaysRunToEnd: true
              NumberAnimation { from: 1.0; to: 0.35; duration: 550; easing.type: Easing.InOutSine }
              NumberAnimation { from: 0.35; to: 1.0; duration: 550; easing.type: Easing.InOutSine }
            }
          }

          Text {
            text: root.statusLabel
            anchors.verticalCenter: parent.verticalCenter
            font.family: Style.font.family
            font.bold: true
            font.pixelSize: Style.font.title
            color: Color.popups.text
          }
        }

        Text {
          id: body
          visible: root.bodyText !== ""
          text: root.bodyText
          width: Math.min(implicitWidth, root.maxTextWidth)
          wrapMode: Text.Wrap
          maximumLineCount: 12
          elide: Text.ElideRight
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Util.alpha(Color.popups.text, root.response !== "" || root.errorMessage !== "" ? 1.0 : 0.7)
        }

        // The compact confirmation request (issue #119): the daemon's
        // one-sentence question, approve/decline, and the countdown. The
        // command itself is elided by design — the window card shows it
        // verbatim (ADR 0014), and this block says where to look rather than
        // truncating ground truth into something misreadable.
        Column {
          id: confirmBlock
          visible: root.confirmPending
          spacing: Style.space(8)

          Text {
            id: confirmGist
            text: root.confirmSummary
            width: Math.min(implicitWidth, root.maxTextWidth)
            wrapMode: Text.Wrap
            maximumLineCount: 2
            elide: Text.ElideRight
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Color.popups.text
          }

          Row {
            id: confirmControls
            spacing: Style.space(8)

            Rectangle {
              id: confirmApprove
              width: confirmApproveLabel.width + Style.space(24)
              height: confirmApproveLabel.height + Style.space(10)
              radius: Style.cornerRadius
              color: Util.alpha(Color.accent, 0.18)
              border.color: Color.accent
              border.width: 1
              Accessible.role: Accessible.Button
              Accessible.name: "Approve — run it"
              Text {
                id: confirmApproveLabel
                anchors.centerIn: parent
                text: "✓ Approve"
                font.family: Style.font.family
                font.pixelSize: Style.font.subtitle
                color: Color.popups.text
              }
              MouseArea {
                anchors.fill: parent
                onClicked: root.answerConfirmation(true)
              }
            }

            Rectangle {
              id: confirmDecline
              width: confirmDeclineLabel.width + Style.space(24)
              height: confirmDeclineLabel.height + Style.space(10)
              radius: Style.cornerRadius
              color: Util.alpha(Color.popups.text, 0.08)
              border.color: Util.alpha(Color.popups.text, 0.5)
              border.width: 1
              Accessible.role: Accessible.Button
              Accessible.name: "Decline — do not run it"
              Text {
                id: confirmDeclineLabel
                anchors.centerIn: parent
                text: "✗ Decline"
                font.family: Style.font.family
                font.pixelSize: Style.font.subtitle
                color: Color.popups.text
              }
              MouseArea {
                anchors.fill: parent
                onClicked: root.answerConfirmation(false)
              }
            }
          }

          // Tool identity, the countdown, and the pointer at the full
          // details — text, never colour alone, deriving from the daemon's
          // deadline exactly as the card does: before the clock starts only
          // the configured maximum can honestly be said.
          Text {
            width: Math.min(implicitWidth, root.maxTextWidth)
            wrapMode: Text.Wrap
            text: root.confirmTool
              + (root.confirmRemainingSec >= 0
                ? " · " + root.confirmRemainingSec + "s left — no answer declines"
                : " · up to " + root.confirmTimeoutSec + "s once asked")
              + " · full command in the Jarvix window"
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Util.alpha(Color.popups.text, 0.7)
          }
        }
      }
    }
  }

  // Ticks the countdown on the pending confirmation. The remaining time is a
  // pure function of the daemon's deadline and the current time; this timer
  // only refreshes "now" — it decides nothing, and only the daemon declines.
  Timer {
    id: confirmTick
    interval: 500
    repeat: true
    running: root.active && root.confirmPending && root.confirmDeadlineMs > 0
    onTriggered: root.confirmNowMs = Date.now()
  }
}
