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
// The tab also hosts the return briefing (#150, ADR 0050) — the full version
// of the account the voice path shortens for speech. It lives here rather
// than in the Activity tab because its subject matter is this tab's: the
// focus threads and the AI sessions anchored to them. The Activity tab is a
// live chronological ring of one row per event, and a composed multi-line
// report is not one of those.
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

  // The return briefing, requested on demand and never cached: it is
  // transient by design (ADR 0050), so a closed tab holds nothing.
  property var briefing: null
  property bool briefingOpen: false

  // The create/edit form (#164). A thread was makeable only by speaking, in
  // pieces — "new thread", then a name, then "with this window", then "check in
  // every twenty minutes" — and its recap mode was not reachable by voice at
  // all, only by hand-editing focus.json. The form sends the whole draft to
  // focus.save, which applies it in ONE write: four spoken acts would be four
  // writes, and a failure between two of them would leave a half-configured
  // thread. Everything it enforces is the voice path's own rule, reached
  // through the same store.
  property bool formOpen: false
  property string formThreadId: "" // "" while creating
  property string draftName: ""
  property string draftRemind: ""
  property string draftRecap: ""      // "" | "always" | "never"
  property int draftAnchors: 0
  property bool draftReanchor: false  // false leaves the anchors untouched
  property var formProblems: []
  property string formError: ""

  readonly property int listRequestId: 500
  readonly property int switchRequestId: 501
  readonly property int endRequestId: 502
  readonly property int sessionEndRequestId: 503
  readonly property int saveRequestId: 504
  readonly property int briefingRequestId: 510

  // The recap modes, in the daemon's own vocabulary (focus.RecapAuto/Always/
  // Never) with the sentence each one means. A cycle rather than a dropdown:
  // three values, and the label says which is chosen.
  readonly property var recapModes: [
    { value: "", label: "Automatic — read an anchored terminal only" },
    { value: "always", label: "Always — read whatever the anchored window is" },
    { value: "never", label: "Never — no model-composed recap for this thread" }
  ]

  function recapLabel(value) {
    for (var i = 0; i < recapModes.length; i++) {
      if (recapModes[i].value === String(value || "")) return recapModes[i].label
    }
    return String(value)
  }

  function stepRecap(delta) {
    var at = 0
    for (var i = 0; i < recapModes.length; i++) {
      if (recapModes[i].value === draftRecap) { at = i; break }
    }
    draftRecap = String(recapModes[(at + delta + recapModes.length) % recapModes.length].value)
  }

  function openCreate() {
    formThreadId = ""
    draftName = ""
    draftRemind = ""
    draftRecap = ""
    draftAnchors = 1
    draftReanchor = true // a new thread is what you just started looking at
    formProblems = []
    formError = ""
    formOpen = true
  }

  function openEdit(t) {
    formThreadId = String(t.id || "")
    draftName = String(t.name || "")
    draftRemind = t.remind_every_min ? String(t.remind_every_min) : ""
    draftRecap = String(t.recap || "")
    draftAnchors = (t.anchors || []).length
    // An edit leaves the anchors alone unless the user says otherwise: a
    // rename must not silently re-point the thread at whatever happens to be
    // in front of the window right now.
    draftReanchor = false
    formProblems = []
    formError = ""
    formOpen = true
  }

  function closeForm() {
    formOpen = false
    formProblems = []
    formError = ""
  }

  function saveForm() {
    var params = { name: draftName, recap: draftRecap }
    if (formThreadId !== "") params.thread = formThreadId
    var minutes = Number(String(draftRemind).trim())
    // A non-number goes as text so the daemon words what is wrong with it,
    // never this file (ADR 0013).
    params.remind_every_min = (String(draftRemind).trim() === "") ? 0
      : (isFinite(minutes) && Math.floor(minutes) === minutes ? minutes : String(draftRemind).trim())
    if (draftReanchor) params.anchors = draftAnchors
    bridge.write(JSON.stringify({ jsonrpc: "2.0", id: saveRequestId,
      method: "focus.save", params: params }) + "\n")
  }

  function handleSaveReply(frame) {
    if (frame.error) {
      formProblems = ((frame.error.data || {}).problems) || []
      formError = String(frame.error.message || "the thread could not be saved")
      return
    }
    formOpen = false
    formProblems = []
    formError = ""
    // Success needs no refresh here: focus.changed triggers one.
  }

  function problemFor(field) {
    var out = []
    for (var i = 0; i < formProblems.length; i++) {
      if (String(formProblems[i].field || "") === field) {
        out.push(String(formProblems[i].message || ""))
      }
    }
    return out.join("\n")
  }

  onActiveChanged: {
    if (active) {
      if (bridge.connected) refresh()
      else bridge.connected = true
    } else {
      bridge.connected = false
      detailId = ""
      banner = ""
      briefingOpen = false
      briefing = null
      formOpen = false
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

  function requestBriefing() {
    bridge.write(JSON.stringify({ jsonrpc: "2.0", id: briefingRequestId,
      method: "briefing.get" }) + "\n")
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
        if (frame.id === focusTab.saveRequestId) {
          focusTab.handleSaveReply(frame)
          return
        }
        if (frame.id === focusTab.briefingRequestId) {
          if (frame.error) {
            focusTab.banner = String(frame.error.message || "the briefing could not be read")
          } else {
            focusTab.banner = ""
            focusTab.briefing = frame.result || null
            focusTab.briefingOpen = true
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

  // The return briefing's way in (#150, ADR 0050). One button, always
  // available: what it answers with — an account, "nothing while you were
  // away", or "you haven't been away long enough" — is composed daemon-side,
  // so the tab never has to know which of those it is about to show.
  Row {
    id: briefingBar
    visible: focusTab.detailId === "" && !focusTab.briefingOpen
    anchors.top: focusBanner.visible ? focusBanner.bottom : parent.top
    anchors.topMargin: focusBanner.visible ? Style.space(8) : 0
    anchors.left: parent.left
    anchors.right: parent.right
    height: visible ? briefingButton.height : 0
    spacing: Style.space(8)

    JarvixFormButton {
      id: briefingButton
      label: "What did I miss?"
      name: "Show what happened while you were away"
      onClicked: focusTab.requestBriefing()
    }
  }

  // The live timebox, one glance from anywhere in the tab.
  Rectangle {
    id: sessionBanner
    visible: focusTab.session !== null && focusTab.detailId === "" && !focusTab.briefingOpen
    anchors.top: briefingBar.visible ? briefingBar.bottom
      : (focusBanner.visible ? focusBanner.bottom : parent.top)
    anchors.topMargin: (briefingBar.visible || focusBanner.visible) ? Style.space(8) : 0
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
      && !focusTab.briefingOpen && !focusTab.formOpen
    anchors.centerIn: parent
    width: parent.width
    text: "No focus threads yet — say “new thread”, then a name.\nAdd “with this window” to anchor it to what you're looking at, or use New thread below."
  }

  ListView {
    id: threadList
    visible: focusTab.threads.length > 0 && focusTab.detailId === ""
      && !focusTab.briefingOpen && !focusTab.formOpen
    anchors.top: sessionBanner.visible ? sessionBanner.bottom
      : (briefingBar.visible ? briefingBar.bottom
        : (focusBanner.visible ? focusBanner.bottom : parent.top))
    anchors.topMargin: (sessionBanner.visible || briefingBar.visible || focusBanner.visible)
      ? Style.space(10) : 0
    anchors.left: parent.left
    anchors.right: parent.right
    anchors.bottom: newThreadRow.top
    anchors.bottomMargin: Style.space(10)
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
      action2Label: "Edit"
      action2Name: "Edit the " + modelData.name + " thread"
      onAction2Triggered: focusTab.openEdit(modelData)
      action3Label: "End"
      action3Name: "End the " + modelData.name + " thread"
      onAction3Triggered: focusTab.endThread(modelData.id)
    }
  }

  Row {
    id: newThreadRow
    visible: focusTab.detailId === "" && !focusTab.briefingOpen && !focusTab.formOpen
    anchors.bottom: parent.bottom
    anchors.left: parent.left

    JarvixFormButton {
      label: "New thread…"
      name: "Create a new focus thread"
      accent: true
      onClicked: focusTab.openCreate()
    }
  }

  // The thread form, on the shared detail scaffold. Everything a thread has:
  // its name, what it is anchored to, how often to check in, and what a switch
  // back should read.
  JarvixDetailPane {
    visible: focusTab.formOpen
    anchors.fill: parent
    backName: "Cancel and go back to the threads"
    actionLabel: "Save"
    actionName: "Save this thread"
    note: focusTab.formThreadId === "" ? "New thread" : "Editing “" + focusTab.draftName + "”"
    onBackRequested: focusTab.closeForm()
    onActionTriggered: focusTab.saveForm()

    Flickable {
      anchors.fill: parent
      clip: true
      contentWidth: width
      contentHeight: formColumn.height + Style.space(12)

      Column {
        id: formColumn
        width: parent.width
        spacing: Style.space(10)

        Text {
          visible: focusTab.formError !== "" || focusTab.problemFor("") !== ""
          width: parent.width
          wrapMode: Text.Wrap
          text: (focusTab.formError !== "" ? focusTab.formError + "\n" : "")
            + focusTab.problemFor("")
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Color.urgent
        }

        JarvixFormField {
          width: parent.width
          label: "Name"
          placeholder: "the refactor"
          hint: "What you will call it out loud — “switch to the refactor”."
          problem: focusTab.problemFor("name")
          Component.onCompleted: text = focusTab.draftName
          onEdited: function(value) { focusTab.draftName = value }
        }

        JarvixFormToggle {
          width: parent.width
          label: "Anchor to the windows in front of you"
          detail: focusTab.formThreadId === ""
            ? "Takes the most recently focused window, the way “with this window” does."
            : "Leave this off to keep the anchors this thread already has."
          checked: focusTab.draftReanchor
          onToggled: function(on) { focusTab.draftReanchor = on }
        }

        Row {
          visible: focusTab.draftReanchor
          spacing: Style.space(8)

          JarvixFormButton {
            label: "1 window"
            name: "Anchor to one window"
            accent: focusTab.draftAnchors === 1
            onClicked: focusTab.draftAnchors = 1
          }
          JarvixFormButton {
            label: "2 windows"
            name: "Anchor to two windows"
            accent: focusTab.draftAnchors === 2
            onClicked: focusTab.draftAnchors = 2
          }
          JarvixFormButton {
            label: "None"
            name: "Anchor to no windows"
            accent: focusTab.draftAnchors === 0
            onClicked: focusTab.draftAnchors = 0
          }
        }

        JarvixFormField {
          width: parent.width
          label: "Check in every (minutes, empty for never)"
          placeholder: "20"
          problem: focusTab.problemFor("remind_every_min")
          Component.onCompleted: text = focusTab.draftRemind
          onEdited: function(value) { focusTab.draftRemind = value }
        }

        Column {
          width: parent.width
          spacing: Style.space(4)

          Text {
            text: "Recap when switching back"
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Color.popups.text
          }
          JarvixFormButton {
            label: focusTab.recapLabel(focusTab.draftRecap) + "  ↔"
            name: "Recap mode: " + focusTab.recapLabel(focusTab.draftRecap)
              + ". Activate to choose another."
            onClicked: focusTab.stepRecap(1)
          }
          Text {
            visible: focusTab.problemFor("recap") !== ""
            width: parent.width
            wrapMode: Text.Wrap
            text: "Problem: " + focusTab.problemFor("recap")
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Color.urgent
          }
        }
      }
    }
  }

  // The briefing, on the shared detail scaffold: the headline the daemon
  // composed, then one section per category in the order the daemon put them
  // in. Nothing here is derived, ordered, counted or worded locally (ADR
  // 0013) — the tab lays out sentences it was handed.
  JarvixDetailPane {
    visible: focusTab.briefingOpen
    anchors.fill: parent
    backName: "Back to the focus threads"
    note: focusTab.briefing && focusTab.briefing.away_spoken
      ? "last here " + focusTab.briefing.away_spoken : ""
    actionLabel: "Refresh"
    actionName: "Read the briefing again"
    onActionTriggered: focusTab.requestBriefing()
    onBackRequested: { focusTab.briefingOpen = false; focusTab.briefing = null }

    Flickable {
      anchors.fill: parent
      clip: true
      contentWidth: width
      contentHeight: briefingColumn.height

      Column {
        id: briefingColumn
        width: parent.width
        spacing: Style.space(12)

        Text {
          width: parent.width
          visible: text !== ""
          text: focusTab.briefing ? String(focusTab.briefing.headline || "") : ""
          wrapMode: Text.Wrap
          font.family: Style.font.family
          font.bold: true
          font.pixelSize: Style.font.subtitle
          color: Color.popups.text
        }

        Repeater {
          model: focusTab.briefing && focusTab.briefing.sections
            ? focusTab.briefing.sections : []

          Column {
            required property var modelData
            width: briefingColumn.width
            spacing: Style.space(4)

            Text {
              text: String(modelData.title || "")
              font.family: Style.font.family
              font.bold: true
              font.pixelSize: Style.font.subtitle
              color: Util.alpha(Color.popups.text, 0.7)
            }
            Repeater {
              model: modelData.lines || []
              Text {
                required property var modelData
                width: briefingColumn.width
                text: String(modelData)
                wrapMode: Text.Wrap
                font.family: Style.font.family
                font.pixelSize: Style.font.subtitle
                color: Color.popups.text
              }
            }
          }
        }
      }
    }
  }

  // One thread's parked thoughts, on the shared detail scaffold.
  JarvixDetailPane {
    visible: focusTab.detailId !== "" && !focusTab.briefingOpen && !focusTab.formOpen
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
