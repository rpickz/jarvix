import QtQuick
// FloatingWindow comes from Quickshell itself — do NOT add
// `import Quickshell.Wayland` here. It was added once on the theory that the
// type lived there; the plugin still loaded and every IPC function still
// answered, but the window silently never mapped: `openWindow` reported
// "open", `visible` read back true, and no Wayland toplevel ever existed.
// Omarchy's own FloatingWindow user (shell/plugins/dev-gallery) imports
// Quickshell alone, which is the check that settled it.
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Ui
import "ActivityState.js" as ActivityState

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

  // The history screen lists archived conversations and shows one read-only
  // (ADR 0027); like settings it replaces the conversation while open. It
  // holds no decisions of its own (ADR 0013): it renders conversation.list /
  // conversation.read, and Resume is one conversation.open call — the daemon
  // owns what reopening means.
  property bool historyOpen: false
  property string historyDetailId: "" // "" shows the listing; an id shows that record

  // The Activity screen (issue #70) shows what Jarvix is doing and has done:
  // the daemon's activity feed, live. Every row arrives already worded —
  // assembled daemon-side from bus events and served by activity.get plus
  // activity.row pushes — so this screen renders text and looks up glyphs
  // (ActivityState.js, generated from Go) and decides nothing (ADR 0013).
  property bool activityOpen: false

  // --- daemon state -------------------------------------------------------
  property bool socketReady: false
  property string sessionState: "idle"
  property string errorStage: ""
  property string errorMessage: ""
  // True while assistant.delta events are building the newest turn.
  property bool assistantStreaming: false

  ListModel { id: turns } // { role: "user"|"assistant", text: string }
  // Activity rows, oldest first, exactly as the daemon rendered them.
  ListModel { id: activityRows } // { seq, time, kind, label, detail, failed }
  // Archived conversations, newest first. "cid" rather than "id" because id
  // is the QML object-id keyword. Unreadable records list too, greyed: one
  // bad file never hides itself, let alone the library.
  ListModel { id: pastConversations } // { cid, preview, turnCount, lastActive, unreadable }
  ListModel { id: pastTurns }         // { role, text } — the record being viewed
  // Search hits over the archive (issue #59), ranked by the daemon. The
  // window renders them and opens the conversation a hit names — every
  // matching, ranking and bounding decision is made daemon-side (ADR 0013).
  ListModel { id: searchResults }     // { cid, turn, passage, lastActive, current }

  function openWindow() { visible = true }
  function closeWindow() { visible = false }
  function toggleWindow() { visible = !visible }

  // Open straight onto the settings screen. Settings live inside this window
  // rather than in a window of their own (issue #9), so "open settings" is
  // "open the window, already showing settings" — what the bar widget's
  // Settings action asks for. Escape still steps back to the conversation
  // before closing, so the shortcut cannot strand anyone.
  function openSettings() {
    activityOpen = false
    settingsOpen = true
    visible = true
  }

  onVisibleChanged: {
    if (visible) {
      if (daemon.connected) requestConversation()
      else daemon.connected = true
      // The window is a place you talk to, not only a place you read: taking
      // focus puts the caret in the composer so a summoned window can be
      // typed into without reaching for the mouse first. Deferred, because
      // this runs before the toplevel is mapped and focus given to an
      // unmapped window goes nowhere.
      Qt.callLater(function() { composerInput.forceActiveFocus() })
    } else {
      daemon.connected = false
    }
  }

  // Leaving the settings screen returns to the conversation — and to the
  // composer, for the same reason.
  onSettingsOpenChanged: { if (visible && !settingsOpen) composerInput.forceActiveFocus() }

  // Events are delivered on the same connection as the conversation.get
  // response, and the daemon writes notifications and responses
  // independently: a live assistant.delta can arrive *before* the snapshot
  // we asked for. Applying the snapshot then would wipe what that event just
  // rendered. So while a request is in flight events are queued, and replayed
  // on top of the snapshot once it lands — the snapshot is the state as of
  // the request, and queued events are strictly newer than it.
  property bool snapshotPending: false
  property var queuedEvents: []

  function requestConversation() {
    snapshotPending = true
    queuedEvents = []
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: 1, method: "conversation.get" }) + "\n")
  }

  // --- activity feed ------------------------------------------------------
  // The snapshot is requested on every connect and the pushes keep it
  // current, so opening the pane costs nothing and survives window
  // close/reopen — the ring lives in the daemon, this model only mirrors it.
  // Reconciliation is by seq: activity.get replaces the model, and any push
  // that raced the snapshot is deduplicated because seq never repeats.
  property int activityLimit: 400
  property int activityRequestId: 0

  function openActivity() {
    settingsOpen = false
    historyOpen = false
    historyDetailId = ""
    activityOpen = true
  }

  function requestActivity() {
    if (!daemon.connected) return
    activityRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: activityRequestId,
      method: "activity.get" }) + "\n")
  }

  function loadActivity(result) {
    activityRows.clear()
    if (result.limit) activityLimit = Number(result.limit)
    var list = result.rows || []
    for (var i = 0; i < list.length; i++) {
      appendActivityRow(list[i])
    }
  }

  function appendActivityRow(row) {
    var seq = Number(row.seq || 0)
    // A push that also made it into the snapshot (or a duplicate replay)
    // would land here with a seq the model already holds; skip it.
    if (activityRows.count > 0 && seq <= activityRows.get(activityRows.count - 1).seq) return
    activityRows.append({
      seq: seq,
      time: String(row.ts || "").substring(11, 19),
      kind: String(row.kind || ""),
      label: String(row.label || ""),
      detail: String(row.detail || ""),
      failed: Boolean(row.failed)
    })
    // Mirror the daemon's own bound so a long-lived window cannot outgrow it.
    while (activityRows.count > activityLimit) activityRows.remove(0)
  }

  // --- routines -----------------------------------------------------------
  // The configured routines (ADR 0026), listed so one click places the
  // desktop. Display and trigger only: routines.run replays the routine's
  // phrase through the daemon's ordinary session path, so the router, the
  // permission gate, and the spoken summary all behave exactly as if the
  // phrase had been spoken — this window decides nothing (ADR 0013).
  property var routines: []

  function requestRoutines() {
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: 15, method: "routines.list" }) + "\n")
  }

  function runRoutine(name) {
    if (!daemon.connected) return
    daemon.write(JSON.stringify({
      jsonrpc: "2.0", id: 16, method: "routines.run", params: { name: name }
    }) + "\n")
  }

  // --- typed turns --------------------------------------------------------
  // Request id 1 is the conversation snapshot; typed submissions take ids
  // from 2 upwards so a reply can be matched to the text that produced it.
  // Dynamic request ids start above the fixed ones this connection also
  // carries (1 = conversation.get, 15/16 = routines list/run; settings has
  // 11–14 on its own socket). Two features merged with each scheme unaware
  // of the other, and a dynamic counter walking into the fixed range would
  // misroute a history reply as a routines one — hence the gap.
  property int nextRequestId: 100
  property int submitRequestId: 0
  // The text of the submission in flight, kept only so a failed submit can
  // give it back. A question typed and then lost to a daemon that died
  // mid-keystroke is the one thing this input must never do.
  property string submitInFlight: ""

  // submitTypedTurn sends the composer's contents as one turn. Everything it
  // could decide is decided in the daemon (ADR 0013): `session.text` starts a
  // session or answers a pending confirmation, interrupts whatever is running
  // when it starts one, and rejects an empty string. The window's own empty
  // check is not that decision — it just avoids a round trip that could only
  // ever come back as an error.
  function submitTypedTurn() {
    var text = composerInput.text
    if (text.trim() === "") {
      composerInput.text = ""
      return
    }
    if (!daemon.connected) return
    submitRequestId = nextRequestId
    nextRequestId++
    submitInFlight = text
    composerInput.text = ""
    daemon.write(JSON.stringify({
      jsonrpc: "2.0", id: submitRequestId, method: "session.text",
      params: { text: text }
    }) + "\n")
  }

  // returnTypedText puts an unsent question back in the composer, unless the
  // user has already started typing the next one — their keystrokes win.
  function returnTypedText() {
    if (submitInFlight === "") return
    if (composerInput.text === "") composerInput.text = submitInFlight
    submitInFlight = ""
  }

  function handleSubmitReply(frame) {
    if (frame.error) {
      errorStage = "input"
      errorMessage = String(frame.error.message || "the question could not be submitted")
      returnTypedText()
      return
    }
    submitInFlight = ""
  }

  // --- history requests ---------------------------------------------------
  // Each history request takes an id from the same counter as typed turns,
  // so a reply is matched to exactly the request that asked for it.
  property int historyListRequestId: 0
  property int historyReadRequestId: 0
  property int historyOpenRequestId: 0
  property int historySearchRequestId: 0
  // True while search results (rather than the library) fill the history
  // screen; cleared by emptying the search box or reopening history.
  property bool searchActive: false

  function openHistory() {
    settingsOpen = false
    activityOpen = false
    historyOpen = true
    historyDetailId = ""
    searchActive = false
    historySearchInput.text = ""
    requestHistory()
  }

  function requestHistory() {
    if (!daemon.connected) return
    historyListRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: historyListRequestId,
      method: "conversation.list" }) + "\n")
  }

  function requestHistoryDetail(cid) {
    if (!daemon.connected) return
    historyDetailId = cid
    pastTurns.clear()
    historyReadRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: historyReadRequestId,
      method: "conversation.read", params: { id: cid } }) + "\n")
  }

  // requestHistorySearch asks the daemon to search the archive. The daemon
  // owns matching, ranking and passage bounds; an emptied box steps back to
  // the library without a round trip, because there is nothing to ask.
  function requestHistorySearch(query) {
    if (query.trim() === "") {
      searchActive = false
      searchResults.clear()
      return
    }
    if (!daemon.connected) return
    historySearchRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: historySearchRequestId,
      method: "conversation.search", params: { query: query, limit: 20 } }) + "\n")
  }

  function loadSearchResults(result) {
    searchResults.clear()
    var list = result.results || []
    for (var i = 0; i < list.length; i++) {
      searchResults.append({
        cid: String(list[i].id),
        turn: Number(list[i].turn || 0),
        passage: String(list[i].passage || ""),
        lastActive: String(list[i].ts || "").substring(0, 10),
        current: Boolean(list[i].current)
      })
    }
    searchActive = true
  }

  function resumeConversation(cid) {
    if (!daemon.connected) return
    historyOpenRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: historyOpenRequestId,
      method: "conversation.open", params: { id: cid } }) + "\n")
  }

  function loadHistoryList(result) {
    pastConversations.clear()
    var list = result.conversations || []
    for (var i = 0; i < list.length; i++) {
      pastConversations.append({
        cid: String(list[i].id),
        preview: String(list[i].preview || ""),
        turnCount: Number(list[i].turns || 0),
        lastActive: String(list[i].last_active || "").substring(0, 10),
        unreadable: false
      })
    }
    var bad = result.unreadable || []
    for (var j = 0; j < bad.length; j++) {
      pastConversations.append({
        cid: String(bad[j].id), preview: "This conversation could not be read.",
        turnCount: 0, lastActive: "", unreadable: true
      })
    }
  }

  function loadHistoryDetail(result) {
    pastTurns.clear()
    var list = result.turns || []
    for (var i = 0; i < list.length; i++) {
      pastTurns.append({ role: String(list[i].role), text: String(list[i].text) })
    }
  }

  function handleHistoryReply(frame) {
    if (frame.id === historyListRequestId) {
      if (frame.result) loadHistoryList(frame.result)
      return
    }
    if (frame.id === historySearchRequestId) {
      if (frame.result) loadSearchResults(frame.result)
      return
    }
    if (frame.id === historyReadRequestId) {
      if (frame.result) loadHistoryDetail(frame.result)
      else historyDetailId = "" // unreadable after all; back to the listing
      return
    }
    if (frame.id === historyOpenRequestId) {
      if (frame.error) {
        errorStage = "history"
        errorMessage = String(frame.error.message || "the conversation could not be reopened")
        return
      }
      // Reopened: back to the conversation view, whose snapshot is the
      // authoritative account of what the thread now is.
      historyOpen = false
      historyDetailId = ""
      requestConversation()
    }
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
    // A window opened by clicking an error notification connects after the
    // `error` event has already gone out, so the snapshot carries it.
    errorStage = String(result.error_stage || "")
    errorMessage = String(result.error_message || "")

    // Replay anything that arrived while the snapshot was in flight.
    snapshotPending = false
    var queued = queuedEvents
    queuedEvents = []
    for (var j = 0; j < queued.length; j++) {
      handleEvent(queued[j].method, queued[j].params)
    }
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
      // One per submitted utterance — normally one a session, but a reply to
      // a pending tool confirmation ("yes", spoken or typed) is a second, and
      // showing it is right: the user answered and should see their answer.
      // Events never repeat, so appending cannot double a turn.
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
    case "activity.row":
      // One rendered feed row, pushed as it happened. Appending is all the
      // logic this window is allowed (ADR 0013) — the wording was decided
      // and tested daemon-side.
      appendActivityRow(params)
      break
    case "conversation.changed":
      // `jarvix new`, a CLI reopen, or a delete changed the thread under us;
      // re-request the authoritative snapshot rather than guessing what the
      // change meant, and refresh the library if it is on screen.
      requestConversation()
      if (historyOpen) requestHistory()
      break
    case "config.changed":
      // A saved config may have added or renamed routines.
      requestRoutines()
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
          if (win.snapshotPending) {
            // Queue: the snapshot we are waiting for predates this event.
            win.queuedEvents.push({ method: frame.method, params: frame.params || {} })
          } else {
            win.handleEvent(frame.method, frame.params || {})
          }
        } else if (frame.id !== undefined && frame.id === win.submitRequestId) {
          win.handleSubmitReply(frame)
        } else if (frame.id !== undefined && frame.id === win.activityRequestId) {
          if (frame.result) win.loadActivity(frame.result)
        } else if (frame.id !== undefined && (frame.id === win.historyListRequestId ||
                   frame.id === win.historyReadRequestId ||
                   frame.id === win.historyOpenRequestId ||
                   frame.id === win.historySearchRequestId)) {
          win.handleHistoryReply(frame)
        } else if (frame.id === 15 && frame.result) {
          win.routines = frame.result.routines || []
        } else if (frame.id === 16 && frame.error) {
          win.errorStage = "routine"
          win.errorMessage = String(frame.error.message || "the routine could not be started")
        } else if (frame.id === 1 && frame.result) {
          win.loadSnapshot(frame.result)
        } else if (frame.id === 1 && frame.error) {
          // The snapshot failed; stop queueing or events would pile up
          // unrendered for the life of the connection.
          win.snapshotPending = false
          win.queuedEvents = []
        }
      }
    }

    onConnectionStateChanged: {
      win.socketReady = connected
      if (connected) {
        win.requestConversation()
        win.requestRoutines()
        // The snapshot replaces the model wholesale (seq keeps replays
        // honest), so a reconnect — possibly to a restarted daemon — always
        // converges on what the daemon actually holds.
        win.requestActivity()
      } else {
        win.sessionState = "idle"
        win.assistantStreaming = false
        // A disconnect mid-request means the snapshot will never arrive;
        // leaving the flag set would queue every event of the next
        // connection unrendered.
        win.snapshotPending = false
        win.queuedEvents = []
        // A submit in flight when the socket died was never delivered: hand
        // the text back rather than swallowing it. The "daemon is not
        // running" panel is the explanation; the error banner is hidden
        // while disconnected and would be replaced by the next snapshot.
        win.returnTypedText()
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
    // Typing "yes" here answers the pending tool call rather than asking
    // something new, so the header has to say that a question is open —
    // otherwise the composer looks like an ordinary empty prompt.
    case "awaiting_confirmation": return "Waiting for your yes or no"
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

      // Activity toggle: the live feed of what Jarvix is doing (issue #70).
      // Same shape as the other header toggles — keyboard-reachable, state
      // as text.
      Rectangle {
        id: activityButton
        visible: win.socketReady
        width: activityButtonText.width + Style.space(20)
        height: activityButtonText.height + Style.space(8)
        anchors.verticalCenter: parent.verticalCenter
        radius: Style.cornerRadius
        color: Util.alpha(Color.popups.text, activityButton.activeFocus ? 0.18 : 0.08)
        border.color: Util.alpha(Color.popups.text, 0.5)
        border.width: activityButton.activeFocus ? 2 : 1
        activeFocusOnTab: true
        Accessible.role: Accessible.Button
        Accessible.name: win.activityOpen ? "Back to conversation" : "Open the activity feed"
        function toggle() {
          if (win.activityOpen) win.activityOpen = false
          else win.openActivity()
        }
        Keys.onReturnPressed: activityButton.toggle()
        Keys.onSpacePressed: activityButton.toggle()
        Text {
          id: activityButtonText
          anchors.centerIn: parent
          text: win.activityOpen ? "Conversation" : "Activity"
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Color.popups.text
        }
        MouseArea { anchors.fill: parent; onClicked: activityButton.toggle() }
      }

      // History toggle: the archived-conversation library (ADR 0027). Same
      // shape as the settings toggle — keyboard-reachable, state as text.
      Rectangle {
        id: historyButton
        visible: win.socketReady
        width: historyButtonText.width + Style.space(20)
        height: historyButtonText.height + Style.space(8)
        anchors.verticalCenter: parent.verticalCenter
        radius: Style.cornerRadius
        color: Util.alpha(Color.popups.text, historyButton.activeFocus ? 0.18 : 0.08)
        border.color: Util.alpha(Color.popups.text, 0.5)
        border.width: historyButton.activeFocus ? 2 : 1
        activeFocusOnTab: true
        Accessible.role: Accessible.Button
        Accessible.name: win.historyOpen ? "Back to conversation" : "Open past conversations"
        function toggle() {
          if (win.historyOpen) { win.historyOpen = false; win.historyDetailId = "" }
          else win.openHistory()
        }
        Keys.onReturnPressed: historyButton.toggle()
        Keys.onSpacePressed: historyButton.toggle()
        Text {
          id: historyButtonText
          anchors.centerIn: parent
          text: win.historyOpen ? "Conversation" : "History"
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Color.popups.text
        }
        MouseArea { anchors.fill: parent; onClicked: historyButton.toggle() }
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
        function toggle() {
          win.historyOpen = false
          win.historyDetailId = ""
          win.activityOpen = false
          win.settingsOpen = !win.settingsOpen
        }
        Keys.onReturnPressed: settingsButton.toggle()
        Keys.onSpacePressed: settingsButton.toggle()
        Text {
          id: settingsButtonText
          anchors.centerIn: parent
          text: win.settingsOpen ? "Conversation" : "Settings"
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Color.popups.text
        }
        MouseArea { anchors.fill: parent; onClicked: settingsButton.toggle() }
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
      visible: win.socketReady && !win.settingsOpen && !win.historyOpen && !win.activityOpen && turns.count === 0
      anchors.centerIn: parent
      text: "No conversation yet — hold Super+Alt+V and speak, or type below."
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

    // The history screen shares the conversation's content area, exactly as
    // settings does: the listing, or one record read-only with a Resume
    // button.
    Item {
      id: historyScreen
      visible: win.socketReady && win.historyOpen && !win.settingsOpen
      anchors.top: header.bottom
      anchors.topMargin: Style.space(12)
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.bottom: errorBanner.visible ? errorBanner.top : parent.bottom
      anchors.bottomMargin: errorBanner.visible ? Style.space(12) : 0

      Text {
        visible: win.historyDetailId === "" && !win.searchActive && pastConversations.count === 0
        anchors.centerIn: parent
        width: parent.width
        horizontalAlignment: Text.AlignHCenter
        wrapMode: Text.Wrap
        text: "No archived conversations yet — they appear here after jarvix new."
        font.family: Style.font.family
        font.pixelSize: Style.font.subtitle
        color: Util.alpha(Color.popups.text, 0.7)
      }

      // The search box (issue #59): type what you are looking for, press
      // Enter, and the daemon searches the archive — this box only displays
      // what comes back. Emptying it returns to the library.
      Rectangle {
        id: historySearchBox
        visible: win.historyDetailId === ""
        anchors.top: parent.top
        anchors.left: parent.left
        anchors.right: parent.right
        height: historySearchInput.height + Style.space(14)
        radius: Style.cornerRadius
        color: Util.alpha(Color.popups.text, 0.06)
        // The focus ring: a colour *and* a thicker border, like the composer.
        border.color: historySearchInput.activeFocus ? Color.accent : Util.alpha(Color.popups.text, 0.4)
        border.width: historySearchInput.activeFocus ? 2 : 1

        TextInput {
          id: historySearchInput
          anchors.verticalCenter: parent.verticalCenter
          anchors.left: parent.left
          anchors.right: parent.right
          anchors.margins: Style.space(10)
          activeFocusOnTab: true
          clip: true
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Color.popups.text
          selectByMouse: true
          Accessible.role: Accessible.EditableText
          Accessible.name: "Search past conversations"
          Accessible.description: "Type what you are looking for and press Enter"

          Keys.onPressed: function(event) {
            if (event.key !== Qt.Key_Return && event.key !== Qt.Key_Enter) return
            event.accepted = true
            win.requestHistorySearch(historySearchInput.text)
          }
          // Clearing the box is itself the way back to the library — no
          // round trip, nothing to cancel.
          onTextChanged: { if (text.trim() === "") win.requestHistorySearch("") }

          Text {
            visible: historySearchInput.text === ""
            anchors.left: parent.left
            anchors.verticalCenter: parent.verticalCenter
            text: "Search past conversations, press Enter"
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Util.alpha(Color.popups.text, 0.45)
          }
        }
      }

      Text {
        visible: win.historyDetailId === "" && win.searchActive && searchResults.count === 0
        anchors.centerIn: parent
        width: parent.width
        horizontalAlignment: Text.AlignHCenter
        wrapMode: Text.Wrap
        text: "Nothing in your past conversations mentions that."
        font.family: Style.font.family
        font.pixelSize: Style.font.subtitle
        color: Util.alpha(Color.popups.text, 0.7)
      }

      // Search results, ranked best first by the daemon. Clicking one opens
      // the conversation it came from.
      ListView {
        id: searchList
        visible: win.historyDetailId === "" && win.searchActive
        anchors.top: historySearchBox.bottom
        anchors.topMargin: Style.space(10)
        anchors.left: parent.left
        anchors.right: parent.right
        anchors.bottom: parent.bottom
        clip: true
        spacing: Style.space(10)
        model: searchResults

        delegate: Rectangle {
          width: searchList.width
          height: searchEntry.height + Style.space(16)
          radius: Style.cornerRadius
          color: Util.alpha(Color.popups.text, 0.06)

          Column {
            id: searchEntry
            anchors.verticalCenter: parent.verticalCenter
            anchors.left: parent.left
            anchors.right: parent.right
            anchors.margins: Style.space(10)
            spacing: Style.space(2)

            Text {
              text: model.passage
              width: parent.width
              elide: Text.ElideRight
              font.family: Style.font.family
              font.bold: true
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
            }
            Text {
              // "This conversation" versus a past one is stated in words —
              // the same distinction the spoken answer draws.
              text: (model.current ? "this conversation" : model.cid)
                + " · turn " + model.turn + " · " + model.lastActive
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: model.current ? Color.accent : Util.alpha(Color.popups.text, 0.7)
            }
          }
          MouseArea {
            anchors.fill: parent
            onClicked: win.requestHistoryDetail(model.cid)
          }
        }
      }

      // The library: id, when, how much, and the first line.
      ListView {
        id: pastList
        visible: win.historyDetailId === "" && !win.searchActive
        anchors.top: historySearchBox.bottom
        anchors.topMargin: Style.space(10)
        anchors.left: parent.left
        anchors.right: parent.right
        anchors.bottom: parent.bottom
        clip: true
        spacing: Style.space(10)
        model: pastConversations

        delegate: Rectangle {
          width: pastList.width
          height: pastEntry.height + Style.space(16)
          radius: Style.cornerRadius
          color: Util.alpha(Color.popups.text, 0.06)
          opacity: model.unreadable ? 0.6 : 1.0

          Column {
            id: pastEntry
            anchors.verticalCenter: parent.verticalCenter
            anchors.left: parent.left
            anchors.right: parent.right
            anchors.margins: Style.space(10)
            spacing: Style.space(2)

            Text {
              text: model.preview !== "" ? model.preview : "(no preview)"
              width: parent.width
              elide: Text.ElideRight
              font.family: Style.font.family
              font.bold: true
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
            }
            Text {
              text: model.unreadable
                ? model.cid + " — unreadable"
                : model.cid + " · " + model.turnCount + " turns · " + model.lastActive
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Util.alpha(Color.popups.text, 0.7)
            }
          }
          MouseArea {
            anchors.fill: parent
            enabled: !model.unreadable
            onClicked: win.requestHistoryDetail(model.cid)
          }
        }
      }

      // One record, read-only. Resume is the explicit action that makes it
      // the active thread again (conversation.open); everything else here
      // only displays.
      Column {
        visible: win.historyDetailId !== ""
        anchors.fill: parent
        spacing: Style.space(8)

        Row {
          spacing: Style.space(8)

          Rectangle {
            id: backButton
            width: backButtonText.width + Style.space(20)
            height: backButtonText.height + Style.space(8)
            radius: Style.cornerRadius
            color: Util.alpha(Color.popups.text, backButton.activeFocus ? 0.18 : 0.08)
            border.color: Util.alpha(Color.popups.text, 0.5)
            border.width: backButton.activeFocus ? 2 : 1
            activeFocusOnTab: true
            Accessible.role: Accessible.Button
            Accessible.name: "Back to the conversation list"
            Keys.onReturnPressed: win.historyDetailId = ""
            Keys.onSpacePressed: win.historyDetailId = ""
            Text {
              id: backButtonText
              anchors.centerIn: parent
              text: "Back"
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
            }
            MouseArea { anchors.fill: parent; onClicked: win.historyDetailId = "" }
          }

          Rectangle {
            id: resumeButton
            width: resumeButtonText.width + Style.space(20)
            height: resumeButtonText.height + Style.space(8)
            radius: Style.cornerRadius
            color: Util.alpha(Color.accent, resumeButton.activeFocus ? 0.3 : 0.15)
            border.color: Color.accent
            border.width: resumeButton.activeFocus ? 2 : 1
            activeFocusOnTab: true
            Accessible.role: Accessible.Button
            Accessible.name: "Continue this conversation"
            Keys.onReturnPressed: win.resumeConversation(win.historyDetailId)
            Keys.onSpacePressed: win.resumeConversation(win.historyDetailId)
            Text {
              id: resumeButtonText
              anchors.centerIn: parent
              text: "Continue this conversation"
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
            }
            MouseArea { anchors.fill: parent; onClicked: win.resumeConversation(win.historyDetailId) }
          }

          Text {
            text: "Read-only"
            anchors.verticalCenter: parent.verticalCenter
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Util.alpha(Color.popups.text, 0.6)
          }
        }

        ListView {
          id: pastTurnList
          width: parent.width
          height: parent.height - parent.spacing - backButton.height
          clip: true
          spacing: Style.space(14)
          model: pastTurns

          delegate: Column {
            width: pastTurnList.width
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
      }
    }

    // The activity screen: what Jarvix is doing right now and has done
    // recently, one rendered row per daemon decision (issue #70). A turn
    // that acted shows its tool rows; a turn that only talked shows the
    // explicit text-only marker; every refusal carries the daemon's reason.
    // With the daemon down, the window's standard not-running panel stands
    // in — this screen, like the others, only exists while connected.
    Item {
      id: activityScreen
      visible: win.socketReady && win.activityOpen && !win.settingsOpen && !win.historyOpen
      anchors.top: header.bottom
      anchors.topMargin: Style.space(12)
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.bottom: errorBanner.visible ? errorBanner.top : parent.bottom
      anchors.bottomMargin: errorBanner.visible ? Style.space(12) : 0

      Text {
        visible: activityRows.count === 0
        anchors.centerIn: parent
        width: parent.width
        horizontalAlignment: Text.AlignHCenter
        wrapMode: Text.Wrap
        text: "Nothing yet — everything Jarvix does will appear here as it happens."
        font.family: Style.font.family
        font.pixelSize: Style.font.subtitle
        color: Util.alpha(Color.popups.text, 0.7)
      }

      ListView {
        id: activityList
        anchors.fill: parent
        clip: true
        spacing: Style.space(8)
        model: activityRows
        // Keyboard scrollable: the list itself takes focus and the arrow and
        // page keys move it, so the feed is reviewable without a mouse.
        activeFocusOnTab: true
        Keys.onUpPressed: activityList.contentY =
          Math.max(0, activityList.contentY - Style.space(48))
        Keys.onDownPressed: activityList.contentY =
          Math.min(Math.max(0, activityList.contentHeight - activityList.height),
                   activityList.contentY + Style.space(48))

        // Follow the newest row while it streams; stop the moment the user
        // scrolls back — the same rule the conversation list applies.
        property bool followTail: true
        onMovementEnded: followTail = atYEnd
        onContentHeightChanged: { if (followTail) positionViewAtEnd() }
        onCountChanged: { if (followTail) positionViewAtEnd() }

        delegate: Row {
          width: activityList.width
          spacing: Style.space(8)

          Text {
            id: activityGlyph
            text: ActivityState.glyphFor(model.kind)
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            // The urgent colour flags a failure but never carries it alone:
            // the label says "refused", "failed", "denied" in words.
            color: model.failed ? Color.urgent : Util.alpha(Color.popups.text, 0.7)
          }

          Column {
            width: parent.width - activityGlyph.width - Style.space(8)
            spacing: Style.space(2)

            Row {
              spacing: Style.space(8)
              Text {
                text: model.time
                font.family: Style.font.family
                font.pixelSize: Style.font.subtitle
                color: Util.alpha(Color.popups.text, 0.5)
              }
              Text {
                text: model.label
                font.family: Style.font.family
                font.bold: true
                font.pixelSize: Style.font.subtitle
                color: model.failed ? Color.urgent : Color.popups.text
              }
            }
            Text {
              visible: model.detail !== ""
              text: model.detail
              width: parent.width
              wrapMode: Text.Wrap
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Util.alpha(Color.popups.text, 0.8)
            }
          }
        }
      }
    }

    ListView {
      id: list
      visible: win.socketReady && !win.settingsOpen && !win.historyOpen && !win.activityOpen
      anchors.top: header.bottom
      anchors.topMargin: Style.space(12)
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.bottom: errorBanner.visible ? errorBanner.top
        : (composer.visible ? composer.top : parent.bottom)
      anchors.bottomMargin: errorBanner.visible || composer.visible ? Style.space(12) : 0
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

    // The composer: type a question instead of saying it (issue #35). Speech
    // is the wrong input for a URL, a path, a flag, or an unusual name, and
    // it is no input at all in a quiet room or on a call.
    //
    // It holds no session logic. Enter calls win.submitTypedTurn(), which
    // sends one `session.text` request and lets the daemon decide what the
    // text means — a new turn (interrupting whatever is running) or the
    // answer to a pending tool confirmation.
    Column {
      id: composer
      visible: !win.settingsOpen && !win.historyOpen && !win.activityOpen
      // A daemon that is down disables the field rather than swallowing the
      // keystrokes; the panel above says why, and the label says it again
      // here, where the caret is.
      enabled: win.socketReady
      opacity: win.socketReady ? 1.0 : 0.55
      anchors.bottom: parent.bottom
      anchors.left: parent.left
      anchors.right: parent.right
      spacing: Style.space(4)

      // Routines panel: each configured routine, its phrases, and a Run
      // button. Sits with the composer because both are ways to start a turn.
      Column {
        id: routinesPanel
        visible: win.socketReady && win.routines.length > 0
        width: parent.width
        spacing: Style.space(4)
        bottomPadding: Style.space(8)

        Text {
          text: "Routines"
          font.family: Style.font.family
          font.bold: true
          font.pixelSize: Style.font.subtitle
          color: Util.alpha(Color.popups.text, 0.7)
        }

        Repeater {
          model: win.routines
          delegate: Row {
            id: routineRow
            required property var modelData
            width: routinesPanel.width
            spacing: Style.space(8)

            Rectangle {
              id: runButton
              width: runLabel.width + Style.space(20)
              height: runLabel.height + Style.space(8)
              anchors.verticalCenter: parent.verticalCenter
              radius: Style.cornerRadius
              color: Util.alpha(Color.accent, runButton.activeFocus ? 0.35 : 0.18)
              border.color: Color.accent
              border.width: runButton.activeFocus ? 2 : 1
              activeFocusOnTab: true
              Accessible.role: Accessible.Button
              Accessible.name: "Run routine " + routineRow.modelData.name
              Keys.onReturnPressed: win.runRoutine(routineRow.modelData.name)
              Keys.onSpacePressed: win.runRoutine(routineRow.modelData.name)
              Text {
                id: runLabel
                anchors.centerIn: parent
                text: "Run"
                font.family: Style.font.family
                font.pixelSize: Style.font.subtitle
                color: Color.popups.text
              }
              MouseArea { anchors.fill: parent; onClicked: win.runRoutine(routineRow.modelData.name) }
            }

            Text {
              width: routineRow.width - runButton.width - Style.space(8)
              anchors.verticalCenter: parent.verticalCenter
              wrapMode: Text.Wrap
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
              text: routineRow.modelData.name
                + "  —  say “" + (routineRow.modelData.phrases || []).join("” or “") + "”"
            }
          }
        }
      }

      Text {
        id: composerLabel
        text: win.socketReady ? "Ask Jarvix" : "Ask Jarvix — start jarvixd to type"
        font.family: Style.font.family
        font.bold: true
        font.pixelSize: Style.font.subtitle
        color: Util.alpha(Color.popups.text, 0.7)
      }

      Rectangle {
        id: composerBox
        width: parent.width
        height: composerInput.height + Style.space(14)
        radius: Style.cornerRadius
        color: Util.alpha(Color.popups.text, 0.06)
        // The focus ring: visible as a colour *and* a thicker border, so it
        // reads for anyone who cannot pick the accent out of the foreground.
        border.color: composerInput.activeFocus ? Color.accent : Util.alpha(Color.popups.text, 0.4)
        border.width: composerInput.activeFocus ? 2 : 1

        TextInput {
          id: composerInput
          anchors.verticalCenter: parent.verticalCenter
          anchors.left: parent.left
          anchors.right: parent.right
          anchors.margins: Style.space(10)
          activeFocusOnTab: composer.enabled
          readOnly: !win.socketReady
          clip: true
          // Every size comes from the shell's Style tokens, so the field
          // grows with the user's font scale like the rest of the window.
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Color.popups.text
          selectByMouse: true
          Accessible.role: Accessible.EditableText
          Accessible.name: composerLabel.text
          Accessible.description: "Type a question and press Enter to send it to Jarvix"

          // Enter sends. Shift+Enter is deliberately swallowed: multi-line
          // composition is not built yet (issue #35 scopes it out), and the
          // reflex of reaching for it must not post half a thought.
          Keys.onPressed: function(event) {
            if (event.key !== Qt.Key_Return && event.key !== Qt.Key_Enter) return
            event.accepted = true
            if (event.modifiers & Qt.ShiftModifier) return
            win.submitTypedTurn()
          }

          Text {
            visible: composerInput.text === ""
            anchors.left: parent.left
            anchors.verticalCenter: parent.verticalCenter
            text: win.socketReady ? "Type a question, press Enter" : "Jarvix daemon is not running"
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Util.alpha(Color.popups.text, 0.45)
          }
        }
      }
    }

    // Failures are stated in words — stage and message — not colour alone.
    Rectangle {
      id: errorBanner
      visible: win.socketReady && win.errorMessage !== ""
      anchors.bottom: composer.visible ? composer.top : parent.bottom
      anchors.bottomMargin: composer.visible ? Style.space(12) : 0
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
      else if (win.historyDetailId !== "") win.historyDetailId = ""
      // Clearing the box also clears the results (onTextChanged), so Escape
      // steps search → library → conversation → closed, one layer at a time.
      else if (win.searchActive) historySearchInput.text = ""
      else if (win.historyOpen) win.historyOpen = false
      else if (win.activityOpen) win.activityOpen = false
      else win.closeWindow()
    }
  }
}
