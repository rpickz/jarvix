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
    // focus — the focus threads (#123, ADR 0041): threads with anchors,
    // parked thoughts and the live timeboxed session, self-contained in
    // JarvixFocusTab.qml (own socket, request ids 500–599, focus.list /
    // focus.changed).
    { id: "focus", label: "Focus" },
    { id: "library", label: "Library" },
    { id: "automations", label: "Automations" },
    { id: "knowledge", label: "Knowledge" },
    { id: "memory", label: "Memory" },
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
    else if (id === "automations") requestAutomations()
    else if (id === "knowledge") requestKnowledge()
    else if (id === "memory") {
      requestMemory()
      requestVocabulary()
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

  ListModel { id: turns } // { role: "user"|"assistant"|"confirmation", text, command, outcome }
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
      ? { name: "", phrases: [""], steps: [{ app: "", workspace: 1 }] }
      : { name: "", phrases: [""], path: "" }
    automationFormProblems = []
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
        var match = String(s.match || "").trim()
        if (match !== "") step.match = match
        if (s.float === true) step.float = true
        var tile = String(s.tile || "").trim()
        if (tile !== "") step.tile = tile
        if (s.size !== undefined) step.size = s.size
        if (s.position !== undefined) step.position = s.position
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
    automationFormNextFire = String(result.next_fire || "")
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

  // automationStepExtraProblems catches a step's problems on the keys the
  // form carries through without an input (size, position, tile) so they
  // still land inside the step that owns them.
  function automationStepExtraProblems(index) {
    var shown = { app: true, workspace: true, match: true, float: true }
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

  // Seconds left before auto-decline, or -1 while the daemon has not started
  // the clock (the question is still being spoken aloud). Clamped at 0: only
  // the daemon declines, so a countdown that reaches zero keeps waiting for
  // the tool.declined event rather than resolving the card itself.
  readonly property int confirmRemainingSec: confirmDeadlineMs > 0
    ? Math.max(0, Math.ceil((confirmDeadlineMs - confirmNowMs) / 1000)) : -1

  function appendConfirmationCard(summary, command, timeoutSec, deadlineMs) {
    turns.append({ role: "confirmation", text: summary, command: command, outcome: "", pos: 0 })
    pendingCardIndex = turns.count - 1
    confirmTimeoutSec = timeoutSec
    confirmDeadlineMs = deadlineMs
    confirmNowMs = Date.now()
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
  function answerConfirmation(approved) {
    if (!daemon.connected || pendingCardIndex < 0) return
    confirmRequestId = nextRequestId
    nextRequestId++
    daemon.write(JSON.stringify({
      jsonrpc: "2.0", id: confirmRequestId, method: "session.confirm",
      params: { approved: approved }
    }) + "\n")
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
  function loadSnapshot(result) {
    // A reader scrolled back must stay where they are through the rebuild;
    // the tail-follower is repositioned by the count/height handlers.
    var keepY = list.followTail ? -1 : list.contentY
    turns.clear()
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
          pos: i + 1 })
        continue
      }
      turns.append({ role: String(snapshot[i].role), text: String(snapshot[i].text),
        command: "", outcome: "", pos: i + 1 })
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
        Number(result.confirmation.deadline_ms || 0))
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
      turns.append({ role: "user", text: String(params.text || ""), command: "", outcome: "", pos: 0 })
      break
    case "assistant.delta":
      if (!assistantStreaming) {
        turns.append({ role: "assistant", text: "", command: "", outcome: "", pos: 0 })
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
        turns.append({ role: "assistant", text: full, command: "", outcome: "", pos: 0 })
      }
      assistantStreaming = false
      break
    case "tool.confirmation_required":
      // The gate asked: render the card in the conversation flow. The
      // command is the daemon's verbatim string; the deadline is unknown
      // until the daemon says the clock has started (the question may still
      // be being spoken), so the countdown starts at "up to timeout_sec".
      appendConfirmationCard(String(params.summary || ""), String(params.command || ""),
        Number(params.timeout_sec || 0), 0)
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
    case "error":
      errorStage = String(params.stage || "")
      errorMessage = String(params.message || "something went wrong")
      assistantStreaming = false
      break
    case "session.finished":
    case "session.cancelled":
      // The daemon never lets a confirmation outlive its session; a card
      // still open here means its resolution event was dropped (this window
      // was a slow client). Close it as ended rather than leaving buttons
      // that look answerable — the daemon would refuse them anyway.
      resolveConfirmationCard("Declined — the session ended")
      assistantStreaming = false
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
      requestKnowledge()
      // And it may have moved the reading-comfort typography (issue #121):
      // re-reading the settings snapshot here is what makes a change apply
      // to the open transcript live, whatever surface saved it.
      requestTypography()
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
        } else if (frame.id !== undefined && frame.id === win.memoryRequestId) {
          if (frame.result) win.loadMemory(frame.result)
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
        } else if (frame.id !== undefined && (frame.id === win.historyListRequestId ||
                   frame.id === win.historyReadRequestId ||
                   frame.id === win.historyOpenRequestId ||
                   frame.id === win.historySearchRequestId)) {
          win.handleHistoryReply(frame)
        } else if (frame.id !== undefined && frame.id === win.typographyRequestId) {
          if (frame.result) win.loadTypography(frame.result)
        } else if (frame.id !== undefined && frame.id === win.automationsRequestId) {
          if (frame.result) win.loadAutomations(frame.result)
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
        win.requestMemory()
        win.requestVocabulary()
        // The transcript's typography settings (issue #121) load with the
        // rest of the connect snapshot; until they arrive the property
        // defaults render the shipped look.
        win.requestTypography()
        if (win.currentTab === "library") win.requestHistory()
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

      JarvixEmptyState {
        visible: win.automations.length === 0 && !win.automationFormOpen
        anchors.centerIn: parent
        width: parent.width
        text: "No routines or scripts yet — the New buttons below create one."
      }

      ListView {
        id: automationsList
        visible: win.automations.length > 0 && !win.automationFormOpen
        anchors.top: parent.top
        anchors.left: parent.left
        anchors.right: parent.right
        anchors.bottom: automationsNewRow.top
        anchors.bottomMargin: Style.space(8)
        clip: true
        spacing: Style.space(10)
        model: win.automations

        delegate: JarvixCollectionRow {
          required property var modelData
          width: automationsList.width
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
          // grammar and the daemon would refuse — so the row does not offer
          // it; Enable is the way back (the Knowledge tab's rule).
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

      // The New buttons (#99), replacing #93's copyable TOML hint: creation
      // is a form now, so the footer opens one instead of handing over text
      // to paste.
      Row {
        id: automationsNewRow
        visible: !win.automationFormOpen
        anchors.bottom: parent.bottom
        anchors.left: parent.left
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
              model: (win.automationDraft.steps || []).length
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
                    placeholder: "firefox"
                    monospace: true
                    problem: win.automationProblemFor("steps[" + index + "].app")
                    Component.onCompleted: text = String((win.automationDraft.steps[index] || {}).app || "")
                    onEdited: function(value) { win.automationDraft.steps[index].app = value }
                    onCommitted: win.validateAutomationDraft()
                  }
                  JarvixFormField {
                    width: parent.width
                    label: "Workspace (1–99)"
                    problem: win.automationProblemFor("steps[" + index + "].workspace")
                    Component.onCompleted: {
                      var w = (win.automationDraft.steps[index] || {}).workspace
                      text = w === undefined ? "" : String(w)
                    }
                    onEdited: function(value) { win.automationDraft.steps[index].workspace = value.trim() }
                    onCommitted: win.validateAutomationDraft()
                  }
                  JarvixFormField {
                    width: parent.width
                    label: "Window match (empty to match on the app name)"
                    problem: win.automationProblemFor("steps[" + index + "].match")
                    Component.onCompleted: text = String((win.automationDraft.steps[index] || {}).match || "")
                    onEdited: function(value) { win.automationDraft.steps[index].match = value }
                    onCommitted: win.validateAutomationDraft()
                  }
                  JarvixFormToggle {
                    width: parent.width
                    label: "Float this window"
                    problem: win.automationProblemFor("steps[" + index + "].float")
                    checked: (win.automationDraft.steps[index] || {}).float === true
                    onToggled: function(state) {
                      win.automationDraft.steps[index].float = state
                      win.reassignAutomationDraft()
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
                  Text {
                    visible: (win.automationDraft.steps[index] || {}).size !== undefined
                      || (win.automationDraft.steps[index] || {}).position !== undefined
                      || (win.automationDraft.steps[index] || {}).tile !== undefined
                    width: parent.width
                    wrapMode: Text.Wrap
                    text: "Captured sizing (size/position/tile) is kept as it is; edit config.toml to change it."
                    font.family: Style.font.family
                    font.pixelSize: Style.font.subtitle
                    color: Util.alpha(Color.popups.text, 0.6)
                  }
                }
              }
            }
            JarvixFormButton {
              label: "Add step"
              name: "Add another step"
              onClicked: {
                if (!win.automationDraft.steps) win.automationDraft.steps = []
                win.automationDraft.steps.push({ app: "", workspace: 1 })
                win.reassignAutomationDraft()
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

    // The Memory tab (issues #92/#100): the fact store from memory.list (ADR
    // 0025) — dates, an expandable supersede trail rendered from the
    // existing `previous` data, filter-as-you-type whose matching is the
    // daemon's own query, and per-fact Forget through the gated tool path:
    // the standard confirmation card appears in Chat (the tab badge points
    // there), and this list refreshes when the daemon's events resolve it.
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
          font.family: Style.font.family
          font.pixelSize: Math.max(1, Math.round(Style.font.subtitle * win.chatTextScale))
          font.letterSpacing: messageBody.font.pixelSize * win.chatLetterSpacing
          lineHeight: win.chatLineSpacing
          lineHeightMode: Text.ProportionalHeight
          color: Color.popups.text
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
            ? "Press Y to approve or N to decline" : "Already answered"

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
