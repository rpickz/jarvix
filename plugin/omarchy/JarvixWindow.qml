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
// The pending assistant turn's wording (issue #158) is compiled from
// internal/desktop/pending.go into the same generated library the bar reads.
// The window imports it to render a string, not to decide one — what a wait
// says, and when it starts saying how long, is tested in Go (ADR 0013).
import "BarState.js" as BarState

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
  // Wide enough for the tab strip to sit on one line at the default font
  // scale; the strip is a Flow, so a narrower window wraps it rather than
  // clipping tabs out of reach.
  implicitWidth: 600
  implicitHeight: 640
  color: Color.background

  // The window's surfaces are tabs (issue #91): one strip across the top,
  // one content pane below it, Chat first and default. Tab selection is
  // presentation state and nothing else (ADR 0013) — every data flow below
  // is unchanged by which tab is showing, so Chat keeps streaming and a
  // pending confirmation keeps counting down behind whatever tab is open.
  //
  // The surfaces the tabs show:
  //   chat        — the conversation, composer, and confirmation card.
  //   activity    — the daemon's activity feed (issue #70): every row
  //                 arrives already worded — assembled daemon-side from bus
  //                 events, served by activity.get plus activity.row pushes —
  //                 so the screen renders text, looks up glyphs
  //                 (ActivityState.js, generated from Go), and decides
  //                 nothing (ADR 0013).
  //   library     — archived conversations (ADR 0027), one read-only with a
  //                 Resume button: conversation.list / conversation.read, and
  //                 Resume is one conversation.open call — the daemon owns
  //                 what reopening means.
  //   automations — the configured routines (ADR 0026) and scripts
  //                 (ADR 0030) as one managed collection (issues #93/#99):
  //                 automations.list for everything shown, Run through the
  //                 existing gated paths, Enable/Disable through
  //                 automations.set_enabled (the surgical config write),
  //                 and New/Edit/Delete through the entry form dialog
  //                 (config.get_entry / validate_entry / upsert_entry /
  //                 delete_entry — validation and the byte-preserving
  //                 rewrite are entirely the daemon's).
  //   knowledge   — the feed cache (ADR 0031): knowledge.status for
  //                 everything shown, refresh/enable through the existing
  //                 verbs, and New/Edit/Delete through the same entry form
  //                 dialog as automations (family knowledge.feeds, #100).
  //   memory      — the fact store (ADR 0025): memory.list for the listing,
  //                 the gated forget, and Add/Edit through memory.add /
  //                 memory.update — the book's own write path (#100).
  //   settings    — the settings screen (issue #9), unchanged inside its tab.
  readonly property var tabs: [
    { id: "chat", label: "Chat" },
    { id: "activity", label: "Activity" },
    // situation — the situation report (#196, ADR 0061): one composed answer
    // to "where are we?", every line linking to the thing it describes
    // through the provenance navigation. Self-contained in
    // JarvixSituationTab.qml (own socket, request ids 600-699,
    // situation.get). Beside Activity rather than inside it: the Activity tab
    // is a live chronological ring of one row per event, and this is the
    // summary of the machine that ring is a record of.
    { id: "situation", label: "Situation" },
    // focus — the focus threads (#123, ADR 0041): threads with anchors,
    // parked thoughts and the live timeboxed session, self-contained in
    // JarvixFocusTab.qml (own socket, request ids 500–599, focus.list /
    // focus.changed).
    { id: "focus", label: "Focus" },
    { id: "library", label: "Library" },
    { id: "automations", label: "Automations" },
    { id: "knowledge", label: "Knowledge" },
    // providers — the endpoints and advisor CLIs Jarvix uses (#163), on the
    // generic entry-admin verbs with family "ai" and "advisors".
    { id: "providers", label: "Providers" },
    { id: "memory", label: "Memory" },
    // approvals — the standing grants (#162, ADR 0053): every command
    // pattern that runs without asking, when it was agreed to, how often it
    // has fired, and a Forget button on each. Its own tab rather than a
    // corner of Settings because a permission you cannot find is a
    // permission you cannot revoke.
    { id: "approvals", label: "Approvals" },
    { id: "settings", label: "Settings" }
  ]
  property string currentTab: "chat"

  property string historyDetailId: "" // "" shows the Library listing; an id shows that record

  function tabIndexOf(id) {
    for (var i = 0; i < tabs.length; i++) {
      if (tabs[i].id === id) return i
    }
    return 0
  }

  // openTab switches the content pane. Each tab keeps its own scroll and
  // state for the life of the window — the surfaces stay instantiated and
  // only visibility changes — so switching refreshes data where it could be
  // stale but never resets what the user was looking at.
  function openTab(id) {
    currentTab = id
    if (id === "library") requestHistory()
    else if (id === "automations") {
      requestAutomations()
      requestSpokenCommands()
      requestReminders()
      requestMonitors()
      requestPlacementVocabulary()
    }
    else if (id === "knowledge") requestKnowledge()
    else if (id === "providers") requestProviders()
    else if (id === "memory") {
      requestMemory()
      requestVocabulary()
    }
    else if (id === "approvals") {
      requestApprovals()
      requestManagedWindows()
    }
    else if (id === "chat" && pendingCardIndex >= 0) {
      // A permission question must never be hidden by tab state: coming back
      // to Chat lands on the card, wherever the list had been scrolled.
      Qt.callLater(function() {
        if (win.pendingCardIndex >= 0) list.positionViewAtIndex(win.pendingCardIndex, ListView.End)
      })
    }
  }

  // stepTab selects the tab `delta` places along the strip, wrapping. The
  // arrow keys also move focus with the selection so the strip behaves like
  // one control; Ctrl+Tab cycles without stealing focus from the content.
  function stepTab(delta, moveFocus) {
    var next = (tabIndexOf(currentTab) + delta + tabs.length) % tabs.length
    openTab(tabs[next].id)
    if (moveFocus) {
      var item = tabRepeater.itemAt(next)
      if (item) item.forceActiveFocus()
    }
  }

  function focusComposer() {
    Qt.callLater(function() {
      if (win.currentTab === "chat") composerInput.forceActiveFocus()
    })
  }

  // --- daemon state -------------------------------------------------------
  property bool socketReady: false
  property string sessionState: "idle"
  property string errorStage: ""
  property string errorMessage: ""
  // True while assistant.delta events are building the newest turn.
  property bool assistantStreaming: false

  // --- the pending assistant turn (issue #158) ----------------------------
  // Between submitting a question and the first token the message list used to
  // show the user's turn and then blank space — measured at ~6 seconds on the
  // current model stack, with nothing in the list to say why. The header's
  // "— Thinking" was too far away to be noticed, and the bar swapped a
  // monochrome glyph. So the wait gets a turn of its own, in the place the
  // user is actually looking.
  //
  // It is one row in `turns`, marked pending, always the last one, and it
  // *becomes* the answer: the first assistant.delta clears the flag and
  // streams into the same row rather than appending a second bubble. That is
  // the whole no-double-bubble/no-flash mechanism — there is only ever one row
  // to see, and a fast answer simply replaces short-lived text in it.
  //
  // What it says comes from BarState.pendingTurnLine (generated from
  // internal/desktop/pending.go): the session state, the tool in flight, and
  // how long the daemon says this phase has been running.
  property int pendingTurnIndex: -1   // index into turns; -1 when none is open
  property string currentTool: ""     // tool.started / tool.finished
  property string toolDetail: ""      // the tool's own progress label, when it has one
  // When the current state began, on the daemon's clock (state.changed's
  // since_ms, conversation.get's state_since_ms). Elapsed is measured from
  // here rather than from when this window saw the transition, so a window
  // opened five seconds into a long think says "5s" instead of starting its
  // own clock at zero and telling a comfortable lie.
  property double stateSinceMs: 0
  property int pendingElapsedSec: 0

  // --- the thinking level (issue #159, ADR 0063) --------------------------
  // Quick / Balanced / Deep: the trade between how fast an answer arrives and
  // how good it is, per conversation, without editing config.toml.
  //
  // The control below is a view of daemon state and nothing more. The level
  // lives in the engine — one place, moved by this control, by the spoken
  // phrases ("switch to deep"), and by nothing else — so a voice change and a
  // click can never leave two surfaces disagreeing. `thinking.changed` is what
  // keeps this window current when the change came from the microphone.
  //
  // The levels come from the daemon too, including the ones this machine
  // cannot serve: a control that silently dropped "Deep" would leave the user
  // wondering whether the feature exists, where one that shows it and says
  // what is missing tells them what to configure. Clicking an unconfigured
  // level is answered here, in the control, rather than at answer time — which
  // is the whole point of asking before the turn instead of during it.
  property string thinking: ""
  property string thinkingLabel: ""
  property var thinkingLevels: []
  property string thinkingNote: ""
  // The tier that is serving the turn in flight, for the pending line. Cleared
  // when the session ends, exactly like currentTool and for the same reason.
  property string pendingTier: ""

  // JSON-RPC ids from this feature's own private range (950-999, the reminders
  // and approvals discipline) so its replies are recognisable by construction.
  property int thinkingGetRequestId: 0
  property int thinkingSetRequestId: 0
  property int nextThinkingRequestId: 950

  function takeThinkingRequestId() {
    var id = nextThinkingRequestId
    nextThinkingRequestId = nextThinkingRequestId >= 999 ? 950 : nextThinkingRequestId + 1
    return id
  }

  function requestThinking() {
    if (!daemon.connected) return
    thinkingGetRequestId = takeThinkingRequestId()
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: thinkingGetRequestId,
      method: "thinking.get" }) + "\n")
  }

  function setThinking(tier) {
    if (!daemon.connected) return
    thinkingNote = ""
    thinkingSetRequestId = takeThinkingRequestId()
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: thinkingSetRequestId,
      method: "thinking.set", params: { thinking: tier } }) + "\n")
  }

  function loadThinking(result) {
    thinking = String(result.thinking || "")
    thinkingLabel = String(result.thinking_label || "")
    thinkingLevels = result.levels || []
  }

  // A refusal is shown where the control stands, in words. The daemon's
  // sentence is used verbatim: it is the same sentence the spoken path says,
  // and re-wording it here would be a second copy of the vocabulary.
  function handleThinkingSetReply(frame) {
    if (frame.error) {
      thinkingNote = String((frame.error && frame.error.message) || "That level is not available.")
      return
    }
    thinkingNote = ""
    if (frame.result) loadThinking(frame.result)
  }

  ListModel { id: turns } // { role: "user"|"assistant"|"confirmation", text, command, outcome, pos, pending, provJson }
  // Activity rows, oldest first, exactly as the daemon rendered them.
  ListModel { id: activityRows } // { seq, time, kind, label, detail, failed }
  // Archived conversations, newest first. "cid" rather than "id" because id
  // is the QML object-id keyword. Unreadable records list too, greyed: one
  // bad file never hides itself, let alone the library.
  ListModel { id: pastConversations } // { cid, preview, turnCount, lastActive, unreadable }
  ListModel { id: pastTurns }         // { role, text, command, outcome } — the record being viewed
  // Search hits over the archive (issue #59), ranked by the daemon. The
  // window renders them and opens the conversation a hit names — every
  // matching, ranking and bounding decision is made daemon-side (ADR 0013).
  ListModel { id: searchResults }     // { cid, turn, passage, lastActive, current }

  function openWindow() { visible = true }
  function closeWindow() { visible = false }
  function toggleWindow() { visible = !visible }

  // Open straight onto the Settings tab. Settings live inside this window
  // rather than in a window of their own (issue #9), so "open settings" is
  // "open the window, already showing settings" — what the bar widget's
  // Settings action asks for. Escape still steps back to Chat before
  // closing, so the shortcut cannot strand anyone.
  function openSettings() {
    openTab("settings")
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
      focusComposer()
    } else {
      daemon.connected = false
    }
  }

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

  // --- reading comfort (issue #121) ---------------------------------------
  // The transcript's typography is a setting, not a constant: line spacing,
  // message text size, and letter spacing come from the daemon's settings
  // registry via config.get — the same snapshot the Settings tab renders —
  // and refresh on config.changed, so a change from the Settings form,
  // `jarvix config set`, or the assistant's own settings tool lands on the
  // open transcript without a restart (ADR 0013: the daemon decides, this
  // window renders). All three are relative units: the size a multiple of
  // the design token, letter spacing in ems of the rendered size, line
  // height proportional — so they ride the shell's font scale rather than
  // fighting it. The property defaults pin the pre-#121 hard-coded
  // rendering (line height ×1.0, Style.font.subtitle ×1.0, no extra letter
  // spacing): with the daemon unreachable, or the settings untouched, the
  // transcript renders exactly as it always did. Scope is transcript
  // messages only — chrome, tabs, and cards keep the design system's scale.
  property real chatLineSpacing: 1.0
  property real chatTextScale: 1.0
  property real chatLetterSpacing: 0.0
  property int typographyRequestId: 0

  function requestTypography() {
    if (!daemon.connected) return
    typographyRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: typographyRequestId,
      method: "config.get" }) + "\n")
  }

  function loadTypography(result) {
    var fields = result.fields || []
    for (var i = 0; i < fields.length; i++) {
      var f = fields[i]
      var v = Number(f.value)
      if (!isFinite(v)) continue
      if (f.key === "ui.line_spacing") chatLineSpacing = v
      else if (f.key === "ui.text_size") chatTextScale = v
      else if (f.key === "ui.letter_spacing") chatLetterSpacing = v
    }
  }

  // --- activity feed ------------------------------------------------------
  // The snapshot is requested on every connect and the pushes keep it
  // current, so opening the pane costs nothing and survives window
  // close/reopen — the ring lives in the daemon, this model only mirrors it.
  // Reconciliation is by seq: activity.get replaces the model, and any push
  // that raced the snapshot is deduplicated because seq never repeats.
  property int activityLimit: 400
  property int activityRequestId: 0

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

  // --- automations --------------------------------------------------------
  // The Automations tab (issue #93): routines (ADR 0026) and scripts (ADR
  // 0030) as one managed collection from automations.list — every row's
  // facts (phrases, enabled, markers, schedule with the daemon's own
  // next-fire, the would-refuse verdict, the last observed run) are decided
  // daemon-side, and this window renders them and calls the verbs (ADR
  // 0013). Run replays the entry's phrase through the daemon's ordinary
  // session path — routines.run / scripts.run, the router, the permission
  // gates, the standard refusal-while-running — exactly as if it had been
  // spoken. Enable/Disable is automations.set_enabled, the surgical config
  // write; the row flips when config.changed triggers the refresh — the
  // daemon's word, not the click's.
  property var automations: []
  // The config file's fingerprint as of the last listing, passed back on
  // set_enabled so a hand edit made while the window sat open is a refused
  // conflict, never a clobber.
  property string automationsFingerprint: ""
  // Live progress per entry ("kind/name" → text), built from the run events
  // (routine.started / routine.step / script.started) and cleared when the
  // finish event refreshes the listing.
  property var automationRuns: ({})
  property int automationsRequestId: 0
  property int automationsRunRequestId: 0
  property int automationsEnableRequestId: 0

  function requestAutomations() {
    if (!daemon.connected) return
    automationsRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: automationsRequestId,
      method: "automations.list" }) + "\n")
  }

  // --- one-shot reminders (#141, ADR 0046) --------------------------------
  // The Automations tab's one-shot section: "remind me at three to …" as a
  // managed list on the shared collection rows. Every row's facts — the text
  // and the daemon-worded due moment — come from reminders.list (ADR 0013),
  // and Cancel is reminders.cancel by id; the section refreshes on the
  // reminders.changed event, so a firing or a spoken cancel updates the tab
  // without a click. JSON-RPC ids use this feature's own private range
  // (850–899, the overlay's confirm-range discipline) so its replies are
  // recognisable by construction.
  property var oneShotReminders: []
  property int reminderListRequestId: 0
  property int reminderCancelRequestId: 0
  property int nextReminderRequestId: 850

  function takeReminderRequestId() {
    var id = nextReminderRequestId
    nextReminderRequestId = nextReminderRequestId >= 899 ? 850 : nextReminderRequestId + 1
    return id
  }

  function requestReminders() {
    if (!daemon.connected) return
    reminderListRequestId = takeReminderRequestId()
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: reminderListRequestId,
      method: "reminders.list" }) + "\n")
  }

  function loadReminders(result) {
    oneShotReminders = result.reminders || []
  }

  function cancelReminder(id) {
    if (!daemon.connected) return
    reminderCancelRequestId = takeReminderRequestId()
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: reminderCancelRequestId,
      method: "reminders.cancel", params: { id: id } }) + "\n")
  }

  // handleReminderCancelReply surfaces a refused cancel in the shared banner
  // and re-requests the listing either way — the daemon's record, not the
  // click's assumption (the row itself refreshes on reminders.changed).
  function handleReminderCancelReply(frame) {
    if (frame.error) {
      errorStage = "automations"
      errorMessage = String(frame.error.message || "the reminder could not be cancelled")
    }
    requestReminders()
  }

  // The create form (#164). A reminder was makeable only by SAYING one, which
  // left anyone who prefers typing with no way to make one at all. The form
  // sends the same two things a spoken reminder carries — the words and the
  // time expression — to reminders.create, which is the spoken path's own verb
  // over the spoken path's own parser. reminders.preview is the one thing the
  // form adds: the resolved moment, in the daemon's words, shown BEFORE the
  // save, because a spoken reminder hears which reading of "at three" won and
  // a typed one would otherwise find out in the morning.
  property bool reminderFormOpen: false
  property string reminderDraftText: ""
  property string reminderDraftWhen: ""
  property string reminderPreview: ""
  property var reminderFormProblems: []
  property string reminderFormError: ""
  property int reminderPreviewRequestId: 0
  property int reminderCreateRequestId: 0

  function openReminderCreate() {
    reminderDraftText = ""
    reminderDraftWhen = ""
    reminderPreview = ""
    reminderFormProblems = []
    reminderFormError = ""
    reminderFormOpen = true
  }

  function closeReminderForm() {
    reminderFormOpen = false
    requestReminders()
  }

  function previewReminder() {
    if (!daemon.connected || !reminderFormOpen) return
    if (String(reminderDraftWhen).trim() === "") {
      reminderPreview = ""
      reminderFormProblems = []
      return
    }
    reminderPreviewRequestId = takeReminderRequestId()
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: reminderPreviewRequestId,
      method: "reminders.preview", params: { when: reminderDraftWhen } }) + "\n")
  }

  function handleReminderPreviewReply(frame) {
    if (frame.error) {
      reminderFormError = String(frame.error.message || "the time could not be read")
      return
    }
    var result = frame.result || {}
    reminderFormError = ""
    reminderFormProblems = result.problems || []
    // The daemon's own wording for the moment ("at three this afternoon"), the
    // same sentence the spoken confirmation says — never re-derived here, which
    // is the whole reason the preview is a round trip rather than arithmetic.
    reminderPreview = result.valid === true ? String(result.due_spoken || "") : ""
  }

  function createReminder() {
    if (!daemon.connected) return
    reminderCreateRequestId = takeReminderRequestId()
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: reminderCreateRequestId,
      method: "reminders.create",
      params: { when: reminderDraftWhen, text: reminderDraftText } }) + "\n")
  }

  function handleReminderCreateReply(frame) {
    if (frame.error) {
      var data = frame.error.data || {}
      reminderFormProblems = data.problems || []
      reminderFormError = String(frame.error.message || "the reminder could not be set")
      return
    }
    reminderFormOpen = false
    requestReminders()
  }

  function reminderProblemFor(field) {
    var out = []
    for (var i = 0; i < reminderFormProblems.length; i++) {
      if (String(reminderFormProblems[i].field || "") === field) {
        out.push(String(reminderFormProblems[i].message || ""))
      }
    }
    return out.join("\n")
  }

  function loadAutomations(result) {
    automationsFingerprint = String(result.fingerprint || "")
    automations = result.automations || []
  }

  // runAutomation triggers one entry through the existing gated run path for
  // its kind — zero new execution code: the daemon starts a session and
  // submits the trigger phrase, so confirmations arrive as the standard card
  // in Chat and a busy session refuses exactly as it always has.
  function runAutomation(kind, name) {
    if (!daemon.connected) return
    automationsRunRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({
      jsonrpc: "2.0", id: automationsRunRequestId,
      method: kind === "routine" ? "routines.run" : "scripts.run",
      params: { name: name }
    }) + "\n")
  }

  // setAutomationEnabled persists the switch through the daemon's surgical
  // config write. The row flips when config.changed triggers the refresh; a
  // refused re-enable (a phrase collision, a conflict) lands in the banner
  // with the daemon's own error.
  function setAutomationEnabled(kind, name, enabled) {
    if (!daemon.connected) return
    automationsEnableRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: automationsEnableRequestId,
      method: "automations.set_enabled",
      params: { kind: kind, name: name, enabled: enabled,
        fingerprint: automationsFingerprint } }) + "\n")
  }

  // handleAutomationsActionReply surfaces a refused run or switch in the tab
  // via the shared banner. A validation refusal (a re-enable collision)
  // carries the daemon's per-problem messages — shown verbatim, because the
  // error is the same one a config load gives. A conflict also re-requests
  // the listing, so the rows and the fingerprint are fresh for the retry.
  function handleAutomationsActionReply(frame) {
    if (frame.error) {
      errorStage = "automations"
      errorMessage = String(frame.error.message || "the automation action failed")
      var data = frame.error.data || {}
      if (data.problems && data.problems.length > 0) {
        errorMessage += " — " + data.problems.join("; ")
      }
      if (data.fingerprint) requestAutomations()
      return
    }
    var result = frame.result || {}
    if (result.fingerprint) automationsFingerprint = String(result.fingerprint)
    if (result.applied === false) {
      // Written but not yet live — the daemon said why (a session is busy);
      // honesty beats a row that pretends the grammar already moved.
      errorStage = "automations"
      errorMessage = "Saved to config.toml, but not applied yet: "
        + String(result.reason || "the daemon is busy") + ". It applies on the next reload."
    }
  }

  // setAutomationRun / clearAutomationRun keep the live-progress map, one
  // reassignment per change so the bindings see it.
  function setAutomationRun(kind, name, text) {
    var runs = {}
    for (var key in automationRuns) runs[key] = automationRuns[key]
    runs[kind + "/" + name] = text
    automationRuns = runs
  }

  function clearAutomationRun(kind, name) {
    var runs = {}
    for (var key in automationRuns) {
      if (key !== kind + "/" + name) runs[key] = automationRuns[key]
    }
    automationRuns = runs
  }

  // automationSubtitle words a row's second line: the kind — the badge every
  // row leads with — and the trigger phrases.
  function automationSubtitle(entry) {
    var kind = entry.kind === "routine" ? "Routine" : "Script"
    return kind + " — say “" + (entry.phrases || []).join("” or “") + "”"
  }

  // automationMeta words a row's status line from the daemon's own facts —
  // presentation, like feedCadence: disabled first (the flag colour never
  // carries a state alone), then the per-kind markers, the schedule with its
  // daemon-computed next fire, the would-refuse warning, the last observed
  // run, and any live progress.
  function automationMeta(entry) {
    var parts = []
    if (entry.enabled === false) parts.push("disabled — kept, phrases will not trigger it")
    if (entry.kind === "routine") {
      parts.push(Number(entry.steps || 0) + " " + (Number(entry.steps || 0) === 1 ? "step" : "steps"))
      if (entry.incomplete) parts.push("incomplete — a launch command still needs filling in")
    }
    if (entry.path_problem) parts.push("broken — " + entry.path_problem)
    if (entry.schedule) {
      var sched = "runs at " + entry.schedule
      if (entry.next_fire) {
        sched += " · next " + String(entry.next_fire).substring(0, 16).replace("T", " ")
      } else if (entry.enabled === false) {
        sched += " · paused while disabled"
      }
      parts.push(sched)
      if (entry.would_refuse) {
        parts.push("will be refused when it fires — " + String(entry.rule || "needs allow"))
      }
      if (entry.running) parts.push("a scheduled run is in flight")
    }
    if (entry.last_run) {
      var last = "last run " + String(entry.last_run.at || "").substring(0, 16).replace("T", " ")
        + " — " + String(entry.last_run.outcome || "")
      if (entry.last_run.duration) last += " · " + entry.last_run.duration
      parts.push(last)
    }
    var progress = automationRuns[entry.kind + "/" + entry.name]
    if (progress) parts.push(progress)
    return parts.join(" · ")
  }

  // automationFlagged: the urgent colour for a row whose meta already says
  // why in words — incomplete, a rotted path, a refused schedule, a failed
  // last run. Never the only carrier (JarvixCollectionRow's contract).
  function automationFlagged(entry) {
    return Boolean(entry.incomplete) || Boolean(entry.path_problem)
      || Boolean(entry.would_refuse)
      || Boolean(entry.last_run && entry.last_run.failed)
  }

  // --- automation entry forms ---------------------------------------------
  // The New/Edit/Delete form dialog (issue #99), replacing #93's copyable
  // TOML hint: entries are created and edited in place, in a form, with the
  // loader's own validation pinned to the offending field. The window stays
  // display-only (ADR 0013): the form round-trips a draft entry over IPC —
  // config.get_entry to open, config.validate_entry for live field errors
  // and the schedule's next-fire preview, config.upsert_entry to save,
  // config.delete_entry after the confirm — and every rule (phrase grammar,
  // collisions, schedule syntax, the zero-argument script shape) is judged
  // daemon-side against the whole rewritten document before anything is
  // written. Saving never runs anything.
  //
  // The draft is the daemon's own entry map: fields the form shows are
  // edited, every other key (report, a step's size) rides along untouched.
  // Text edits mutate the draft in place — no binding storm, no focus loss —
  // while structural changes (add/remove/reorder) reassign it so the
  // Repeaters rebuild. The fingerprint is captured when the form opens; the
  // daemon refuses the save if the file changed outside the window since.
  property bool automationFormOpen: false
  property string automationFormFamily: "" // "routines" | "scripts"
  property string automationFormOriginalName: "" // "" while creating
  property var automationDraft: ({})
  property var automationFormOriginal: ({}) // keys the loaded entry carried
  property var automationFormProblems: [] // [{field, message}] from the daemon
  // Notes are the daemon's "true, but not a reason to refuse" statements
  // (#163's channel, reused by #175): a step naming a program this machine
  // does not have saves fine and is skipped at run time, so the form says so
  // as a caution rather than blocking the save. Authoring a routine for
  // something you are about to install is a thing people do.
  property var automationFormNotes: [] // [{field, message}] from the daemon
  property string automationFormNextFire: ""
  property string automationFormError: "" // transport/conflict line, verbatim
  property bool automationDeleteConfirm: false
  property int automationEntryGetRequestId: 0
  property int automationValidateRequestId: 0
  property int automationSaveRequestId: 0
  property int automationDeleteRequestId: 0

  function automationFormKindWord() {
    return automationFormFamily === "routines" ? "routine" : "script"
  }

  // openAutomationCreate opens an empty form. The fingerprint is the
  // listing's — the file version the New button was rendered from.
  function openAutomationCreate(family) {
    automationFormFamily = family
    automationFormOriginalName = ""
    automationFormOriginal = {}
    automationDraft = family === "routines"
      ? { name: "", phrases: [""], steps: [{ app: "", args: [], workspace: 1 }] }
      : { name: "", phrases: [""], path: "" }
    automationFormProblems = []
    automationFormNotes = []
    automationFormPreview = {}
    automationFormNextFire = ""
    automationFormError = ""
    automationDeleteConfirm = false
    automationFormOpen = true
  }

  // openAutomationEdit asks the daemon for the whole entry — the listing
  // never carries every key — and opens when it answers.
  function openAutomationEdit(kind, name) {
    if (!daemon.connected) return
    automationEntryGetRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: automationEntryGetRequestId,
      method: "config.get_entry",
      params: { family: kind === "routine" ? "routines" : "scripts", name: name } }) + "\n")
  }

  function loadAutomationEntry(frame) {
    if (frame.error) {
      errorStage = "automations"
      errorMessage = String(frame.error.message || "the entry could not be read")
      requestAutomations()
      return
    }
    var result = frame.result || {}
    automationFormFamily = String(result.family || "")
    automationDraft = result.entry || {}
    automationFormOriginal = result.entry || {}
    automationFormOriginalName = String((result.entry || {}).name || "")
    automationsFingerprint = String(result.fingerprint || automationsFingerprint)
    automationFormProblems = []
    automationFormNotes = []
    automationFormPreview = {}
    automationFormNextFire = ""
    automationFormError = ""
    automationDeleteConfirm = false
    automationFormOpen = true
    validateAutomationDraft()
  }

  function closeAutomationForm() {
    automationFormOpen = false
    automationDeleteConfirm = false
    requestAutomations()
  }

  // reassignAutomationDraft clones the draft so the Repeaters see the
  // structural change — the automationRuns pattern, one reassignment per
  // add/remove/reorder.
  function reassignAutomationDraft() {
    var clone = {}
    for (var key in automationDraft) clone[key] = automationDraft[key]
    automationDraft = clone
    validateAutomationDraft()
  }

  // automationDraftEntry serialises the draft for the wire: shown fields as
  // typed (trimmed; numbers passed through so the daemon judges a bad one
  // and answers with its field problem), unshown keys carried verbatim.
  // Phrase indices are preserved exactly as displayed so a returned
  // "phrases[1]" problem pins to the right row.
  function automationDraftEntry() {
    var d = automationDraft
    var entry = { name: String(d.name || "").trim() }
    var phrases = []
    var list = d.phrases || []
    for (var i = 0; i < list.length; i++) phrases.push(String(list[i] || "").trim())
    entry.phrases = phrases
    var schedule = String(d.schedule || "").trim()
    if (schedule !== "") entry.schedule = schedule
    if (d.announce === true || "announce" in automationFormOriginal) {
      entry.announce = d.announce === true
    }
    if (d.enabled === false || "enabled" in automationFormOriginal) {
      entry.enabled = d.enabled !== false
    }
    if (automationFormFamily === "scripts") {
      entry.path = String(d.path || "").trim()
      var timeout = String(d.timeout_sec === undefined ? "" : d.timeout_sec).trim()
      if (timeout !== "") entry.timeout_sec = automationFormNumber(timeout)
      if (d.report !== undefined) entry.report = d.report
    } else {
      var steps = []
      var drafted = d.steps || []
      for (var j = 0; j < drafted.length; j++) {
        var s = drafted[j]
        var step = { app: String(s.app || "").trim(),
          workspace: automationFormNumber(String(s.workspace === undefined ? "" : s.workspace).trim()) }
        // The launching half (#175). Each key is written only when it says
        // something, so a step that names none of them is byte-identical to
        // one written before they existed — and `app` is dropped entirely
        // when a desktop entry is named, because the daemon refuses a step
        // that says what to launch twice.
        var entryName = String(s.desktop_entry || "").trim()
        if (entryName !== "") {
          step.desktop_entry = entryName
          if (step.app === "") delete step.app
        }
        var args = []
        var drafted_args = s.args || []
        for (var a = 0; a < drafted_args.length; a++) {
          // Passed exactly as typed, including spaces: the daemon hands these
          // to the program as an argv and nothing splits them. Empty rows are
          // dropped — an empty argument is a row someone added and did not
          // fill in, never something a program wants.
          var arg = String(drafted_args[a] || "")
          if (arg !== "") args.push(arg)
        }
        if (args.length > 0) step.args = args
        var identity = String(s.identity || "").trim()
        if (identity !== "") step.identity = identity
        var launch = String(s.launch || "").trim()
        if (launch !== "") step.launch = launch
        var match = String(s.match || "").trim()
        if (match !== "") step.match = match
        // The superseded spellings (float, size, tile), carried through
        // byte-for-byte: a form that silently dropped a key it has no widget
        // for would delete a working routine's placement the first time
        // someone edited its name. The step's own clear button is how they
        // go, so their removal is something the user did.
        if (s.float === true) step.float = true
        for (var k = 0; k < automationPlacementKeys.length; k++) {
          var key = automationPlacementKeys[k]
          if (s[key] !== undefined && String(s[key]) !== "") step[key] = s[key]
        }
        // The window-placement vocabulary (ADR 0056), which #181 gave its own
        // controls. Each key is written only when it says something, so a
        // step that places nothing stays byte-identical to one written before
        // the vocabulary existed — and the values are whatever the pickers
        // were handed, never a word composed here.
        var monitor = String(s.monitor || "").trim()
        if (monitor !== "") step.monitor = monitor
        var mode = String(s.mode || "").trim()
        if (mode !== "") step.mode = mode
        var width = String(s.width || "").trim()
        if (width !== "") step.width = width
        var height = String(s.height || "").trim()
        if (height !== "") step.height = height
        var placeNext = String(s.place_next || "").trim()
        if (placeNext !== "") step.place_next = placeNext
        if (s.master === true) step.master = true
        var focus = String(s.focus || "").trim()
        if (focus !== "") step.focus = focus
        var position = win.automationStepPosition(s)
        if (position !== undefined) step.position = position
        steps.push(step)
      }
      entry.steps = steps
    }
    return entry
  }

  // automationFormNumber passes an integral value as a number and anything
  // else through as text, so the daemon — not this window — words what a
  // non-number in a number field means (ADR 0013).
  function automationFormNumber(text) {
    var n = Number(text)
    return (text !== "" && isFinite(n) && Math.floor(n) === n) ? n : text
  }

  // automationStepPosition renders a step's floating position for the wire.
  // Each half goes through automationFormNumber, and a HALF-filled pair is
  // sent exactly as it stands: "position must be a pair of whole numbers" is
  // a sentence the daemon already writes, and filling the missing number in
  // here would put a window somewhere nobody asked for.
  function automationStepPosition(step) {
    var pair = step.position || []
    var x = String(pair[0] === undefined ? "" : pair[0]).trim()
    var y = String(pair[1] === undefined ? "" : pair[1]).trim()
    if (x === "" && y === "") return undefined
    return [automationFormNumber(x), automationFormNumber(y)]
  }

  // automationStepPositionAt reads one half of a step's position back out for
  // its input, so the two fields and the pair on the wire stay one value.
  function automationStepPositionAt(step, half) {
    var pair = (step || {}).position || []
    return String(pair[half] === undefined ? "" : pair[half])
  }

  // automationSetStepPosition writes one half of a step's position, creating
  // the pair the first time either half is typed into.
  function automationSetStepPosition(index, half, value) {
    var step = automationDraft.steps[index]
    if (!step.position) step.position = ["", ""]
    step.position[half] = value
  }

  // automationStepSuperseded lists the pre-ADR-0056 keys a step still carries
  // — `float`, `size`, `tile` — so the form can say which ones it is keeping
  // and offer to remove them. They have no controls because they say what
  // mode/width/height say, and the daemon refuses a step that says it twice.
  function automationStepSuperseded(index) {
    var step = (automationDraft.steps || [])[index] || {}
    var out = []
    if (step.float === true) out.push("float")
    for (var i = 0; i < automationPlacementKeys.length; i++) {
      var key = automationPlacementKeys[i]
      if (step[key] !== undefined && String(step[key]) !== "") out.push(key)
    }
    return out
  }

  // automationClearSuperseded drops those keys from a step. It is a button
  // rather than something the form does quietly on open: deleting a key
  // someone wrote by hand is an edit, and an edit the user did not ask for is
  // the kind a form should never make.
  function automationClearSuperseded(index) {
    var step = automationDraft.steps[index]
    delete step.float
    for (var i = 0; i < automationPlacementKeys.length; i++) {
      delete step[automationPlacementKeys[i]]
    }
    reassignAutomationDraft()
  }

  // validateAutomationDraft is the dry run: field problems and the next-fire
  // preview, from the daemon, nothing written. Called when a field commits
  // and after every structural change.
  function validateAutomationDraft() {
    if (!daemon.connected || !automationFormOpen) return
    automationValidateRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: automationValidateRequestId,
      method: "config.validate_entry",
      params: { family: automationFormFamily, name: automationFormOriginalName,
        entry: automationDraftEntry() } }) + "\n")
  }

  function handleAutomationValidateReply(frame) {
    if (frame.error) {
      automationFormError = String(frame.error.message || "validation failed")
      return
    }
    var result = frame.result || {}
    automationFormProblems = result.problems || []
    automationFormNotes = result.notes || []
    automationFormNextFire = String(result.next_fire || "")
    // The diagram (#181), computed by the daemon from the document this draft
    // would save. It travels with the problems so the picture and the
    // refusals can never be one edit apart, and an empty object is the honest
    // answer for a draft too broken to describe — the problems say why.
    automationFormPreview = result.preview || {}
    automationFormError = ""
  }

  function saveAutomationForm() {
    if (!daemon.connected) return
    automationSaveRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: automationSaveRequestId,
      method: "config.upsert_entry",
      params: { family: automationFormFamily, name: automationFormOriginalName,
        entry: automationDraftEntry(), fingerprint: automationsFingerprint } }) + "\n")
  }

  function deleteAutomationEntry() {
    if (!daemon.connected) return
    automationDeleteRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: automationDeleteRequestId,
      method: "config.delete_entry",
      params: { family: automationFormFamily, name: automationFormOriginalName,
        fingerprint: automationsFingerprint } }) + "\n")
  }

  // handleAutomationFormReply lands a save or delete: refused validation
  // pins the daemon's problems to their fields, a fingerprint conflict shows
  // the daemon's "changed outside the window" sentence verbatim, success
  // closes the form (the refreshed listing shows the result).
  function handleAutomationFormReply(frame) {
    if (frame.error) {
      var data = frame.error.data || {}
      if (data.problems !== undefined) {
        automationFormProblems = data.problems || []
      }
      automationFormError = String(frame.error.message || "the save failed")
      automationDeleteConfirm = false
      return
    }
    var result = frame.result || {}
    if (result.fingerprint) automationsFingerprint = String(result.fingerprint)
    automationFormOpen = false
    automationDeleteConfirm = false
    requestAutomations()
    if (result.applied === false) {
      errorStage = "automations"
      errorMessage = "Saved to config.toml, but not applied yet: "
        + String(result.reason || "the daemon is busy") + ". It applies on the next reload."
    }
  }

  // automationProblemFor collects the daemon's messages for one field key
  // (or its sub-keys: "steps[1]" also gathers "steps[1].app"), joined for
  // the field's problem line. Field "" is the form-level area.
  function automationProblemFor(field) {
    var out = []
    for (var i = 0; i < automationFormProblems.length; i++) {
      var f = String(automationFormProblems[i].field || "")
      if (f === field || (field !== "" && f.indexOf(field + ".") === 0)) {
        out.push(String(automationFormProblems[i].message || ""))
      }
    }
    return out.join("\n")
  }

  // automationNoteFor is the same lookup over the NOTES: what is true about
  // the draft without being a reason to refuse it. Kept separate from
  // automationProblemFor so the two can never be shown in each other's words
  // — a caution rendered as "Problem:" would read as a refusal, and a
  // refusal rendered as a caution would look saveable.
  function automationNoteFor(field) {
    var out = []
    for (var i = 0; i < automationFormNotes.length; i++) {
      if (String(automationFormNotes[i].field || "") === field) {
        out.push(String(automationFormNotes[i].message || ""))
      }
    }
    return out.join("\n")
  }

  // automationGeneralProblems is the form-level area's catch-all: problems
  // with no field, plus problems on entry keys the form has no input for
  // (report rides along uneditable) — named so the message still says where
  // to look. Nothing the daemon says may be dropped.
  function automationGeneralProblems() {
    var out = []
    for (var i = 0; i < automationFormProblems.length; i++) {
      var f = String(automationFormProblems[i].field || "")
      var msg = String(automationFormProblems[i].message || "")
      if (f === "") out.push(msg)
      else if (f === "report") out.push("report: " + msg)
    }
    return out.join("\n")
  }

  // --- the placement vocabulary (#181) --------------------------------------
  // Every closed set the step form's pickers offer, served whole by
  // `placement.vocabulary`: how a window may sit, where the next one goes,
  // whether the view follows, and what to do about a window that is already
  // open — each as a value to write and the words to show for it.
  //
  // None of it is spelled here on purpose (ADR 0013, ADR 0056). The
  // vocabulary is declared once, in one package, and a mode added there has
  // to appear in this form without anyone remembering to add it — which a
  // hard-coded list in QML cannot do, and would go stale silently rather than
  // loudly.
  property var placementModes: []
  property var placementPlaceNext: []
  property var placementFocusChoices: []
  property var placementLaunchChoices: []
  property var placementUnsupported: []
  property int placementWorkspaceMin: 0
  property int placementWorkspaceMax: 0
  property int placementVocabularyRequestId: 0

  function requestPlacementVocabulary() {
    if (!daemon.connected) return
    placementVocabularyRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: placementVocabularyRequestId,
      method: "placement.vocabulary" }) + "\n")
  }

  function loadPlacementVocabulary(result) {
    placementModes = result.modes || []
    placementPlaceNext = result.place_next || []
    placementFocusChoices = result.focus || []
    placementLaunchChoices = result.launch || []
    placementUnsupported = result.unsupported || []
    var bounds = result.workspace || {}
    placementWorkspaceMin = Number(bounds.min || 0)
    placementWorkspaceMax = Number(bounds.max || 0)
  }

  // placementWorkspaceLabel is the workspace field's label, with the bounds
  // the vocabulary declares rather than a range typed in here — the numbers
  // are the daemon's and the field must not claim a different pair.
  function placementWorkspaceLabel() {
    if (placementWorkspaceMax <= 0) return "Workspace"
    return "Workspace (" + placementWorkspaceMin + "–" + placementWorkspaceMax + ")"
  }

  // placementUnsupportedHint is the mode picker's explainer: the window states
  // the compositor does offer and the vocabulary declines, each with the
  // reason it was declined. Shown rather than omitted, because an option that
  // is simply missing reads as an oversight and the same question gets asked
  // again.
  function placementUnsupportedHint() {
    var out = []
    for (var i = 0; i < placementUnsupported.length; i++) {
      var u = placementUnsupported[i]
      out.push(String(u.name) + " — " + String(u.reason))
    }
    if (out.length === 0) return ""
    return "Not offered: " + out.join("; ")
  }

  // --- the preview (#181) ---------------------------------------------------
  // What the routine WOULD do, computed by the daemon and carried in the same
  // `config.validate_entry` reply the field problems arrive in — so the
  // diagram and the validation can never be one edit apart, and the picture
  // redraws on exactly the events the problems do: a field committing, a step
  // being added, removed, or MOVED.
  property var automationFormPreview: ({})

  function automationPreviewWorkspaces() {
    return (automationFormPreview || {}).workspaces || []
  }

  // automationStepSummary is one step's sentence — the arrangement in words,
  // beside the fields that produced it. It is composed daemon-side; this
  // looks it up by the step's position and renders it.
  function automationStepSummary(index) {
    var steps = (automationFormPreview || {}).steps || []
    for (var i = 0; i < steps.length; i++) {
      if (Number(steps[i].index) === index) return String(steps[i].summary || "")
    }
    return ""
  }

  // automationPlacementKeys are the window-placement vocabulary's step keys
  // the form has no control of its own for. The daemon owns their meaning and
  // their validation; this list exists only so the form carries them through
  // an edit untouched. It is down to the superseded spellings now that #181
  // gave the vocabulary proper its own controls — a key with a widget is
  // written by that widget, and carrying it here as well would write it twice.
  readonly property var automationPlacementKeys: ["size", "tile"]

  // automationStepNoteFor collects the notes for one step, whichever of its
  // launching keys the daemon keyed them to — the caution belongs to the
  // step, and which of `app` or `desktop_entry` carries it is the daemon's
  // detail rather than something the form should have an opinion about.
  function automationStepNoteFor(index) {
    var out = []
    var prefix = "steps[" + index + "]."
    for (var i = 0; i < automationFormNotes.length; i++) {
      var f = String(automationFormNotes[i].field || "")
      if (f.indexOf(prefix) === 0) out.push(String(automationFormNotes[i].message || ""))
    }
    return out.join("\n")
  }

  // automationStepExtraProblems catches a step's problems on the keys the
  // form carries through without an input — the superseded float/size/tile —
  // and any whole-step message, so they still land inside the step that owns
  // them. Nothing the daemon says about a step may be dropped, including a
  // sentence about a key this form deliberately has no control for.
  function automationStepExtraProblems(index) {
    var shown = { app: true, desktop_entry: true, args: true, identity: true,
      match: true, launch: true, workspace: true, monitor: true, mode: true,
      width: true, height: true, position: true, place_next: true,
      master: true, focus: true }
    var prefix = "steps[" + index + "]"
    var out = []
    for (var i = 0; i < automationFormProblems.length; i++) {
      var f = String(automationFormProblems[i].field || "")
      var msg = String(automationFormProblems[i].message || "")
      if (f === prefix) out.push(msg)
      else if (f.indexOf(prefix + ".") === 0 && !shown[f.substring(prefix.length + 1)]) {
        out.push(msg)
      }
    }
    return out.join("\n")
  }

  // --- spoken commands ([[intents.custom]], #164) --------------------------
  // The third collection of the Automations tab: the phrases the user
  // invented, each with the command it runs and what Jarvix says back. It is
  // the generic entry surface again and nothing else — config.list_entries to
  // list, config.get_entry to open, config.validate_entry for live field
  // errors, config.upsert_entry to save, config.delete_entry to remove — so
  // every rule is the daemon's: the phrase is compiled by the REAL intent
  // router on every keystroke that commits, which is what makes a taken phrase
  // name its owner under the phrase field instead of being discovered at load.
  //
  // The family's identity is its `match` rather than a `name`, which the
  // daemon knows and this file simply passes through: the draft carries the
  // key the daemon returned, and `name` in the verbs' params is whichever
  // entry is being edited.
  property var spokenCommands: []
  property string spokenFingerprint: ""
  property bool spokenFormOpen: false
  property string spokenFormOriginalMatch: "" // "" while creating
  property var spokenDraft: ({})
  property var spokenFormProblems: []
  property string spokenFormError: ""
  property bool spokenDeleteConfirm: false
  property int spokenListRequestId: 0
  property int spokenGetRequestId: 0
  property int spokenValidateRequestId: 0
  property int spokenSaveRequestId: 0
  property int spokenDeleteRequestId: 0

  function requestSpokenCommands() {
    if (!daemon.connected) return
    spokenListRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: spokenListRequestId,
      method: "config.list_entries", params: { family: "intents.custom" } }) + "\n")
  }

  function loadSpokenCommands(result) {
    spokenFingerprint = String(result.fingerprint || "")
    var rows = result.entries || []
    var out = []
    for (var i = 0; i < rows.length; i++) out.push(rows[i].entry || {})
    spokenCommands = out
  }

  function openSpokenCreate() {
    spokenFormOriginalMatch = ""
    spokenDraft = { match: "", run: "", say: "" }
    spokenFormProblems = []
    spokenFormError = ""
    spokenDeleteConfirm = false
    spokenFormOpen = true
  }

  function openSpokenEdit(match) {
    if (!daemon.connected) return
    spokenGetRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: spokenGetRequestId,
      method: "config.get_entry",
      params: { family: "intents.custom", name: match } }) + "\n")
  }

  function loadSpokenEntry(frame) {
    if (frame.error) {
      errorStage = "automations"
      errorMessage = String(frame.error.message || "the spoken command could not be read")
      requestSpokenCommands()
      return
    }
    var result = frame.result || {}
    spokenDraft = result.entry || {}
    spokenFormOriginalMatch = String((result.entry || {}).match || "")
    spokenFingerprint = String(result.fingerprint || spokenFingerprint)
    spokenFormProblems = []
    spokenFormError = ""
    spokenDeleteConfirm = false
    spokenFormOpen = true
    validateSpokenDraft()
  }

  function closeSpokenForm() {
    spokenFormOpen = false
    spokenDeleteConfirm = false
    requestSpokenCommands()
  }

  function spokenDraftEntry() {
    var d = spokenDraft
    var entry = { match: String(d.match || "").trim(), run: String(d.run || "").trim() }
    var say = String(d.say || "").trim()
    // An absent `say` is the daemon's own default ("Done."), so an empty field
    // leaves the key out rather than writing an empty acknowledgement — those
    // are different states and only one of them is silence.
    if (say !== "" || "say" in d) entry.say = say
    return entry
  }

  function validateSpokenDraft() {
    if (!daemon.connected || !spokenFormOpen) return
    spokenValidateRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: spokenValidateRequestId,
      method: "config.validate_entry",
      params: { family: "intents.custom", name: spokenFormOriginalMatch,
        entry: spokenDraftEntry() } }) + "\n")
  }

  function handleSpokenValidateReply(frame) {
    if (frame.error) {
      spokenFormError = String(frame.error.message || "validation failed")
      return
    }
    spokenFormProblems = (frame.result || {}).problems || []
    spokenFormError = ""
  }

  function saveSpokenForm() {
    if (!daemon.connected) return
    spokenSaveRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: spokenSaveRequestId,
      method: "config.upsert_entry",
      params: { family: "intents.custom", name: spokenFormOriginalMatch,
        entry: spokenDraftEntry(), fingerprint: spokenFingerprint } }) + "\n")
  }

  function deleteSpokenEntry() {
    if (!daemon.connected) return
    spokenDeleteRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: spokenDeleteRequestId,
      method: "config.delete_entry",
      params: { family: "intents.custom", name: spokenFormOriginalMatch,
        fingerprint: spokenFingerprint } }) + "\n")
  }

  function handleSpokenFormReply(frame) {
    if (frame.error) {
      var data = frame.error.data || {}
      if (data.problems !== undefined) spokenFormProblems = data.problems || []
      spokenFormError = String(frame.error.message || "the save failed")
      spokenDeleteConfirm = false
      return
    }
    var result = frame.result || {}
    if (result.fingerprint) spokenFingerprint = String(result.fingerprint)
    spokenFormOpen = false
    spokenDeleteConfirm = false
    requestSpokenCommands()
    if (result.applied === false) {
      errorStage = "automations"
      errorMessage = "Saved to config.toml, but the grammar has not been rebuilt yet: "
        + String(result.reason || "the daemon is busy") + ". It applies on the next reload."
    }
  }

  // spokenProblemFor pins the daemon's message for one field key; "" is the
  // form-level area, which also catches problems on keys with no input so
  // nothing the daemon says is dropped.
  function spokenProblemFor(field) {
    var out = []
    for (var i = 0; i < spokenFormProblems.length; i++) {
      if (String(spokenFormProblems[i].field || "") === field) {
        out.push(String(spokenFormProblems[i].message || ""))
      }
    }
    return out.join("\n")
  }

  function spokenGeneralProblems() {
    var shown = { match: true, run: true, say: true }
    var out = []
    for (var i = 0; i < spokenFormProblems.length; i++) {
      var f = String(spokenFormProblems[i].field || "")
      var msg = String(spokenFormProblems[i].message || "")
      if (f === "") out.push(msg)
      else if (!shown[f]) out.push(f + ": " + msg)
    }
    return out.join("\n")
  }

  // --- knowledge feeds ----------------------------------------------------
  // The Knowledge tab (issues #91/#92): the daemon's feed cache as cards —
  // knowledge.status for everything shown, knowledge.refresh_now and
  // knowledge.set_enabled for the two per-feed operations. Every decision is
  // the daemon's (ADR 0013): the single-flight on a refresh, the surgical
  // config write and its fingerprint check, the scheduler drop/readopt — this
  // window renders the status, calls the verbs, and words the daemon's own
  // facts. Cards refresh on knowledge.updated (a fetch completed) and
  // config.changed (the tables moved); feed values appear on screen but are
  // never logged or forwarded anywhere else.
  property var knowledgeFeeds: []
  property bool knowledgeEnabled: true
  // The config file's fingerprint as of the last status, passed back on
  // set_enabled so a hand edit made while the window sat open is a refused
  // conflict, never a clobber.
  property string knowledgeFingerprint: ""
  property int knowledgeRequestId: 0
  property int knowledgeRefreshRequestId: 0
  property int knowledgeEnableRequestId: 0

  function requestKnowledge() {
    if (!daemon.connected) return
    knowledgeRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: knowledgeRequestId,
      method: "knowledge.status" }) + "\n")
  }

  function loadKnowledge(result) {
    knowledgeEnabled = result.enabled !== false
    knowledgeFingerprint = String(result.fingerprint || "")
    knowledgeFeeds = result.feeds || []
    revealKnowledgeRow()
  }

  // refreshFeed asks for an immediate fetch. The reply only acknowledges the
  // start; the card updates when the daemon's knowledge.updated event says
  // the fetch — this one or one already in flight (single-flight, decided
  // daemon-side) — completed.
  function refreshFeed(name) {
    if (!daemon.connected) return
    knowledgeRefreshRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: knowledgeRefreshRequestId,
      method: "knowledge.refresh_now", params: { name: name } }) + "\n")
  }

  // setFeedEnabled persists the switch through the daemon's surgical config
  // write. The card flips when config.changed triggers the status refresh —
  // the daemon's word, not the click's.
  function setFeedEnabled(name, enabled) {
    if (!daemon.connected) return
    knowledgeEnableRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: knowledgeEnableRequestId,
      method: "knowledge.set_enabled",
      params: { name: name, enabled: enabled, fingerprint: knowledgeFingerprint } }) + "\n")
  }

  // handleKnowledgeActionReply surfaces a refused refresh or switch in the
  // tab (via the shared banner, which the tab anchors around). A conflict —
  // the config changed on disk underneath us — also re-requests the status,
  // so the cards and the fingerprint are fresh for the retry the message
  // suggests.
  function handleKnowledgeActionReply(frame) {
    if (frame.error) {
      errorStage = "knowledge"
      errorMessage = String(frame.error.message || "the feed action failed")
      if (frame.error.data && frame.error.data.fingerprint) requestKnowledge()
      return
    }
    var result = frame.result || {}
    if (result.fingerprint) knowledgeFingerprint = String(result.fingerprint)
    if (result.applied === false) {
      // Written but not yet live — the daemon said why (a session is busy);
      // honesty beats a card that pretends the scheduler already moved.
      errorStage = "knowledge"
      errorMessage = "Saved to config.toml, but not applied yet: "
        + String(result.reason || "the daemon is busy") + ". It applies on the next reload."
    }
  }

  // feedFreshness words one feed's state — presentation, like stateLabel:
  // every fact in the sentence is the daemon's own report (the spoken-style
  // age included), a stale value is marked STALE in words, and a failing
  // feed always carries failing-since and its reason.
  function feedFreshness(feed) {
    if (feed.failing) {
      var line = "failing since "
        + String(feed.failing_since || "").substring(0, 16).replace("T", " ")
      if (feed.last_error) line += " — " + String(feed.last_error)
      return line
    }
    if (!feed.has_value) return "no value fetched yet"
    var fetched = "fetched " + String(feed.age_spoken || "some time ago")
    return feed.stale ? "STALE — " + fetched : fetched
  }

  // feedCadence words the card's second line: mode, cadence, freshness
  // window, injection — and says "disabled" first when the feed is parked,
  // because the flag colour never carries that alone.
  function feedCadence(feed) {
    var parts = []
    if (feed.enabled === false) parts.push("disabled — kept, not fetched")
    if (feed.mode === "eager") {
      parts.push("eager · refreshes every " + feedSpan(Number(feed.interval_sec || 0)))
    } else {
      parts.push("lazy · fetched when asked")
    }
    parts.push("fresh for " + feedSpan(Number(feed.ttl_sec || 0)))
    if (feed.inject) parts.push("offered to the model each turn")
    return parts.join(" · ")
  }

  function feedSpan(sec) {
    if (sec < 60) return sec + "s"
    if (sec < 3600) return Math.floor(sec / 60) + "m"
    if (sec < 86400) return Math.floor(sec / 3600) + "h"
    return Math.floor(sec / 86400) + "d"
  }

  // --- knowledge feed forms -----------------------------------------------
  // The feed New/Edit/Delete dialog (issue #100), replacing #92's copyable
  // TOML hint: feeds are created and edited in place, in a form, through the
  // exact entry-admin verbs the automations forms use (#99/ADR 0033) with
  // family "knowledge.feeds" — the daemon's registry row, not new window
  // logic. Every rule (name uniqueness, mode vocabulary, cadence floors, the
  // command shape) is judged daemon-side against the whole rewritten
  // document; this form renders fields, ships drafts, and pins returned
  // problems (ADR 0013). Saving never fetches: the command is written, and
  // only a refresh — scheduled or the row's button, behind the existing
  // gate — ever runs it.
  property bool knowledgeFormOpen: false
  property string knowledgeFormOriginalName: "" // "" while creating
  property var knowledgeDraft: ({})
  property var knowledgeFormOriginal: ({}) // keys the loaded entry carried
  property var knowledgeFormProblems: [] // [{field, message}] from the daemon
  property string knowledgeFormError: "" // transport/conflict line, verbatim
  property bool knowledgeDeleteConfirm: false
  property int knowledgeEntryGetRequestId: 0
  property int knowledgeValidateRequestId: 0
  property int knowledgeSaveRequestId: 0
  property int knowledgeDeleteRequestId: 0

  // openKnowledgeCreate opens an empty feed form. The fingerprint is the
  // status listing's — the file version the New button was rendered from.
  function openKnowledgeCreate() {
    knowledgeFormOriginalName = ""
    knowledgeFormOriginal = {}
    knowledgeDraft = { name: "", description: "", command: [""], mode: "eager" }
    knowledgeFormProblems = []
    knowledgeFormError = ""
    knowledgeDeleteConfirm = false
    knowledgeFormOpen = true
  }

  // openKnowledgeEdit asks the daemon for the whole entry — the status cards
  // never carry the command — and opens when it answers.
  function openKnowledgeEdit(name) {
    if (!daemon.connected) return
    knowledgeEntryGetRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: knowledgeEntryGetRequestId,
      method: "config.get_entry",
      params: { family: "knowledge.feeds", name: name } }) + "\n")
  }

  function loadKnowledgeEntry(frame) {
    if (frame.error) {
      errorStage = "knowledge"
      errorMessage = String(frame.error.message || "the feed could not be read")
      requestKnowledge()
      return
    }
    var result = frame.result || {}
    knowledgeDraft = result.entry || {}
    knowledgeFormOriginal = result.entry || {}
    knowledgeFormOriginalName = String((result.entry || {}).name || "")
    knowledgeFingerprint = String(result.fingerprint || knowledgeFingerprint)
    knowledgeFormProblems = []
    knowledgeFormError = ""
    knowledgeDeleteConfirm = false
    knowledgeFormOpen = true
    validateKnowledgeDraft()
  }

  function closeKnowledgeForm() {
    knowledgeFormOpen = false
    knowledgeDeleteConfirm = false
    requestKnowledge()
  }

  // reassignKnowledgeDraft clones the draft so the Repeaters see a
  // structural change — the automations pattern, one reassignment per
  // add/remove/toggle.
  function reassignKnowledgeDraft() {
    var clone = {}
    for (var key in knowledgeDraft) clone[key] = knowledgeDraft[key]
    knowledgeDraft = clone
    validateKnowledgeDraft()
  }

  // knowledgeDraftEntry serialises the draft for the wire: shown fields as
  // typed (trimmed; numbers passed through so the daemon judges a bad one),
  // unshown keys carried verbatim. Command indices are preserved exactly as
  // displayed so a returned "command" problem sits under the right list.
  function knowledgeDraftEntry() {
    var d = knowledgeDraft
    var entry = { name: String(d.name || "").trim() }
    var description = String(d.description || "").trim()
    if (description !== "" || "description" in knowledgeFormOriginal) {
      entry.description = description
    }
    var command = []
    var list = d.command || []
    for (var i = 0; i < list.length; i++) command.push(String(list[i] || "").trim())
    entry.command = command
    entry.mode = d.mode === "lazy" ? "lazy" : "eager"
    var numbers = ["interval_sec", "ttl_sec", "timeout_sec"]
    for (var j = 0; j < numbers.length; j++) {
      var key = numbers[j]
      var value = String(d[key] === undefined ? "" : d[key]).trim()
      if (value !== "") entry[key] = automationFormNumber(value)
    }
    if (d.inject === true || "inject" in knowledgeFormOriginal) {
      entry.inject = d.inject === true
    }
    if (d.enabled === false || "enabled" in knowledgeFormOriginal) {
      entry.enabled = d.enabled !== false
    }
    return entry
  }

  // validateKnowledgeDraft is the dry run: field problems from the daemon,
  // nothing written. Called when a field commits and after every structural
  // change.
  function validateKnowledgeDraft() {
    if (!daemon.connected || !knowledgeFormOpen) return
    knowledgeValidateRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: knowledgeValidateRequestId,
      method: "config.validate_entry",
      params: { family: "knowledge.feeds", name: knowledgeFormOriginalName,
        entry: knowledgeDraftEntry() } }) + "\n")
  }

  function handleKnowledgeValidateReply(frame) {
    if (frame.error) {
      knowledgeFormError = String(frame.error.message || "validation failed")
      return
    }
    knowledgeFormProblems = (frame.result || {}).problems || []
    knowledgeFormError = ""
  }

  function saveKnowledgeForm() {
    if (!daemon.connected) return
    knowledgeSaveRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: knowledgeSaveRequestId,
      method: "config.upsert_entry",
      params: { family: "knowledge.feeds", name: knowledgeFormOriginalName,
        entry: knowledgeDraftEntry(), fingerprint: knowledgeFingerprint } }) + "\n")
  }

  function deleteKnowledgeEntry() {
    if (!daemon.connected) return
    knowledgeDeleteRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: knowledgeDeleteRequestId,
      method: "config.delete_entry",
      params: { family: "knowledge.feeds", name: knowledgeFormOriginalName,
        fingerprint: knowledgeFingerprint } }) + "\n")
  }

  // handleKnowledgeFormReply lands a save or delete: refused validation pins
  // the daemon's problems to their fields, a fingerprint conflict shows the
  // daemon's "changed outside the window" sentence verbatim, success closes
  // the form (the refreshed status shows the result — including the honest
  // applied=false reason when the first feed needs a restart to fetch).
  function handleKnowledgeFormReply(frame) {
    if (frame.error) {
      var data = frame.error.data || {}
      if (data.problems !== undefined) {
        knowledgeFormProblems = data.problems || []
      }
      knowledgeFormError = String(frame.error.message || "the save failed")
      knowledgeDeleteConfirm = false
      return
    }
    var result = frame.result || {}
    if (result.fingerprint) knowledgeFingerprint = String(result.fingerprint)
    knowledgeFormOpen = false
    knowledgeDeleteConfirm = false
    requestKnowledge()
    if (result.applied === false) {
      errorStage = "knowledge"
      errorMessage = "Saved to config.toml, but not applied yet: "
        + String(result.reason || "the daemon is busy") + "."
    }
  }

  // knowledgeProblemFor collects the daemon's messages for one field key,
  // joined for the field's problem line. Field "" is the form-level area.
  function knowledgeProblemFor(field) {
    var out = []
    for (var i = 0; i < knowledgeFormProblems.length; i++) {
      if (String(knowledgeFormProblems[i].field || "") === field) {
        out.push(String(knowledgeFormProblems[i].message || ""))
      }
    }
    return out.join("\n")
  }

  // --- providers ----------------------------------------------------------
  // The Providers tab (issue #163): the endpoints Jarvix thinks with
  // ([ai.<name>]) and the assistant CLIs it consults ([advisors.<name>]),
  // listed, added, edited, tested and removed through the SAME entry-admin
  // verbs the automations and knowledge forms use (ADR 0033) — two registry
  // rows daemon-side, no window logic of their own. config.list_entries
  // populates the lists, config.get_entry / validate_entry / upsert_entry /
  // delete_entry drive the form, config.test_entry runs the probe.
  //
  // The credential is the part this window is deliberately unable to get
  // wrong: it never receives one. A row carries only whether a key is set,
  // where it comes from, and — for the environment indirection — which
  // variable is expected and whether it currently resolves. The form's
  // credential control ships an INSTRUCTION (keep / set / clear), so the
  // draft that round-trips through here has no field to lose or leak.
  //
  // Every judgement stays daemon-side (ADR 0013): which permission tier an
  // advisor earns, whether an endpoint may be deleted, whether a base URL is
  // usable, what a probe found. This renders fields, ships drafts, and pins
  // returned problems and notes.
  property var providerEndpoints: []
  property var providerAdvisors: []
  property string providerInUse: "" // the endpoint ai.provider names
  // The config file's fingerprint as of the last listing, passed back on
  // every write so a hand edit made while the window sat open is a refused
  // conflict, never a clobber.
  property string providersFingerprint: ""
  property int providersEndpointsRequestId: 0
  property int providersAdvisorsRequestId: 0

  function requestProviders() {
    if (!daemon.connected) return
    providersEndpointsRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: providersEndpointsRequestId,
      method: "config.list_entries", params: { family: "ai" } }) + "\n")
    providersAdvisorsRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: providersAdvisorsRequestId,
      method: "config.list_entries", params: { family: "advisors" } }) + "\n")
  }

  function loadProviderEndpoints(result) {
    providerEndpoints = result.entries || []
    providerInUse = String(result.in_use || "")
    providersFingerprint = String(result.fingerprint || "")
  }

  function loadProviderAdvisors(result) {
    providerAdvisors = result.entries || []
    providersFingerprint = String(result.fingerprint || providersFingerprint)
  }

  // endpointCredentialLine words one endpoint's credential state — presence
  // and provenance only, because presence and provenance are all the window
  // is given. There is no masked prefix here on purpose: a prefix is a
  // fragment of the key, and a mask of the right length is its length.
  function endpointCredentialLine(row) {
    var secrets = row.secrets || {}
    var state = secrets.api_key || {}
    if (state.source === "env") {
      return "key set — read from $" + String(state.env || "")
    }
    if (state.source === "config") {
      return "key set — stored in config.toml"
    }
    if (String(state.env || "") !== "") {
      return "no key — $" + String(state.env) + " is expected but is not set in this session"
    }
    return "no key set"
  }

  // providerNoteLine joins the daemon's notes for a listing row — the
  // advisor's earned permission tier, stated where it can be read without
  // opening the form.
  function providerNoteLine(row) {
    var notes = row.notes || []
    var out = []
    for (var i = 0; i < notes.length; i++) out.push(String(notes[i].message || ""))
    return out.join(" ")
  }

  // --- providers form -----------------------------------------------------
  // One form serves both families, exactly as the automations form serves
  // routines and scripts: the family travels in a property and the fields
  // shown follow it.
  property bool providerFormOpen: false
  property string providerFormFamily: "" // "ai" | "advisors"
  property string providerFormOriginalName: "" // "" while creating
  property var providerDraft: ({})
  property var providerFormOriginal: ({}) // keys the loaded entry carried
  property var providerFormSecrets: ({}) // the daemon's presence report
  property var providerFormProblems: [] // [{field, message}] from the daemon
  property var providerFormNotes: [] // [{field, message}] — earned, not wrong
  property string providerFormError: "" // transport/conflict line, verbatim
  property bool providerDeleteConfirm: false
  // The credential instruction the next save will carry: "keep" (the daemon's
  // default too), "set" with the typed value, or "clear".
  property string providerSecretAction: "keep"
  property string providerSecretValue: ""
  property var providerTestResult: ({}) // the last probe's own words
  property bool providerTestRunning: false
  property int providerGetRequestId: 0
  property int providerValidateRequestId: 0
  property int providerSaveRequestId: 0
  property int providerDeleteRequestId: 0
  property int providerTestRequestId: 0

  function resetProviderForm(family) {
    providerFormFamily = family
    providerFormProblems = []
    providerFormNotes = []
    providerFormError = ""
    providerDeleteConfirm = false
    providerSecretAction = "keep"
    providerSecretValue = ""
    providerTestResult = ({})
    providerTestRunning = false
  }

  function openProviderCreate(family) {
    resetProviderForm(family)
    providerFormOriginalName = ""
    providerFormOriginal = {}
    providerFormSecrets = {}
    providerDraft = family === "ai"
      ? { name: "", base_url: "", api_key_env: "" }
      : { name: "", binary: "" }
    providerFormOpen = true
  }

  // openProviderEdit asks the daemon for the whole entry — the listing rows
  // carry no credential state beyond presence, and the form needs the keys it
  // has no column for — and opens when it answers.
  function openProviderEdit(family, name) {
    if (!daemon.connected) return
    providerFormFamily = family
    providerGetRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: providerGetRequestId,
      method: "config.get_entry", params: { family: family, name: name } }) + "\n")
  }

  function loadProviderEntry(frame) {
    if (frame.error) {
      errorStage = "providers"
      errorMessage = String(frame.error.message || "the entry could not be read")
      requestProviders()
      return
    }
    var result = frame.result || {}
    resetProviderForm(String(result.family || providerFormFamily))
    providerDraft = result.entry || {}
    providerFormOriginal = result.entry || {}
    providerFormSecrets = result.secrets || {}
    providerFormNotes = result.notes || []
    providerFormOriginalName = String((result.entry || {}).name || "")
    providersFingerprint = String(result.fingerprint || providersFingerprint)
    providerFormOpen = true
    validateProviderDraft()
  }

  function closeProviderForm() {
    providerFormOpen = false
    providerDeleteConfirm = false
    requestProviders()
  }

  // reassignProviderDraft clones the draft so Repeaters see a structural
  // change — the automations pattern, one reassignment per add/remove.
  function reassignProviderDraft() {
    var clone = {}
    for (var key in providerDraft) clone[key] = providerDraft[key]
    providerDraft = clone
    validateProviderDraft()
  }

  // providerDraftEntry serialises the draft for the wire: shown fields as
  // typed (trimmed), unshown keys carried verbatim. The credential is NOT
  // here and cannot be — the daemon refuses an entry that carries one, and
  // this form has nothing to put in it.
  function providerDraftEntry() {
    var d = providerDraft
    var entry = { name: String(d.name || "").trim() }
    if (providerFormFamily === "ai") {
      entry.base_url = String(d.base_url || "").trim()
      var env = String(d.api_key_env || "").trim()
      if (env !== "" || "api_key_env" in providerFormOriginal) entry.api_key_env = env
      return entry
    }
    entry.binary = String(d.binary || "").trim()
    // An absent args key is not an empty one: absence is what inherits the
    // shipped preset, and inheriting the preset is what earns the allow tier
    // (ADR 0016). The form only sends args once the user has added a row.
    if (d.args !== undefined) {
      var args = []
      for (var i = 0; i < d.args.length; i++) args.push(String(d.args[i] || "").trim())
      entry.args = args
    }
    var timeout = String(d.timeout_sec === undefined ? "" : d.timeout_sec).trim()
    if (timeout !== "") entry.timeout_sec = automationFormNumber(timeout)
    var description = String(d.description || "").trim()
    if (description !== "" || "description" in providerFormOriginal) {
      entry.description = description
    }
    return entry
  }

  // providerSecretParam is the write-only credential channel: an instruction,
  // never a copy of what is stored. "keep" is sent explicitly so the wire
  // says what the form means rather than relying on an omission.
  function providerSecretParam() {
    if (providerFormFamily !== "ai") return undefined
    if (providerSecretAction === "set") {
      return { api_key: { action: "set", value: providerSecretValue } }
    }
    if (providerSecretAction === "clear") return { api_key: { action: "clear" } }
    return { api_key: { action: "keep" } }
  }

  function validateProviderDraft() {
    if (!daemon.connected || !providerFormOpen) return
    providerValidateRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: providerValidateRequestId,
      method: "config.validate_entry",
      params: { family: providerFormFamily, name: providerFormOriginalName,
        entry: providerDraftEntry(), secrets: providerSecretParam() } }) + "\n")
  }

  function handleProviderValidateReply(frame) {
    if (frame.error) {
      providerFormError = String(frame.error.message || "validation failed")
      return
    }
    var result = frame.result || {}
    providerFormProblems = result.problems || []
    providerFormNotes = result.notes || []
    providerFormError = ""
  }

  function saveProviderForm() {
    if (!daemon.connected) return
    providerSaveRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: providerSaveRequestId,
      method: "config.upsert_entry",
      params: { family: providerFormFamily, name: providerFormOriginalName,
        entry: providerDraftEntry(), secrets: providerSecretParam(),
        fingerprint: providersFingerprint } }) + "\n")
  }

  function deleteProviderEntry() {
    if (!daemon.connected) return
    providerDeleteRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: providerDeleteRequestId,
      method: "config.delete_entry",
      params: { family: providerFormFamily, name: providerFormOriginalName,
        fingerprint: providersFingerprint } }) + "\n")
  }

  // testProviderEndpoint asks the daemon to make a real request to the
  // endpoint AS SAVED. It reports what happened; this window never decides
  // that something worked, and shows nothing at all until an answer arrives.
  function testProviderEndpoint(name) {
    if (!daemon.connected) return
    providerTestRunning = true
    providerTestResult = ({})
    providerTestRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: providerTestRequestId,
      method: "config.test_entry", params: { family: "ai", name: name } }) + "\n")
  }

  function handleProviderTestReply(frame) {
    providerTestRunning = false
    if (frame.error) {
      providerTestResult = { outcome: "unreachable",
        summary: String(frame.error.message || "the test could not run") }
      return
    }
    providerTestResult = frame.result || {}
  }

  // providerTestLine words the last probe: the daemon's own summary, then the
  // service's own error text when there was one. Never a verdict of this
  // window's own making.
  function providerTestLine() {
    if (providerTestRunning) return "Testing…"
    var r = providerTestResult || {}
    if (!r.outcome) return ""
    var line = String(r.summary || r.outcome)
    if (r.status) line += " (HTTP " + r.status + ")"
    if (r.detail) line += "\n" + String(r.detail)
    return line
  }

  function handleProviderFormReply(frame) {
    if (frame.error) {
      var data = frame.error.data || {}
      if (data.problems !== undefined) providerFormProblems = data.problems || []
      providerFormError = String(frame.error.message || "the save failed")
      providerDeleteConfirm = false
      return
    }
    var result = frame.result || {}
    if (result.fingerprint) providersFingerprint = String(result.fingerprint)
    providerFormOpen = false
    providerDeleteConfirm = false
    requestProviders()
    if (result.applied === false) {
      errorStage = "providers"
      errorMessage = "Saved to config.toml, but not applied yet: "
        + String(result.reason || "the daemon is busy") + "."
    }
  }

  // providerProblemFor collects the daemon's messages for one field key.
  // Field "" is the form-level area.
  function providerProblemFor(field) {
    var out = []
    for (var i = 0; i < providerFormProblems.length; i++) {
      if (String(providerFormProblems[i].field || "") === field) {
        out.push(String(providerFormProblems[i].message || ""))
      }
    }
    return out.join("\n")
  }

  // providerNoteFor is the same lookup over the notes: what the draft EARNS,
  // shown beside the field that decides it rather than as a problem.
  function providerNoteFor(field) {
    var out = []
    for (var i = 0; i < providerFormNotes.length; i++) {
      if (String(providerFormNotes[i].field || "") === field) {
        out.push(String(providerFormNotes[i].message || ""))
      }
    }
    return out.join("\n")
  }

  // providerFormSecretLine words the stored credential inside the form, from
  // the same presence report the listing uses.
  function providerFormSecretLine() {
    var state = (providerFormSecrets || {}).api_key || {}
    if (providerSecretAction === "set") return "A new key will be saved when you save this endpoint."
    if (providerSecretAction === "clear") return "The stored key will be removed when you save this endpoint."
    if (state.source === "env") {
      return "A key is set: read from $" + String(state.env || "") + " (Jarvix never displays it)."
    }
    if (state.source === "config") {
      return "A key is set: stored in config.toml (Jarvix never displays it)."
    }
    if (String(state.env || "") !== "") {
      return "No key is available: $" + String(state.env)
        + " is expected but is not set in the session Jarvix runs in."
    }
    return "No key is set."
  }

  // --- memory -------------------------------------------------------------
  // The Memory tab (issues #91/#92): the fact store from memory.list (ADR
  // 0025) — each fact with its dates, its supersede trail expandable in
  // place, a filter-as-you-type box whose matching is the daemon's (the same
  // query memory.list has always taken), and a per-fact Forget that routes
  // through the gated tool path (memory.forget_gated): the standard
  // confirmation card appears in Chat naming the exact fact, and this list
  // refreshes when the daemon's own events say the question was resolved.
  property var memoryFacts: []
  property bool memoryEnabled: true
  property int memoryFactCount: 0
  property int memoryFactMax: 0
  property string memoryQuery: ""
  // The daemon's over-budget sentence (#104): non-empty when facts are
  // being left out of the prompt against the user's intent. Rendered as
  // words in the tab — the never-silent contract — and decided entirely
  // daemon-side; this window only relays it.
  property string memoryWarning: ""
  property int memoryRequestId: 0
  property int memoryForgetRequestId: 0
  property int memoryPinRequestId: 0

  function requestMemory() {
    if (!daemon.connected) return
    memoryRequestId = nextRequestId
    nextRequestId++
    var frame = { jsonrpc: "2.0", id: memoryRequestId, method: "memory.list" }
    if (memoryQuery.trim() !== "") frame.params = { query: memoryQuery }
    daemon.write(JSON.stringify(frame) + "\n")
  }

  // filterMemory is the box's every-keystroke hook: remember the query and
  // ask the daemon — matching is its job (ADR 0013), and a local socket
  // round trip per keystroke costs nothing.
  function filterMemory(query) {
    memoryQuery = query
    requestMemory()
  }

  // forgetFact starts the gated forget. Nothing changes here on the reply:
  // the confirmation card appears in Chat via the ordinary events (the tab
  // badge points there), and the resolution events refresh this list.
  function forgetFact(id) {
    if (!daemon.connected) return
    memoryForgetRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: memoryForgetRequestId,
      method: "memory.forget_gated", params: { id: id } }) + "\n")
  }

  function loadMemory(result) {
    memoryEnabled = result.enabled !== false
    memoryFacts = result.facts || []
    memoryFactCount = Number(result.count || 0)
    memoryFactMax = Number(result.max || 0)
    memoryWarning = String(result.warning || "")
    // A reveal that arrived before the listing did (issue #168) lands now.
    revealMemoryRow()
  }

  // setFactPinned is the card's Pin/Unpin button (#104): one ungated verb,
  // the exact inverse a second press. The reply refreshes the listing, which
  // also brings back any over-budget warning the new pin state created.
  function setFactPinned(id, pinned) {
    if (!daemon.connected) return
    memoryPinRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: memoryPinRequestId,
      method: "memory.set_pinned", params: { id: id, pinned: pinned } }) + "\n")
  }

  // --- memory entry form --------------------------------------------------
  // Add and Edit for remembered facts (issue #100). Deliberately NOT the
  // config entry dialog: memory.toml is not config.toml, so these calls go
  // to memory.add / memory.update — the memory book's own write path, the
  // same one the memory.remember tool takes — and the daemon's refusals
  // arrive in the same {field, message} wire shape the config forms pin, so
  // this pane shares the form components and the placement logic. Ungated,
  // per ADR 0025's reversibility split: an add is undone with one forget and
  // an edit supersedes onto the fact's trail, while Forget — the destructive
  // verb — keeps its confirmation card.
  property bool memoryFormOpen: false
  property string memoryFormId: "" // "" while adding
  property string memoryFormContent: ""
  property bool memoryFormPinned: false
  property var memoryFormProblems: [] // [{field, message}] from the daemon
  property string memoryFormError: ""
  property int memorySaveRequestId: 0

  function openMemoryAdd() {
    memoryFormId = ""
    memoryFormContent = ""
    memoryFormPinned = false
    memoryFormProblems = []
    memoryFormError = ""
    memoryFormOpen = true
  }

  // openMemoryEdit opens the form on one fact. The listing already carries
  // the full content and pin state (memory.list serves them), so no round
  // trip is needed.
  function openMemoryEdit(id, content, pinned) {
    memoryFormId = String(id)
    memoryFormContent = String(content)
    memoryFormPinned = pinned === true
    memoryFormProblems = []
    memoryFormError = ""
    memoryFormOpen = true
  }

  function closeMemoryForm() {
    memoryFormOpen = false
    requestMemory()
  }

  function saveMemoryForm() {
    if (!daemon.connected) return
    memorySaveRequestId = nextRequestId
    nextRequestId++
    var frame = { jsonrpc: "2.0", id: memorySaveRequestId }
    if (memoryFormId === "") {
      frame.method = "memory.add"
      frame.params = { content: memoryFormContent, pinned: memoryFormPinned }
    } else {
      // Content and pin travel together; the daemon compares against the
      // stored fact and writes only what actually changed — a pin-only save
      // must not manufacture a revision of unchanged text.
      frame.method = "memory.update"
      frame.params = { id: memoryFormId, content: memoryFormContent, pinned: memoryFormPinned }
    }
    daemon.write(JSON.stringify(frame) + "\n")
  }

  // handleMemoryFormReply lands a save: a refusal pins the daemon's problems
  // exactly like a config form's (empty text on the content field, a full
  // store in the general area), success closes the form and re-requests the
  // listing — and the book's near-cap warning, when it sends one, lands in
  // the shared banner so the cap is never a surprise.
  function handleMemoryFormReply(frame) {
    if (frame.error) {
      var data = frame.error.data || {}
      if (data.problems !== undefined) {
        memoryFormProblems = data.problems || []
      }
      memoryFormError = String(frame.error.message || "the save failed")
      return
    }
    memoryFormOpen = false
    requestMemory()
    var warning = String((frame.result || {}).warning || "")
    if (warning !== "") {
      errorStage = "memory"
      errorMessage = warning
    }
  }

  function memoryProblemFor(field) {
    var out = []
    for (var i = 0; i < memoryFormProblems.length; i++) {
      if (String(memoryFormProblems[i].field || "") === field) {
        out.push(String(memoryFormProblems[i].message || ""))
      }
    }
    return out.join("\n")
  }

  // factMeta words one fact's dates for its row: the pin state (words, not
  // colour), stored, confirmed (when a later turn re-confirmed it), the
  // length of its supersede trail, and — only when a retrieval has actually
  // happened — the retrieval stats (#104). The daemon omits the stats keys
  // for never-retrieved facts, and this function follows: no key, no line,
  // never a fabricated "retrieved 0 times".
  function factMeta(fact) {
    var meta = fact.pinned === true ? "pinned · " : ""
    meta += "stored " + String(fact.stored || "").substring(0, 10)
    var updated = String(fact.updated || "").substring(0, 10)
    if (updated !== "" && updated !== String(fact.stored || "").substring(0, 10)) {
      meta += " · confirmed " + updated
    }
    var previous = fact.previous || []
    if (previous.length > 0) {
      meta += " · " + previous.length
        + (previous.length === 1 ? " earlier version" : " earlier versions")
    }
    var retrieved = Number(fact.times_retrieved || 0)
    if (retrieved > 0) {
      meta += " · retrieved " + retrieved + (retrieved === 1 ? " time" : " times")
      var last = String(fact.last_retrieved_spoken || "")
      if (last !== "") meta += " · last " + last
    }
    return meta
  }

  // --- vocabulary ---------------------------------------------------------
  // The Vocabulary section (issue #129) lives INSIDE the Memory tab, as the
  // second collection under the facts: both are "what Jarvix keeps about
  // you", taught explicitly, stored in a hand-editable file, and a seventh
  // tab for a list that is usually short would cost more navigation than it
  // buys. Same thin-client shape as the facts: vocabulary.list for the
  // listing, teach/update through the store's own write path (ungated — the
  // reversibility split), Delete through the gated tool path so the standard
  // confirmation card names the exact phrase.
  property var vocabEntries: []
  property bool vocabEnabled: true
  property int vocabCount: 0
  property int vocabMax: 0
  property int vocabBiasCount: 0
  property int vocabBiasMax: 0
  // The daemon's over-budget sentence: non-empty when taught words are being
  // left out of the prompt. Rendered verbatim — a trim is never silent —
  // and decided entirely daemon-side; this window only relays it.
  property string vocabWarning: ""
  property int vocabRequestId: 0
  property int vocabForgetRequestId: 0

  function requestVocabulary() {
    if (!daemon.connected) return
    vocabRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: vocabRequestId,
      method: "vocabulary.list" }) + "\n")
  }

  function loadVocabulary(result) {
    vocabEnabled = result.enabled !== false
    vocabEntries = result.entries || []
    revealMemoryRow()
    vocabCount = Number(result.count || 0)
    vocabMax = Number(result.max || 0)
    vocabBiasCount = Number(result.bias_count || 0)
    vocabBiasMax = Number(result.bias_max || 0)
    vocabWarning = String(result.warning || "")
  }

  // forgetVocabEntry starts the gated delete. Nothing changes here on the
  // reply: the confirmation card appears in Chat via the ordinary events,
  // and the resolution events refresh this list (the forgetFact shape).
  function forgetVocabEntry(id) {
    if (!daemon.connected) return
    vocabForgetRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: vocabForgetRequestId,
      method: "vocabulary.forget_gated", params: { id: id } }) + "\n")
  }

  // --- vocabulary entry form ----------------------------------------------
  // Add ("Teach a word…") and Edit share one form on the #100 machinery:
  // saves go to vocabulary.teach / vocabulary.update — the store's own path,
  // the same one the voice teach takes — and refusals arrive field-keyed in
  // the entry-form wire shape this pane pins like the config forms do.
  property bool vocabFormOpen: false
  property string vocabFormId: "" // "" while teaching a new word
  property string vocabFormPhrase: ""
  property string vocabFormMeaning: ""
  property string vocabFormNote: ""
  property bool vocabFormHard: false
  property var vocabFormProblems: [] // [{field, message}] from the daemon
  property string vocabFormError: ""
  property int vocabSaveRequestId: 0

  // One flag for "a Memory-tab form is open": the facts form and the
  // vocabulary form each take over the whole tab, so every listing element
  // hides behind either.
  readonly property bool memoryTabFormOpen: memoryFormOpen || vocabFormOpen

  function openVocabAdd() {
    vocabFormId = ""
    vocabFormPhrase = ""
    vocabFormMeaning = ""
    vocabFormNote = ""
    vocabFormHard = false
    vocabFormProblems = []
    vocabFormError = ""
    vocabFormOpen = true
  }

  // openVocabEdit opens the form on one entry. The listing already carries
  // every field (vocabulary.list serves them), so no round trip is needed.
  function openVocabEdit(entry) {
    vocabFormId = String(entry.id)
    vocabFormPhrase = String(entry.phrase || "")
    vocabFormMeaning = String(entry.meaning || "")
    vocabFormNote = String(entry.note || "")
    vocabFormHard = entry.hard_to_hear === true
    vocabFormProblems = []
    vocabFormError = ""
    vocabFormOpen = true
  }

  function closeVocabForm() {
    vocabFormOpen = false
    requestVocabulary()
  }

  function saveVocabForm() {
    if (!daemon.connected) return
    vocabSaveRequestId = nextRequestId
    nextRequestId++
    var frame = { jsonrpc: "2.0", id: vocabSaveRequestId }
    var params = { phrase: vocabFormPhrase, meaning: vocabFormMeaning,
      note: vocabFormNote, hard_to_hear: vocabFormHard }
    if (vocabFormId === "") {
      // Teach, not a bare add: the daemon supersedes an existing phrase in
      // place, so typing a known word updates it — never a second entry.
      frame.method = "vocabulary.teach"
    } else {
      frame.method = "vocabulary.update"
      params.id = vocabFormId
    }
    frame.params = params
    daemon.write(JSON.stringify(frame) + "\n")
  }

  // handleVocabFormReply lands a save exactly as the fact form's does: a
  // refusal pins the daemon's field-keyed problems, success closes the form
  // and re-requests the listing, and any warning (near-cap, bias budget)
  // lands in the shared banner so a cap is never a surprise.
  function handleVocabFormReply(frame) {
    if (frame.error) {
      var data = frame.error.data || {}
      if (data.problems !== undefined) {
        vocabFormProblems = data.problems || []
      }
      vocabFormError = String(frame.error.message || "the save failed")
      return
    }
    vocabFormOpen = false
    requestVocabulary()
    var warning = String((frame.result || {}).warning || "")
    if (warning !== "") {
      errorStage = "vocabulary"
      errorMessage = warning
    }
  }

  function vocabProblemFor(field) {
    var out = []
    for (var i = 0; i < vocabFormProblems.length; i++) {
      if (String(vocabFormProblems[i].field || "") === field) {
        out.push(String(vocabFormProblems[i].message || ""))
      }
    }
    return out.join("\n")
  }

  // vocabMeta words one entry's state for its row: the note, the taught /
  // re-taught dates, the listen flag (words, not colour), and the length of
  // its supersede trail — the factMeta shape.
  function vocabMeta(entry) {
    var meta = "taught " + String(entry.taught || "").substring(0, 10)
    var updated = String(entry.updated || "").substring(0, 10)
    if (updated !== "" && updated !== String(entry.taught || "").substring(0, 10)) {
      meta += " · re-taught " + updated
    }
    if (entry.hard_to_hear === true) meta += " · listened for"
    var previous = entry.previous || []
    if (previous.length > 0) {
      meta += " · " + previous.length
        + (previous.length === 1 ? " earlier meaning" : " earlier meanings")
    }
    return meta
  }

  // --- screen names (#180) -------------------------------------------------
  // The Screens section lives INSIDE the Automations tab, under the routines
  // and the spoken commands: a screen name is placement vocabulary, and the
  // things that use it — a routine step's `monitor` key — are on this tab.
  // (It is deliberately NOT in Memory beside the taught words: "top" is a
  // fact about furniture, not about the user.)
  //
  // Same thin-client shape as every other collection: monitors.list for the
  // listing, monitors.name / monitors.repoint / monitors.forget for the
  // writes, and every rule — the collision matrix, the reserved words, which
  // screens exist — decided daemon-side.
  property var monitorScreens: []      // outputs plugged in right now
  property var monitorNicknames: []    // every stored name, present or not
  property string monitorPath: ""      // the file the names live in
  property int monitorCount: 0
  property int monitorMax: 0
  property var monitorReserved: []     // words a name may not take
  property string monitorCurrentRef: "current"
  property int monitorRequestId: 0

  function requestMonitors() {
    if (!daemon.connected) return
    monitorRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: monitorRequestId,
      method: "monitors.list" }) + "\n")
  }

  function loadMonitors(result) {
    monitorScreens = result.monitors || []
    monitorNicknames = result.nicknames || []
    monitorPath = String(result.path || "")
    monitorCount = Number(result.count || 0)
    monitorMax = Number(result.max || 0)
    monitorReserved = result.reserved || []
    monitorCurrentRef = String(result.current || "current")
  }

  // monitorPickerOptions is the picker's data: "the current monitor" first,
  // because it is the answer that keeps working when everything else moves,
  // then each present output with its size and any name it already answers
  // to. Every word of every label comes from the daemon's reply.
  function monitorPickerOptions() {
    var out = [{ value: "", label: "the current monitor" }]
    for (var i = 0; i < monitorScreens.length; i++) {
      var m = monitorScreens[i]
      var label = String(m.describe || m.connector)
      if (String(m.nickname || "") !== "") label += " — called " + String(m.nickname)
      out.push({ value: String(m.connector), label: label })
    }
    return out
  }

  // monitorRowMeta words one stored name for its row: which screen it means,
  // and — the case the whole feature exists for — that the screen is not
  // plugged in right now. Said in words, never by colour alone.
  function monitorRowMeta(entry) {
    var meta = "means " + String(entry.connector)
    if (entry.present !== true) meta += " · not plugged in right now"
    var named = String(entry.named || "").substring(0, 10)
    if (named !== "") meta += " · named " + named
    var updated = String(entry.updated || "").substring(0, 10)
    if (updated !== "" && updated !== named) meta += " · moved " + updated
    return meta
  }

  // --- screen name form ----------------------------------------------------
  // Add ("Name a screen…") and Edit share one form. A new name is
  // monitors.name; editing an existing one is monitors.repoint, which is a
  // separate verb daemon-side precisely because moving a name changes what
  // every routine using it does — so it is something done on a name you can
  // see, never something a misheard sentence can do.
  property bool monitorFormOpen: false
  property string monitorFormExisting: "" // "" while naming a screen for the first time
  property string monitorFormName: ""
  property string monitorFormConnector: "" // "" means the current monitor
  property var monitorFormProblems: []     // [{field, message}] from the daemon
  property string monitorFormError: ""
  property int monitorSaveRequestId: 0
  property int monitorForgetRequestId: 0

  function openMonitorAdd() {
    monitorFormExisting = ""
    monitorFormName = ""
    monitorFormConnector = ""
    monitorFormProblems = []
    monitorFormError = ""
    monitorFormOpen = true
  }

  // openMonitorEdit opens the form on one stored name. The listing carries
  // every field, so no round trip is needed.
  function openMonitorEdit(entry) {
    monitorFormExisting = String(entry.name)
    monitorFormName = String(entry.name)
    monitorFormConnector = String(entry.connector || "")
    monitorFormProblems = []
    monitorFormError = ""
    monitorFormOpen = true
  }

  function closeMonitorForm() {
    monitorFormOpen = false
    requestMonitors()
  }

  function saveMonitorForm() {
    if (!daemon.connected) return
    monitorSaveRequestId = nextRequestId
    nextRequestId++
    var frame = { jsonrpc: "2.0", id: monitorSaveRequestId }
    frame.method = monitorFormExisting === "" ? "monitors.name" : "monitors.repoint"
    frame.params = { name: monitorFormName, connector: monitorFormConnector }
    daemon.write(JSON.stringify(frame) + "\n")
  }

  // forgetMonitorNickname drops a name. Ungated, like the assignment: it
  // changes nothing on screen and naming it again undoes it.
  function forgetMonitorNickname(name) {
    if (!daemon.connected) return
    monitorForgetRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: monitorForgetRequestId,
      method: "monitors.forget", params: { name: name } }) + "\n")
  }

  function handleMonitorFormReply(frame) {
    if (frame.error) {
      var data = frame.error.data || {}
      if (data.problems !== undefined) {
        monitorFormProblems = data.problems || []
      }
      monitorFormError = String(frame.error.message || "the save failed")
      return
    }
    monitorFormOpen = false
    requestMonitors()
  }

  function monitorProblemFor(field) {
    var out = []
    for (var i = 0; i < monitorFormProblems.length; i++) {
      if (String(monitorFormProblems[i].field || "") === field) {
        out.push(String(monitorFormProblems[i].message || ""))
      }
    }
    return out.join("\n")
  }

  // --- what went into this answer (issue #168) ----------------------------
  // One panel at a time: the expanded turn is named by its record position,
  // and expanding another simply moves the pointer. That keeps the resolved
  // list a single window-level fact rather than per-delegate state a recycled
  // delegate could carry to the wrong turn.
  //
  // Nothing is resolved until the user asks. The turn carries references —
  // ids, names, paths — and the words beside them, whether each source still
  // exists, and which actions it has are all the daemon's answer, composed
  // against the live stores at the moment of the press (ADR 0013). That is
  // why a forgotten fact can say it was forgotten instead of offering a
  // button that would do nothing.
  property int provenancePos: -1
  property var provenanceItems: []
  property bool provenanceLoading: false
  property string provenanceError: ""
  property int provenanceRequestId: 0

  // toggleProvenance opens the panel on a turn, or closes it if it is already
  // open. The second press must not re-ask: a fold is a fold.
  function toggleProvenance(pos, provJson) {
    if (provenancePos === pos) {
      provenancePos = -1
      provenanceItems = []
      provenanceError = ""
      return
    }
    provenancePos = pos
    provenanceItems = []
    provenanceError = ""
    if (!daemon.connected || provJson === "") return
    var record = {}
    try {
      record = JSON.parse(provJson)
    } catch (e) {
      provenanceError = "This turn's sources could not be read."
      return
    }
    provenanceLoading = true
    provenanceRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: provenanceRequestId,
      method: "provenance.resolve",
      params: { sources: record.sources || [] } }) + "\n")
  }

  // provenanceCount is the number the collapsed control shows, read from the
  // turn's own references so it needs no round trip.
  function provenanceCount(provJson) {
    if (provJson === "") return 0
    try {
      var record = JSON.parse(provJson)
      return (record.sources || []).length
    } catch (e) {
      return 0
    }
  }

  // provenanceTruncated is how many sources the turn's cap left out, so the
  // panel can say the list is short rather than quietly being short.
  function provenanceTruncated(provJson) {
    if (provJson === "") return 0
    try {
      return Number(JSON.parse(provJson).truncated || 0)
    } catch (e) {
      return 0
    }
  }

  // runProvenanceAction carries out one source's action. A tab action is this
  // window's own navigation; anything else is the daemon's, because it leaves
  // this process — a file in its viewer, a page in a browser, a window the
  // compositor has to raise.
  function runProvenanceAction(item, action) {
    var tab = String(action.tab || "")
    if (tab !== "") {
      revealIn(tab, String(action.ref || ""))
      return
    }
    if (!daemon.connected) return
    provenanceError = ""
    provenanceRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: provenanceRequestId,
      method: "provenance.open",
      params: { kind: String(item.kind || ""), ref: String(item.ref || ""),
        action: String(action.id || "") } }) + "\n")
  }

  // revealIn opens a tab already showing one item. Each tab keeps its own
  // idea of "showing an item" — the Library has a record id, the Focus tab a
  // detail id, and the two flat listings are scrolled to the row — so the
  // reveal is per tab rather than one mechanism pretending to fit all.
  function revealIn(tab, ref) {
    if (tab === "library") {
      openTab("library")
      requestHistoryDetail(ref)
      return
    }
    if (tab === "focus") {
      focusScreen.detailId = ref
      openTab("focus")
      return
    }
    if (tab === "memory") {
      // A filter in the box would hide the row rather than find it, so the
      // box is cleared and the list is scrolled instead.
      memoryReveal = ref
      if (memoryQuery !== "") {
        memoryQuery = ""
        memoryFilterInput.text = ""
      }
      openTab("memory")
      revealMemoryRow()
      return
    }
    if (tab === "knowledge") {
      knowledgeReveal = ref
      openTab("knowledge")
      revealKnowledgeRow()
      return
    }
    if (tab === "automations") {
      // The Automations tab holds several listings — schedules, reminders,
      // spoken commands — and no single "showing an item" state to set, so
      // this opens the tab and stops there. The daemon labels the action
      // accordingly ("Show in Automations"), which is why this is a weaker
      // promise rather than a broken one.
      openTab("automations")
    }
  }

  // The item a reveal is waiting to scroll to, cleared once it lands. The
  // listing may still be in flight when the tab opens, so the position is
  // attempted now and again when the listing arrives.
  property string memoryReveal: ""
  property string knowledgeReveal: ""

  function revealMemoryRow() {
    if (memoryReveal === "") return
    for (var i = 0; i < memoryFacts.length; i++) {
      if (String(memoryFacts[i].id) === memoryReveal) {
        var index = i
        Qt.callLater(function() { memoryList.positionViewAtIndex(index, ListView.Contain) })
        memoryReveal = ""
        return
      }
    }
    for (var j = 0; j < vocabEntries.length; j++) {
      if (String(vocabEntries[j].id) === memoryReveal) {
        var vindex = j
        Qt.callLater(function() { vocabList.positionViewAtIndex(vindex, ListView.Contain) })
        memoryReveal = ""
        return
      }
    }
  }

  function revealKnowledgeRow() {
    if (knowledgeReveal === "") return
    for (var i = 0; i < knowledgeFeeds.length; i++) {
      if (String(knowledgeFeeds[i].name) === knowledgeReveal) {
        var index = i
        Qt.callLater(function() { knowledgeList.positionViewAtIndex(index, ListView.Contain) })
        knowledgeReveal = ""
        return
      }
    }
  }

  // --- typed turns --------------------------------------------------------
  // Request id 1 is the conversation snapshot; everything else takes dynamic
  // ids from this counter so a reply can be matched to exactly the request
  // that asked for it. The counter starts above the historical fixed range
  // (settings still holds 11–14 on its own socket) so a dynamic id can never
  // collide with a fixed one.
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

  // --- new chat ------------------------------------------------------------
  // The Chat tab's New chat button is a thin client of the daemon's one
  // explicit-end verb, `conversation.new` (issue #117) — the same verb
  // "start a new conversation" (voice), `jarvix new`, and the bar menu reach.
  // The window decides nothing (ADR 0013): the daemon cancels any session in
  // flight (committing its exchange, marked interrupted, into the ending
  // thread), archives, and resets; this view refreshes on the
  // conversation.changed event like every other thread change.
  property int newChatRequestId: 0
  function startNewChat() {
    if (!daemon.connected) return
    newChatRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({
      jsonrpc: "2.0", id: newChatRequestId, method: "conversation.new"
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

  // --- confirmation card ---------------------------------------------------
  // The permission gate's question, rendered in the conversation flow (issue
  // #76): the spoken question, the exact command verbatim from the daemon
  // (never summarised here — ADR 0014's property that the model cannot
  // describe `rm -rf ~` as tidying up extends to this card), and two buttons.
  // The card decides nothing (ADR 0013): the buttons call session.confirm
  // with an explicit boolean — the same single gate path every other answer
  // mode resolves through — and the card's state changes only on the
  // daemon's own events and snapshot.
  property int pendingCardIndex: -1    // index into turns; -1 when no card is open
  property int confirmTimeoutSec: 0    // the configured window, from the daemon
  property double confirmDeadlineMs: 0 // absolute auto-decline time; 0 = clock not started
  property double confirmNowMs: 0      // ticked by confirmCountdown so the binding updates
  property int confirmRequestId: 0
  // The remember offer for the open card (#162): the exact rule the daemon
  // would add, or the one short sentence saying why it will not offer one.
  // Both come from the daemon — this file derives nothing about permissions
  // (ADR 0013) — and exactly one of them is ever non-empty.
  property string confirmRememberPattern: ""
  property string confirmRememberReason: ""

  // Seconds left before auto-decline, or -1 while the daemon has not started
  // the clock (the question is still being spoken aloud). Clamped at 0: only
  // the daemon declines, so a countdown that reaches zero keeps waiting for
  // the tool.declined event rather than resolving the card itself.
  readonly property int confirmRemainingSec: confirmDeadlineMs > 0
    ? Math.max(0, Math.ceil((confirmDeadlineMs - confirmNowMs) / 1000)) : -1

  function appendConfirmationCard(summary, command, timeoutSec, deadlineMs,
                                 rememberPattern, rememberReason) {
    // The pending turn steps aside and comes back underneath (issue #158): a
    // card that landed *below* the row still saying "Thinking" would read as
    // the question having arrived after the wait it interrupted.
    takePendingTurn()
    turns.append({ role: "confirmation", text: summary, command: command,
      outcome: "", pos: 0, pending: false, provJson: "" })
    pendingCardIndex = turns.count - 1
    confirmTimeoutSec = timeoutSec
    confirmDeadlineMs = deadlineMs
    confirmNowMs = Date.now()
    confirmRememberPattern = rememberPattern || ""
    confirmRememberReason = rememberReason || ""
    syncPendingTurn()
  }

  // resolveConfirmationCard marks the open card with its outcome and stops
  // the countdown. The card stays in the transcript — the record of what was
  // asked and answered. A resolution with no card open (the decline that
  // follows an unavailable gate never had a question) is a no-op.
  function resolveConfirmationCard(outcome) {
    if (pendingCardIndex < 0) return
    turns.setProperty(pendingCardIndex, "outcome", outcome)
    pendingCardIndex = -1
    confirmDeadlineMs = 0
    confirmRememberPattern = ""
    confirmRememberReason = ""
  }

  // declineOutcome words a tool.declined source for the card. The source
  // vocabulary is the daemon's and closed (docs/ipc.md); mapping it to a
  // sentence is presentation, like stateLabel. An unknown source shows as
  // itself — a refusal must never lose its reason.
  function declineOutcome(source) {
    switch (source) {
    case "cli": case "text": case "voice":
      return "Declined — you said no"
    case "timeout":
      return "Declined — timed out after " + confirmTimeoutSec + "s"
    case "interrupted":
      return "Declined — the session was interrupted"
    case "error": case "unavailable":
      return "Declined — you could not be asked"
    }
    return "Declined — " + source
  }

  // confirmationRecordOutcome words a resolved confirmation record from the
  // daemon's structured outcome (conversation.get / conversation.read, issue
  // #118). The vocabulary — approved, declined, timed_out — is the daemon's
  // and closed (docs/ipc.md); a timed-out record carries its own timeout_sec
  // because the configured window may have changed since it was recorded.
  // Anything else falls through to declineOutcome so an unknown source still
  // shows its reason.
  function confirmationRecordOutcome(rec) {
    var outcome = String(rec.outcome || "")
    if (outcome === "approved") return "Approved"
    if (outcome === "timed_out")
      return "Declined — timed out after " + Number(rec.timeout_sec || 0) + "s"
    return declineOutcome(String(rec.source || ""))
  }

  // answerConfirmation is what the ✓ and ✗ buttons (and Y/N on the focused
  // card) do: one session.confirm call carrying a literal boolean. No text is
  // interpreted here — the yes/no vocabulary lives in the daemon, once. The
  // card resolves on the daemon's tool.confirmed / tool.declined event, not
  // on the click.
  //
  // remember is the scope word for "approve and don't ask again" (#162):
  // "always", "conversation", or "" for the ordinary approve-once. It is a
  // SCOPE and never a pattern — the rule to write is the one the daemon
  // derived and published on this very card, so this surface has no way to
  // name one and nothing that reaches this surface does either.
  function answerConfirmation(approved, remember) {
    if (!daemon.connected || pendingCardIndex < 0) return
    confirmRequestId = nextRequestId
    nextRequestId++
    var params = { approved: approved }
    if (remember) params.remember = remember
    daemon.write(JSON.stringify({
      jsonrpc: "2.0", id: confirmRequestId, method: "session.confirm",
      params: params
    }) + "\n")
  }

  // --- approvals -----------------------------------------------------------
  // The Approvals tab (#162, ADR 0053): what runs without asking. Display
  // only, like every other surface here — the daemon composes each row's
  // facts, this file places them, and Forget is one verb call.
  property var approvals: []
  property var denials: []
  property string approvalsPath: ""
  // The add form (#164, ADR 0054). The card path still derives its pattern and
  // still refuses to accept one over IPC — that rule is about a pattern the
  // MODEL could reach, and nothing here is reachable from the model. What this
  // form types goes to approvals.add, where the allow list is judged by the
  // confirmation card's own refusal matrix; the refusal that comes back is the
  // matrix's own sentence, shown verbatim rather than reworded here.
  property bool approvalFormOpen: false
  property string approvalFormList: "allow" // "allow" | "deny"
  property string approvalFormPattern: ""
  property string approvalFormProblem: ""
  property string approvalFormError: ""
  // The deny removal's confirmation: the daemon's sentence naming what the
  // rule protected, and the pattern it belongs to. Held here rather than
  // composed here — a client that wrote its own version of this sentence would
  // be deciding, in prose, how serious the act is (ADR 0013).
  property string denyRemovalPattern: ""
  property string denyRemovalConfirmation: ""
  // JSON-RPC ids from this feature's own private range (900–949, the
  // reminders and overlay-confirm discipline) so its replies are
  // recognisable by construction.
  property int approvalsRequestId: 0
  property int approvalsForgetRequestId: 0
  property int approvalsAddRequestId: 0
  property int nextApprovalsRequestId: 900

  function takeApprovalsRequestId() {
    var id = nextApprovalsRequestId
    nextApprovalsRequestId = nextApprovalsRequestId >= 949 ? 900 : nextApprovalsRequestId + 1
    return id
  }

  function requestApprovals() {
    if (!daemon.connected) return
    approvalsRequestId = takeApprovalsRequestId()
    daemon.write(JSON.stringify({
      jsonrpc: "2.0", id: approvalsRequestId, method: "approvals.list"
    }) + "\n")
  }

  function loadApprovals(result) {
    approvals = result.approved || []
    denials = result.denied || []
    approvalsPath = String(result.path || "")
  }

  function forgetApproval(pattern) {
    if (!daemon.connected) return
    denyRemovalPattern = ""
    denyRemovalConfirmation = ""
    approvalsForgetRequestId = takeApprovalsRequestId()
    daemon.write(JSON.stringify({
      jsonrpc: "2.0", id: approvalsForgetRequestId, method: "approvals.forget",
      params: { pattern: pattern }
    }) + "\n")
  }

  // removeDeny asks the daemon to remove a deny rule. The first call is
  // deliberately not the removal: it comes back with the sentence saying what
  // the rule protected, which this window shows and then answers with
  // confirmed:true. The two-step lives on the wire rather than in this file so
  // no client can skip it by not implementing it.
  function removeDeny(pattern, confirmed) {
    if (!daemon.connected) return
    approvalsForgetRequestId = takeApprovalsRequestId()
    daemon.write(JSON.stringify({
      jsonrpc: "2.0", id: approvalsForgetRequestId, method: "approvals.forget",
      params: { pattern: pattern, list: "deny", confirmed: confirmed === true }
    }) + "\n")
  }

  function handleApprovalsForgetReply(frame) {
    if (frame.error) {
      errorStage = "approvals"
      errorMessage = String(frame.error.message || "the rule could not be removed")
      return
    }
    var result = frame.result || {}
    if (result.confirm_required === true) {
      denyRemovalPattern = String(result.pattern || "")
      denyRemovalConfirmation = String(result.confirmation || "")
      return
    }
    denyRemovalPattern = ""
    denyRemovalConfirmation = ""
  }

  function openApprovalAdd(list) {
    approvalFormList = list
    approvalFormPattern = ""
    approvalFormProblem = ""
    approvalFormError = ""
    approvalFormOpen = true
  }

  function closeApprovalForm() {
    approvalFormOpen = false
    approvalFormProblem = ""
    approvalFormError = ""
  }

  function submitApprovalAdd() {
    if (!daemon.connected) return
    approvalsAddRequestId = takeApprovalsRequestId()
    daemon.write(JSON.stringify({
      jsonrpc: "2.0", id: approvalsAddRequestId, method: "approvals.add",
      params: { pattern: approvalFormPattern, list: approvalFormList }
    }) + "\n")
  }

  // handleApprovalsAddReply shows the refusal exactly as the daemon wrote it.
  // The matrix's sentences were written to be read aloud on a confirmation
  // card; the form that typed the rule shows the same words for the same
  // refusal, which is what "the two routes cannot disagree" means to a person
  // rather than to a test.
  function handleApprovalsAddReply(frame) {
    if (frame.error) {
      var problems = ((frame.error.data || {}).problems) || []
      approvalFormProblem = problems.length > 0
        ? String(problems[0].message || "")
        : ""
      approvalFormError = String(frame.error.message || "the rule could not be added")
      return
    }
    var result = frame.result || {}
    approvalFormProblem = ""
    approvalFormError = ""
    if (result.added === false) {
      approvalFormError = String(result.reason || "that rule is already on the list")
      return
    }
    approvalFormOpen = false
    // A new deny rule may beat allow rules the user granted months ago. Being
    // told which is the difference between tightening the gate and finding out
    // later that something quietly stopped working.
    var shadows = result.shadows || []
    if (shadows.length > 0) {
      errorStage = "approvals"
      errorMessage = "The deny rule “" + String(result.pattern || "")
        + "” now overrides your allow " + (shadows.length === 1 ? "rule " : "rules ")
        + "“" + shadows.join("”, “") + "” — deny always wins."
    }
  }

  // --- managed windows (#197, ADR 0062) ------------------------------------
  // The windows Jarvix manages, listed on the Approvals tab — beneath the
  // standing command grants, because it is the same kind of thing and the
  // tab's own argument applies verbatim: a permission you cannot find is a
  // permission you cannot revoke. Handing a window over grants access to it;
  // this is where that grant is visible and where it is taken back.
  //
  // Release is one ungated verb and there is deliberately no Manage button to
  // match it. Taking a window over is a grant, and a grant is made out loud
  // on a card that names the window; giving one up needs no permission at
  // all, which is why the button is here and its opposite is not.
  //
  // Same thin-client shape as every other collection: windows.managed for the
  // listing, windows.release for the one write, and every rule — which window
  // a row names, whether typing is even possible — decided daemon-side.
  property var managedWindows: []
  property string managedPath: ""
  property bool managedTyping: true
  // JSON-RPC ids from this feature's own private range (950–999), so its
  // replies are recognisable by construction and disjoint from every other
  // surface's.
  property int managedRequestId: 0
  property int managedReleaseRequestId: 0
  property int nextManagedRequestId: 950

  function takeManagedRequestId() {
    var id = nextManagedRequestId
    nextManagedRequestId = nextManagedRequestId >= 999 ? 950 : nextManagedRequestId + 1
    return id
  }

  function requestManagedWindows() {
    if (!daemon.connected) return
    managedRequestId = takeManagedRequestId()
    daemon.write(JSON.stringify({
      jsonrpc: "2.0", id: managedRequestId, method: "windows.managed"
    }) + "\n")
  }

  function loadManagedWindows(result) {
    managedWindows = result.windows || []
    managedPath = String(result.path || "")
    managedTyping = result.typing !== false
  }

  // releaseManagedWindow hands one back. Ungated, immediate, and no
  // confirmation: giving up power needs no permission.
  function releaseManagedWindow(reference) {
    if (!daemon.connected || String(reference || "") === "") return
    managedReleaseRequestId = takeManagedRequestId()
    daemon.write(JSON.stringify({
      jsonrpc: "2.0", id: managedReleaseRequestId, method: "windows.release",
      params: { window: String(reference) }
    }) + "\n")
  }

  // managedSubtitle says how the window came to be managed, and — the fact a
  // reader of this tab most needs — what that does NOT include.
  function managedSubtitle(w) {
    var how = String(w.source || "") === "launched"
      ? "Jarvix opened it" + (String(w.program || "") !== "" ? " to run " + String(w.program) : "")
      : "You handed it over"
    if (w.terminal === true) {
      return how + " · a terminal, so anything typed here is confirmed command by command"
    }
    return how
  }

  // managedMeta places the window the way a person would look for it.
  function managedMeta(w) {
    var parts = ["workspace " + String(w.workspace || "")]
    if (w.focused === true) parts.push("the one you are in")
    if (String(w.since || "") !== "") parts.push("since " + String(w.since).slice(0, 10))
    if (String(w.reference || "") === "") {
      // No unambiguous way to name this window back to the daemon, so no
      // Release button either — a button that released a sibling would be
      // worse than none. Say the way out rather than leaving a dead row.
      parts.push("say \u201clet this go\u201d while it has focus")
    }
    return parts.join(" \u00b7 ")
  }

  // approvalSubtitle says what the grant IS — how long it lasts — because
  // that is the fact a person revoking needs first.
  function approvalSubtitle(a) {
    if (String(a.scope || "") === "conversation")
      return "Just this conversation — never written to disk"
    return "Permanent until you forget it"
  }

  // approvalMeta says where it came from and what it has done. A rule that
  // has never fired is called out: an unused standing permission is the one
  // most worth taking back.
  function approvalMeta(a) {
    var parts = []
    if (a.added) parts.push("added " + String(a.added).slice(0, 10))
    else if (String(a.source || "") === "hand") parts.push("added by hand")
    var uses = Number(a.uses || 0)
    if (uses > 0) {
      parts.push("used " + uses + (uses === 1 ? " time" : " times")
        + (a.last_used ? ", last " + String(a.last_used).slice(0, 10) : ""))
    } else if (String(a.scope || "") !== "conversation") {
      parts.push("never used")
    }
    return parts.join(" · ")
  }

  // --- speak again ---------------------------------------------------------
  // The per-message replay control (issue #122) is a thin client of the
  // daemon's `speech.replay` verb: it sends the row's record position (and
  // its role, as a staleness guard) and displays what comes back — the
  // daemon resolves the text from its own record, speaks it through the
  // standard pipeline, and decides every precedence question (live speech
  // wins; the newest replay wins). Request ids 750–799 are reserved for this
  // control, the FocusTab discipline (500–599), so its traffic can never be
  // mistaken for the window's own.
  readonly property int replayRequestId: 750
  // The refusal cue: the daemon's own sentence, shown briefly over the
  // transcript — a refused replay must be visible, not a dead click.
  property string replayCue: ""

  function requestReplay(pos, role) {
    if (!daemon.connected || pos <= 0) return
    daemon.write(JSON.stringify({ jsonrpc: "2.0", id: replayRequestId,
      method: "speech.replay", params: { turn: pos, role: role } }) + "\n")
  }

  function handleReplayReply(frame) {
    if (!frame.error) return // success speaks for itself: tts.* and state.changed render it
    replayCue = String(frame.error.message || "that message cannot be spoken right now")
    replayCueTimer.restart()
    // A bad address means this view trailed the record; the fresh snapshot
    // realigns it (and its replay controls) for the next click.
    requestConversation()
  }

  Timer {
    id: replayCueTimer
    interval: 4000
    repeat: false
    onTriggered: win.replayCue = ""
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
      // Confirmation records render in the read-only view too (issue #118):
      // command and worded outcome ride the row; empty on utterances so the
      // delegate can branch on them.
      var rec = list[i].confirmation
      var isRecord = String(list[i].role) === "confirmation" && rec
      pastTurns.append({ role: String(list[i].role), text: String(list[i].text),
        command: isRecord ? String(rec.command || "") : "",
        outcome: isRecord ? confirmationRecordOutcome(rec) : "" })
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
      // Reopened: back to the Chat tab, whose snapshot is the authoritative
      // account of what the thread now is.
      historyDetailId = ""
      openTab("chat")
      focusComposer()
      requestConversation()
    }
  }

  // loadSnapshot replaces the model with the daemon's authoritative view;
  // events append incrementally from here on.
  //
  // Each snapshot row carries `pos`, its 1-based position in the daemon's
  // record — the address `speech.replay` speaks by (issue #122). Only the
  // snapshot assigns positions: rows appended live carry pos 0 (no replay
  // control) because the live view is an approximation the record can move
  // past — an intent turn's acknowledgement never streams here, and a typed
  // confirmation reply shows as a row the record does not keep. The
  // turn-boundary re-request in handleEvent replaces the approximation with
  // the record, which is when every row gains its address.
  // --- pending turn mechanics ---------------------------------------------
  // The pending row is always the *last* row. Everything that appends to the
  // transcript takes it off first and puts it back afterwards, so a user turn
  // or a confirmation card can never land underneath the thing that is still
  // waiting. Removing it loses nothing: it holds no user content, only a
  // rendering of daemon state this window can recompute at any moment.

  // takePendingTurn removes the pending row if one is open, and reports
  // whether it did.
  function takePendingTurn() {
    if (pendingTurnIndex < 0) return false
    turns.remove(pendingTurnIndex)
    pendingTurnIndex = -1
    return true
  }

  // resolvePendingTurn ends a wait in words — "Cancelled", or the failure —
  // and stops the clock by closing the index. The row stays in the transcript
  // reading as the quiet non-answer it is, until the snapshot re-requested at
  // the turn boundary replaces this window's live approximation with the
  // daemon's record.
  function resolvePendingTurn(outcome) {
    if (pendingTurnIndex < 0) return
    turns.setProperty(pendingTurnIndex, "text", String(outcome || ""))
    pendingTurnIndex = -1
  }

  // refreshPendingElapsed recomputes how long the current phase has run, from
  // the daemon's phase start. Both clocks are this machine's, so the
  // subtraction is honest; a snapshot that carried no start (an older daemon)
  // simply never shows a count rather than inventing one.
  function refreshPendingElapsed() {
    if (stateSinceMs <= 0) {
      pendingElapsedSec = 0
      return
    }
    pendingElapsedSec = Math.max(0, Math.floor((Date.now() - stateSinceMs) / 1000))
  }

  // syncPendingTurn brings the pending row into line with what the daemon is
  // doing now: opened when there is something to say, updated in place while
  // it stays true, and closed the moment the daemon stops being in a phase of
  // a session (BarState.pendingTurnLine answers "" for idle, error, and every
  // non-session state). The closing half is the important one — an indicator
  // that stops updating but stays on screen reads as a hang, which is the
  // failure this whole feature exists to remove.
  //
  // Nothing opens while an answer is streaming: that row *is* the pending turn,
  // adopted on the first delta.
  function syncPendingTurn() {
    // The error state is *held*, not closed. failLocked publishes the state
    // transition before the `error` event that explains it, so closing here
    // would make the row vanish a millisecond before it could say why — and
    // the transition to idle that follows closes it anyway if the error event
    // never reaches this window.
    if (sessionState === "error") return
    // Computed here rather than read off a binding: this runs inside the same
    // call that just assigned sessionState, and a line that lagged one event
    // behind would word the wait wrongly for as long as nothing else changed.
    var line = BarState.pendingTurnLine(sessionState, currentTool, toolDetail,
      pendingElapsedSec)
    if (line === "") {
      takePendingTurn()
      return
    }

    // The tier answering it, separator and all decided in Go (#159): the
    // speed/quality trade is visible while it is being paid for, not only
    // afterwards in the record.
    line += BarState.pendingTurnTierNote(pendingTier)

    if (assistantStreaming) return
    if (pendingTurnIndex < 0) {
      turns.append({ role: "assistant", text: line, command: "", outcome: "",
        pos: 0, pending: true, provJson: "" })
      pendingTurnIndex = turns.count - 1
      return
    }
    turns.setProperty(pendingTurnIndex, "text", line)
  }

  function loadSnapshot(result) {
    // A reader scrolled back must stay where they are through the rebuild;
    // the tail-follower is repositioned by the count/height handlers.
    var keepY = list.followTail ? -1 : list.contentY
    turns.clear()
    pendingTurnIndex = -1
    pendingCardIndex = -1
    confirmDeadlineMs = 0
    var snapshot = result.turns || []
    for (var i = 0; i < snapshot.length; i++) {
      // A resolved confirmation arrives as a turn of its own (issue #118):
      // the daemon decided it is part of the record and where it sits; this
      // window only renders it — as the same card the live exchange showed,
      // already resolved, so a close-and-reopen (or the #108 kill-rebuild)
      // never erases what was asked and answered. The outcome text is worded
      // from the daemon's structured record, exactly as the live resolution
      // words the daemon's event.
      var rec = snapshot[i].confirmation
      if (String(snapshot[i].role) === "confirmation" && rec) {
        turns.append({ role: "confirmation", text: String(snapshot[i].text),
          command: String(rec.command || ""), outcome: confirmationRecordOutcome(rec),
          pos: i + 1, pending: false, provJson: "" })
        continue
      }
      // What went into this answer (issue #168) rides the turn as the
      // references the daemon recorded. They are carried as text rather than
      // as a nested list because a ListModel flattens nested objects, and
      // because nothing here reads them: the panel hands them straight back
      // to provenance.resolve, which is what turns them into words.
      var prov = snapshot[i].provenance
      turns.append({ role: String(snapshot[i].role), text: String(snapshot[i].text),
        command: "", outcome: "", pos: i + 1, pending: false,
        provJson: (prov && prov.sources && prov.sources.length > 0)
          ? JSON.stringify(prov) : "" })
    }
    if (keepY >= 0) list.contentY = keepY
    // A window opened *during* a confirmation wait missed the events that
    // announced it, so the snapshot carries the pending question (issue #76)
    // and the card renders here — same facts, same daemon, no blindness.
    if (result.confirmation) {
      appendConfirmationCard(
        String(result.confirmation.summary || ""),
        String(result.confirmation.command || ""),
        Number(result.confirmation.timeout_sec || 0),
        Number(result.confirmation.deadline_ms || 0),
        String(result.confirmation.remember_pattern || ""),
        String(result.confirmation.remember_reason || ""))
    }
    sessionState = String(result.state || "idle")
    assistantStreaming = false
    // Everything the pending turn needs to be rebuilt from scratch (issue
    // #158): when this phase began, and the tool call in flight if there is
    // one. A window opened during a long think — or rebuilt after a
    // compositor kill (#108) — therefore shows exactly what a window that was
    // open all along shows, counting from the same instant.
    stateSinceMs = Number(result.state_since_ms || 0)
    currentTool = String(result.tool || "")
    toolDetail = String(result.tool_detail || "")
    // The thinking level and the tier serving the turn in flight (#159). Both
    // absent when no tiers are configured, and then the control is not drawn
    // at all — there is no trade to offer on a machine with one model.
    thinking = String(result.thinking || "")
    thinkingLabel = String(result.thinking_label || "")
    thinkingLevels = result.thinking_levels || []
    pendingTier = String(result.tier || "")
    refreshPendingElapsed()
    // A window opened by clicking an error notification connects after the
    // `error` event has already gone out, so the snapshot carries it.
    errorStage = String(result.error_stage || "")
    errorMessage = String(result.error_message || "")

    // The pending turn is rebuilt last, so it lands under the restored
    // transcript and any confirmation card the snapshot carried.
    syncPendingTurn()

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
      // No session, no tool: a stale "Consulting claude" outliving the turn
      // that ran it would be the pending turn's worst possible lie.
      if (next === "idle" || next === "error") {
        currentTool = ""
        toolDetail = ""
        pendingTier = ""
      }
      sessionState = next
      // The phase's start on the daemon's own clock (issue #158). The elapsed
      // count is derived from it rather than from when this window noticed,
      // which is what lets a window opened mid-think agree with one that was
      // already open.
      stateSinceMs = Number(params.since_ms || 0)
      refreshPendingElapsed()
      syncPendingTurn()
      break
    case "assistant.started":
      // Which model is answering (#159), known the moment the turn reaches
      // the provider — early enough for the pending line to name it for the
      // whole of the wait rather than only in the record afterwards.
      pendingTier = String(params.tier || "")
      syncPendingTurn()
      break
    case "thinking.changed":
      // The level moved somewhere else — a spoken "switch to deep", or
      // another window. One place it lives, so this window follows rather
      // than keeping its own idea of it.
      thinking = String(params.thinking || "")
      thinkingLabel = String(params.label || "")
      thinkingNote = ""
      break
    case "tool.started":
      // The step the pending turn narrates. `detail` is the tool's own
      // present-tense label where it has one ("Consulting claude…"); the
      // wording rules are in Go and this only carries the facts to them.
      currentTool = String(params.tool || "")
      toolDetail = String(params.detail || "")
      syncPendingTurn()
      break
    case "transcript.final":
      // One per submitted utterance — normally one a session, but a reply to
      // a pending tool confirmation ("yes", spoken or typed) is a second, and
      // showing it is right: the user answered and should see their answer.
      // Events never repeat, so appending cannot double a turn.
      // The pending turn steps aside so the user's words land above it and it
      // stays the last row (issue #158) — a spoken turn reaches this while the
      // session is already listening, so there is usually one open.
      takePendingTurn()
      turns.append({ role: "user", text: String(params.text || ""), command: "",
        outcome: "", pos: 0, pending: false, provJson: "" })
      syncPendingTurn()
      break
    case "assistant.delta":
      if (!assistantStreaming) {
        // The pending turn *becomes* the answer (issue #158): the same row,
        // its text replaced. This is the whole no-double-bubble mechanism —
        // appending here instead would put a second bubble under the one the
        // user has been reading, and for an answer that starts inside the
        // elapsed threshold the placeholder would flash into existence and
        // straight back out. The index check keeps the invariant honest: the
        // pending row is always the last one, and streaming writes to the
        // last one.
        if (pendingTurnIndex >= 0 && pendingTurnIndex === turns.count - 1) {
          turns.setProperty(pendingTurnIndex, "pending", false)
          turns.setProperty(pendingTurnIndex, "text", "")
          pendingTurnIndex = -1
        } else {
          takePendingTurn()
          turns.append({ role: "assistant", text: "", command: "", outcome: "",
            pos: 0, pending: false, provJson: "" })
        }
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
        // No deltas reached us (a slow client the bus dropped): the final text
        // adopts the pending row the same way a delta would have.
        if (pendingTurnIndex >= 0 && pendingTurnIndex === turns.count - 1) {
          turns.setProperty(pendingTurnIndex, "pending", false)
          turns.setProperty(pendingTurnIndex, "text", full)
          pendingTurnIndex = -1
        } else {
          takePendingTurn()
          turns.append({ role: "assistant", text: full, command: "", outcome: "",
            pos: 0, pending: false, provJson: "" })
        }
      }
      assistantStreaming = false
      break
    case "tool.confirmation_required":
      // The gate asked: render the card in the conversation flow. The
      // command is the daemon's verbatim string; the deadline is unknown
      // until the daemon says the clock has started (the question may still
      // be being spoken), so the countdown starts at "up to timeout_sec".
      appendConfirmationCard(String(params.summary || ""), String(params.command || ""),
        Number(params.timeout_sec || 0), 0,
        String(params.remember_pattern || ""), String(params.remember_reason || ""))
      break
    case "tool.confirmation_deadline":
      // The countdown starts: the daemon computed the deadline from its
      // configured timeout. Everything the ticker shows derives from this.
      confirmDeadlineMs = Number(params.deadline_ms || 0)
      confirmNowMs = Date.now()
      break
    case "tool.confirmed":
      resolveConfirmationCard("Approved")
      break
    case "tool.declined":
      resolveConfirmationCard(declineOutcome(String(params.source || "")))
      // A declined forget resolves the Memory tab's pending question: the
      // list is unchanged, but re-requesting it is how this window stays a
      // mirror rather than a guesser (ADR 0013).
      if (params.tool === "memory.forget") requestMemory()
      if (params.tool === "vocabulary.forget") requestVocabulary()
      break
    case "tool.finished":
      // Nothing is in flight any more, so the pending turn goes back to
      // narrating the state (issue #158).
      currentTool = ""
      toolDetail = ""
      syncPendingTurn()
      // The gated forget executed (from this window's button or the model's
      // own call — the store changed either way): refresh the listing.
      if (params.tool === "memory.forget") requestMemory()
      if (params.tool === "vocabulary.forget") requestVocabulary()
      break
    case "knowledge.updated":
      // A fetch completed — scheduled or Refresh now. The event carries the
      // feed's name only; the fresh value rides the status reply.
      requestKnowledge()
      break
    case "approvals.changed":
      // A standing grant was added or revoked — here, on a card, or from the
      // CLI. The event carries the pattern only; the listing reply is where
      // the history travels.
      requestApprovals()
      break
    case "desktop.action":
      // A window was handed over or let go (#197) — by voice, by the button
      // below, or by a launch Jarvix made. The event names the verb and the
      // window in words; the listing reply is where the rest travels. Only
      // the two verbs that change the managed set re-read, so focusing and
      // moving windows costs this tab nothing.
      if (params.verb === "manage" || params.verb === "release" || params.verb === "launch")
        requestManagedWindows()
      break
    case "memory.entry_changed":
      // A fact was added or edited — from this window's form or another
      // client's. The event carries id and size only; the listing reply is
      // where content travels (ADR 0025's privacy split).
      requestMemory()
      break
    case "vocabulary.entry_changed":
      // A word was taught, edited, or flagged — from this window's form, a
      // spoken "when I say X I mean Y", or the model's teach tool. Same
      // privacy split: the event carries id and size only.
      requestVocabulary()
      break
    case "routine.started":
      // Live progress for the Automations tab (issue #93): the run events
      // carry the facts; these lines only relay them, and the finish always
      // re-requests the listing so the row ends on the daemon's own record.
      setAutomationRun("routine", String(params.routine || ""),
        "running — " + Number(params.steps || 0) + " steps")
      break
    case "routine.step":
      setAutomationRun("routine", String(params.routine || ""),
        "step " + Number(params.step || 0) + ": " + String(params.app || "")
        + (String(params.status || "") === "failed" ? " failed" : ""))
      break
    case "routine.finished":
      clearAutomationRun("routine", String(params.routine || ""))
      requestAutomations()
      break
    case "script.started":
      setAutomationRun("script", String(params.script || ""), "running")
      break
    case "script.finished":
      clearAutomationRun("script", String(params.script || ""))
      requestAutomations()
      break
    case "automation.fired":
    case "automation.skipped":
    case "automation.refused":
      // The clock moved (ADR 0032): last-fired, next-fire, or the refusal
      // record changed — re-request rather than guess (ADR 0013).
      requestAutomations()
      break
    case "reminders.changed":
      // A one-shot reminder was created, fired, deferred, or cancelled
      // (#141): the section re-requests rather than guessing what the
      // change meant (ADR 0013).
      requestReminders()
      break
    case "error":
      errorStage = String(params.stage || "")
      errorMessage = String(params.message || "something went wrong")
      assistantStreaming = false
      // A wait that ends badly says so where the waiting was shown (issue
      // #158). Leaving the last "Thinking · 41s" on screen would go on
      // claiming Jarvix is working towards an answer that will never arrive;
      // the words are the activity feed's own sentence for the same failure.
      resolvePendingTurn(BarState.pendingTurnFailed(errorStage, errorMessage))
      break
    case "session.nothing_heard":
      // The capture produced no words (issue #191) — a muted microphone, a
      // silent room, or a transcript that was only Jarvix's own bias prompt
      // handed back. Resolved with a sentence rather than dropped, because a
      // pending row that simply vanishes reads as the question having been
      // lost. Deliberately *not* errorMessage: nothing failed, and setting it
      // would light the urgent banner and hold it until the next session. The
      // measurement behind the reason is in the activity feed, where a user
      // debugging a microphone is looking.
      assistantStreaming = false
      resolvePendingTurn(BarState.pendingTurnNothingHeard)
      // fall through to the shared turn-boundary handling
    case "session.cancelled":
      // Cancelled is an outcome, not an absence. Usually the transition to
      // idle has already closed the pending row by the time this arrives, and
      // the record re-requested below carries the interrupted turn's own
      // annotation — the better answer. This covers the path where there was
      // no transition to close it: a session cancelled before it ever left
      // idle publishes no state.changed at all, and the row would otherwise
      // sit there until something else happened.
      resolvePendingTurn(BarState.pendingTurnCancelled)
      // fall through to the shared turn-boundary handling
    case "session.finished":
      // The daemon never lets a confirmation outlive its session; a card
      // still open here means its resolution event was dropped (this window
      // was a slow client). Close it as ended rather than leaving buttons
      // that look answerable — the daemon would refuse them anyway.
      resolveConfirmationCard("Declined — the session ended")
      assistantStreaming = false
      // A turn that ended with the pending row still open produced no answer
      // of its own — an intent acknowledgement, or a failure already worded
      // above. Drop it: the snapshot re-requested below carries the record.
      takePendingTurn()
      // A turn boundary: re-request the snapshot so the transcript becomes
      // the daemon's record rather than this window's live approximation —
      // which omits intent acknowledgements, keeps confirmation replies the
      // record does not, and lacks an interrupted turn's annotation. It is
      // also what gives every row its record position, and with it the
      // speak-again control (issue #122): replay addresses the record, so
      // the control appears exactly when the row provably is the record.
      requestConversation()
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
      if (currentTab === "library") requestHistory()
      break
    case "config.changed":
      // A saved config may have added, renamed, or switched routines,
      // scripts, or knowledge feeds — all three collections live in
      // config.toml.
      requestAutomations()
      requestSpokenCommands()
      requestKnowledge()
      // The endpoints and advisors are config tables too (#163), so a save
      // from any surface — this form, a hand edit, the settings screen
      // pointing ai.provider elsewhere — refreshes the Providers lists.
      requestProviders()
      // And it may have moved the reading-comfort typography (issue #121):
      // re-reading the settings snapshot here is what makes a change apply
      // to the open transcript live, whatever surface saved it.
      requestTypography()
      // A saved [ai.tiers] table changes which levels exist (#159), so the
      // control follows the file the same way the Providers lists do.
      requestThinking()
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
        } else if (frame.id !== undefined && frame.id === win.confirmRequestId) {
          // A click that lost the race — the confirmation had already
          // resolved by voice, text, CLI, timeout, or interruption — comes
          // back as an error. Deliberately swallowed: the resolution event
          // renders the card's real outcome, and a banner for "you were a
          // moment late" would alarm without informing.
        } else if (frame.id !== undefined && frame.id === win.replayRequestId) {
          win.handleReplayReply(frame)
        } else if (frame.id !== undefined && frame.id === win.thinkingGetRequestId) {
          if (frame.result) win.loadThinking(frame.result)
        } else if (frame.id !== undefined && frame.id === win.thinkingSetRequestId) {
          win.handleThinkingSetReply(frame)
        } else if (frame.id !== undefined && frame.id === win.activityRequestId) {
          if (frame.result) win.loadActivity(frame.result)
        } else if (frame.id !== undefined && frame.id === win.knowledgeRequestId) {
          if (frame.result) win.loadKnowledge(frame.result)
        } else if (frame.id !== undefined && (frame.id === win.knowledgeRefreshRequestId ||
                   frame.id === win.knowledgeEnableRequestId)) {
          win.handleKnowledgeActionReply(frame)
        } else if (frame.id !== undefined && frame.id === win.knowledgeEntryGetRequestId) {
          win.loadKnowledgeEntry(frame)
        } else if (frame.id !== undefined && frame.id === win.knowledgeValidateRequestId) {
          win.handleKnowledgeValidateReply(frame)
        } else if (frame.id !== undefined && (frame.id === win.knowledgeSaveRequestId ||
                   frame.id === win.knowledgeDeleteRequestId)) {
          win.handleKnowledgeFormReply(frame)
        } else if (frame.id !== undefined && frame.id === win.providersEndpointsRequestId) {
          if (frame.result) win.loadProviderEndpoints(frame.result)
        } else if (frame.id !== undefined && frame.id === win.providersAdvisorsRequestId) {
          if (frame.result) win.loadProviderAdvisors(frame.result)
        } else if (frame.id !== undefined && frame.id === win.providerGetRequestId) {
          win.loadProviderEntry(frame)
        } else if (frame.id !== undefined && frame.id === win.providerValidateRequestId) {
          win.handleProviderValidateReply(frame)
        } else if (frame.id !== undefined && (frame.id === win.providerSaveRequestId ||
                   frame.id === win.providerDeleteRequestId)) {
          win.handleProviderFormReply(frame)
        } else if (frame.id !== undefined && frame.id === win.providerTestRequestId) {
          win.handleProviderTestReply(frame)
        } else if (frame.id !== undefined && frame.id === win.memorySaveRequestId) {
          win.handleMemoryFormReply(frame)
        } else if (frame.id !== undefined && frame.id === win.memoryForgetRequestId) {
          // Success needs no handling — the confirmation card's events carry
          // the flow from here — but a refusal (unknown id, memory disabled)
          // must be seen.
          if (frame.error) {
            win.errorStage = "memory"
            win.errorMessage = String(frame.error.message || "the fact could not be forgotten")
          }
        } else if (frame.id !== undefined && frame.id === win.memoryPinRequestId) {
          // Success refreshes the listing (the pin and any over-budget
          // warning arrive with it); a refusal must be seen in words.
          if (frame.error) {
            win.errorStage = "memory"
            win.errorMessage = String(frame.error.message || "the pin could not be changed")
          } else {
            win.requestMemory()
          }
        } else if (frame.id !== undefined && frame.id === win.provenanceRequestId) {
          // One id serves both provenance verbs: resolve fills the panel, and
          // open answers only to say it could not act. An error is rendered
          // as words inside the panel rather than the window's error banner:
          // a source that has since gone is about one row, not the daemon.
          win.provenanceLoading = false
          if (frame.error) {
            win.provenanceError = String(frame.error.message || "That source could not be reached.")
          } else if (frame.result && frame.result.items !== undefined) {
            win.provenanceItems = frame.result.items || []
          }
        } else if (frame.id !== undefined && frame.id === win.memoryRequestId) {
          if (frame.result) win.loadMemory(frame.result)
        } else if (frame.id !== undefined && frame.id === win.managedRequestId) {
          if (frame.result) win.loadManagedWindows(frame.result)
        } else if (frame.id !== undefined && frame.id === win.managedReleaseRequestId) {
          // A refusal must be seen — the window may have closed between the
          // listing and the click, and "nothing happened" with no reason is
          // the shrug this whole feature exists to replace.
          if (frame.error) {
            win.errorStage = "managed windows"
            win.errorMessage = String(frame.error.message || "the window could not be released")
          }
          win.requestManagedWindows()
        } else if (frame.id !== undefined && frame.id === win.approvalsRequestId) {
          if (frame.result) win.loadApprovals(frame.result)
        } else if (frame.id !== undefined && frame.id === win.approvalsForgetRequestId) {
          // A refusal must be seen in words, and a deny removal comes back
          // asking its question; a completed removal is followed by the
          // approvals.changed event, which re-reads the list for every open
          // window rather than only this one.
          win.handleApprovalsForgetReply(frame)
        } else if (frame.id !== undefined && frame.id === win.approvalsAddRequestId) {
          win.handleApprovalsAddReply(frame)
        } else if (frame.id !== undefined && frame.id === win.spokenListRequestId) {
          if (frame.result) win.loadSpokenCommands(frame.result)
        } else if (frame.id !== undefined && frame.id === win.spokenGetRequestId) {
          win.loadSpokenEntry(frame)
        } else if (frame.id !== undefined && frame.id === win.spokenValidateRequestId) {
          win.handleSpokenValidateReply(frame)
        } else if (frame.id !== undefined && (frame.id === win.spokenSaveRequestId ||
                   frame.id === win.spokenDeleteRequestId)) {
          win.handleSpokenFormReply(frame)
        } else if (frame.id !== undefined && frame.id === win.reminderPreviewRequestId) {
          win.handleReminderPreviewReply(frame)
        } else if (frame.id !== undefined && frame.id === win.reminderCreateRequestId) {
          win.handleReminderCreateReply(frame)
        } else if (frame.id !== undefined && frame.id === win.vocabSaveRequestId) {
          win.handleVocabFormReply(frame)
        } else if (frame.id !== undefined && frame.id === win.vocabForgetRequestId) {
          // Success needs no handling — the confirmation card's events carry
          // the flow — but a refusal (unknown id, vocabulary disabled) must
          // be seen.
          if (frame.error) {
            win.errorStage = "vocabulary"
            win.errorMessage = String(frame.error.message || "the word could not be forgotten")
          }
        } else if (frame.id !== undefined && frame.id === win.vocabRequestId) {
          if (frame.result) win.loadVocabulary(frame.result)
        } else if (frame.id !== undefined && frame.id === win.monitorSaveRequestId) {
          win.handleMonitorFormReply(frame)
        } else if (frame.id !== undefined && frame.id === win.monitorForgetRequestId) {
          if (frame.error) {
            win.errorStage = "monitors"
            win.errorMessage = String(frame.error.message || "the screen name could not be forgotten")
          } else {
            win.requestMonitors()
          }
        } else if (frame.id !== undefined && frame.id === win.monitorRequestId) {
          if (frame.result) win.loadMonitors(frame.result)
        } else if (frame.id !== undefined && frame.id === win.placementVocabularyRequestId) {
          if (frame.result) win.loadPlacementVocabulary(frame.result)
        } else if (frame.id !== undefined && (frame.id === win.historyListRequestId ||
                   frame.id === win.historyReadRequestId ||
                   frame.id === win.historyOpenRequestId ||
                   frame.id === win.historySearchRequestId)) {
          win.handleHistoryReply(frame)
        } else if (frame.id !== undefined && frame.id === win.typographyRequestId) {
          if (frame.result) win.loadTypography(frame.result)
        } else if (frame.id !== undefined && frame.id === win.automationsRequestId) {
          if (frame.result) win.loadAutomations(frame.result)
        } else if (frame.id !== undefined && frame.id === win.reminderListRequestId) {
          if (frame.result) win.loadReminders(frame.result)
        } else if (frame.id !== undefined && frame.id === win.reminderCancelRequestId) {
          win.handleReminderCancelReply(frame)
        } else if (frame.id !== undefined && (frame.id === win.automationsRunRequestId ||
                   frame.id === win.automationsEnableRequestId)) {
          win.handleAutomationsActionReply(frame)
        } else if (frame.id !== undefined && frame.id === win.automationEntryGetRequestId) {
          win.loadAutomationEntry(frame)
        } else if (frame.id !== undefined && frame.id === win.automationValidateRequestId) {
          win.handleAutomationValidateReply(frame)
        } else if (frame.id !== undefined && (frame.id === win.automationSaveRequestId ||
                   frame.id === win.automationDeleteRequestId)) {
          win.handleAutomationFormReply(frame)
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
        win.requestAutomations()
        win.requestReminders()
        // The snapshot replaces the model wholesale (seq keeps replays
        // honest), so a reconnect — possibly to a restarted daemon — always
        // converges on what the daemon actually holds.
        win.requestActivity()
        // The collection tabs are populated on connect too, so whichever tab
        // the window reopens on shows data, not a stale blank. The Library
        // listing is only fetched while its tab is the one showing — it can
        // be large, and every route to it refreshes it (openTab,
        // conversation.changed, here).
        win.requestKnowledge()
        win.requestProviders()
        win.requestMemory()
        win.requestVocabulary()
        win.requestMonitors()
        win.requestPlacementVocabulary()
        win.requestManagedWindows()
        // The transcript's typography settings (issue #121) load with the
        // rest of the connect snapshot; until they arrive the property
        // defaults render the shipped look.
        win.requestTypography()
        // Which levels this machine can serve, and which one this
        // conversation is on (#159). The conversation snapshot carries both
        // too; this is what keeps the control right when a reload changed the
        // tiers without a turn happening.
        win.requestThinking()
        if (win.currentTab === "library") win.requestHistory()
      } else {
        win.sessionState = "idle"
        win.assistantStreaming = false
        // The wait died with the connection that reported it. A pending turn
        // left counting up against a daemon that is gone would be the one
        // thing this indicator must never do; the "daemon is not running"
        // panel is the explanation now.
        win.takePendingTurn()
        win.currentTool = ""
        win.toolDetail = ""
        win.stateSinceMs = 0
        win.pendingElapsedSec = 0
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

  // Ticks the pending turn's elapsed count. Twice a second rather than once,
  // so crossing the two-second threshold is prompt rather than looking like a
  // hiccup; it runs only while a pending turn is actually open, so a window
  // sitting at rest costs nothing. Like the confirmation countdown below, the
  // figure is always *derived* from the daemon's absolute phase start — this
  // timer only refreshes the arithmetic, so a missed or slow tick can never
  // drift it.
  Timer {
    id: pendingClock
    interval: 500
    repeat: true
    running: win.visible && win.socketReady && win.pendingTurnIndex >= 0
    onTriggered: {
      win.refreshPendingElapsed()
      win.syncPendingTurn()
    }
  }

  // Ticks the countdown on the open confirmation card. The remaining time is
  // always *derived* from the daemon's absolute deadline — this timer only
  // refreshes the arithmetic, so a missed or slow tick can never drift it.
  Timer {
    id: confirmCountdown
    interval: 250
    repeat: true
    running: win.visible && win.pendingCardIndex >= 0 && win.confirmDeadlineMs > 0
    onTriggered: win.confirmNowMs = Date.now()
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
    }

    // The tab strip (issue #91): every surface in its place. Each tab is
    // focusable (Tab walks the strip, Left/Right move between tabs,
    // Enter/Space select, Ctrl+Tab cycles from anywhere); the active tab is
    // conveyed by bold text and an underline, never colour alone. A pending
    // permission question badges the Chat tab in text — it must never be
    // hidden by tab state. A Flow rather than a Row, so a narrow window
    // wraps the strip instead of clipping tabs out of reach.
    Flow {
      id: tabStrip
      anchors.top: header.bottom
      anchors.topMargin: Style.space(10)
      anchors.left: parent.left
      anchors.right: parent.right
      spacing: Style.space(4)
      Accessible.role: Accessible.PageTabList
      Accessible.name: "Jarvix window tabs"

      Repeater {
        id: tabRepeater
        model: win.tabs

        delegate: Rectangle {
          id: tabButton
          required property var modelData
          required property int index
          readonly property bool selected: win.currentTab === modelData.id
          // The confirmation badge: text first — the accessible name says
          // what it means, the urgent colour only underlines it.
          readonly property bool badge: modelData.id === "chat" && win.pendingCardIndex >= 0

          width: tabBody.width + Style.space(16)
          height: tabBody.height + Style.space(10)
          radius: Style.cornerRadius
          color: Util.alpha(Color.popups.text,
            tabButton.activeFocus ? 0.18 : (tabButton.selected ? 0.10 : 0.04))
          border.color: Util.alpha(Color.popups.text, 0.5)
          border.width: tabButton.activeFocus ? 2 : 0
          activeFocusOnTab: true
          Accessible.role: Accessible.PageTab
          Accessible.name: modelData.label
            + (tabButton.selected ? ", current tab" : "")
            + (tabButton.badge ? " — a permission question is waiting" : "")

          function choose() {
            win.openTab(tabButton.modelData.id)
            if (tabButton.modelData.id === "chat") win.focusComposer()
          }
          Keys.onReturnPressed: tabButton.choose()
          Keys.onSpacePressed: tabButton.choose()
          Keys.onLeftPressed: win.stepTab(-1, true)
          Keys.onRightPressed: win.stepTab(1, true)

          Column {
            id: tabBody
            anchors.centerIn: parent
            spacing: Style.space(2)

            Row {
              id: tabLabelRow
              spacing: Style.space(4)

              Text {
                text: tabButton.modelData.label
                font.family: Style.font.family
                font.bold: tabButton.selected
                font.pixelSize: Style.font.subtitle
                color: Color.popups.text
              }
              Text {
                visible: tabButton.badge
                text: "!"
                font.family: Style.font.family
                font.bold: true
                font.pixelSize: Style.font.subtitle
                color: Color.urgent
              }
            }

            // The underline half of the active state — the second non-colour
            // signal beside the bold label.
            Rectangle {
              width: tabLabelRow.width
              height: Math.max(2, Style.space(2))
              radius: 1
              color: tabButton.selected ? Color.accent : "transparent"
            }
          }

          MouseArea { anchors.fill: parent; onClicked: tabButton.choose() }
        }
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

    JarvixEmptyState {
      visible: win.socketReady && win.currentTab === "chat" && turns.count === 0
      anchors.centerIn: parent
      width: parent.width
      text: "No conversation yet — hold Super+Alt+V and speak, or type below."
    }

    // The Focus tab (#123): self-contained in its own file with its own
    // daemon socket, like the settings screen below — the window only places
    // it and gates its connection on visibility.
    JarvixFocusTab {
      id: focusScreen
      visible: win.socketReady && win.currentTab === "focus"
      active: win.visible && win.currentTab === "focus"
      anchors.top: tabStrip.bottom
      anchors.topMargin: Style.space(12)
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.bottom: errorBanner.visible ? errorBanner.top : parent.bottom
      anchors.bottomMargin: errorBanner.visible ? Style.space(12) : 0
    }

    // The Situation tab (#196): self-contained in its own file with its own
    // daemon socket, on the Focus tab's terms — the window only places it,
    // gates its connection on visibility, and answers its one signal, which
    // is a navigation only the window that owns the tabs can perform.
    JarvixSituationTab {
      id: situationScreen
      visible: win.socketReady && win.currentTab === "situation"
      active: win.visible && win.currentTab === "situation"
      anchors.top: tabStrip.bottom
      anchors.topMargin: Style.space(12)
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.bottom: errorBanner.visible ? errorBanner.top : parent.bottom
      anchors.bottomMargin: errorBanner.visible ? Style.space(12) : 0
      onNavigate: function(tab, ref) { win.revealIn(tab, ref) }
    }

    // The settings screen fills the content pane while its tab is current.
    JarvixSettings {
      id: settingsScreen
      visible: win.currentTab === "settings"
      active: win.visible && win.currentTab === "settings" && win.socketReady
      anchors.top: tabStrip.bottom
      anchors.topMargin: Style.space(12)
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.bottom: errorBanner.visible ? errorBanner.top : parent.bottom
      anchors.bottomMargin: errorBanner.visible ? Style.space(12) : 0
    }

    // The Library tab: the archived-conversation listing, or one record
    // read-only with a Resume button.
    Item {
      id: historyScreen
      visible: win.socketReady && win.currentTab === "library"
      anchors.top: tabStrip.bottom
      anchors.topMargin: Style.space(12)
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.bottom: errorBanner.visible ? errorBanner.top : parent.bottom
      anchors.bottomMargin: errorBanner.visible ? Style.space(12) : 0

      JarvixEmptyState {
        visible: win.historyDetailId === "" && !win.searchActive && pastConversations.count === 0
        anchors.centerIn: parent
        width: parent.width
        text: "No archived conversations yet — they appear here after jarvix new."
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

      JarvixEmptyState {
        visible: win.historyDetailId === "" && win.searchActive && searchResults.count === 0
        anchors.centerIn: parent
        width: parent.width
        text: "Nothing in your past conversations mentions that."
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

      // One record, read-only, on the shared detail scaffold (issue #91).
      // Resume is the explicit action that makes it the active thread again
      // (conversation.open); everything else here only displays.
      JarvixDetailPane {
        visible: win.historyDetailId !== ""
        anchors.fill: parent
        backName: "Back to the conversation list"
        actionLabel: "Continue this conversation"
        note: "Read-only"
        onBackRequested: win.historyDetailId = ""
        onActionTriggered: win.resumeConversation(win.historyDetailId)

        ListView {
          id: pastTurnList
          anchors.fill: parent
          clip: true
          spacing: Style.space(14)
          model: pastTurns

          delegate: Column {
            width: pastTurnList.width
            spacing: Style.space(4)

            Text {
              text: model.role === "user" ? "You"
                : model.role === "confirmation" ? "Jarvix asked permission" : "Jarvix"
              font.family: Style.font.family
              font.bold: true
              font.pixelSize: Style.font.subtitle
              color: model.role === "user"
                ? Util.alpha(Color.popups.text, 0.7)
                : Color.accent
            }
            Text {
              visible: model.role !== "confirmation"
              text: model.text
              width: parent.width
              wrapMode: Text.Wrap
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
            }

            // A confirmation record, read-only (issue #118): the same facts
            // the chat card shows — question, verbatim command in monospace,
            // outcome in words — without buttons or countdown, because a past
            // conversation has nothing left to answer. Styled like the chat
            // card so the record reads as the exchange it was.
            Rectangle {
              visible: model.role === "confirmation"
              width: parent.width
              height: visible ? pastRecordBody.height + Style.space(20) : 0
              radius: Style.cornerRadius
              color: Util.alpha(Color.accent, 0.08)
              border.color: Util.alpha(Color.accent, 0.5)
              border.width: 1
              Accessible.role: Accessible.Grouping
              Accessible.name: "Permission question: " + model.text
                + " Command: " + model.command + " " + model.outcome

              Column {
                id: pastRecordBody
                anchors.top: parent.top
                anchors.left: parent.left
                anchors.right: parent.right
                anchors.margins: Style.space(10)
                spacing: Style.space(8)

                Text {
                  text: model.text
                  width: parent.width
                  wrapMode: Text.Wrap
                  font.family: Style.font.family
                  font.pixelSize: Style.font.subtitle
                  color: Color.popups.text
                }
                Rectangle {
                  width: parent.width
                  height: pastRecordCommand.height + Style.space(12)
                  radius: Style.cornerRadius
                  color: Util.alpha(Color.popups.text, 0.08)

                  Text {
                    id: pastRecordCommand
                    anchors.verticalCenter: parent.verticalCenter
                    anchors.left: parent.left
                    anchors.right: parent.right
                    anchors.margins: Style.space(8)
                    text: model.command
                    wrapMode: Text.WrapAnywhere
                    font.family: "monospace"
                    font.pixelSize: Style.font.subtitle
                    color: Color.popups.text
                  }
                }
                Text {
                  text: model.outcome
                  width: parent.width
                  wrapMode: Text.Wrap
                  font.family: Style.font.family
                  font.bold: true
                  font.pixelSize: Style.font.subtitle
                  color: Color.popups.text
                }
              }
            }
          }
        }
      }
    }

    // The Activity tab: what Jarvix is doing right now and has done
    // recently, one rendered row per daemon decision (issue #70). A turn
    // that acted shows its tool rows; a turn that only talked shows the
    // explicit text-only marker; every refusal carries the daemon's reason.
    // With the daemon down, the window's standard not-running panel stands
    // in — this tab, like the others, only renders while connected.
    Item {
      id: activityScreen
      visible: win.socketReady && win.currentTab === "activity"
      anchors.top: tabStrip.bottom
      anchors.topMargin: Style.space(12)
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.bottom: errorBanner.visible ? errorBanner.top : parent.bottom
      anchors.bottomMargin: errorBanner.visible ? Style.space(12) : 0

      JarvixEmptyState {
        visible: activityRows.count === 0
        anchors.centerIn: parent
        width: parent.width
        text: "Nothing yet — everything Jarvix does will appear here as it happens."
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

    // The Automations tab (issues #93/#99): routines and scripts as one
    // managed list on the shared collection rows — kind badge and phrases in
    // the subtitle, the script's exact path in the monospace detail line (it
    // is what the script.run gate's confirmation names, ADR 0030), and a
    // status line carrying the daemon's own facts: the enabled switch, the
    // incomplete/validity markers, the schedule with its daemon-computed
    // next fire and would-refuse warning, the last observed run, and live
    // progress from the run events. Run replays the entry's phrase through
    // the existing gated path; Enable/Disable is the surgical config write.
    // Clicking a row opens it in the edit form (#99); the footer's New
    // buttons open an empty one — the form pane replaces the listing while
    // it is open, and every rule it enforces is the daemon's.
    Item {
      id: automationsScreen
      visible: win.socketReady && win.currentTab === "automations"
      anchors.top: tabStrip.bottom
      anchors.topMargin: Style.space(12)
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.bottom: errorBanner.visible ? errorBanner.top : parent.bottom
      anchors.bottomMargin: errorBanner.visible ? Style.space(12) : 0

      // Three collections in one scroll (#164), the Providers section's shape
      // rather than #141's fixed 40/60 split: the tab now holds routines and
      // scripts, the user's own spoken commands, and the one-shot reminders,
      // and three fixed shares of a 600px pane would leave every one of them
      // too short to read. One Flickable, headings between, each collection's
      // own empty state and its own New button — so a collection with nothing
      // in it costs three lines rather than a third of the tab.
      Flickable {
        id: automationsScroll
        visible: !win.automationFormOpen && !win.spokenFormOpen && !win.reminderFormOpen
          && !win.monitorFormOpen
        anchors.fill: parent
        contentHeight: automationsColumn.height + Style.space(12)
        clip: true

        Column {
          id: automationsColumn
          width: automationsScroll.width
          spacing: Style.space(10)

          Text {
            text: "Routines and scripts"
            font.family: Style.font.family
            font.bold: true
            font.pixelSize: Style.font.subtitle
            color: Color.popups.text
          }

          JarvixEmptyState {
            visible: win.automations.length === 0
            width: parent.width
            text: "No routines or scripts yet — the New buttons below create one."
          }

          Repeater {
            model: win.automations

            delegate: JarvixCollectionRow {
              required property var modelData
              width: automationsColumn.width
              title: modelData.name
              subtitle: win.automationSubtitle(modelData)
              detail: String(modelData.path || "")
              meta: win.automationMeta(modelData)
              flagged: win.automationFlagged(modelData)
              // The row itself opens the edit form (#99) — name, phrases,
              // schedule, steps, and Delete live there.
              interactive: true
              onActivated: win.openAutomationEdit(modelData.kind, modelData.name)
              // A disabled entry cannot run — its phrases are out of the
              // grammar and the daemon would refuse — so the row does not
              // offer it; Enable is the way back (the Knowledge tab's rule).
              actionLabel: modelData.enabled === false ? "" : "Run"
              actionName: "Run the " + modelData.name + " " + modelData.kind
              onActionTriggered: win.runAutomation(modelData.kind, modelData.name)
              action2Label: modelData.enabled === false ? "Enable" : "Disable"
              action2Name: (modelData.enabled === false ? "Enable" : "Disable")
                + " the " + modelData.name + " " + modelData.kind
              onAction2Triggered: win.setAutomationEnabled(modelData.kind, modelData.name,
                modelData.enabled === false)
            }
          }

          // The New buttons (#99), replacing #93's copyable TOML hint:
          // creation is a form now, so the footer opens one instead of
          // handing over text to paste.
          Row {
            id: automationsNewRow
            spacing: Style.space(8)

            JarvixFormButton {
              label: "New routine…"
              name: "Create a new routine"
              accent: true
              onClicked: win.openAutomationCreate("routines")
            }
            JarvixFormButton {
              label: "New script…"
              name: "Create a new script"
              accent: true
              onClicked: win.openAutomationCreate("scripts")
            }
          }

          // The user's own spoken commands ([[intents.custom]], #164): a
          // phrase, the command it runs, and what Jarvix says back. Editable
          // here rather than in a text editor, with the router's own collision
          // rules judging the phrase — see openSpokenEdit.
          Text {
            text: "Spoken commands"
            font.family: Style.font.family
            font.bold: true
            font.pixelSize: Style.font.subtitle
            color: Color.popups.text
          }

          JarvixEmptyState {
            visible: win.spokenCommands.length === 0
            width: parent.width
            text: "No spoken commands yet — one is a phrase you say and a command it runs."
          }

          Repeater {
            model: win.spokenCommands

            delegate: JarvixCollectionRow {
              required property var modelData
              width: automationsColumn.width
              title: "“" + String(modelData.match || "") + "”"
              subtitle: String(modelData.say || "") === ""
                ? "Says “Done.” when it finishes"
                : "Says “" + String(modelData.say) + "” when it finishes"
              // The command verbatim, monospaced, exactly as the confirmation
              // card shows one: what runs is what is on screen (ADR 0014).
              detail: String(modelData.run || "")
              interactive: true
              onActivated: win.openSpokenEdit(String(modelData.match || ""))
            }
          }

          JarvixFormButton {
            label: "New spoken command…"
            name: "Create a new spoken command"
            accent: true
            onClicked: win.openSpokenCreate()
          }

          // The one-shot reminders section (#141, ADR 0046): "remind me at
          // three to …", from reminders.list, with Cancel. Since #164 it has a
          // New button too — a reminder is still made by saying one, and now
          // also by typing one, through the same parser and the same store.
          Text {
            text: win.oneShotReminders.length === 0
              ? "Reminders"
              : "Reminders — " + win.oneShotReminders.length + " pending"
            font.family: Style.font.family
            font.bold: true
            font.pixelSize: Style.font.subtitle
            color: Color.popups.text
          }

          JarvixEmptyState {
            visible: win.oneShotReminders.length === 0
            width: parent.width
            text: "No reminders set — say “remind me at three to call the pharmacy”, or use New reminder."
          }

          Repeater {
            model: win.oneShotReminders

            // One pending reminder: its words, and the daemon's own wording
            // for when it fires — this window does no clock arithmetic (ADR
            // 0013). Cancel is the only operation: a reminder is not edited,
            // it is cancelled and said again.
            delegate: JarvixCollectionRow {
              required property var modelData
              width: automationsColumn.width
              title: modelData.text
              meta: String(modelData.due_spoken || "")
              actionLabel: "Cancel"
              actionName: "Cancel the reminder to " + modelData.text
              onActionTriggered: win.cancelReminder(modelData.id)
            }
          }

          JarvixFormButton {
            label: "New reminder…"
            name: "Create a new reminder"
            accent: true
            onClicked: win.openReminderCreate()
          }

          // The screen names (#180, ADR 0057): what the user calls their
          // monitors, so a routine step can say `monitor = "top"` and keep
          // working after a cable moves. On this tab because that step is on
          // this tab; a fourth collection rather than a tab of its own for
          // the reason the other three share one scroll.
          Text {
            text: "Screens"
            font.family: Style.font.family
            font.bold: true
            font.pixelSize: Style.font.subtitle
            color: Color.popups.text
          }

          JarvixEmptyState {
            visible: win.monitorNicknames.length === 0
            width: parent.width
            text: "No screens have names — say “call this monitor top”, or use Name a screen."
          }

          Repeater {
            model: win.monitorNicknames

            // One screen name. The detail line is the connector it means —
            // monospace, because it is the exact word a routine step and the
            // window manager both use — and the meta line says in words when
            // that screen is not plugged in, which is the state this feature
            // exists to make visible rather than mysterious.
            delegate: JarvixCollectionRow {
              required property var modelData
              width: automationsColumn.width
              title: modelData.name
              detail: String(modelData.connector)
              meta: win.monitorRowMeta(modelData)
              flagged: modelData.present !== true
              interactive: true
              onActivated: win.openMonitorEdit(modelData)
              actionLabel: "Edit"
              actionName: "Change which screen " + modelData.name + " means"
              onActionTriggered: win.openMonitorEdit(modelData)
              action2Label: "Forget"
              action2Name: "Forget the screen name " + modelData.name
              onAction2Triggered: win.forgetMonitorNickname(modelData.name)
            }
          }

          JarvixFormButton {
            label: "Name a screen…"
            name: "Give a screen a name"
            accent: true
            onClicked: win.openMonitorAdd()
          }

          Text {
            visible: win.monitorPath !== ""
            width: parent.width
            wrapMode: Text.Wrap
            text: "Names are kept in " + win.monitorPath + " — edit it by hand if you prefer."
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Util.alpha(Color.popups.text, 0.7)
          }
        }
      }

      // The entry form (#99): a pane that replaces the listing — at the
      // window's 600px width a pane beats a floating popover — on the shared
      // detail scaffold: Back cancels, the accent action saves. It is loaded
      // fresh per open so every input initialises from the draft; from there
      // text edits mutate the draft in place and problems/preview arrive as
      // property changes, so typing never rebuilds the form.
      JarvixDetailPane {
        id: automationFormPane
        visible: win.automationFormOpen
        anchors.fill: parent
        backName: "Cancel and go back to the list"
        actionLabel: "Save"
        actionName: "Save the " + win.automationFormKindWord()
        note: (win.automationFormOriginalName === ""
          ? "New " + win.automationFormKindWord()
          : "Editing " + win.automationFormKindWord() + " “" + win.automationFormOriginalName + "”")
        onBackRequested: win.closeAutomationForm()
        onActionTriggered: win.saveAutomationForm()

        Loader {
          anchors.fill: parent
          active: win.automationFormOpen
          sourceComponent: automationFormBody
        }
      }

      // The spoken-command form (#164), on the same scaffold as every other
      // entry form: Back cancels, the accent action saves, the body is loaded
      // fresh per open so each input initialises from the draft.
      JarvixDetailPane {
        id: spokenFormPane
        visible: win.spokenFormOpen
        anchors.fill: parent
        backName: "Cancel and go back to the list"
        actionLabel: "Save"
        actionName: "Save the spoken command"
        note: win.spokenFormOriginalMatch === ""
          ? "New spoken command"
          : "Editing “" + win.spokenFormOriginalMatch + "”"
        onBackRequested: win.closeSpokenForm()
        onActionTriggered: win.saveSpokenForm()

        Loader {
          anchors.fill: parent
          active: win.spokenFormOpen
          sourceComponent: spokenFormBody
        }
      }

      // The reminder form (#164). Not an entry form — a reminder is not in
      // config.toml — but the same scaffold and the same discipline: the
      // daemon parses the time, the daemon words the moment, this shows both.
      JarvixDetailPane {
        id: reminderFormPane
        visible: win.reminderFormOpen
        anchors.fill: parent
        backName: "Cancel and go back to the list"
        actionLabel: "Set reminder"
        actionName: "Set this reminder"
        note: "New reminder"
        onBackRequested: win.closeReminderForm()
        onActionTriggered: win.createReminder()

        Loader {
          anchors.fill: parent
          active: win.reminderFormOpen
          sourceComponent: reminderFormBody
        }
      }

      // The screen-name form (#180). Not an entry form — screen names are not
      // in config.toml — but the same scaffold and the same discipline: the
      // daemon owns the collision matrix, this shows the refusal on the field
      // it belongs to.
      JarvixDetailPane {
        id: monitorFormPane
        visible: win.monitorFormOpen
        anchors.fill: parent
        backName: "Cancel and go back to the list"
        actionLabel: "Save"
        actionName: "Save this screen name"
        note: win.monitorFormExisting === ""
          ? "Name a screen"
          : "Editing “" + win.monitorFormExisting + "”"
        onBackRequested: win.closeMonitorForm()
        onActionTriggered: win.saveMonitorForm()

        Loader {
          anchors.fill: parent
          active: win.monitorFormOpen
          sourceComponent: monitorFormBody
        }
      }
    }

    // The form body, built per open (see the Loader above). Every field
    // pins the daemon's own message for its key — automationProblemFor —
    // and the general area shows whole-entry problems and conflicts, so a
    // refused save is never silent and never colour-only.
    Component {
      id: automationFormBody

      Flickable {
        id: formScroll
        contentHeight: formColumn.height + Style.space(12)
        clip: true

        Column {
          id: formColumn
          width: formScroll.width
          spacing: Style.space(10)

          // The form-level area: transport errors, the fingerprint
          // conflict's "changed outside the window" sentence, and problems
          // the daemon could not pin to a field — all verbatim.
          Text {
            visible: win.automationFormError !== "" || win.automationGeneralProblems() !== ""
            width: parent.width
            wrapMode: Text.Wrap
            text: (win.automationFormError !== "" ? win.automationFormError + "\n" : "")
              + win.automationGeneralProblems()
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Color.urgent
          }

          JarvixFormField {
            width: parent.width
            label: "Name"
            problem: win.automationProblemFor("name")
            Component.onCompleted: text = String(win.automationDraft.name || "")
            onEdited: function(value) { win.automationDraft.name = value }
            onCommitted: win.validateAutomationDraft()
          }

          // Phrases: one row each, so a daemon problem keyed "phrases[1]"
          // sits under exactly the phrase it means.
          Column {
            width: parent.width
            spacing: Style.space(6)

            Text {
              text: "Trigger phrases"
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
            }
            Repeater {
              model: (win.automationDraft.phrases || []).length
              delegate: Row {
                required property int index
                width: parent.width
                spacing: Style.space(8)

                JarvixFormField {
                  width: parent.width - phraseRemove.width - Style.space(8)
                  label: "Phrase " + (index + 1)
                  placeholder: "the words to say"
                  problem: win.automationProblemFor("phrases[" + index + "]")
                  Component.onCompleted: text = String((win.automationDraft.phrases || [])[index] || "")
                  onEdited: function(value) { win.automationDraft.phrases[index] = value }
                  onCommitted: win.validateAutomationDraft()
                }
                JarvixFormButton {
                  id: phraseRemove
                  label: "Remove"
                  name: "Remove phrase " + (index + 1)
                  onClicked: {
                    win.automationDraft.phrases.splice(index, 1)
                    win.reassignAutomationDraft()
                  }
                }
              }
            }
            Text {
              visible: win.automationProblemFor("phrases") !== ""
              width: parent.width
              wrapMode: Text.Wrap
              text: "Problem: " + win.automationProblemFor("phrases")
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.urgent
            }
            JarvixFormButton {
              label: "Add phrase"
              name: "Add another trigger phrase"
              onClicked: {
                if (!win.automationDraft.phrases) win.automationDraft.phrases = []
                win.automationDraft.phrases.push("")
                win.reassignAutomationDraft()
              }
            }
          }

          JarvixFormField {
            visible: win.automationFormFamily === "scripts"
            width: parent.width
            label: "Command (absolute path, run with no arguments)"
            placeholder: "/home/you/bin/backup-notes.sh"
            monospace: true
            problem: win.automationProblemFor("path")
            hint: "The file is run exactly as named — nothing spoken or typed here ever reaches it."
            Component.onCompleted: text = String(win.automationDraft.path || "")
            onEdited: function(value) { win.automationDraft.path = value }
            onCommitted: win.validateAutomationDraft()
          }

          JarvixFormField {
            visible: win.automationFormFamily === "scripts"
            width: parent.width
            label: "Timeout in seconds (empty for the default)"
            placeholder: "60"
            problem: win.automationProblemFor("timeout_sec")
            Component.onCompleted: text = win.automationDraft.timeout_sec === undefined
              ? "" : String(win.automationDraft.timeout_sec)
            onEdited: function(value) {
              if (value.trim() === "") delete win.automationDraft.timeout_sec
              else win.automationDraft.timeout_sec = value.trim()
            }
            onCommitted: win.validateAutomationDraft()
          }

          JarvixFormField {
            width: parent.width
            label: "Schedule (empty for phrase-triggered only)"
            placeholder: "08:30 mon-fri"
            problem: win.automationProblemFor("schedule")
            // The daemon's own next-fire arithmetic, previewed before the
            // save — the form never computes a date itself.
            hint: win.automationFormNextFire !== ""
              ? "Next fire: " + win.automationFormNextFire.substring(0, 16).replace("T", " ")
              : ""
            Component.onCompleted: text = String(win.automationDraft.schedule || "")
            onEdited: function(value) { win.automationDraft.schedule = value }
            onCommitted: win.validateAutomationDraft()
          }

          JarvixFormToggle {
            width: parent.width
            label: "Announce scheduled runs out loud"
            detail: "Off means scheduled runs report through the activity feed and a notification only."
            problem: win.automationProblemFor("announce")
            checked: win.automationDraft.announce === true
            onToggled: function(state) {
              win.automationDraft.announce = state
              win.reassignAutomationDraft()
            }
          }

          JarvixFormToggle {
            width: parent.width
            label: "Enabled"
            detail: "Off keeps the entry but takes its phrases out of the grammar and its schedule off the clock."
            problem: win.automationProblemFor("enabled")
            checked: win.automationDraft.enabled !== false
            onToggled: function(state) {
              win.automationDraft.enabled = state
              win.reassignAutomationDraft()
            }
          }

          // The routine's steps: add, remove, reorder — each step editing
          // the launch fields; sizing keys captured by #62 (size, position,
          // tile) ride along untouched and say so.
          Column {
            visible: win.automationFormFamily === "routines"
            width: parent.width
            spacing: Style.space(6)

            Text {
              text: "Steps (run in order)"
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
            }
            Text {
              visible: win.automationProblemFor("steps") !== ""
              width: parent.width
              wrapMode: Text.Wrap
              text: "Problem: " + win.automationProblemFor("steps")
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.urgent
            }
            Repeater {
              // A FRESH array on every structural change, so moving a step
              // rebuilds the delegates and each one re-reads the step it now
              // is. A model that was only the length would keep the same
              // delegates through a swap — the inputs are filled on
              // completion, which never runs again — and every field would
              // then be showing the other step's values while the draft, the
              // validation and the diagram had all moved on. Adding and
              // removing happened to work because the length changed; moving
              // did not, and moving is what this editor has to make visible.
              model: (win.automationDraft.steps || []).slice()
              delegate: Rectangle {
                required property int index
                width: parent.width
                height: stepBody.height + Style.space(16)
                radius: Style.cornerRadius
                color: Util.alpha(Color.popups.text, 0.04)
                border.color: Util.alpha(Color.popups.text, 0.25)
                border.width: 1

                Column {
                  id: stepBody
                  anchors.top: parent.top
                  anchors.left: parent.left
                  anchors.right: parent.right
                  anchors.margins: Style.space(8)
                  spacing: Style.space(6)

                  Row {
                    width: parent.width
                    spacing: Style.space(8)

                    Text {
                      text: "Step " + (index + 1)
                      anchors.verticalCenter: parent.verticalCenter
                      font.family: Style.font.family
                      font.bold: true
                      font.pixelSize: Style.font.subtitle
                      color: Color.popups.text
                    }
                    JarvixFormButton {
                      visible: index > 0
                      label: "Up"
                      name: "Move step " + (index + 1) + " up"
                      onClicked: {
                        var steps = win.automationDraft.steps
                        var s = steps[index]
                        steps[index] = steps[index - 1]
                        steps[index - 1] = s
                        win.reassignAutomationDraft()
                      }
                    }
                    JarvixFormButton {
                      visible: index < (win.automationDraft.steps || []).length - 1
                      label: "Down"
                      name: "Move step " + (index + 1) + " down"
                      onClicked: {
                        var steps = win.automationDraft.steps
                        var s = steps[index]
                        steps[index] = steps[index + 1]
                        steps[index + 1] = s
                        win.reassignAutomationDraft()
                      }
                    }
                    JarvixFormButton {
                      label: "Remove"
                      name: "Remove step " + (index + 1)
                      onClicked: {
                        win.automationDraft.steps.splice(index, 1)
                        win.reassignAutomationDraft()
                      }
                    }
                  }

                  JarvixFormField {
                    width: parent.width
                    label: "App (one executable name or absolute path)"
                    placeholder: "chromium"
                    monospace: true
                    hint: "Leave empty and name a desktop entry below instead."
                    problem: win.automationProblemFor("steps[" + index + "].app")
                    Component.onCompleted: text = String((win.automationDraft.steps[index] || {}).app || "")
                    onEdited: function(value) { win.automationDraft.steps[index].app = value }
                    onCommitted: win.validateAutomationDraft()
                  }
                  // What this machine cannot launch right now, in the
                  // daemon's own words (#175). A caution, not a problem: the
                  // routine saves either way, and the step is skipped by name
                  // when it runs — authoring a routine for something you have
                  // not installed yet is a thing people legitimately do, and
                  // on a machine being set up it is the normal case.
                  Text {
                    visible: win.automationStepNoteFor(index) !== ""
                    width: parent.width
                    wrapMode: Text.Wrap
                    text: "Not here yet: " + win.automationStepNoteFor(index)
                    font.family: Style.font.family
                    font.pixelSize: Style.font.subtitle
                    color: Util.alpha(Color.popups.text, 0.7)
                  }
                  // The desktop entry (#175): the name from the applications
                  // menu, for the many things on this desktop that have no
                  // binary of their own — the web apps, Signal, Discord. The
                  // daemon says whether it exists, on this field.
                  JarvixFormField {
                    width: parent.width
                    label: "…or desktop entry"
                    placeholder: "ChatGPT"
                    monospace: true
                    hint: "As it appears in the applications menu. Its own Exec line is what runs."
                    problem: win.automationProblemFor("steps[" + index + "].desktop_entry")
                    Component.onCompleted: text = String((win.automationDraft.steps[index] || {}).desktop_entry || "")
                    onEdited: function(value) { win.automationDraft.steps[index].desktop_entry = value }
                    onCommitted: win.validateAutomationDraft()
                  }
                  // Arguments, one per row, exactly as phrases are: a daemon
                  // problem keyed "steps[2].args[1]" then sits under the
                  // argument it means. Each row is ONE argument and is passed
                  // as one — nothing here is split on spaces, which is why a
                  // profile name with a space in it works.
                  Column {
                    id: argsColumn
                    // The step this argument list belongs to, captured here
                    // because the inner Repeater's delegate has an `index` of
                    // its own — the argument's — and the two would otherwise
                    // be the same word meaning two things.
                    readonly property int stepIndex: index
                    width: parent.width
                    spacing: Style.space(6)

                    Text {
                      text: "Arguments (one per row, passed exactly as typed)"
                      font.family: Style.font.family
                      font.pixelSize: Style.font.subtitle
                      color: Color.popups.text
                    }
                    Repeater {
                      model: ((win.automationDraft.steps[argsColumn.stepIndex] || {}).args || []).length
                      delegate: Row {
                        required property int index
                        width: parent.width
                        spacing: Style.space(8)

                        JarvixFormField {
                          width: parent.width - argRemove.width - Style.space(8)
                          label: "Argument " + (index + 1)
                          placeholder: "--profile-directory=Profile 3"
                          monospace: true
                          problem: win.automationProblemFor(
                            "steps[" + argsColumn.stepIndex + "].args[" + index + "]")
                          Component.onCompleted: text = String(
                            (win.automationDraft.steps[argsColumn.stepIndex].args || [])[index] || "")
                          onEdited: function(value) {
                            win.automationDraft.steps[argsColumn.stepIndex].args[index] = value
                          }
                          onCommitted: win.validateAutomationDraft()
                        }
                        JarvixFormButton {
                          id: argRemove
                          label: "Remove"
                          name: "Remove argument " + (index + 1)
                          onClicked: {
                            win.automationDraft.steps[argsColumn.stepIndex].args.splice(index, 1)
                            win.reassignAutomationDraft()
                          }
                        }
                      }
                    }
                    Text {
                      visible: win.automationProblemFor("steps[" + argsColumn.stepIndex + "].args") !== ""
                      width: parent.width
                      wrapMode: Text.Wrap
                      text: "Problem: " + win.automationProblemFor(
                        "steps[" + argsColumn.stepIndex + "].args")
                      font.family: Style.font.family
                      font.pixelSize: Style.font.subtitle
                      color: Color.urgent
                    }
                    JarvixFormButton {
                      label: "Add argument"
                      name: "Add an argument to step " + (argsColumn.stepIndex + 1)
                      onClicked: {
                        var step = win.automationDraft.steps[argsColumn.stepIndex]
                        if (!step.args) step.args = []
                        step.args.push("")
                        win.reassignAutomationDraft()
                      }
                    }
                  }
                  // The identity (#175): a window class this step gives the
                  // window it opens, so it can find its own afterwards. It is
                  // the only way to tell two Chromium profiles apart —
                  // Chromium runs them in one process, so class, PID and
                  // command line are identical for both.
                  JarvixFormField {
                    width: parent.width
                    label: "Window identity (optional)"
                    placeholder: "work-browser"
                    monospace: true
                    hint: "Launches the window with a class of its own, for programs that accept one."
                    problem: win.automationProblemFor("steps[" + index + "].identity")
                    Component.onCompleted: text = String((win.automationDraft.steps[index] || {}).identity || "")
                    onEdited: function(value) { win.automationDraft.steps[index].identity = value }
                    onCommitted: win.validateAutomationDraft()
                  }
                  // Adopt or launch, per step (#175). A closed set of two, so
                  // it is a picker over the daemon's own two options rather
                  // than a toggle whose "on" this file would have to spell.
                  JarvixMonitorPicker {
                    width: parent.width
                    label: "When a matching window is already open"
                    options: win.placementLaunchChoices
                    emptyLabel: "waiting for the daemon"
                    problem: win.automationProblemFor("steps[" + index + "].launch")
                    Component.onCompleted: value = String(
                      (win.automationDraft.steps[index] || {}).launch || "")
                    onChosen: function(chosen) {
                      value = chosen
                      win.automationDraft.steps[index].launch = chosen
                      win.validateAutomationDraft()
                    }
                  }
                  JarvixFormField {
                    width: parent.width
                    label: "Window match (empty to match on the app name)"
                    problem: win.automationProblemFor("steps[" + index + "].match")
                    Component.onCompleted: text = String((win.automationDraft.steps[index] || {}).match || "")
                    onEdited: function(value) { win.automationDraft.steps[index].match = value }
                    onCommitted: win.validateAutomationDraft()
                  }

                  // Where the window goes (#181): the window-placement
                  // vocabulary (ADR 0056), one control per key. Every closed
                  // set is the daemon's list and every message is the
                  // daemon's sentence — this form renders them and decides
                  // nothing about what fits.
                  JarvixFormField {
                    width: parent.width
                    label: win.placementWorkspaceLabel()
                    problem: win.automationProblemFor("steps[" + index + "].workspace")
                    Component.onCompleted: {
                      var w = (win.automationDraft.steps[index] || {}).workspace
                      text = w === undefined ? "" : String(w)
                    }
                    onEdited: function(value) { win.automationDraft.steps[index].workspace = value.trim() }
                    onCommitted: win.validateAutomationDraft()
                  }
                  // The screens, from the same picker and the same
                  // monitors.list reply the Screens section below uses, so a
                  // name given there is offered here without a reload (#180).
                  JarvixMonitorPicker {
                    width: parent.width
                    label: "Which screen"
                    options: win.monitorPickerOptions()
                    emptyLabel: "no screens reported"
                    problem: win.automationProblemFor("steps[" + index + "].monitor")
                    hint: "Leaving it on the current monitor keeps the workspace where the compositor has it."
                    Component.onCompleted: value = String(
                      (win.automationDraft.steps[index] || {}).monitor || "")
                    onChosen: function(chosen) {
                      value = chosen
                      win.automationDraft.steps[index].monitor = chosen
                      win.validateAutomationDraft()
                    }
                  }
                  JarvixMonitorPicker {
                    width: parent.width
                    label: "How the window sits"
                    options: win.placementModes
                    emptyLabel: "waiting for the daemon"
                    problem: win.automationProblemFor("steps[" + index + "].mode")
                    hint: win.placementUnsupportedHint()
                    Component.onCompleted: value = String(
                      (win.automationDraft.steps[index] || {}).mode || "")
                    onChosen: function(chosen) {
                      value = chosen
                      win.automationDraft.steps[index].mode = chosen
                      win.validateAutomationDraft()
                    }
                  }
                  JarvixFormField {
                    width: parent.width
                    label: "Width (a share of the screen, or pixels)"
                    placeholder: "66%"
                    monospace: true
                    problem: win.automationProblemFor("steps[" + index + "].width")
                    Component.onCompleted: text = String((win.automationDraft.steps[index] || {}).width || "")
                    onEdited: function(value) { win.automationDraft.steps[index].width = value }
                    onCommitted: win.validateAutomationDraft()
                  }
                  JarvixFormField {
                    width: parent.width
                    label: "Height (a share of the screen, or pixels)"
                    placeholder: "50%"
                    monospace: true
                    problem: win.automationProblemFor("steps[" + index + "].height")
                    Component.onCompleted: text = String((win.automationDraft.steps[index] || {}).height || "")
                    onEdited: function(value) { win.automationDraft.steps[index].height = value }
                    onCommitted: win.validateAutomationDraft()
                  }
                  // The floating position, as two numbers rather than one
                  // "x, y" box: a single field would mean this file splitting
                  // a value and inventing a rule for what is between them.
                  Row {
                    id: positionRow
                    readonly property int stepIndex: index
                    width: parent.width
                    spacing: Style.space(8)

                    JarvixFormField {
                      width: (parent.width - Style.space(8)) / 2
                      label: "Position across (floating only)"
                      placeholder: "100"
                      problem: win.automationProblemFor(
                        "steps[" + positionRow.stepIndex + "].position")
                      Component.onCompleted: text = win.automationStepPositionAt(
                        win.automationDraft.steps[positionRow.stepIndex], 0)
                      onEdited: function(value) {
                        win.automationSetStepPosition(positionRow.stepIndex, 0, value.trim())
                      }
                      onCommitted: win.validateAutomationDraft()
                    }
                    JarvixFormField {
                      width: (parent.width - Style.space(8)) / 2
                      label: "Position down (floating only)"
                      placeholder: "200"
                      Component.onCompleted: text = win.automationStepPositionAt(
                        win.automationDraft.steps[positionRow.stepIndex], 1)
                      onEdited: function(value) {
                        win.automationSetStepPosition(positionRow.stepIndex, 1, value.trim())
                      }
                      onCommitted: win.validateAutomationDraft()
                    }
                  }
                  // Where the NEXT window goes. This is the control that makes
                  // step order matter, which is why the diagram below has to
                  // redraw when a step moves.
                  JarvixMonitorPicker {
                    width: parent.width
                    label: "Where the next window on this workspace goes"
                    options: win.placementPlaceNext
                    emptyLabel: "waiting for the daemon"
                    problem: win.automationProblemFor("steps[" + index + "].place_next")
                    Component.onCompleted: value = String(
                      (win.automationDraft.steps[index] || {}).place_next || "")
                    onChosen: function(chosen) {
                      value = chosen
                      win.automationDraft.steps[index].place_next = chosen
                      win.validateAutomationDraft()
                    }
                  }
                  JarvixFormToggle {
                    width: parent.width
                    label: "Promote it into the layout's master pane"
                    detail: "Only master-family layouts have one; on any other the run says so."
                    problem: win.automationProblemFor("steps[" + index + "].master")
                    Component.onCompleted: checked =
                      (win.automationDraft.steps[index] || {}).master === true
                    onToggled: function(state) {
                      checked = state
                      win.automationDraft.steps[index].master = state
                      win.validateAutomationDraft()
                    }
                  }
                  JarvixMonitorPicker {
                    width: parent.width
                    label: "After it is placed"
                    options: win.placementFocusChoices
                    emptyLabel: "waiting for the daemon"
                    problem: win.automationProblemFor("steps[" + index + "].focus")
                    Component.onCompleted: value = String(
                      (win.automationDraft.steps[index] || {}).focus || "")
                    onChosen: function(chosen) {
                      value = chosen
                      win.automationDraft.steps[index].focus = chosen
                      win.validateAutomationDraft()
                    }
                  }
                  Text {
                    visible: win.automationStepExtraProblems(index) !== ""
                    width: parent.width
                    wrapMode: Text.Wrap
                    text: "Problem: " + win.automationStepExtraProblems(index)
                    font.family: Style.font.family
                    font.pixelSize: Style.font.subtitle
                    color: Color.urgent
                  }
                  // The step's own sentence: the arrangement in words, beside
                  // the fields that produced it. Composed by the daemon and
                  // rendered verbatim, so the diagram is never the only way to
                  // read what this step does.
                  Text {
                    visible: win.automationStepSummary(index) !== ""
                    width: parent.width
                    wrapMode: Text.Wrap
                    text: win.automationStepSummary(index)
                    font.family: Style.font.family
                    font.pixelSize: Style.font.subtitle
                    color: Util.alpha(Color.popups.text, 0.85)
                  }
                  // The pre-ADR-0056 keys a hand edit or an old capture left
                  // behind. They say what the controls above say, and the
                  // daemon refuses a step that says it twice — so the form
                  // keeps them until the user presses the button, and never
                  // deletes something nobody asked it to.
                  Column {
                    id: supersededColumn
                    readonly property int stepIndex: index
                    readonly property var keys: win.automationStepSuperseded(index)
                    visible: keys.length > 0
                    width: parent.width
                    spacing: Style.space(6)

                    Text {
                      width: parent.width
                      wrapMode: Text.Wrap
                      text: "This step still carries the older spelling: "
                        + supersededColumn.keys.join(", ")
                        + ". The controls above are the current one."
                      font.family: Style.font.family
                      font.pixelSize: Style.font.subtitle
                      color: Util.alpha(Color.popups.text, 0.7)
                    }
                    JarvixFormButton {
                      label: "Remove " + supersededColumn.keys.join(", ")
                      name: "Remove the older placement keys from step "
                        + (supersededColumn.stepIndex + 1)
                      onClicked: win.automationClearSuperseded(supersededColumn.stepIndex)
                    }
                  }
                }
              }
            }
            JarvixFormButton {
              label: "Add step"
              name: "Add another step"
              onClicked: {
                if (!win.automationDraft.steps) win.automationDraft.steps = []
                win.automationDraft.steps.push({ app: "", args: [], workspace: 1 })
                win.reassignAutomationDraft()
              }
            }
          }

          // The preview (#181): what this routine would look like, one
          // drawing per workspace, updating on every change the validation
          // updates on — a field committing, a step added, removed, or moved.
          //
          // Every number in it is the daemon's (ADR 0013). This section
          // decides only what order to stack the drawings in, which is the
          // order the daemon sent them: the order the routine first mentions
          // each workspace.
          Column {
            visible: win.automationFormFamily === "routines"
            width: parent.width
            spacing: Style.space(10)

            Text {
              text: "What this will look like"
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
            }
            Text {
              visible: win.automationPreviewWorkspaces().length === 0
              width: parent.width
              wrapMode: Text.Wrap
              text: "Nothing to draw yet — the preview appears once the routine has a name and a step."
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Util.alpha(Color.popups.text, 0.7)
            }
            Repeater {
              model: win.automationPreviewWorkspaces().length
              delegate: JarvixLayoutPreview {
                required property int index
                width: parent.width
                workspace: win.automationPreviewWorkspaces()[index]
              }
            }
          }

          // Delete, behind its confirm (#99): the byte-preserving removal —
          // phrases leave the grammar, the schedule stops — only after the
          // question is answered in the dialog.
          Column {
            visible: win.automationFormOriginalName !== ""
            width: parent.width
            spacing: Style.space(6)

            JarvixFormButton {
              visible: !win.automationDeleteConfirm
              label: "Delete this " + win.automationFormKindWord() + "…"
              name: "Delete the " + win.automationFormOriginalName + " " + win.automationFormKindWord()
              onClicked: win.automationDeleteConfirm = true
            }
            Text {
              visible: win.automationDeleteConfirm
              width: parent.width
              wrapMode: Text.Wrap
              text: "Delete “" + win.automationFormOriginalName + "”? Its phrases stop triggering and"
                + " any schedule stops firing. Everything else in config.toml stays as it is."
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
            }
            Row {
              visible: win.automationDeleteConfirm
              spacing: Style.space(8)

              JarvixFormButton {
                label: "Delete"
                name: "Confirm deleting " + win.automationFormOriginalName
                accent: true
                onClicked: win.deleteAutomationEntry()
              }
              JarvixFormButton {
                label: "Keep it"
                name: "Keep " + win.automationFormOriginalName
                onClicked: win.automationDeleteConfirm = false
              }
            }
          }
        }
      }
    }

    // The spoken-command form body (#164), built per open. Three fields, each
    // pinning the daemon's own message for its key: the phrase (where a
    // collision names its owner), the command (verbatim, monospaced, the
    // shell.run display doctrine applied at authoring time), and the
    // acknowledgement.
    Component {
      id: spokenFormBody

      Flickable {
        id: spokenScroll
        contentHeight: spokenColumn.height + Style.space(12)
        clip: true

        Column {
          id: spokenColumn
          width: spokenScroll.width
          spacing: Style.space(10)

          Text {
            visible: win.spokenFormError !== "" || win.spokenGeneralProblems() !== ""
            width: parent.width
            wrapMode: Text.Wrap
            text: (win.spokenFormError !== "" ? win.spokenFormError + "\n" : "")
              + win.spokenGeneralProblems()
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Color.urgent
          }

          JarvixFormField {
            width: parent.width
            label: "Phrase to say"
            placeholder: "lock the screen"
            hint: "Said exactly, with no placeholders — a slot would have to be pasted into the command."
            problem: win.spokenProblemFor("match")
            Component.onCompleted: text = String(win.spokenDraft.match || "")
            onEdited: function(value) { win.spokenDraft.match = value }
            onCommitted: win.validateSpokenDraft()
          }

          JarvixFormField {
            width: parent.width
            label: "Command to run"
            placeholder: "hyprlock"
            monospace: true
            hint: "Run through the same permission gate as anything else Jarvix runs."
            problem: win.spokenProblemFor("run")
            Component.onCompleted: text = String(win.spokenDraft.run || "")
            onEdited: function(value) { win.spokenDraft.run = value }
            onCommitted: win.validateSpokenDraft()
          }

          JarvixFormField {
            width: parent.width
            label: "What Jarvix says back"
            placeholder: "Done."
            hint: "Left empty, it says “Done.”"
            problem: win.spokenProblemFor("say")
            Component.onCompleted: text = String(win.spokenDraft.say || "")
            onEdited: function(value) { win.spokenDraft.say = value }
            onCommitted: win.validateSpokenDraft()
          }

          // Delete, behind a confirmation that names what goes — the entry
          // forms' shape, so removing a spoken command reads the same as
          // removing a routine.
          Column {
            visible: win.spokenFormOriginalMatch !== ""
            width: parent.width
            spacing: Style.space(6)

            JarvixFormButton {
              visible: !win.spokenDeleteConfirm
              label: "Delete this spoken command…"
              name: "Delete the spoken command " + win.spokenFormOriginalMatch
              onClicked: win.spokenDeleteConfirm = true
            }
            Text {
              visible: win.spokenDeleteConfirm
              width: parent.width
              wrapMode: Text.Wrap
              text: "Remove “" + win.spokenFormOriginalMatch
                + "”? The phrase stops being recognised on the next reload."
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
            }
            Row {
              visible: win.spokenDeleteConfirm
              spacing: Style.space(8)

              JarvixFormButton {
                label: "Delete"
                name: "Confirm removing " + win.spokenFormOriginalMatch
                accent: true
                onClicked: win.deleteSpokenEntry()
              }
              JarvixFormButton {
                label: "Keep it"
                name: "Keep " + win.spokenFormOriginalMatch
                onClicked: win.spokenDeleteConfirm = false
              }
            }
          }
        }
      }
    }

    // The reminder form body (#164): the words and the time, and the moment
    // the daemon resolved them to, shown before the save. The resolution line
    // is the daemon's own sentence — the same one a spoken reminder hears
    // back — so an ambiguous "at three" is settled on screen rather than
    // tomorrow morning.
    Component {
      id: reminderFormBody

      Flickable {
        id: reminderScroll
        contentHeight: reminderColumn.height + Style.space(12)
        clip: true

        Column {
          id: reminderColumn
          width: reminderScroll.width
          spacing: Style.space(10)

          Text {
            visible: win.reminderFormError !== "" || win.reminderProblemFor("") !== ""
            width: parent.width
            wrapMode: Text.Wrap
            text: (win.reminderFormError !== "" ? win.reminderFormError + "\n" : "")
              + win.reminderProblemFor("")
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Color.urgent
          }

          JarvixFormField {
            width: parent.width
            label: "Remind me to"
            placeholder: "call the pharmacy"
            problem: win.reminderProblemFor("text")
            Component.onCompleted: text = String(win.reminderDraftText || "")
            onEdited: function(value) { win.reminderDraftText = value }
          }

          JarvixFormField {
            width: parent.width
            label: "When"
            placeholder: "at three, or in twenty minutes"
            hint: "The same words you would say — “at 15:00”, “tomorrow at nine”, “in half an hour”."
            problem: win.reminderProblemFor("when")
            Component.onCompleted: text = String(win.reminderDraftWhen || "")
            onEdited: function(value) { win.reminderDraftWhen = value }
            onCommitted: win.previewReminder()
          }

          Text {
            visible: win.reminderPreview !== ""
            width: parent.width
            wrapMode: Text.Wrap
            text: "Fires " + win.reminderPreview + "."
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Color.popups.text
          }
        }
      }
    }

    // The screen-name form body (#180), built per open. The name field pins
    // the daemon's message for "name" — every collision the ticket names
    // arrives there — and the picker pins "connector".
    Component {
      id: monitorFormBody

      Flickable {
        id: monitorFormScroll
        contentHeight: monitorFormColumn.height + Style.space(12)
        clip: true

        Column {
          id: monitorFormColumn
          width: monitorFormScroll.width
          spacing: Style.space(10)

          Text {
            visible: win.monitorFormError !== "" || win.monitorProblemFor("") !== ""
            width: parent.width
            wrapMode: Text.Wrap
            text: win.monitorProblemFor("") !== "" ? win.monitorProblemFor("") : win.monitorFormError
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Color.urgent
          }

          JarvixFormField {
            width: parent.width
            label: "Call this screen"
            placeholder: "top"
            problem: win.monitorProblemFor("name")
            hint: win.monitorFormExisting === ""
              ? "One word, yours to choose — but not a connector name and not "
                + win.monitorReserved.join(" or ") + "."
              : "Changing the name here is not supported; forget it and name the screen again."
            Component.onCompleted: text = win.monitorFormName
            onEdited: function(value) { win.monitorFormName = value }
            onCommitted: {}
          }

          JarvixMonitorPicker {
            width: parent.width
            label: "Which screen"
            options: win.monitorPickerOptions()
            emptyLabel: "no screens reported"
            value: win.monitorFormConnector
            problem: win.monitorProblemFor("connector")
            hint: "The screens plugged in right now, and “the current monitor” for the one you are on."
            onChosen: function(value) { win.monitorFormConnector = value }
          }
        }
      }
    }

    // The Knowledge tab (issues #92/#100): the feed cache as cards — name,
    // mode and cadence, the current value (or "not fetched yet") with its
    // spoken-style age, STALE marked in words, failing-since with the error —
    // and the operations: Refresh now (knowledge.refresh_now, through the
    // daemon's scheduled-fetch path), Enable/Disable (knowledge.set_enabled,
    // the surgical config write), and New/Edit/Delete through the entry form
    // dialog (#100) — clicking a card opens it, the footer's New button
    // opens an empty one, and every rule the form enforces is the daemon's.
    Item {
      id: knowledgeScreen
      visible: win.socketReady && win.currentTab === "knowledge"
      anchors.top: tabStrip.bottom
      anchors.topMargin: Style.space(12)
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.bottom: errorBanner.visible ? errorBanner.top : parent.bottom
      anchors.bottomMargin: errorBanner.visible ? Style.space(12) : 0

      JarvixEmptyState {
        visible: win.knowledgeFeeds.length === 0 && !win.knowledgeFormOpen
        anchors.centerIn: parent
        width: parent.width
        text: win.knowledgeEnabled
          ? "No knowledge feeds yet — the New feed button below creates one."
          : "No knowledge feeds are configured yet — the New feed button below creates one (the first feed needs a daemon restart to start fetching)."
      }

      ListView {
        id: knowledgeList
        visible: win.knowledgeFeeds.length > 0 && !win.knowledgeFormOpen
        anchors.top: parent.top
        anchors.left: parent.left
        anchors.right: parent.right
        anchors.bottom: knowledgeNewRow.top
        anchors.bottomMargin: Style.space(8)
        clip: true
        spacing: Style.space(10)
        model: win.knowledgeFeeds

        delegate: JarvixCollectionRow {
          required property var modelData
          width: knowledgeList.width
          title: modelData.name
          subtitle: win.feedCadence(modelData)
          // The value itself, verbatim (the user's own data on the user's
          // own screen — shown here, never logged; the daemon holds the
          // same rule).
          detail: modelData.has_value ? String(modelData.value) : "not fetched yet"
          meta: win.feedFreshness(modelData)
          // Failing and stale both flag the title; the words are already in
          // meta, so the colour is never the only carrier.
          flagged: Boolean(modelData.failing)
            || (Boolean(modelData.stale) && modelData.enabled !== false)
          // The card itself opens the edit form (#100) — name, command,
          // cadence, and Delete live there.
          interactive: true
          onActivated: win.openKnowledgeEdit(modelData.name)
          // A parked feed cannot be refreshed — the daemon would refuse —
          // so the card does not offer it; Enable is the way back.
          actionLabel: modelData.enabled === false ? "" : "Refresh now"
          actionName: "Refresh the " + modelData.name + " feed now"
          onActionTriggered: win.refreshFeed(modelData.name)
          action2Label: modelData.enabled === false ? "Enable" : "Disable"
          action2Name: (modelData.enabled === false ? "Enable" : "Disable")
            + " the " + modelData.name + " feed"
          onAction2Triggered: win.setFeedEnabled(modelData.name, modelData.enabled === false)
        }
      }

      // The New button (#100), replacing #92's copyable TOML hint: creation
      // is a form now.
      Row {
        id: knowledgeNewRow
        visible: !win.knowledgeFormOpen
        anchors.bottom: parent.bottom
        anchors.left: parent.left
        spacing: Style.space(8)

        JarvixFormButton {
          label: "New feed…"
          name: "Create a new knowledge feed"
          accent: true
          onClicked: win.openKnowledgeCreate()
        }
      }

      // The feed form (#100): a pane that replaces the listing, on the
      // shared detail scaffold — Back cancels, the accent action saves.
      // Loaded fresh per open so every input initialises from the draft.
      JarvixDetailPane {
        id: knowledgeFormPane
        visible: win.knowledgeFormOpen
        anchors.fill: parent
        backName: "Cancel and go back to the feeds"
        actionLabel: "Save"
        actionName: "Save the feed"
        note: (win.knowledgeFormOriginalName === ""
          ? "New feed"
          : "Editing feed “" + win.knowledgeFormOriginalName + "”")
        onBackRequested: win.closeKnowledgeForm()
        onActionTriggered: win.saveKnowledgeForm()

        Loader {
          anchors.fill: parent
          active: win.knowledgeFormOpen
          sourceComponent: knowledgeFormBody
        }
      }
    }

    // The feed form body, built per open (see the Loader above). Every field
    // pins the daemon's own message for its key — knowledgeProblemFor — and
    // the general area shows whole-entry problems and conflicts, so a
    // refused save is never silent and never colour-only.
    Component {
      id: knowledgeFormBody

      Flickable {
        id: feedFormScroll
        contentHeight: feedFormColumn.height + Style.space(12)
        clip: true

        Column {
          id: feedFormColumn
          width: feedFormScroll.width
          spacing: Style.space(10)

          // The form-level area: transport errors, the fingerprint
          // conflict's "changed outside the window" sentence, and problems
          // the daemon could not pin to a field — all verbatim.
          Text {
            visible: win.knowledgeFormError !== "" || win.knowledgeProblemFor("") !== ""
            width: parent.width
            wrapMode: Text.Wrap
            text: (win.knowledgeFormError !== "" ? win.knowledgeFormError + "\n" : "")
              + win.knowledgeProblemFor("")
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Color.urgent
          }

          JarvixFormField {
            width: parent.width
            label: "Name (what you and the model call this feed)"
            placeholder: "amd"
            problem: win.knowledgeProblemFor("name")
            Component.onCompleted: text = String(win.knowledgeDraft.name || "")
            onEdited: function(value) { win.knowledgeDraft.name = value }
            onCommitted: win.validateKnowledgeDraft()
          }

          JarvixFormField {
            width: parent.width
            label: "Description (tells the model what this feed watches)"
            placeholder: "AMD share price in dollars"
            problem: win.knowledgeProblemFor("description")
            Component.onCompleted: text = String(win.knowledgeDraft.description || "")
            onEdited: function(value) { win.knowledgeDraft.description = value }
            onCommitted: win.validateKnowledgeDraft()
          }

          // The command: one row per argv element, so the fixed program and
          // its arguments read exactly as they will run — never a shell
          // line, and nothing typed here runs on save.
          Column {
            width: parent.width
            spacing: Style.space(6)

            Text {
              text: "Command (the program that prints the value, one argument per row)"
              width: parent.width
              wrapMode: Text.Wrap
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
            }
            Repeater {
              model: (win.knowledgeDraft.command || []).length
              delegate: Row {
                required property int index
                width: parent.width
                spacing: Style.space(8)

                JarvixFormField {
                  width: parent.width - commandRemove.width - Style.space(8)
                  label: index === 0 ? "Program" : "Argument " + index
                  placeholder: index === 0 ? "/home/you/bin/amd-price" : ""
                  monospace: true
                  Component.onCompleted: text = String((win.knowledgeDraft.command || [])[index] || "")
                  onEdited: function(value) { win.knowledgeDraft.command[index] = value }
                  onCommitted: win.validateKnowledgeDraft()
                }
                JarvixFormButton {
                  id: commandRemove
                  label: "Remove"
                  name: index === 0 ? "Remove the program row" : "Remove argument " + index
                  onClicked: {
                    win.knowledgeDraft.command.splice(index, 1)
                    win.reassignKnowledgeDraft()
                  }
                }
              }
            }
            Text {
              visible: win.knowledgeProblemFor("command") !== ""
              width: parent.width
              wrapMode: Text.Wrap
              text: "Problem: " + win.knowledgeProblemFor("command")
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.urgent
            }
            Text {
              width: parent.width
              wrapMode: Text.Wrap
              text: "Saving never runs the command — it only runs when the feed refreshes."
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Util.alpha(Color.popups.text, 0.6)
            }
            JarvixFormButton {
              label: "Add argument"
              name: "Add another command argument"
              onClicked: {
                if (!win.knowledgeDraft.command) win.knowledgeDraft.command = []
                win.knowledgeDraft.command.push("")
                win.reassignKnowledgeDraft()
              }
            }
          }

          JarvixFormToggle {
            width: parent.width
            label: "Eager — refreshed on a schedule"
            detail: "Off means lazy: fetched on first use, then cached until it goes stale."
            problem: win.knowledgeProblemFor("mode")
            checked: win.knowledgeDraft.mode !== "lazy"
            onToggled: function(state) {
              win.knowledgeDraft.mode = state ? "eager" : "lazy"
              win.reassignKnowledgeDraft()
            }
          }

          JarvixFormField {
            width: parent.width
            label: "Refresh cadence in seconds (empty for the default; eager feeds only)"
            placeholder: "300"
            problem: win.knowledgeProblemFor("interval_sec")
            Component.onCompleted: text = win.knowledgeDraft.interval_sec === undefined
              ? "" : String(win.knowledgeDraft.interval_sec)
            onEdited: function(value) {
              if (value.trim() === "") delete win.knowledgeDraft.interval_sec
              else win.knowledgeDraft.interval_sec = value.trim()
            }
            onCommitted: win.validateKnowledgeDraft()
          }

          JarvixFormField {
            width: parent.width
            label: "Fresh for, in seconds (empty for the default)"
            placeholder: "600"
            problem: win.knowledgeProblemFor("ttl_sec")
            Component.onCompleted: text = win.knowledgeDraft.ttl_sec === undefined
              ? "" : String(win.knowledgeDraft.ttl_sec)
            onEdited: function(value) {
              if (value.trim() === "") delete win.knowledgeDraft.ttl_sec
              else win.knowledgeDraft.ttl_sec = value.trim()
            }
            onCommitted: win.validateKnowledgeDraft()
          }

          JarvixFormField {
            width: parent.width
            label: "Fetch timeout in seconds (empty for the default)"
            placeholder: "30"
            problem: win.knowledgeProblemFor("timeout_sec")
            Component.onCompleted: text = win.knowledgeDraft.timeout_sec === undefined
              ? "" : String(win.knowledgeDraft.timeout_sec)
            onEdited: function(value) {
              if (value.trim() === "") delete win.knowledgeDraft.timeout_sec
              else win.knowledgeDraft.timeout_sec = value.trim()
            }
            onCommitted: win.validateKnowledgeDraft()
          }

          JarvixFormToggle {
            width: parent.width
            label: "Offer the value to the model every turn"
            detail: "On injects the cached value into each conversation turn, under the knowledge budget."
            problem: win.knowledgeProblemFor("inject")
            checked: win.knowledgeDraft.inject === true
            onToggled: function(state) {
              win.knowledgeDraft.inject = state
              win.reassignKnowledgeDraft()
            }
          }

          JarvixFormToggle {
            width: parent.width
            label: "Enabled"
            detail: "Off keeps the feed and its last value but stops every fetch."
            problem: win.knowledgeProblemFor("enabled")
            checked: win.knowledgeDraft.enabled !== false
            onToggled: function(state) {
              win.knowledgeDraft.enabled = state
              win.reassignKnowledgeDraft()
            }
          }

          // Delete, behind its confirm (#100): byte-preserving removal — the
          // cached value stops serving — only after the question is answered.
          Column {
            visible: win.knowledgeFormOriginalName !== ""
            width: parent.width
            spacing: Style.space(6)

            JarvixFormButton {
              visible: !win.knowledgeDeleteConfirm
              label: "Delete this feed…"
              name: "Delete the " + win.knowledgeFormOriginalName + " feed"
              onClicked: win.knowledgeDeleteConfirm = true
            }
            Text {
              visible: win.knowledgeDeleteConfirm
              width: parent.width
              wrapMode: Text.Wrap
              text: "Delete “" + win.knowledgeFormOriginalName + "”? Its fetches stop and its cached"
                + " value no longer serves. Everything else in config.toml stays as it is."
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
            }
            Row {
              visible: win.knowledgeDeleteConfirm
              spacing: Style.space(8)

              JarvixFormButton {
                label: "Delete"
                name: "Confirm deleting " + win.knowledgeFormOriginalName
                accent: true
                onClicked: win.deleteKnowledgeEntry()
              }
              JarvixFormButton {
                label: "Keep it"
                name: "Keep " + win.knowledgeFormOriginalName
                onClicked: win.knowledgeDeleteConfirm = false
              }
            }
          }
        }
      }
    }

    // The Providers tab (#163): two lists on one screen — the endpoints
    // Jarvix thinks with, and the assistant CLIs it can consult — each row
    // saying what a user needs before opening anything: which endpoint is in
    // use, whether a credential is available and where from, and which
    // permission tier an advisor's configuration earns.
    Item {
      id: providersScreen
      visible: win.socketReady && win.currentTab === "providers"
      anchors.top: tabStrip.bottom
      anchors.topMargin: Style.space(12)
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.bottom: errorBanner.visible ? errorBanner.top : parent.bottom
      anchors.bottomMargin: errorBanner.visible ? Style.space(12) : 0

      Flickable {
        id: providersScroll
        visible: !win.providerFormOpen
        anchors.fill: parent
        contentHeight: providersColumn.height + Style.space(12)
        clip: true

        Column {
          id: providersColumn
          width: providersScroll.width
          spacing: Style.space(10)

          Text {
            width: parent.width
            wrapMode: Text.Wrap
            text: "Endpoints — where Jarvix sends what you say"
            font.family: Style.font.family
            font.pixelSize: Style.font.title
            font.bold: true
            color: Color.popups.text
          }

          JarvixEmptyState {
            visible: win.providerEndpoints.length === 0
            width: parent.width
            text: "No endpoints are configured — the New endpoint button below adds one."
          }

          Repeater {
            model: win.providerEndpoints

            delegate: JarvixCollectionRow {
              required property var modelData
              readonly property var entry: modelData.entry || ({})
              readonly property bool inUse: String(entry.name || "") === win.providerInUse
              width: providersColumn.width
              title: String(entry.name || "")
              subtitle: inUse
                ? "in use — ai.provider points here"
                : "configured, not selected"
              detail: String(entry.base_url || "")
              meta: win.endpointCredentialLine(modelData)
              interactive: true
              onActivated: win.openProviderEdit("ai", String(entry.name || ""))
              // Test is offered on the row as well as in the form: proving an
              // endpoint answers is the thing a user wants to do before they
              // trust it, and making them open a form first would put a step
              // between the doubt and the answer.
              actionLabel: "Test"
              actionName: "Test the " + String(entry.name || "") + " endpoint"
              onActionTriggered: win.testProviderEndpoint(String(entry.name || ""))
            }
          }

          Text {
            visible: win.providerTestRunning || String(win.providerTestResult.outcome || "") !== ""
            width: parent.width
            wrapMode: Text.Wrap
            text: win.providerTestLine()
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            // Words first: the outcome is spelled out in the sentence, so the
            // colour is never the only thing carrying it.
            color: String(win.providerTestResult.outcome || "") === "reachable"
              ? Color.popups.text : Color.urgent
          }

          JarvixFormButton {
            label: "New endpoint…"
            name: "Add a new AI endpoint"
            accent: true
            onClicked: win.openProviderCreate("ai")
          }

          Text {
            width: parent.width
            wrapMode: Text.Wrap
            text: "Advisors — stronger assistants Jarvix can consult"
            font.family: Style.font.family
            font.pixelSize: Style.font.title
            font.bold: true
            color: Color.popups.text
          }

          JarvixEmptyState {
            visible: win.providerAdvisors.length === 0
            width: parent.width
            text: "No advisors are configured — the New advisor button below adds one."
          }

          Repeater {
            model: win.providerAdvisors

            delegate: JarvixCollectionRow {
              required property var modelData
              readonly property var entry: modelData.entry || ({})
              width: providersColumn.width
              title: String(entry.name || "")
              subtitle: String(entry.description || "an assistant CLI on this computer")
              detail: String(entry.binary || "")
                + (entry.args ? " " + entry.args.join(" ") : "")
              // The earned permission tier, in the daemon's own words, on the
              // row — so "this one asks first" is readable without opening
              // anything (ADR 0016).
              meta: win.providerNoteLine(modelData)
              interactive: true
              onActivated: win.openProviderEdit("advisors", String(entry.name || ""))
            }
          }

          JarvixFormButton {
            label: "New advisor…"
            name: "Add a new advisor"
            accent: true
            onClicked: win.openProviderCreate("advisors")
          }
        }
      }

      // The provider form: a pane that replaces the lists, on the shared
      // detail scaffold. Loaded fresh per open so every input initialises
      // from the draft rather than binding to it.
      JarvixDetailPane {
        id: providerFormPane
        visible: win.providerFormOpen
        anchors.fill: parent
        backName: "Cancel and go back to the providers"
        actionLabel: "Save"
        actionName: "Save this provider"
        note: (win.providerFormOriginalName === ""
          ? (win.providerFormFamily === "ai" ? "New endpoint" : "New advisor")
          : "Editing “" + win.providerFormOriginalName + "”")
        onBackRequested: win.closeProviderForm()
        onActionTriggered: win.saveProviderForm()

        Loader {
          anchors.fill: parent
          active: win.providerFormOpen
          sourceComponent: providerFormBody
        }
      }
    }

    // The provider form body, built per open (see the Loader above). Every
    // field pins the daemon's own message for its key — providerProblemFor —
    // and the general area shows whole-entry problems and conflicts, so a
    // refused save is never silent and never colour-only.
    Component {
      id: providerFormBody

      Flickable {
        id: providerFormScroll
        contentHeight: providerFormColumn.height + Style.space(12)
        clip: true

        Column {
          id: providerFormColumn
          width: providerFormScroll.width
          spacing: Style.space(10)

          // The form-level area: transport errors, the fingerprint
          // conflict's "changed outside the window" sentence, the in-use
          // delete refusal, and problems the daemon could not pin to a field
          // — all verbatim.
          Text {
            visible: win.providerFormError !== "" || win.providerProblemFor("") !== ""
            width: parent.width
            wrapMode: Text.Wrap
            text: (win.providerFormError !== "" ? win.providerFormError + "\n" : "")
              + win.providerProblemFor("")
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Color.urgent
          }

          JarvixFormField {
            width: parent.width
            label: win.providerFormFamily === "ai"
              ? "Name (what ai.provider selects this endpoint by)"
              : "Name (the advisor you ask for by name)"
            placeholder: win.providerFormFamily === "ai" ? "openai" : "claude"
            problem: win.providerProblemFor("name")
            Component.onCompleted: text = String(win.providerDraft.name || "")
            onEdited: function(value) { win.providerDraft.name = value }
            onCommitted: win.validateProviderDraft()
          }

          // ----- endpoint fields -----
          JarvixFormField {
            visible: win.providerFormFamily === "ai"
            width: parent.width
            label: "Base URL (the API root, ending in /v1 for most services)"
            placeholder: "https://api.openai.com/v1"
            monospace: true
            problem: win.providerProblemFor("base_url")
            Component.onCompleted: text = String(win.providerDraft.base_url || "")
            onEdited: function(value) { win.providerDraft.base_url = value }
            onCommitted: win.validateProviderDraft()
          }

          Column {
            visible: win.providerFormFamily === "ai"
            width: parent.width
            spacing: Style.space(6)

            Text {
              width: parent.width
              wrapMode: Text.Wrap
              text: "Credential"
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              font.bold: true
              color: Color.popups.text
            }
            // What the daemon reports about the stored key: whether one is
            // available and where from. Never the key, never a prefix of it,
            // never a mask the length of it — this window is not sent one.
            Text {
              width: parent.width
              wrapMode: Text.Wrap
              text: win.providerFormSecretLine()
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Util.alpha(Color.popups.text, 0.7)
            }
            Text {
              visible: win.providerProblemFor("api_key") !== ""
              width: parent.width
              wrapMode: Text.Wrap
              text: "Problem: " + win.providerProblemFor("api_key")
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.urgent
            }

            JarvixFormField {
              width: parent.width
              label: "Environment variable holding the key (the safer choice — "
                + "the key stays out of config.toml and out of every backup of it)"
              placeholder: "OPENAI_API_KEY"
              monospace: true
              problem: win.providerProblemFor("api_key_env")
              Component.onCompleted: text = String(win.providerDraft.api_key_env || "")
              onEdited: function(value) { win.providerDraft.api_key_env = value }
              onCommitted: win.validateProviderDraft()
            }

            Row {
              width: parent.width
              spacing: Style.space(8)

              JarvixFormButton {
                visible: win.providerSecretAction !== "set"
                label: "Set a key…"
                name: "Type a new API key for this endpoint"
                onClicked: {
                  win.providerSecretValue = ""
                  win.providerSecretAction = "set"
                }
              }
              JarvixFormButton {
                visible: win.providerSecretAction === "set"
                label: "Cancel key change"
                name: "Keep the stored key"
                onClicked: {
                  win.providerSecretValue = ""
                  win.providerSecretAction = "keep"
                  win.validateProviderDraft()
                }
              }
              JarvixFormButton {
                visible: win.providerSecretAction !== "clear"
                  && ((win.providerFormSecrets.api_key || {}).inline_key === true)
                label: "Clear the stored key"
                name: "Remove the key stored in config.toml"
                onClicked: {
                  win.providerSecretValue = ""
                  win.providerSecretAction = "clear"
                  win.validateProviderDraft()
                }
              }
              JarvixFormButton {
                visible: win.providerSecretAction === "clear"
                label: "Keep the stored key"
                name: "Keep the key stored in config.toml"
                onClicked: {
                  win.providerSecretAction = "keep"
                  win.validateProviderDraft()
                }
              }
            }

            // The one input in this form that carries a credential, and it
            // only ever travels outward: what is typed here is sent with the
            // save and is never read back, because nothing sends it back.
            JarvixFormField {
              visible: win.providerSecretAction === "set"
              width: parent.width
              label: "New API key (stored in config.toml; it is never displayed again)"
              placeholder: "sk-…"
              monospace: true
              Component.onCompleted: text = String(win.providerSecretValue || "")
              onEdited: function(value) { win.providerSecretValue = value }
              onCommitted: win.validateProviderDraft()
            }
          }

          Column {
            visible: win.providerFormFamily === "ai" && win.providerFormOriginalName !== ""
            width: parent.width
            spacing: Style.space(6)

            JarvixFormButton {
              label: "Test this endpoint"
              name: "Make a real request to " + win.providerFormOriginalName
              onClicked: win.testProviderEndpoint(win.providerFormOriginalName)
            }
            // The probe tests what is SAVED, so an unsaved edit would be
            // tested against the old values — saying so beats a result that
            // quietly answers a different question.
            Text {
              width: parent.width
              wrapMode: Text.Wrap
              text: win.providerTestLine() !== ""
                ? win.providerTestLine()
                : "Tests the endpoint as it is saved — save your changes first to test them."
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: String(win.providerTestResult.outcome || "") === ""
                ? Util.alpha(Color.popups.text, 0.7)
                : (String(win.providerTestResult.outcome) === "reachable"
                  ? Color.popups.text : Color.urgent)
            }
          }

          // ----- advisor fields -----
          JarvixFormField {
            visible: win.providerFormFamily === "advisors"
            width: parent.width
            label: "Binary (an absolute path, or a name found on PATH)"
            placeholder: "/usr/bin/claude"
            monospace: true
            problem: win.providerProblemFor("binary")
            Component.onCompleted: text = String(win.providerDraft.binary || "")
            onEdited: function(value) { win.providerDraft.binary = value }
            onCommitted: win.validateProviderDraft()
          }

          Column {
            visible: win.providerFormFamily === "advisors"
            width: parent.width
            spacing: Style.space(6)

            Text {
              width: parent.width
              wrapMode: Text.Wrap
              text: "Arguments"
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              font.bold: true
              color: Color.popups.text
            }
            // The tier the current configuration earns, in the daemon's own
            // words, next to the field that decides it — so nobody loosens or
            // tightens a permission gate by typing a flag without seeing it
            // (ADR 0016).
            Text {
              visible: win.providerNoteFor("args") !== ""
              width: parent.width
              wrapMode: Text.Wrap
              text: win.providerNoteFor("args")
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Util.alpha(Color.popups.text, 0.7)
            }
            Text {
              visible: win.providerProblemFor("args") !== ""
              width: parent.width
              wrapMode: Text.Wrap
              text: "Problem: " + win.providerProblemFor("args")
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.urgent
            }

            Repeater {
              model: (win.providerDraft.args || []).length

              delegate: Row {
                required property int index
                width: parent.width
                spacing: Style.space(8)

                JarvixFormField {
                  width: parent.width - argRemove.width - Style.space(8)
                  label: "Argument " + (index + 1)
                  placeholder: index === 0 ? "-p" : "{question}"
                  monospace: true
                  Component.onCompleted: text = String((win.providerDraft.args || [])[index] || "")
                  onEdited: function(value) { win.providerDraft.args[index] = value }
                  onCommitted: win.validateProviderDraft()
                }
                JarvixFormButton {
                  id: argRemove
                  label: "Remove"
                  name: "Remove argument " + (index + 1)
                  onClicked: {
                    win.providerDraft.args.splice(index, 1)
                    if (win.providerDraft.args.length === 0) delete win.providerDraft.args
                    win.reassignProviderDraft()
                  }
                }
              }
            }

            Row {
              width: parent.width
              spacing: Style.space(8)

              JarvixFormButton {
                label: "Add argument"
                name: "Add an argument to this advisor's command line"
                onClicked: {
                  if (win.providerDraft.args === undefined) win.providerDraft.args = []
                  win.providerDraft.args.push("")
                  win.reassignProviderDraft()
                }
              }
              JarvixFormButton {
                visible: win.providerDraft.args !== undefined
                label: "Use the shipped preset"
                name: "Drop these arguments and use the shipped preset for this advisor"
                onClicked: {
                  delete win.providerDraft.args
                  win.reassignProviderDraft()
                }
              }
            }
            Text {
              width: parent.width
              wrapMode: Text.Wrap
              text: "Write {question} as an argument of its own to pass the question there; "
                + "with no {question} the question goes to the program's standard input."
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Util.alpha(Color.popups.text, 0.7)
            }
          }

          JarvixFormField {
            visible: win.providerFormFamily === "advisors"
            width: parent.width
            label: "Timeout in seconds (empty for the default)"
            placeholder: "120"
            problem: win.providerProblemFor("timeout_sec")
            Component.onCompleted: text = win.providerDraft.timeout_sec === undefined
              ? "" : String(win.providerDraft.timeout_sec)
            onEdited: function(value) {
              if (value.trim() === "") delete win.providerDraft.timeout_sec
              else win.providerDraft.timeout_sec = value.trim()
            }
            onCommitted: win.validateProviderDraft()
          }

          JarvixFormField {
            visible: win.providerFormFamily === "advisors"
            width: parent.width
            label: "Description (tells the model what this advisor is good for)"
            placeholder: "deep reasoning, code review, long-context analysis"
            problem: win.providerProblemFor("description")
            Component.onCompleted: text = String(win.providerDraft.description || "")
            onEdited: function(value) { win.providerDraft.description = value }
            onCommitted: win.validateProviderDraft()
          }

          // Delete, behind a confirm. The daemon refuses the endpoint
          // ai.provider is using, with that reason — which lands in the
          // form-level area above rather than being predicted here.
          Column {
            visible: win.providerFormOriginalName !== ""
            width: parent.width
            spacing: Style.space(6)

            JarvixFormButton {
              visible: !win.providerDeleteConfirm
              label: win.providerFormFamily === "ai" ? "Delete this endpoint…" : "Delete this advisor…"
              name: "Delete " + win.providerFormOriginalName
              onClicked: win.providerDeleteConfirm = true
            }
            Text {
              visible: win.providerDeleteConfirm
              width: parent.width
              wrapMode: Text.Wrap
              text: "Delete “" + win.providerFormOriginalName + "”? "
                + "It is removed from config.toml; everything else in the file is left alone."
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
            }
            Row {
              visible: win.providerDeleteConfirm
              spacing: Style.space(8)

              JarvixFormButton {
                label: "Delete"
                name: "Delete " + win.providerFormOriginalName
                accent: true
                onClicked: win.deleteProviderEntry()
              }
              JarvixFormButton {
                label: "Keep it"
                name: "Keep " + win.providerFormOriginalName
                onClicked: win.providerDeleteConfirm = false
              }
            }
          }
        }
      }
    }

    // The Memory tab (issues #92/#100): the fact store from memory.list (ADR
    // 0025) — dates, an expandable supersede trail rendered from the
    // existing `previous` data, filter-as-you-type whose matching is the
    // daemon's own query, and per-fact Forget through the gated tool path:
    // the standard confirmation card appears in Chat (the tab badge points
    // there), and this list refreshes when the daemon's events resolve it.
    // The Approvals tab (#162, ADR 0053; extended by #164, ADR 0054): both
    // halves of the gate's configured vocabulary — every command pattern that
    // runs without being asked about, with when it was agreed to and how often
    // it has fired, and every deny rule that refuses one whatever else says
    // otherwise.
    //
    // A rule can now be added here as well as on the confirmation card, and the
    // two routes are deliberately not equivalent: the CARD derives its pattern
    // and this form does not, so what a person types is judged by the card's own
    // refusal matrix before it can land, and the refusal it comes back with is
    // the matrix's sentence verbatim. Removing an ALLOW rule is immediate —
    // tightening the gate is never something to make hard — while removing a
    // DENY rule asks first, with the daemon's own sentence saying what the rule
    // protected.
    Item {
      id: approvalsScreen
      visible: win.socketReady && win.currentTab === "approvals"
      anchors.top: tabStrip.bottom
      anchors.topMargin: Style.space(12)
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.bottom: errorBanner.visible ? errorBanner.top : parent.bottom
      anchors.bottomMargin: errorBanner.visible ? Style.space(12) : 0

      Flickable {
        id: approvalsScroll
        visible: !win.approvalFormOpen
        anchors.top: parent.top
        anchors.left: parent.left
        anchors.right: parent.right
        anchors.bottom: approvalsFooter.top
        anchors.bottomMargin: Style.space(8)
        contentHeight: approvalsColumn.height + Style.space(12)
        clip: true

        Column {
          id: approvalsColumn
          width: approvalsScroll.width
          spacing: Style.space(10)

          Text {
            text: "Runs without asking"
            font.family: Style.font.family
            font.bold: true
            font.pixelSize: Style.font.subtitle
            color: Color.popups.text
          }

          JarvixEmptyState {
            visible: win.approvals.length === 0
            width: parent.width
            text: "Nothing is pre-approved — every command still asks first.\n"
              + "Answer a permission question with \u201cApprove and don\u2019t ask again\u201d "
              + "to add a rule here."
          }

          Repeater {
            model: win.approvals

            delegate: JarvixCollectionRow {
              required property var modelData
              width: approvalsColumn.width
              // The pattern verbatim: it is a command prefix, and the card
              // that added it showed it this way.
              title: modelData.pattern
              subtitle: win.approvalSubtitle(modelData)
              meta: win.approvalMeta(modelData)
              actionLabel: "Forget"
              actionName: "Forget the pre-approval " + modelData.pattern
                + " — that command will ask again"
              onActionTriggered: win.forgetApproval(modelData.pattern)
            }
          }

          JarvixFormButton {
            label: "Add an allow rule…"
            name: "Add a command prefix that runs without asking"
            onClicked: win.openApprovalAdd("allow")
          }

          Text {
            text: "Always refused"
            font.family: Style.font.family
            font.bold: true
            font.pixelSize: Style.font.subtitle
            color: Color.popups.text
          }

          JarvixEmptyState {
            visible: win.denials.length === 0
            width: parent.width
            text: "No deny rules — only the always-risky commands are refused outright."
          }

          Repeater {
            model: win.denials

            delegate: JarvixCollectionRow {
              required property var modelData
              width: approvalsColumn.width
              title: modelData.pattern
              subtitle: "Refused outright — deny beats every allow rule"
              actionLabel: "Remove…"
              actionName: "Remove the deny rule " + modelData.pattern
              onActionTriggered: win.removeDeny(modelData.pattern, false)
            }
          }

          // The confirmation a deny removal asks for, in the daemon's words:
          // what the rule protected, and what happens without it. It appears
          // under the list rather than as a popover because it is a paragraph
          // to read, not a yes/no reflex.
          Column {
            visible: win.denyRemovalConfirmation !== ""
            width: parent.width
            spacing: Style.space(6)

            Text {
              width: parent.width
              wrapMode: Text.Wrap
              text: win.denyRemovalConfirmation
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
            }
            Row {
              spacing: Style.space(8)

              JarvixFormButton {
                label: "Remove the rule"
                name: "Confirm removing the deny rule " + win.denyRemovalPattern
                accent: true
                onClicked: win.removeDeny(win.denyRemovalPattern, true)
              }
              JarvixFormButton {
                label: "Keep it"
                name: "Keep the deny rule " + win.denyRemovalPattern
                onClicked: {
                  win.denyRemovalPattern = ""
                  win.denyRemovalConfirmation = ""
                }
              }
            }
          }

          JarvixFormButton {
            label: "Add a deny rule…"
            name: "Add a command prefix that is always refused"
            accent: true
            onClicked: win.openApprovalAdd("deny")
          }

          // --- the windows Jarvix manages (#197) ---------------------------
          // Beneath the command grants because it is the same kind of thing
          // seen from the other side: those say which commands run without
          // asking, this says which windows Jarvix may act inside. Both are
          // grants, and both belong where a person can find and undo them.
          Text {
            text: "Windows Jarvix manages"
            font.family: Style.font.family
            font.bold: true
            font.pixelSize: Style.font.subtitle
            color: Color.popups.text
          }

          Text {
            width: parent.width
            wrapMode: Text.Wrap
            text: win.managedTyping
              ? "Managed windows may be read, moved and typed into. Being managed is not "
                + "permission to run anything: text typed into a terminal is confirmed command "
                + "by command, exactly as a shell command is."
              : "Managed windows may be read and moved. Typing is switched off in your "
                + "configuration ([tools.typing] enable), so nothing is typed into them."
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Util.alpha(Color.popups.text, 0.7)
          }

          JarvixEmptyState {
            visible: win.managedWindows.length === 0
            width: parent.width
            text: "Jarvix manages no windows — every window on your desktop is yours alone.\n"
              + "Say \u201ctake control of this terminal\u201d to hand one over."
          }

          Repeater {
            model: win.managedWindows

            delegate: JarvixCollectionRow {
              required property var modelData
              width: approvalsColumn.width
              title: String(modelData.nickname || "") !== ""
                ? String(modelData.nickname) + " \u2014 " + String(modelData.app || "")
                : String(modelData.app || "")
              subtitle: win.managedSubtitle(modelData)
              meta: win.managedMeta(modelData)
              // No button for a row with no unambiguous reference: releasing
              // the wrong one of three identical terminals would be worse
              // than the button not being there, and managedMeta says what to
              // do instead.
              actionLabel: String(modelData.reference || "") === "" ? "" : "Release"
              actionName: "Stop managing " + String(modelData.app || "") + " — Jarvix keeps its hands off it"
              onActionTriggered: win.releaseManagedWindow(modelData.reference)
            }
          }
        }
      }

      // The add form. One field, because a rule IS one field: the leading
      // words of a command. The refusal that comes back sits under it like any
      // other field problem.
      JarvixDetailPane {
        id: approvalFormPane
        visible: win.approvalFormOpen
        anchors.fill: parent
        backName: "Cancel and go back to the list"
        actionLabel: "Add rule"
        actionName: win.approvalFormList === "deny"
          ? "Add this deny rule" : "Add this allow rule"
        note: win.approvalFormList === "deny"
          ? "New deny rule — always refused"
          : "New allow rule — runs without asking"
        onBackRequested: win.closeApprovalForm()
        onActionTriggered: win.submitApprovalAdd()

        Loader {
          anchors.fill: parent
          active: win.approvalFormOpen
          sourceComponent: approvalFormBody
        }
      }

      Text {
        id: approvalsFooter
        visible: !win.approvalFormOpen
        anchors.bottom: parent.bottom
        anchors.left: parent.left
        anchors.right: parent.right
        wrapMode: Text.Wrap
        text: win.approvalsPath === "" ? ""
          : "Both lists live in " + win.approvalsPath
            + " under [tools.policy] — yours to edit. "
            + "Deny rules and always-risky commands still ask, or refuse, whatever is allowed here."
            + (win.managedPath === "" ? ""
               : " The managed windows live in " + win.managedPath
                 + "; deleting an entry there releases it, exactly as the button does.")
        font.family: Style.font.family
        font.pixelSize: Style.font.subtitle
        color: Util.alpha(Color.popups.text, 0.7)
      }
    }

    // The add form's body, built per open. The hint under the field says what
    // a rule means, in the same vocabulary the card uses, because the whole
    // risk of typing one is thinking it is narrower than it is.
    Component {
      id: approvalFormBody

      Column {
        spacing: Style.space(10)

        Text {
          visible: win.approvalFormError !== ""
          width: parent.width
          wrapMode: Text.Wrap
          text: win.approvalFormError
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Color.urgent
        }

        JarvixFormField {
          width: parent.width
          label: win.approvalFormList === "deny"
            ? "Command prefix to refuse"
            : "Command prefix to allow"
          placeholder: "docker ps"
          monospace: true
          hint: win.approvalFormList === "deny"
            ? "Leading words, then anything: “git push” refuses every git push. Deny always wins."
            : "Leading words, then anything: “docker ps” covers “docker ps --format x”. "
              + "Risky commands and deny rules still stop it."
          problem: win.approvalFormProblem
          Component.onCompleted: text = String(win.approvalFormPattern || "")
          onEdited: function(value) { win.approvalFormPattern = value }
          onCommitted: win.submitApprovalAdd()
        }
      }
    }

    // Add and Edit (#100) open a form pane whose saves go to memory.add /
    // memory.update — the book's own write path, never the config editor —
    // ungated because nothing they do destroys (Forget keeps its card).
    Item {
      id: memoryScreen
      visible: win.socketReady && win.currentTab === "memory"
      anchors.top: tabStrip.bottom
      anchors.topMargin: Style.space(12)
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.bottom: errorBanner.visible ? errorBanner.top : parent.bottom
      anchors.bottomMargin: errorBanner.visible ? Style.space(12) : 0

      Rectangle {
        id: memoryFilterBox
        visible: win.memoryEnabled && !win.memoryTabFormOpen
          && (win.memoryFacts.length > 0 || win.memoryQuery !== "")
        anchors.top: parent.top
        anchors.left: parent.left
        anchors.right: parent.right
        height: memoryFilterInput.height + Style.space(16)
        radius: Style.cornerRadius
        color: Util.alpha(Color.popups.text, 0.06)
        // The focus ring: a colour *and* a thicker border, like the composer.
        border.color: memoryFilterInput.activeFocus ? Color.accent : Util.alpha(Color.popups.text, 0.4)
        border.width: memoryFilterInput.activeFocus ? 2 : 1

        TextInput {
          id: memoryFilterInput
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
          Accessible.name: "Filter remembered facts"
          Accessible.description: "The list narrows as you type; clear the box to see everything"
          // Every keystroke asks the daemon: the matching lives there, once
          // (the same query the CLI's memory list takes), and this box only
          // relays it.
          onTextChanged: win.filterMemory(text)

          Text {
            visible: memoryFilterInput.text === ""
            anchors.left: parent.left
            anchors.verticalCenter: parent.verticalCenter
            text: "Filter facts as you type"
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Util.alpha(Color.popups.text, 0.45)
          }
        }
      }

      // Anchored inside the facts half rather than centred in the tab: the
      // Vocabulary section (#129) owns the lower part of this pane, and an
      // empty-facts sentence floating over it would read as its caption.
      JarvixEmptyState {
        visible: win.memoryFacts.length === 0 && !win.memoryTabFormOpen
        anchors.top: parent.top
        anchors.topMargin: Style.space(48)
        width: parent.width
        text: !win.memoryEnabled
          ? "Memory is switched off (memory.enabled = false)."
          : win.memoryQuery.trim() !== ""
            ? "No remembered fact matches “" + win.memoryQuery + "” — clear the box to see everything."
            : "Nothing remembered yet — say “remember …”, or add a fact with the button below."
      }

      Text {
        id: memoryCountLine
        visible: win.memoryFacts.length > 0 && !win.memoryTabFormOpen
        anchors.top: memoryFilterBox.visible ? memoryFilterBox.bottom : parent.top
        anchors.topMargin: memoryFilterBox.visible ? Style.space(8) : 0
        width: parent.width
        text: win.memoryQuery.trim() !== ""
          ? win.memoryFacts.length + " of " + win.memoryFactCount + " facts match"
          : win.memoryFactCount + " of " + win.memoryFactMax + " facts remembered"
        font.family: Style.font.family
        font.pixelSize: Style.font.subtitle
        color: Util.alpha(Color.popups.text, 0.7)
      }

      // The over-budget warning (#104): the daemon's sentence, verbatim, in
      // words — urgent colour flags it but never carries it alone. Present
      // exactly when the daemon sent one; a trim is never silent here.
      Text {
        id: memoryWarningLine
        visible: win.memoryWarning !== "" && !win.memoryTabFormOpen
        anchors.top: memoryCountLine.visible ? memoryCountLine.bottom
          : (memoryFilterBox.visible ? memoryFilterBox.bottom : parent.top)
        anchors.topMargin: Style.space(6)
        width: parent.width
        wrapMode: Text.Wrap
        text: "Warning: " + win.memoryWarning
        font.family: Style.font.family
        font.pixelSize: Style.font.subtitle
        color: Color.urgent
      }

      ListView {
        id: memoryList
        visible: win.memoryFacts.length > 0 && !win.memoryTabFormOpen
        anchors.top: memoryWarningLine.visible ? memoryWarningLine.bottom : memoryCountLine.bottom
        anchors.topMargin: Style.space(8)
        anchors.left: parent.left
        anchors.right: parent.right
        anchors.bottom: memoryNewRow.top
        anchors.bottomMargin: Style.space(8)
        clip: true
        spacing: Style.space(10)
        model: win.memoryFacts

        // One fact: the shared row plus, when expanded, the supersede trail
        // — the values this fact held before, straight from the `previous`
        // the daemon already serves. Expansion is presentation state; it
        // resets when the list reloads, and nothing is fetched for it.
        delegate: Column {
          id: factDelegate
          required property var modelData
          property bool expanded: false
          width: memoryList.width
          spacing: Style.space(4)

          JarvixCollectionRow {
            width: parent.width
            title: factDelegate.modelData.content
            meta: win.factMeta(factDelegate.modelData)
              + ((factDelegate.modelData.previous || []).length > 0
                ? (factDelegate.expanded ? " · press to fold the history" : " · press to unfold the history")
                : "")
            interactive: (factDelegate.modelData.previous || []).length > 0
            onActivated: factDelegate.expanded = !factDelegate.expanded
            // Edit opens the form (#100): the correction path for a fact
            // whose wording is wrong, superseding rather than destroying.
            actionLabel: "Edit"
            actionName: "Edit: " + factDelegate.modelData.content
            onActionTriggered: win.openMemoryEdit(String(factDelegate.modelData.id),
              String(factDelegate.modelData.content),
              factDelegate.modelData.pinned === true)
            // Pin second, Forget last (#104): the reversible toggle sits
            // above the one destructive act, which keeps its confirmation
            // card.
            action2Label: factDelegate.modelData.pinned === true ? "Unpin" : "Pin"
            action2Name: (factDelegate.modelData.pinned === true ? "Unpin: " : "Pin: ")
              + factDelegate.modelData.content
            onAction2Triggered: win.setFactPinned(String(factDelegate.modelData.id),
              factDelegate.modelData.pinned !== true)
            action3Label: "Forget"
            action3Name: "Forget: " + factDelegate.modelData.content
            onAction3Triggered: win.forgetFact(String(factDelegate.modelData.id))
          }

          Column {
            visible: factDelegate.expanded
            width: parent.width
            spacing: Style.space(2)

            Repeater {
              model: factDelegate.expanded ? (factDelegate.modelData.previous || []) : []
              delegate: Text {
                required property var modelData
                width: factDelegate.width - Style.space(16)
                x: Style.space(16)
                wrapMode: Text.Wrap
                text: "was: “" + String(modelData.content) + "” — "
                  + String(modelData.stored || "").substring(0, 10)
                  + " until " + String(modelData.superseded || "").substring(0, 10)
                font.family: Style.font.family
                font.pixelSize: Style.font.subtitle
                color: Util.alpha(Color.popups.text, 0.7)
              }
            }
          }
        }
      }

      // The Add button (#100). Hidden with memory disabled — the daemon
      // would refuse, and the empty state already says why. Since #129 it
      // bottoms out on the Vocabulary section rather than the pane.
      Row {
        id: memoryNewRow
        visible: win.memoryEnabled && !win.memoryTabFormOpen
        anchors.bottom: vocabSection.top
        anchors.bottomMargin: Style.space(12)
        anchors.left: parent.left
        spacing: Style.space(8)

        JarvixFormButton {
          label: "Add a fact…"
          name: "Add a new remembered fact"
          accent: true
          onClicked: win.openMemoryAdd()
        }
      }

      // The Vocabulary section (issue #129): the second collection of the
      // Memory tab — the words the user taught, from vocabulary.list, with
      // Teach/Edit through the shared form machinery and Delete through the
      // gated tool path. A fixed share of the pane rather than a flow: both
      // collections keep their own scroll, and neither can push the other
      // off screen.
      Item {
        id: vocabSection
        visible: !win.memoryTabFormOpen
        anchors.bottom: parent.bottom
        anchors.left: parent.left
        anchors.right: parent.right
        height: Math.round(parent.height * 0.45)

        Text {
          id: vocabHeader
          anchors.top: parent.top
          width: parent.width
          text: {
            if (!win.vocabEnabled) return "Vocabulary"
            var line = "Vocabulary — " + win.vocabCount + " of " + win.vocabMax
              + (win.vocabCount === 1 ? " word" : " words") + " taught"
            if (win.vocabBiasCount > 0) {
              line += " · " + win.vocabBiasCount + " of " + win.vocabBiasMax + " listened for"
            }
            return line
          }
          font.family: Style.font.family
          font.bold: true
          font.pixelSize: Style.font.subtitle
          color: Color.popups.text
        }

        // The over-budget disclosure: the daemon's sentence, verbatim, in
        // words — urgent colour flags it but never carries it alone.
        Text {
          id: vocabWarningLine
          visible: win.vocabWarning !== ""
          anchors.top: vocabHeader.bottom
          anchors.topMargin: Style.space(6)
          width: parent.width
          wrapMode: Text.Wrap
          text: "Warning: " + win.vocabWarning
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Color.urgent
        }

        JarvixEmptyState {
          visible: win.vocabEntries.length === 0
          anchors.top: vocabWarningLine.visible ? vocabWarningLine.bottom : vocabHeader.bottom
          anchors.topMargin: Style.space(24)
          width: parent.width
          text: !win.vocabEnabled
            ? "Vocabulary is switched off (vocabulary.enabled = false)."
            : "No words taught yet — say “when I say quid I mean pounds”, or teach one with the button below."
        }

        ListView {
          id: vocabList
          visible: win.vocabEntries.length > 0
          anchors.top: vocabWarningLine.visible ? vocabWarningLine.bottom : vocabHeader.bottom
          anchors.topMargin: Style.space(8)
          anchors.left: parent.left
          anchors.right: parent.right
          anchors.bottom: vocabNewRow.top
          anchors.bottomMargin: Style.space(8)
          clip: true
          spacing: Style.space(10)
          model: win.vocabEntries

          // One taught word: the shared row plus, when expanded, the
          // supersede trail — the meanings this phrase held before, straight
          // from the `previous` the daemon serves (the facts' idiom).
          delegate: Column {
            id: vocabDelegate
            required property var modelData
            property bool expanded: false
            width: vocabList.width
            spacing: Style.space(4)

            JarvixCollectionRow {
              width: parent.width
              title: "“" + vocabDelegate.modelData.phrase + "” — " + vocabDelegate.modelData.meaning
              subtitle: String(vocabDelegate.modelData.note || "")
              meta: win.vocabMeta(vocabDelegate.modelData)
                + ((vocabDelegate.modelData.previous || []).length > 0
                  ? (vocabDelegate.expanded ? " · press to fold the history" : " · press to unfold the history")
                  : "")
              interactive: (vocabDelegate.modelData.previous || []).length > 0
              onActivated: vocabDelegate.expanded = !vocabDelegate.expanded
              actionLabel: "Edit"
              actionName: "Edit: " + vocabDelegate.modelData.phrase
              onActionTriggered: win.openVocabEdit(vocabDelegate.modelData)
              // Delete keeps its confirmation card (the gated path): the one
              // destructive act on this list, quieter than Edit.
              action2Label: "Delete"
              action2Name: "Delete: " + vocabDelegate.modelData.phrase
              onAction2Triggered: win.forgetVocabEntry(String(vocabDelegate.modelData.id))
            }

            Column {
              visible: vocabDelegate.expanded
              width: parent.width
              spacing: Style.space(2)

              Repeater {
                model: vocabDelegate.expanded ? (vocabDelegate.modelData.previous || []) : []
                delegate: Text {
                  required property var modelData
                  width: vocabDelegate.width - Style.space(16)
                  x: Style.space(16)
                  wrapMode: Text.Wrap
                  text: "meant: “" + String(modelData.meaning) + "” — "
                    + String(modelData.taught || "").substring(0, 10)
                    + " until " + String(modelData.superseded || "").substring(0, 10)
                  font.family: Style.font.family
                  font.pixelSize: Style.font.subtitle
                  color: Util.alpha(Color.popups.text, 0.7)
                }
              }
            }
          }
        }

        Row {
          id: vocabNewRow
          visible: win.vocabEnabled
          anchors.bottom: parent.bottom
          anchors.left: parent.left
          spacing: Style.space(8)

          JarvixFormButton {
            label: "Teach a word…"
            name: "Teach a new word or phrase"
            accent: true
            onClicked: win.openVocabAdd()
          }
        }
      }

      // The fact form (#100): one text field on the shared scaffold. Back
      // cancels, Save goes to the memory book's own write path.
      JarvixDetailPane {
        id: memoryFormPane
        visible: win.memoryFormOpen
        anchors.fill: parent
        backName: "Cancel and go back to the facts"
        actionLabel: "Save"
        actionName: "Save the fact"
        note: (win.memoryFormId === ""
          ? "New fact"
          : "Editing fact " + win.memoryFormId)
        onBackRequested: win.closeMemoryForm()
        onActionTriggered: win.saveMemoryForm()

        Loader {
          anchors.fill: parent
          active: win.memoryFormOpen
          sourceComponent: memoryFormBody
        }
      }

      // The vocabulary form (#129): phrase, meaning, note, and the listen
      // toggle on the same scaffold. Back cancels, Save goes to the store's
      // own write path (teach or update — the daemon supersedes, never
      // duplicates).
      JarvixDetailPane {
        id: vocabFormPane
        visible: win.vocabFormOpen
        anchors.fill: parent
        backName: "Cancel and go back to the vocabulary"
        actionLabel: "Save"
        actionName: "Save the word"
        note: (win.vocabFormId === ""
          ? "New word"
          : "Editing word " + win.vocabFormId)
        onBackRequested: win.closeVocabForm()
        onActionTriggered: win.saveVocabForm()

        Loader {
          anchors.fill: parent
          active: win.vocabFormOpen
          sourceComponent: vocabFormBody
        }
      }
    }

    // The fact form body, built per open. The field pins the daemon's
    // content problems; the general area carries whole-store refusals (the
    // cap) and transport errors — all verbatim, never colour alone.
    Component {
      id: memoryFormBody

      Column {
        spacing: Style.space(10)

        Text {
          visible: win.memoryFormError !== "" || win.memoryProblemFor("") !== ""
          width: parent.width
          wrapMode: Text.Wrap
          text: (win.memoryFormError !== "" ? win.memoryFormError + "\n" : "")
            + win.memoryProblemFor("")
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Color.urgent
        }

        JarvixFormField {
          width: parent.width
          label: "The fact, in words"
          placeholder: "the staging server is called atlas"
          problem: win.memoryProblemFor("content")
          hint: win.memoryFormId === ""
            ? "Kept until you forget it."
            : "Saving a wording change keeps the old wording on the fact's history."
          Component.onCompleted: text = win.memoryFormContent
          onEdited: function(value) { win.memoryFormContent = value }
          onCommitted: {}
        }

        // The pin toggle (#104): ambient versus searchable, in words.
        JarvixFormToggle {
          width: parent.width
          label: "Pinned"
          detail: "A pinned fact rides every prompt; unpinned facts are found on demand with memory.search."
          checked: win.memoryFormPinned
          onToggled: function(checked) { win.memoryFormPinned = checked }
        }
      }
    }

    // The vocabulary form body (#129), built per open. Each field pins the
    // daemon's problems; the general area carries whole-store refusals (the
    // caps) and transport errors — all verbatim, never colour alone.
    Component {
      id: vocabFormBody

      Column {
        spacing: Style.space(10)

        Text {
          visible: win.vocabFormError !== "" || win.vocabProblemFor("") !== ""
          width: parent.width
          wrapMode: Text.Wrap
          text: (win.vocabFormError !== "" ? win.vocabFormError + "\n" : "")
            + win.vocabProblemFor("")
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Color.urgent
        }

        JarvixFormField {
          width: parent.width
          label: "The word or phrase, as you say it"
          placeholder: "quid"
          problem: win.vocabProblemFor("phrase")
          hint: win.vocabFormId === ""
            ? "Teaching a phrase you already taught updates its meaning instead."
            : "Renaming keeps the entry; its history stays with it."
          Component.onCompleted: text = win.vocabFormPhrase
          onEdited: function(value) { win.vocabFormPhrase = value }
          onCommitted: {}
        }

        JarvixFormField {
          width: parent.width
          label: "What it means when you say it"
          placeholder: "pounds"
          problem: win.vocabProblemFor("meaning")
          hint: "Saving a meaning change keeps the old meaning on the word's history."
          Component.onCompleted: text = win.vocabFormMeaning
          onEdited: function(value) { win.vocabFormMeaning = value }
          onCommitted: {}
        }

        JarvixFormField {
          width: parent.width
          label: "Note (optional)"
          placeholder: "UK money slang"
          problem: win.vocabProblemFor("note")
          Component.onCompleted: text = win.vocabFormNote
          onEdited: function(value) { win.vocabFormNote = value }
          onCommitted: {}
        }

        // The listen toggle: the STT-bias flag, in words — including that
        // the budget is small, so its refusal (field-keyed from the daemon)
        // never arrives unexplained.
        JarvixFormToggle {
          width: parent.width
          label: "Listen for it"
          detail: "Biases speech recognition toward the phrase when it keeps being misheard. "
            + "Only a few words fit (" + win.vocabBiasCount + " of " + win.vocabBiasMax + " used)."
          problem: win.vocabProblemFor("hard_to_hear")
          checked: win.vocabFormHard
          onToggled: function(checked) { win.vocabFormHard = checked }
        }
      }
    }

    ListView {
      id: list
      visible: win.socketReady && win.currentTab === "chat"
      anchors.top: tabStrip.bottom
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

        Row {
          spacing: Style.space(8)

          Text {
            anchors.verticalCenter: parent.verticalCenter
            text: model.role === "user" ? "You"
              : model.role === "confirmation" ? "Jarvix asks permission" : "Jarvix"
            font.family: Style.font.family
            font.bold: true
            font.pixelSize: Style.font.subtitle
            color: model.role === "user"
              ? Util.alpha(Color.popups.text, 0.7)
              : Color.accent
          }

          // The speak-again control (issue #122): one click asks the daemon
          // to say this message again. Display-only (ADR 0013) — the row's
          // record position and role go over the wire, the daemon resolves
          // the text from its own record and owns every precedence decision.
          // Present only on rows that carry a record position (pos > 0):
          // live-appended rows gain theirs at the next turn boundary, and a
          // replay is refused mid-turn anyway.
          Rectangle {
            id: replayButton
            visible: model.pos > 0
            enabled: visible && win.socketReady
            anchors.verticalCenter: parent.verticalCenter
            width: replayLabel.width + Style.space(12)
            height: replayLabel.height + Style.space(4)
            radius: Style.cornerRadius
            color: Util.alpha(Color.popups.text, replayButton.activeFocus ? 0.18 : 0.06)
            border.color: Util.alpha(Color.popups.text, 0.5)
            border.width: replayButton.activeFocus ? 2 : 0
            activeFocusOnTab: enabled
            Accessible.role: Accessible.Button
            Accessible.name: "Say this message again"
            Keys.onReturnPressed: win.requestReplay(model.pos, model.role)
            Keys.onSpacePressed: win.requestReplay(model.pos, model.role)

            Text {
              id: replayLabel
              anchors.centerIn: parent
              text: "Say again"
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Util.alpha(Color.popups.text, 0.7)
            }
            MouseArea {
              anchors.fill: parent
              enabled: replayButton.enabled
              onClicked: win.requestReplay(model.pos, model.role)
            }
          }
        }
        // The message body follows the user's reading-comfort settings
        // (issue #121); everything else in the window — speaker labels,
        // tabs, cards, chrome — keeps the design system's scale. The size
        // multiplies the design token (so it still follows the shell's font
        // scale), letter spacing is ems of the rendered size, and line
        // height is proportional. At the defaults (1.0 / 1.0 / 0.0) every
        // expression below reduces to exactly the hard-coded original.
        Text {
          id: messageBody
          visible: model.role !== "confirmation"
          text: model.text
          width: parent.width
          wrapMode: Text.Wrap

          // The rendered size, held once so that both font bindings can read
          // it without either depending on the other.
          //
          // Deriving letter spacing from `font.pixelSize` instead — which is
          // what this delegate did until issue #203 — is a binding loop.
          // `font` is one grouped value, not a bag of independent
          // properties: reading any member subscribes to the whole group and
          // writing any member notifies it, so a letterSpacing binding that
          // reads pixelSize depends on a property that same binding
          // participates in. Qt says so at runtime ("Binding loop detected
          // for property font.letterSpacing") and then breaks the cycle the
          // only way it can, by dropping a binding. Which one it drops is an
          // evaluation-order detail, so `ui.text_size` and
          // `ui.letter_spacing` could quietly stop applying, or apply to some
          // messages and not others. Those are the reading-comfort settings
          // (#121) — set deliberately, for readability — and a setting that
          // silently stops working is worse than one that was never offered.
          //
          // Reading a plain property breaks the cycle without moving a single
          // number: this is the same expression the pixelSize binding always
          // had, so at the defaults (text size ×1.0, letter spacing 0.0) both
          // lines below still reduce to the pre-#121 hard-coded rendering
          // that TestReadingComfortDefaultsPinTheHardCodedRendering pins.
          readonly property int bodyPixelSize:
            Math.max(1, Math.round(Style.font.subtitle * win.chatTextScale))

          font.family: Style.font.family
          font.pixelSize: messageBody.bodyPixelSize
          font.letterSpacing: messageBody.bodyPixelSize * win.chatLetterSpacing
          lineHeight: win.chatLineSpacing
          lineHeightMode: Text.ProportionalHeight
          // The pending turn (issue #158) reads a shade quieter than an
          // answer, so a glance can tell "still working" from "here it is"
          // without reading. It is never the *only* signal and never a colour
          // one: the row says "Thinking", "Running a shell command",
          // "Consulting claude · 8s" in words, which is the whole point — and
          // it says them without a single moving pixel, because unexplained
          // waiting is expensive and gratuitous motion is aversive.
          color: model.pending ? Util.alpha(Color.popups.text, 0.75) : Color.popups.text
        }

        // What went into this answer (issue #168): one control, collapsed,
        // and nothing at all on a turn that consumed nothing — absence is
        // information, and an affordance that is always there says nothing.
        //
        // The label is deliberately "what went into this", never "sources"
        // or "citations": the daemon knows what it put in front of the model
        // and what a tool returned, and it does not know which of those the
        // model leaned on. Each row then says which of the two claims it is,
        // in words, because they are two different claims.
        Column {
          id: provenancePanel
          visible: model.role === "assistant" && win.provenanceCount(model.provJson) > 0
          width: parent.width
          spacing: Style.space(4)

          readonly property bool open: win.provenancePos === model.pos && model.pos > 0
          readonly property int count: win.provenanceCount(model.provJson)
          readonly property int hidden: win.provenanceTruncated(model.provJson)

          Rectangle {
            id: provenanceToggle
            width: provenanceToggleLabel.width + Style.space(12)
            height: provenanceToggleLabel.height + Style.space(4)
            radius: Style.cornerRadius
            color: Util.alpha(Color.popups.text, provenanceToggle.activeFocus ? 0.18 : 0.06)
            border.color: Util.alpha(Color.popups.text, 0.5)
            border.width: provenanceToggle.activeFocus ? 2 : 0
            activeFocusOnTab: true
            Accessible.role: Accessible.Button
            Accessible.name: provenanceToggleLabel.text
            Accessible.description: "Lists what was available to this answer and what a tool returned while it was being written"
            Keys.onReturnPressed: win.toggleProvenance(model.pos, model.provJson)
            Keys.onSpacePressed: win.toggleProvenance(model.pos, model.provJson)

            Text {
              id: provenanceToggleLabel
              anchors.centerIn: parent
              text: "What went into this · " + provenancePanel.count
                + (provenancePanel.open ? " · press to fold" : " · press to unfold")
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Util.alpha(Color.popups.text, 0.7)
            }
            MouseArea {
              anchors.fill: parent
              onClicked: win.toggleProvenance(model.pos, model.provJson)
            }
          }

          Text {
            visible: provenancePanel.open && win.provenanceLoading
            width: parent.width
            wrapMode: Text.Wrap
            text: "Looking these up…"
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Util.alpha(Color.popups.text, 0.6)
          }

          Text {
            visible: provenancePanel.open && win.provenanceError !== ""
            width: parent.width
            wrapMode: Text.Wrap
            text: win.provenanceError
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Color.popups.text
          }

          // One row per source, on the shared collection row, so a source
          // reads exactly like a fact or a feed does in its own tab. The
          // wording, the liveness and the buttons are all the daemon's answer
          // — this only draws them.
          Repeater {
            model: provenancePanel.open ? win.provenanceItems : []
            delegate: JarvixCollectionRow {
              required property var modelData
              width: provenancePanel.width
              title: String(modelData.name || "")
              subtitle: String(modelData.strength_phrase || "")
              // A source that no longer exists says so here, and its actions
              // were never sent — never a dead button, never a silent no-op.
              meta: String(modelData.note || "")
              flagged: Boolean(modelData.gone)
              actionLabel: (modelData.actions || []).length > 0
                ? String(modelData.actions[0].label) : ""
              actionName: actionLabel + " for " + title
              onActionTriggered: win.runProvenanceAction(modelData, modelData.actions[0])
              action2Label: (modelData.actions || []).length > 1
                ? String(modelData.actions[1].label) : ""
              action2Name: action2Label + " for " + title
              onAction2Triggered: win.runProvenanceAction(modelData, modelData.actions[1])
            }
          }

          Text {
            visible: provenancePanel.open && provenancePanel.hidden > 0
            width: parent.width
            wrapMode: Text.Wrap
            text: provenancePanel.hidden === 1
              ? "One more source went unrecorded — this turn used more than Jarvix keeps."
              : provenancePanel.hidden + " more sources went unrecorded — this turn used more than Jarvix keeps."
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Util.alpha(Color.popups.text, 0.6)
          }
        }

        // The confirmation card (issue #76): the question, the exact command
        // verbatim from the daemon in a monospace block, approve/decline
        // buttons, and the countdown. Resolved cards stay in the transcript
        // with their outcome as text and the buttons disabled — the record of
        // what was asked and answered, twin to the activity feed's gate rows.
        Rectangle {
          id: confirmCard
          visible: model.role === "confirmation"
          width: parent.width
          height: visible ? cardBody.height + Style.space(20) : 0
          radius: Style.cornerRadius
          color: Util.alpha(Color.accent, 0.08)
          // The focus ring: a colour *and* a thicker border, like the composer.
          border.color: confirmCard.activeFocus ? Color.accent : Util.alpha(Color.accent, 0.5)
          border.width: confirmCard.activeFocus ? 2 : 1
          activeFocusOnTab: visible && model.outcome === ""
          Accessible.role: Accessible.Grouping
          Accessible.name: "Permission question: " + model.text
            + " Command: " + model.command
            + (model.outcome !== "" ? " " + model.outcome : "")
          Accessible.description: model.outcome === ""
            ? (win.confirmRememberPattern !== ""
                ? "Press Y to approve once, A to approve and add the rule "
                  + win.confirmRememberPattern
                  + ", C to allow it for this conversation, or N to decline"
                : "Press Y to approve or N to decline")
            : "Already answered"

          // Y and N answer the focused card — the click's keyboard twin. The
          // keys carry a literal boolean to the same session.confirm call;
          // nothing here interprets words (the composer's typed "yes" goes
          // through session.text and is read in the daemon).
          Keys.onPressed: function(event) {
            if (model.outcome !== "") return
            if (event.key === Qt.Key_Y) {
              event.accepted = true
              win.answerConfirmation(true)
            } else if (event.key === Qt.Key_N) {
              event.accepted = true
              win.answerConfirmation(false)
            } else if (event.key === Qt.Key_A && win.confirmRememberPattern !== "") {
              // A for always, C for this conversation (#162). Separate keys
              // rather than a modifier on Y: a standing grant must never be
              // one slipped finger away from an approve-once, and it must be
              // reachable without a mouse — the remember control is keyboard
              // equipment like every other button on this card.
              event.accepted = true
              win.answerConfirmation(true, "always")
            } else if (event.key === Qt.Key_C && win.confirmRememberPattern !== "") {
              event.accepted = true
              win.answerConfirmation(true, "conversation")
            }
          }

          Column {
            id: cardBody
            anchors.top: parent.top
            anchors.left: parent.left
            anchors.right: parent.right
            anchors.margins: Style.space(10)
            spacing: Style.space(8)

            Text {
              text: model.text
              width: parent.width
              wrapMode: Text.Wrap
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
            }

            // The exact command, monospace, exactly as the daemon published
            // it. Never summarised or reworded here: this block is the ground
            // truth the user is approving (ADR 0014).
            Rectangle {
              width: parent.width
              height: commandText.height + Style.space(12)
              radius: Style.cornerRadius
              color: Util.alpha(Color.popups.text, 0.08)

              Text {
                id: commandText
                anchors.verticalCenter: parent.verticalCenter
                anchors.left: parent.left
                anchors.right: parent.right
                anchors.margins: Style.space(8)
                text: model.command
                wrapMode: Text.WrapAnywhere
                font.family: "monospace"
                font.pixelSize: Style.font.subtitle
                color: Color.popups.text
              }
            }

            Row {
              spacing: Style.space(8)

              Rectangle {
                id: approveButton
                enabled: model.outcome === "" && win.socketReady
                opacity: enabled ? 1.0 : 0.45
                width: approveLabel.width + Style.space(24)
                height: approveLabel.height + Style.space(10)
                radius: Style.cornerRadius
                color: Util.alpha(Color.accent, approveButton.activeFocus ? 0.35 : 0.18)
                border.color: Color.accent
                border.width: approveButton.activeFocus ? 2 : 1
                activeFocusOnTab: enabled
                Accessible.role: Accessible.Button
                Accessible.name: "Approve — run the command"
                Keys.onReturnPressed: win.answerConfirmation(true)
                Keys.onSpacePressed: win.answerConfirmation(true)
                Text {
                  id: approveLabel
                  anchors.centerIn: parent
                  text: "✓ Approve"
                  font.family: Style.font.family
                  font.pixelSize: Style.font.subtitle
                  color: Color.popups.text
                }
                MouseArea {
                  anchors.fill: parent
                  enabled: approveButton.enabled
                  onClicked: win.answerConfirmation(true)
                }
              }

              Rectangle {
                id: declineButton
                enabled: model.outcome === "" && win.socketReady
                opacity: enabled ? 1.0 : 0.45
                width: declineLabel.width + Style.space(24)
                height: declineLabel.height + Style.space(10)
                radius: Style.cornerRadius
                color: Util.alpha(Color.popups.text, declineButton.activeFocus ? 0.18 : 0.08)
                border.color: Util.alpha(Color.popups.text, 0.5)
                border.width: declineButton.activeFocus ? 2 : 1
                activeFocusOnTab: enabled
                Accessible.role: Accessible.Button
                Accessible.name: "Decline — do not run the command"
                Keys.onReturnPressed: win.answerConfirmation(false)
                Keys.onSpacePressed: win.answerConfirmation(false)
                Text {
                  id: declineLabel
                  anchors.centerIn: parent
                  text: "✗ Decline"
                  font.family: Style.font.family
                  font.pixelSize: Style.font.subtitle
                  color: Color.popups.text
                }
                MouseArea {
                  anchors.fill: parent
                  enabled: declineButton.enabled
                  onClicked: win.answerConfirmation(false)
                }
              }
            }

            // The remember row (#162). Deliberately BELOW the approve/decline
            // row and deliberately quiet: Approve-once stays the primary
            // action, and a standing grant has to read as the deliberate
            // choice it is. Text, never colour alone — the button carries the
            // exact rule it would add, verbatim, so nothing is generalised
            // behind the user's back.
            Row {
              visible: model.outcome === "" && win.confirmRememberPattern !== ""
              spacing: Style.space(8)

              Rectangle {
                id: rememberAlwaysButton
                enabled: model.outcome === "" && win.socketReady
                opacity: enabled ? 1.0 : 0.45
                width: rememberAlwaysLabel.width + Style.space(24)
                height: rememberAlwaysLabel.height + Style.space(10)
                radius: Style.cornerRadius
                color: Util.alpha(Color.popups.text,
                  rememberAlwaysButton.activeFocus ? 0.18 : 0.06)
                border.color: Util.alpha(Color.popups.text, 0.4)
                border.width: rememberAlwaysButton.activeFocus ? 2 : 1
                activeFocusOnTab: enabled
                Accessible.role: Accessible.Button
                Accessible.name: "Approve and do not ask again — adds the rule "
                  + win.confirmRememberPattern + " permanently"
                Keys.onReturnPressed: win.answerConfirmation(true, "always")
                Keys.onSpacePressed: win.answerConfirmation(true, "always")
                Text {
                  id: rememberAlwaysLabel
                  anchors.centerIn: parent
                  text: "Approve and don\u2019t ask again: " + win.confirmRememberPattern
                  font.family: Style.font.family
                  font.pixelSize: Style.font.subtitle
                  color: Color.popups.text
                }
                MouseArea {
                  anchors.fill: parent
                  enabled: rememberAlwaysButton.enabled
                  onClicked: win.answerConfirmation(true, "always")
                }
              }

              Rectangle {
                id: rememberConversationButton
                enabled: model.outcome === "" && win.socketReady
                opacity: enabled ? 1.0 : 0.45
                width: rememberConversationLabel.width + Style.space(24)
                height: rememberConversationLabel.height + Style.space(10)
                radius: Style.cornerRadius
                color: Util.alpha(Color.popups.text,
                  rememberConversationButton.activeFocus ? 0.18 : 0.06)
                border.color: Util.alpha(Color.popups.text, 0.4)
                border.width: rememberConversationButton.activeFocus ? 2 : 1
                activeFocusOnTab: enabled
                Accessible.role: Accessible.Button
                Accessible.name: "Approve for this conversation only — allows "
                  + win.confirmRememberPattern
                  + " until this conversation ends, and never saves it"
                Keys.onReturnPressed: win.answerConfirmation(true, "conversation")
                Keys.onSpacePressed: win.answerConfirmation(true, "conversation")
                Text {
                  id: rememberConversationLabel
                  anchors.centerIn: parent
                  text: "\u2026just this conversation"
                  font.family: Style.font.family
                  font.pixelSize: Style.font.subtitle
                  color: Color.popups.text
                }
                MouseArea {
                  anchors.fill: parent
                  enabled: rememberConversationButton.enabled
                  onClicked: win.answerConfirmation(true, "conversation")
                }
              }
            }

            // The scope sentence, always shown beside an offered rule: a
            // permanent grant and a conversation-scoped one look alike on a
            // button and are not alike at all.
            Text {
              visible: model.outcome === "" && win.confirmRememberPattern !== ""
              text: "Permanent unless you revoke it (jarvix approvals forget). "
                + "The conversation-only version is never written to disk."
              width: parent.width
              wrapMode: Text.Wrap
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Util.alpha(Color.popups.text, 0.7)
            }

            // …and the refusal in its place when there is no rule to offer
            // (#162's refusal matrix). One short honest sentence from the
            // daemon, shown rather than swallowed: a missing button with no
            // explanation is the thing a user works around.
            Text {
              visible: model.outcome === "" && win.confirmRememberPattern === ""
                && win.confirmRememberReason !== ""
              text: "Can\u2019t be remembered: " + win.confirmRememberReason
              width: parent.width
              wrapMode: Text.Wrap
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Util.alpha(Color.popups.text, 0.7)
            }

            // The countdown while pending, the outcome once resolved — both
            // as text, never colour alone. The seconds derive from the
            // daemon's deadline; before the clock starts (the question is
            // still being spoken) only the configured maximum can be said.
            Text {
              text: model.outcome !== "" ? model.outcome
                : win.confirmRemainingSec >= 0
                  ? win.confirmRemainingSec + "s left to answer — no answer declines"
                  : "Up to " + win.confirmTimeoutSec + "s to answer once the question is asked"
              width: parent.width
              wrapMode: Text.Wrap
              font.family: Style.font.family
              font.bold: model.outcome !== ""
              font.pixelSize: Style.font.subtitle
              color: model.outcome === "" ? Util.alpha(Color.popups.text, 0.7) : Color.popups.text
            }
          }
        }
      }
    }

    // The replay refusal cue (issue #122): the daemon's sentence for a
    // replay it would not play — the conversation is speaking, or the click
    // outran the record — shown briefly over the transcript's tail and
    // cleared by its timer. Words, not colour alone, like every state here;
    // an overlay so the transcript and composer never move for it.
    Rectangle {
      visible: win.socketReady && win.currentTab === "chat" && win.replayCue !== ""
      anchors.bottom: list.bottom
      anchors.bottomMargin: Style.space(8)
      anchors.horizontalCenter: list.horizontalCenter
      width: Math.min(list.width, replayCueText.implicitWidth + Style.space(24))
      height: replayCueText.height + Style.space(14)
      radius: Style.cornerRadius
      color: Util.alpha(Color.background, 0.92)
      border.color: Util.alpha(Color.popups.text, 0.4)
      border.width: 1
      Accessible.role: Accessible.StaticText
      Accessible.name: win.replayCue

      Text {
        id: replayCueText
        anchors.centerIn: parent
        width: Math.min(implicitWidth, list.width - Style.space(24))
        wrapMode: Text.Wrap
        text: win.replayCue
        font.family: Style.font.family
        font.pixelSize: Style.font.subtitle
        color: Color.popups.text
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
      visible: win.currentTab === "chat"
      // A daemon that is down disables the field rather than swallowing the
      // keystrokes; the panel above says why, and the label says it again
      // here, where the caret is.
      enabled: win.socketReady
      opacity: win.socketReady ? 1.0 : 0.55
      anchors.bottom: parent.bottom
      anchors.left: parent.left
      anchors.right: parent.right
      spacing: Style.space(4)

      // Label on the left, New chat on the right: the affordance that ends a
      // conversation lives where conversations happen, beside the composer.
      Item {
        width: parent.width
        height: Math.max(composerLabel.height, newChatButton.height)

        Text {
          id: composerLabel
          anchors.left: parent.left
          anchors.verticalCenter: parent.verticalCenter
          text: win.socketReady ? "Ask Jarvix" : "Ask Jarvix — start jarvixd to type"
          font.family: Style.font.family
          font.bold: true
          font.pixelSize: Style.font.subtitle
          color: Util.alpha(Color.popups.text, 0.7)
        }

        JarvixFormButton {
          id: newChatButton
          anchors.right: parent.right
          anchors.verticalCenter: parent.verticalCenter
          label: "New chat"
          name: "New chat — archive this conversation and start a fresh one"
          onClicked: win.startNewChat()
        }
      }

      // The thinking level (issue #159, ADR 0063). Quick / Balanced / Deep,
      // beside the composer because it is a decision about the question about
      // to be asked, not a preference buried in a settings screen.
      //
      // Absent entirely when the daemon reports no levels: a machine with one
      // model has no trade to offer, and a control that could not move would
      // be furniture. The current level is stated as *text* to the left of the
      // buttons as well as being marked on one of them — colour alone is not a
      // legible setting.
      Row {
        id: thinkingControl
        visible: win.thinkingLevels.length > 0
        width: parent.width
        spacing: Style.space(6)

        Text {
          id: thinkingCurrent
          anchors.verticalCenter: parent.verticalCenter
          text: "Thinking: " + (win.thinkingLabel === "" ? "—" : win.thinkingLabel)
          font.family: Style.font.family
          font.pixelSize: Style.font.subtitle
          color: Util.alpha(Color.popups.text, 0.7)
          Accessible.role: Accessible.StaticText
          Accessible.name: text
        }

        Repeater {
          model: win.thinkingLevels

          delegate: JarvixFormButton {
            required property var modelData
            label: String(modelData.label || "")
            accent: String(modelData.tier || "") === win.thinking
            // An unconfigured level is dimmed but still pressable: pressing it
            // is how the user finds out *why* it is not there, in one sentence
            // beneath the control, rather than by asking a question and
            // waiting for a worse answer than they expected.
            opacity: modelData.available ? 1.0 : 0.55
            name: String(modelData.label || "") + " — " + String(modelData.description || "") +
              (modelData.available ? "" : " (not configured on this computer)")
            onClicked: win.setThinking(String(modelData.tier || ""))
          }
        }
      }

      // Why a level could not be taken, said where the control stands. The
      // daemon's own sentence, so the click and the spoken phrase refuse in
      // the same words.
      Text {
        id: thinkingNoteText
        visible: win.thinkingNote !== ""
        width: parent.width
        wrapMode: Text.WordWrap
        text: win.thinkingNote
        font.family: Style.font.family
        font.pixelSize: Style.font.small
        color: Util.alpha(Color.popups.text, 0.75)
        Accessible.role: Accessible.StaticText
        Accessible.name: text
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

  // Escape steps back one layer at a time: record → listing, search →
  // library, any tab → Chat, Chat → closed. Returning to Chat also returns
  // the caret to the composer, so the shortcut cannot strand anyone.
  Shortcut {
    sequences: ["Escape"]
    onActivated: {
      if (win.currentTab === "library" && win.historyDetailId !== "") {
        win.historyDetailId = ""
      } else if (win.currentTab === "library" && win.searchActive) {
        // Clearing the box also clears the results (onTextChanged).
        historySearchInput.text = ""
      } else if (win.currentTab !== "chat") {
        win.openTab("chat")
        win.focusComposer()
      } else {
        win.closeWindow()
      }
    }
  }

  // Ctrl+Tab / Ctrl+Shift+Tab cycle the tabs from anywhere in the window —
  // the keyboard path that needs no journey through the strip.
  Shortcut {
    sequences: ["Ctrl+Tab"]
    onActivated: win.stepTab(1, false)
  }
  Shortcut {
    sequences: ["Ctrl+Shift+Tab"]
    onActivated: win.stepTab(-1, false)
  }
}
