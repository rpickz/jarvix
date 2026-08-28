import QtQuick
import QtQuick.Controls
import QtQuick.Layouts
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Ui
import "BarState.js" as BarState

// Jarvix bar widget: one icon in the bar and one panel behind it.
//
// The icon is Jarvix's permanent home on screen — what it is doing right now,
// readable at a glance, whether or not a session is running. The panel is the
// short list of things worth reaching for without speaking. Between the two,
// Jarvix stops being a thing you have to already know how to summon.
//
// Like every other Jarvix surface this file is display-only (ADR 0013). It
// renders daemon state from the event stream and runs commands that already
// exist; every decision it appears to make — which glyph, which words, which
// actions, which icon for an artifact kind — was made in Go and compiled into
// BarState.js by `go generate ./internal/desktop`. Changing what the widget
// says means changing internal/desktop/barstatus.go, where there are tests.
//
// Colours, fonts, and spacing come from the injected `bar` object and the
// shell's Color/Style tokens, so the widget follows the user's theme and font
// scale without knowing anything about either.
Panel {
  id: root
  moduleName: "jarvix"
  // The overlay already owns the "jarvix" IPC target (JarvixOverlay.qml).
  // A second handler on the same target would collide, so the widget takes
  // its own — and `manageIpc: false` stops the Panel base registering the
  // default one on top of ours.
  ipcTarget: "jarvix.bar"
  manageIpc: false

  // --- daemon state -------------------------------------------------------
  // Four facts, straight off the wire. Everything visible derives from them.
  property bool socketReady: false
  property string sessionState: "idle"
  // Held until the next session starts, matching the conversation window's
  // banner rule: the daemon returns to idle after a failed turn, so clearing
  // this on `idle` would erase the failure the instant it happened.
  property string errorMessage: ""
  // Background wake-word listening: "off", "armed", or "muted" (ADR 0024).
  // This is the microphone indicator the privacy requirement asks for, and it
  // is a second dimension rather than another session state — the session
  // states describe a turn, this describes the microphone between turns.
  property string wakeState: "off"

  // The active focus thread (#123, the chip deferred from PR #132): its name,
  // and whether a timeboxed focus session is live. Kept current by
  // focus.changed alone — the event carries active id, name, and the session
  // flag on every change precisely so no round trip is needed — and seeded by
  // one focus.list on connect, because a bar that loads mid-thread must not
  // stay blank until the next switch. Display is a static text chip beside
  // the icon: no timer, no countdown, no animation (the #127 anti-goals hold
  // here too; the Focus tab carries the detail).
  property string activeThreadName: ""
  property bool focusSessionLive: false

  // Live detail for the tooltip (issue #70): which tool is running (and its
  // long-form label for a slow one, e.g. "Consulting claude…"), the question
  // a pending confirmation is asking, and when the current phase began. All
  // straight off the event stream — no polling; the elapsed counter is a
  // local clock over a locally observed start. What the tooltip *says* with
  // these is decided in Go (desktop.LiveTooltip → BarState.liveTooltip).
  property string currentTool: ""
  property string toolDetail: ""
  property string confirmQuestion: ""
  property double phaseStart: Date.now()
  property int elapsedSec: 0

  readonly property var status: BarState.statusFor(socketReady, sessionState, errorMessage, wakeState)
  readonly property var menuActions: BarState.actions(socketReady, wakeState)

  // --- theme --------------------------------------------------------------
  readonly property color foreground: bar ? bar.foreground : Color.foreground
  readonly property color urgent: bar ? bar.urgent : Color.urgent
  readonly property color dim: Qt.darker(foreground, 1.55)
  readonly property string fontFamily: bar ? bar.fontFamily : Style.font.family

  // --- panel cursor -------------------------------------------------------
  // One flat cursor over the actions followed by the artifact rows, so Up and
  // Down walk the whole panel and Enter activates whatever is under it.
  property bool cursorActive: false
  property int cursorIndex: 0
  property var artifacts: []
  property string artifactDir: ""
  property string artifactError: ""
  readonly property int artifactCount: Math.max(0, Math.min(20, parseInt(String(setting("artifactCount", 5)), 10) || 0))
  readonly property int rowCount: menuActions.length + artifacts.length

  function clampCursor() {
    if (cursorIndex >= rowCount) cursorIndex = Math.max(0, rowCount - 1)
    if (cursorIndex < 0) cursorIndex = 0
  }

  function moveCursor(dy) {
    if (rowCount === 0) return
    if (!cursorActive) { cursorActive = true; return }
    cursorIndex = Math.max(0, Math.min(rowCount - 1, cursorIndex + dy))
  }

  function setCursor(index) {
    cursorActive = true
    cursorIndex = index
  }

  function activateCursor() {
    if (!cursorActive || rowCount === 0) return
    if (cursorIndex < menuActions.length) runAction(menuActions[cursorIndex])
    else openArtifact(artifacts[cursorIndex - menuActions.length])
  }

  // Every action is a command line decided in Go. The widget runs it and
  // closes — it never interprets the result, and it never reimplements what
  // the command does.
  function runCommand(command) {
    if (!command || !bar) return
    bar.run(command)
  }

  function runAction(action) {
    if (!action) return
    runCommand(action.command)
    close()
  }

  // Opening a file goes through execDetached rather than bar.run: bar.run
  // hands the string to `bash -lc`, and an artifact's name comes from whatever
  // the assistant called it. Passing argv directly means there is no shell to
  // quote for.
  function openPath(path) {
    if (!path) return
    Quickshell.execDetached(["xdg-open", String(path)])
    close()
  }

  function openArtifact(artifact) {
    if (artifact) openPath(artifact.path)
  }

  // --- daemon connection --------------------------------------------------
  // The widget's own socket, like the overlay's and the window's: each Jarvix
  // surface is an independent client of the same event stream (ADR 0013). It
  // never polls — `state.changed` arrives as it happens, so the icon follows
  // the daemon within a frame — and a slow widget is just another slow bus
  // client whose events the daemon drops rather than blocking on.
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
        } else if (frame.id === 800 && frame.result) {
          // Answer to the focus.list sent on connect (id 800, this file's
          // slot in the 800-849 range): the seed for the active-thread chip.
          // The daemon lists the active thread first and flags it, so the
          // first flagged row is the whole answer.
          root.activeThreadName = ""
          root.focusSessionLive = !!frame.result.session
          var threads = frame.result.threads || []
          for (var i = 0; i < threads.length; i++) {
            if (threads[i].active) { root.activeThreadName = String(threads[i].name || ""); break }
          }
        } else if (frame.result && frame.result.state !== undefined) {
          // Answer to the status.get sent on connect: the daemon may already
          // be mid-session — and already listening in the background — when
          // the bar loads. Both come from the one call, so the indicator is
          // right immediately rather than after the next event.
          root.sessionState = String(frame.result.state)
          if (frame.result.wake_state !== undefined)
            root.wakeState = String(frame.result.wake_state)
        }
      }
    }

    onConnectionStateChanged: {
      root.socketReady = connected
      if (connected) {
        write(JSON.stringify({ jsonrpc: "2.0", id: 1, method: "status.get" }) + "\n")
        write(JSON.stringify({ jsonrpc: "2.0", id: 800, method: "focus.list" }) + "\n")
      } else {
        // Not "unknown" — "not running", which is what the icon says. The
        // held error goes with the connection that reported it, and so does
        // the wake state: with no daemon there is no capture process, and
        // leaving a stale "armed" on screen would be the worst possible lie
        // for this particular indicator to tell.
        root.sessionState = "idle"
        root.errorMessage = ""
        root.wakeState = "off"
        // Live detail died with the connection that reported it: with no
        // daemon there is no tool in flight and no question pending.
        root.currentTool = ""
        root.toolDetail = ""
        root.confirmQuestion = ""
        root.elapsedSec = 0
        // The thread chip too: with no daemon the active thread is
        // unknowable, and a stale name is worse than none.
        root.activeThreadName = ""
        root.focusSessionLive = false
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

  function handleEvent(method, params) {
    switch (method) {
    case "state.changed":
      var next = String(params.state || "idle")
      // A new session begins: the previous failure is history now.
      if (sessionState === "idle" && next !== "idle") errorMessage = ""
      // "Elapsed in the phase", so the clock restarts on every transition.
      if (next !== sessionState) { phaseStart = Date.now(); elapsedSec = 0 }
      if (next === "idle" || next === "error") {
        currentTool = ""
        toolDetail = ""
        confirmQuestion = ""
      }
      sessionState = next
      break
    case "tool.started":
      currentTool = String(params.tool || "")
      toolDetail = String(params.detail || "")
      break
    case "tool.finished":
      currentTool = ""
      toolDetail = ""
      break
    case "tool.confirmation_required":
      // The spoken question, which is what the user is being asked; the
      // exact command stays the window's and overlay's job to display.
      confirmQuestion = String(params.summary || "")
      break
    case "tool.confirmed":
    case "tool.declined":
    case "tool.denied":
      confirmQuestion = ""
      break
    case "error":
      errorMessage = String(params.message || "something went wrong")
      break
    case "wake.changed":
      wakeState = String(params.state || "off")
      break
    case "focus.changed":
      // Carried on every focus event (docs/ipc.md): active id, its name,
      // and the session flag — the chip updates from the event alone.
      activeThreadName = String(params.active_name || "")
      focusSessionLive = !!params.session
      break
    }
  }

  // One tick a second while a session runs, so the tooltip's "Thinking — 12s"
  // stays honest. Not polling: nothing is asked of the daemon, this only
  // advances a counter between the events that reset it.
  Timer {
    interval: 1000
    repeat: true
    running: root.socketReady && root.sessionState !== "idle"
    onTriggered: root.elapsedSec = Math.round((Date.now() - root.phaseStart) / 1000)
  }

  IpcHandler {
    target: root.ipcTarget
    function open(): void { root.open() }
    function close(): void { root.close() }
    function show(): void { root.open() }
    function hide(): void { root.close() }
    function toggle(): void { root.toggle() }
    // The state the bar is showing, for scripts and for a human checking the
    // widget without squinting at it.
    function state(): string { return root.status.key }
  }

  // --- recent artifacts ---------------------------------------------------
  // `jarvix artifacts --json` reads the artifact directory and reports the
  // recent ones already named, dated, and typed. The panel draws that list;
  // it does not know where artifacts live or what an extension means.
  Process {
    id: artifactScan
    running: false
    // Through a login shell for the same reason bar.run uses one: `jarvix`
    // installs to ~/.local/bin, which is on the user's PATH but not
    // necessarily on the shell process's.
    command: ["bash", "-lc", "jarvix artifacts --json"]
    stdout: StdioCollector { id: artifactOut; waitForEnd: true }
    stderr: StdioCollector { id: artifactErr; waitForEnd: true }
    onExited: function(exitCode) {
      if (exitCode !== 0) {
        root.artifacts = []
        // A missing `jarvix` on PATH is the common case here, and saying so
        // beats an empty section that looks like "you have made nothing".
        root.artifactError = String(artifactErr.text || "").trim() || "Could not list artifacts"
        return
      }
      root.applyArtifacts(String(artifactOut.text || ""))
    }
  }

  function applyArtifacts(raw) {
    var listing
    try { listing = JSON.parse(raw) } catch (e) { listing = null }
    if (!listing) {
      root.artifacts = []
      root.artifactError = "Could not read the artifact listing"
      return
    }
    root.artifactError = ""
    root.artifactDir = String(listing.dir || "")
    root.artifacts = (listing.artifacts || []).slice(0, root.artifactCount)
    root.clampCursor()
  }

  function refreshArtifacts() {
    if (artifactScan.running || artifactCount === 0) return
    artifactScan.running = true
  }

  // --- bar icon -----------------------------------------------------------
  // The widget is the icon, plus the status chip while something is happening
  // (issue #158), plus the static thread chip while a focus thread is active;
  // the widget's width follows so the bar lays its neighbours out correctly in
  // every combination. At rest both chips are absent and the widget is the
  // icon it always was.
  implicitWidth: button.implicitWidth
    + (statusChip.visible ? statusChip.implicitWidth + Style.space(4) : 0)
    + (threadChip.visible ? threadChip.implicitWidth + Style.space(4) : 0)
  implicitHeight: button.implicitHeight

  onOpenedChanged: if (opened) {
    cursorActive = false
    cursorIndex = 0
    if (panelFlick) panelFlick.contentY = 0
    refreshArtifacts()
    Qt.callLater(function() { keyCatcher.forceActiveFocus() })
  }

  BarIconButton {
    id: button
    anchors.left: parent.left
    anchors.top: parent.top
    anchors.bottom: parent.bottom
    width: button.implicitWidth

    // The pulse is the only animation, and it runs on exactly the states the
    // overlay pulses on, so the two surfaces never disagree about when Jarvix
    // looks busy. alwaysRunToEnd lets the loop finish on full opacity rather
    // than freezing the icon half-faded when the state changes mid-fade.
    // Scoped to the icon — it used to fade the whole widget, which was the
    // same thing when the widget WAS the icon; the thread chip beside it is
    // a static surface (#123/#127's no-animation rule) and must not breathe.
    SequentialAnimation on opacity {
      running: root.status.pulse
      loops: Animation.Infinite
      alwaysRunToEnd: true
      NumberAnimation { from: 1.0; to: 0.45; duration: 620; easing.type: Easing.InOutSine }
      NumberAnimation { from: 0.45; to: 1.0; duration: 620; easing.type: Easing.InOutSine }
    }

    bar: root.bar
    text: root.status.glyph
    // The theme's urgent colour, for the two states the user must act on.
    // It is never the only signal: the glyph differs and the tooltip says so
    // in words.
    active: root.status.urgent
    // A stopped daemon reads as present-but-inert. It must never disappear —
    // an absent icon cannot be told apart from a plugin that is not installed.
    dimmed: root.status.dim
    tooltipText: BarState.liveTooltip(root.status, root.elapsedSec,
      root.currentTool, root.toolDetail, root.confirmQuestion)
    onPressed: function(buttonCode) {
      // Left click is the headline gesture: toggle the conversation window
      // through the plugin's existing IpcHandler — the same route `jarvix
      // window` and a clicked notification take, not a second window. Right
      // click opens the panel of actions.
      if (buttonCode === Qt.RightButton) root.toggle()
      else if (buttonCode === Qt.MiddleButton) root.runCommand("jarvix new")
      else root.runCommand("omarchy-shell jarvix toggleWindow")
    }
  }

  // The status chip (issue #158): what Jarvix is doing, in words, beside the
  // glyph. The glyph alone was a monochrome swap in a dense bar — a shape
  // change nobody notices and a screen reader only reaches by hovering, which
  // is not a thing you do to a bar you are not already suspicious of. So the
  // state says itself, in the fewest words that still say it: "Thinking 4s".
  // The fuller sentence — which tool, which question — stays the tooltip's
  // job; this is the headline, and it is the half that has to be readable
  // without doing anything.
  //
  // Every decision here was made in Go: which states earn a chip, what each
  // one says, and when the elapsed count joins it (desktop.BarChipLabel →
  // BarState.chipLabel). Empty text means the resting states — idle, muted,
  // wake-armed, daemon down — draw the bare icon exactly as they always did,
  // so nothing is added to the bar in normal use.
  //
  // Static by the same rule as the thread chip: no animation of its own. The
  // icon's pulse is the only motion, it is opacity-only, and the words here
  // carry the state without it.
  Text {
    id: statusChip
    readonly property string label: BarState.chipLabel(root.status, root.elapsedSec)
    visible: label !== ""
    text: label
    anchors.left: button.right
    anchors.leftMargin: Style.space(4)
    anchors.verticalCenter: parent.verticalCenter
    color: root.status.urgent ? root.urgent : root.foreground
    font.family: root.fontFamily
    font.pixelSize: Style.font.caption
    // The tooltip's fuller sentence is the icon's; this is the same fact in
    // fewer words, and a reader that lands on it must not be told twice.
    Accessible.role: Accessible.StaticText
    Accessible.name: statusChip.label
  }

  // The active-thread chip (#123's deferred bar surface): the current
  // thread's name, in text, always in peripheral vision — "which front am I
  // on?" answered without opening anything. Static by rule: no timer, no
  // countdown, no animation; the one extra mark is a small filled dot while
  // a timeboxed focus session is live (with its accessible text carrying
  // the same fact, never colour alone). Absent entirely when no thread is
  // active or no daemon is running.
  Rectangle {
    id: threadChip
    visible: root.socketReady && root.activeThreadName !== ""
    anchors.left: statusChip.visible ? statusChip.right : button.right
    anchors.leftMargin: Style.space(4)
    anchors.verticalCenter: parent.verticalCenter
    implicitWidth: chipRow.implicitWidth + Style.space(12)
    implicitHeight: chipLabel.implicitHeight + Style.space(6)
    radius: Style.cornerRadius
    color: Util.alpha(root.foreground, 0.08)
    border.color: Util.alpha(root.foreground, 0.25)
    border.width: 1

    Accessible.role: Accessible.StaticText
    Accessible.name: "Focus thread: " + root.activeThreadName
      + (root.focusSessionLive ? " (focus session running)" : "")

    Row {
      id: chipRow
      anchors.centerIn: parent
      spacing: Style.space(5)

      Rectangle {
        visible: root.focusSessionLive
        width: Style.space(6)
        height: width
        radius: width / 2
        anchors.verticalCenter: parent.verticalCenter
        color: root.foreground
      }

      Text {
        id: chipLabel
        text: root.activeThreadName
        anchors.verticalCenter: parent.verticalCenter
        color: root.foreground
        font.family: root.fontFamily
        font.pixelSize: Style.font.caption
        elide: Text.ElideRight
        width: Math.min(implicitWidth, Style.space(120))
      }
    }
  }

  // --- panel --------------------------------------------------------------
  KeyboardPanel {
    id: panel
    anchorItem: button
    owner: root
    bar: root.bar
    open: root.opened
    focusTarget: keyCatcher
    contentWidth: panel.fittedContentWidth(Style.space(340))
    contentHeight: panel.fittedContentHeight(column.implicitHeight, Style.space(560))

    PanelKeyCatcher {
      id: keyCatcher
      anchors.fill: parent

      onMoveRequested: function(dx, dy) { if (dy !== 0) root.moveCursor(dy) }
      onActivateRequested: root.activateCursor()
      onCloseRequested: root.close()
      onTabRequested: function(direction) { root.switchPanel(direction) }
      onTextKey: function(t) {
        if (t === "r" || t === "R") root.refreshArtifacts()
        else if (t === "w" || t === "W") root.runCommand("omarchy-shell jarvix toggleWindow")
      }

      Flickable {
        id: panelFlick
        anchors.fill: parent
        contentWidth: width
        contentHeight: column.implicitHeight
        clip: true
        boundsBehavior: Flickable.StopAtBounds
        flickableDirection: Flickable.VerticalFlick
        interactive: contentHeight > height
        ScrollBar.vertical: ScrollBar { policy: ScrollBar.AsNeeded }

        Column {
          id: column
          width: panelFlick.width
          spacing: Style.space(12)

          // ---------- Hero: what Jarvix is doing, in words ----------
          // The same sentence the bar tooltip carries, so the icon and the
          // panel can never tell different stories. State is text here, not
          // just an icon and a colour.
          PanelHero {
            width: parent.width
            title: root.status.label
            foreground: root.status.urgent ? root.urgent : root.foreground
            fontFamily: root.fontFamily
            iconOpacity: root.status.dim ? 0.55 : 1.0
            iconComponent: Component {
              Text {
                text: root.status.glyph
                color: root.status.urgent ? root.urgent : root.foreground
                font.family: root.fontFamily
                font.pixelSize: Style.font.display
              }
            }
          }

          Text {
            width: parent.width
            text: root.status.detail
            color: root.dim
            font.family: root.fontFamily
            font.pixelSize: Style.font.bodySmall
            wrapMode: Text.WordWrap
          }

          PanelSeparator { foreground: root.foreground }

          // ---------- Actions ----------
          Column {
            id: actionColumn
            width: parent.width
            spacing: Style.space(6)

            Repeater {
              model: root.menuActions
              ActionRow {
                required property var modelData
                required property int index
                width: actionColumn.width
                action: modelData
                rowIndex: index
              }
            }
          }

          // ---------- Recent artifacts ----------
          PanelSeparator {
            visible: root.artifactCount > 0
            foreground: root.foreground
          }

          Column {
            visible: root.artifactCount > 0
            width: parent.width
            spacing: Style.space(8)

            Item {
              width: parent.width
              implicitHeight: sectionHeader.implicitHeight

              PanelSectionHeader {
                id: sectionHeader
                anchors.left: parent.left
                anchors.verticalCenter: parent.verticalCenter
                text: "RECENT ARTIFACTS"
                foreground: root.foreground
                fontFamily: root.fontFamily
              }

              PanelActionButton {
                anchors.right: parent.right
                anchors.verticalCenter: parent.verticalCenter
                visible: root.artifactDir !== ""
                focusable: true
                iconText: BarState.folderGlyph
                tooltipText: "Open the artifacts folder"
                foreground: root.foreground
                fontFamily: root.fontFamily
                onClicked: root.openPath(root.artifactDir)
              }
            }

            Text {
              visible: root.artifacts.length === 0
              width: parent.width
              text: root.artifactError !== ""
                ? root.artifactError
                : "Nothing yet. Ask Jarvix for a diagram, a document, or a sketch."
              color: root.artifactError !== "" ? root.urgent : root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.bodySmall
              wrapMode: Text.WordWrap
              horizontalAlignment: Text.AlignHCenter
            }

            Column {
              id: artifactColumn
              visible: root.artifacts.length > 0
              width: parent.width
              spacing: Style.space(4)

              Repeater {
                model: root.artifacts
                ArtifactRow {
                  required property var modelData
                  required property int index
                  width: artifactColumn.width
                  artifact: modelData
                  rowIndex: index
                }
              }
            }
          }
        }
      }
    }
  }

  // --- rows ---------------------------------------------------------------
  // Both row types are the same shape: a glyph, a label, a second line, and
  // one thing that happens when you pick them. The cursor is shared, so the
  // keyboard walks actions and artifacts as one list.

  component ActionRow: CursorSurface {
    id: actionRow
    property var action: null
    property int rowIndex: 0

    hasCursor: root.cursorActive && root.cursorIndex === rowIndex
    foreground: root.foreground
    implicitHeight: actionContent.implicitHeight + Style.spacing.rowPaddingX

    Accessible.role: Accessible.Button
    Accessible.name: actionRow.action ? actionRow.action.label : ""
    Accessible.description: actionRow.action ? actionRow.action.detail : ""

    MouseArea {
      anchors.fill: parent
      hoverEnabled: true
      cursorShape: Qt.PointingHandCursor
      onEntered: root.setCursor(actionRow.rowIndex)
      onClicked: root.runAction(actionRow.action)
    }

    RowLayout {
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.verticalCenter: parent.verticalCenter
      anchors.leftMargin: Style.space(10)
      anchors.rightMargin: Style.space(10)
      spacing: Style.space(10)

      Text {
        text: actionRow.action ? actionRow.action.glyph : ""
        color: root.foreground
        font.family: root.fontFamily
        font.pixelSize: Style.font.icon
        Layout.alignment: Qt.AlignVCenter
      }

      ColumnLayout {
        id: actionContent
        Layout.fillWidth: true
        spacing: Style.space(1)

        Text {
          Layout.fillWidth: true
          text: actionRow.action ? actionRow.action.label : ""
          color: root.foreground
          font.family: root.fontFamily
          font.pixelSize: Style.font.body
          elide: Text.ElideRight
        }

        Text {
          Layout.fillWidth: true
          text: actionRow.action ? actionRow.action.detail : ""
          color: root.dim
          font.family: root.fontFamily
          font.pixelSize: Style.font.caption
          elide: Text.ElideRight
        }
      }
    }
  }

  component ArtifactRow: CursorSurface {
    id: artifactRow
    property var artifact: null
    property int rowIndex: 0
    readonly property string artifactName: artifact ? String(artifact.name || "") : ""

    hasCursor: root.cursorActive && root.cursorIndex === root.menuActions.length + rowIndex
    foreground: root.foreground
    implicitHeight: artifactContent.implicitHeight + Style.spacing.rowPaddingX

    Accessible.role: Accessible.Button
    Accessible.name: artifactRow.artifactName
    Accessible.description: artifactRow.artifact
      ? String(artifactRow.artifact.kind || "") + ", " + String(artifactRow.artifact.age || "")
      : ""

    MouseArea {
      anchors.fill: parent
      hoverEnabled: true
      cursorShape: Qt.PointingHandCursor
      onEntered: root.setCursor(root.menuActions.length + artifactRow.rowIndex)
      onClicked: root.openArtifact(artifactRow.artifact)
    }

    RowLayout {
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.verticalCenter: parent.verticalCenter
      anchors.leftMargin: Style.space(10)
      anchors.rightMargin: Style.space(10)
      spacing: Style.space(10)

      Text {
        text: BarState.artifactGlyph(artifactRow.artifact ? artifactRow.artifact.kind : "")
        color: root.foreground
        font.family: root.fontFamily
        font.pixelSize: Style.font.icon
        Layout.alignment: Qt.AlignVCenter
      }

      ColumnLayout {
        id: artifactContent
        Layout.fillWidth: true
        spacing: Style.space(1)

        Text {
          Layout.fillWidth: true
          text: artifactRow.artifactName
          color: root.foreground
          font.family: root.fontFamily
          font.pixelSize: Style.font.body
          elide: Text.ElideRight
        }

        Text {
          Layout.fillWidth: true
          text: artifactRow.artifact
            ? String(artifactRow.artifact.kind || "") + " · " + String(artifactRow.artifact.age || "")
            : ""
          color: root.dim
          font.family: root.fontFamily
          font.pixelSize: Style.font.caption
          elide: Text.ElideRight
        }
      }
    }
  }
}
