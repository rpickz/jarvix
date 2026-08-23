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
  //                 (ADR 0030) as one managed collection (issue #93):
  //                 automations.list for everything shown, Run through the
  //                 existing gated paths, Enable/Disable through
  //                 automations.set_enabled (the surgical config write).
  //   knowledge   — the feed cache (ADR 0031), read-only from
  //                 knowledge.status.
  //   memory      — the fact store (ADR 0025), read-only from memory.list.
  //   settings    — the settings screen (issue #9), unchanged inside its tab.
  readonly property var tabs: [
    { id: "chat", label: "Chat" },
    { id: "activity", label: "Activity" },
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
    else if (id === "memory") requestMemory()
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

  // newAutomationTOML is the copyable hint block (issue #93): creating a
  // routine or script stays a hand edit in v1 — definitions are
  // code-adjacent — so the tab hands over the exact TOML to paste into
  // config.toml instead of a form.
  readonly property string newAutomationTOML: "[[routines]]\n"
    + "name = \"morning setup\"\n"
    + "phrases = [\"morning setup\"]\n"
    + "# schedule = \"08:30 mon-fri\"  # optional: run it on a clock too\n"
    + "\n"
    + "  [[routines.steps]]\n"
    + "  app = \"firefox\"\n"
    + "  workspace = 2\n"
    + "\n"
    + "[[scripts]]\n"
    + "name = \"backup notes\"\n"
    + "phrases = [\"backup my notes\"]\n"
    + "path = \"/home/you/bin/backup-notes.sh\"\n"

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

  // newFeedTOML is the copyable hint block (issue #92): creating a feed
  // stays a hand edit in v1 — definitions are code-adjacent — so the tab
  // hands over the exact TOML to paste into config.toml instead of a form.
  readonly property string newFeedTOML: "[[knowledge.feeds]]\n"
    + "name = \"amd\"\n"
    + "description = \"AMD share price in dollars\"\n"
    + "command = [\"/home/you/bin/amd-price\"]\n"
    + "mode = \"eager\"     # or \"lazy\": fetch on first use\n"
    + "interval_sec = 300 # eager refresh cadence\n"

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
  property int memoryRequestId: 0
  property int memoryForgetRequestId: 0

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
  }

  // factMeta words one fact's dates for its row: stored, confirmed (when a
  // later turn re-confirmed it), and the length of its supersede trail.
  function factMeta(fact) {
    var meta = "stored " + String(fact.stored || "").substring(0, 10)
    var updated = String(fact.updated || "").substring(0, 10)
    if (updated !== "" && updated !== String(fact.stored || "").substring(0, 10)) {
      meta += " · confirmed " + updated
    }
    var previous = fact.previous || []
    if (previous.length > 0) {
      meta += " · " + previous.length
        + (previous.length === 1 ? " earlier version" : " earlier versions")
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
    turns.append({ role: "confirmation", text: summary, command: command, outcome: "" })
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
  function loadSnapshot(result) {
    turns.clear()
    pendingCardIndex = -1
    confirmDeadlineMs = 0
    var list = result.turns || []
    for (var i = 0; i < list.length; i++) {
      turns.append({ role: String(list[i].role), text: String(list[i].text),
        command: "", outcome: "" })
    }
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
      turns.append({ role: "user", text: String(params.text || ""), command: "", outcome: "" })
      break
    case "assistant.delta":
      if (!assistantStreaming) {
        turns.append({ role: "assistant", text: "", command: "", outcome: "" })
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
        turns.append({ role: "assistant", text: full, command: "", outcome: "" })
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
      break
    case "tool.finished":
      // The gated forget executed (from this window's button or the model's
      // own call — the store changed either way): refresh the listing.
      if (params.tool === "memory.forget") requestMemory()
      break
    case "knowledge.updated":
      // A fetch completed — scheduled or Refresh now. The event carries the
      // feed's name only; the fresh value rides the status reply.
      requestKnowledge()
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
        } else if (frame.id !== undefined && frame.id === win.activityRequestId) {
          if (frame.result) win.loadActivity(frame.result)
        } else if (frame.id !== undefined && frame.id === win.knowledgeRequestId) {
          if (frame.result) win.loadKnowledge(frame.result)
        } else if (frame.id !== undefined && (frame.id === win.knowledgeRefreshRequestId ||
                   frame.id === win.knowledgeEnableRequestId)) {
          win.handleKnowledgeActionReply(frame)
        } else if (frame.id !== undefined && frame.id === win.memoryForgetRequestId) {
          // Success needs no handling — the confirmation card's events carry
          // the flow from here — but a refusal (unknown id, memory disabled)
          // must be seen.
          if (frame.error) {
            win.errorStage = "memory"
            win.errorMessage = String(frame.error.message || "the fact could not be forgotten")
          }
        } else if (frame.id !== undefined && frame.id === win.memoryRequestId) {
          if (frame.result) win.loadMemory(frame.result)
        } else if (frame.id !== undefined && (frame.id === win.historyListRequestId ||
                   frame.id === win.historyReadRequestId ||
                   frame.id === win.historyOpenRequestId ||
                   frame.id === win.historySearchRequestId)) {
          win.handleHistoryReply(frame)
        } else if (frame.id !== undefined && frame.id === win.automationsRequestId) {
          if (frame.result) win.loadAutomations(frame.result)
        } else if (frame.id !== undefined && (frame.id === win.automationsRunRequestId ||
                   frame.id === win.automationsEnableRequestId)) {
          win.handleAutomationsActionReply(frame)
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

    // The Automations tab (issue #93): routines and scripts as one managed
    // list on the shared collection rows — kind badge and phrases in the
    // subtitle, the script's exact path in the monospace detail line (it is
    // what the script.run gate's confirmation names, ADR 0030), and a status
    // line carrying the daemon's own facts: the enabled switch, the
    // incomplete/validity markers, the schedule with its daemon-computed
    // next fire and would-refuse warning, the last observed run, and live
    // progress from the run events. Run replays the entry's phrase through
    // the existing gated path; Enable/Disable is the surgical config write;
    // a disabled row says so and loses its Run button. The footer hands over
    // the TOML for a new entry — creation stays a hand edit in v1.
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
        visible: win.automations.length === 0
        anchors.centerIn: parent
        width: parent.width
        text: "No routines or scripts yet — copy the block below into config.toml to add one."
      }

      ListView {
        id: automationsList
        visible: win.automations.length > 0
        anchors.top: parent.top
        anchors.left: parent.left
        anchors.right: parent.right
        anchors.bottom: automationsHint.top
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

      // The new-entry hint (issue #93): a collapsed block that unfolds into
      // the exact TOML to copy — the Knowledge tab's pattern, because
      // routine and script definitions are code-adjacent and stay
      // hand-edited in v1.
      Column {
        id: automationsHint
        anchors.bottom: parent.bottom
        anchors.left: parent.left
        anchors.right: parent.right
        spacing: Style.space(6)

        Rectangle {
          id: automationsHintToggle
          width: automationsHintToggleLabel.width + Style.space(20)
          height: automationsHintToggleLabel.height + Style.space(8)
          radius: Style.cornerRadius
          color: Util.alpha(Color.popups.text, automationsHintToggle.activeFocus ? 0.16 : 0.06)
          border.color: automationsHintToggle.activeFocus
            ? Color.accent : Util.alpha(Color.popups.text, 0.4)
          border.width: automationsHintToggle.activeFocus ? 2 : 1
          activeFocusOnTab: true
          property bool open: false
          Accessible.role: Accessible.Button
          Accessible.name: open ? "Hide the new-automation TOML"
            : "Show the TOML for adding a routine or script"
          Keys.onReturnPressed: automationsHintToggle.open = !automationsHintToggle.open
          Keys.onSpacePressed: automationsHintToggle.open = !automationsHintToggle.open
          Text {
            id: automationsHintToggleLabel
            anchors.centerIn: parent
            text: automationsHintToggle.open ? "Hide the TOML" : "Add a routine or script…"
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Color.popups.text
          }
          MouseArea {
            anchors.fill: parent
            onClicked: automationsHintToggle.open = !automationsHintToggle.open
          }
        }

        Rectangle {
          visible: automationsHintToggle.open
          width: parent.width
          height: automationsHintBody.height + Style.space(20)
          radius: Style.cornerRadius
          color: Util.alpha(Color.popups.text, 0.06)

          Column {
            id: automationsHintBody
            anchors.top: parent.top
            anchors.left: parent.left
            anchors.right: parent.right
            anchors.margins: Style.space(10)
            spacing: Style.space(6)

            Text {
              width: parent.width
              wrapMode: Text.Wrap
              text: "Copy what you need into config.toml — a [[routines]] table, a "
                + "[[scripts]] table, or both — then reload; the new row appears here."
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Util.alpha(Color.popups.text, 0.7)
            }
            TextEdit {
              id: automationsHintTOML
              width: parent.width
              readOnly: true
              selectByMouse: true
              wrapMode: TextEdit.WrapAnywhere
              text: win.newAutomationTOML
              font.family: "monospace"
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
              selectionColor: Util.alpha(Color.accent, 0.4)
              Accessible.role: Accessible.StaticText
              Accessible.name: "The TOML block for a new routine or script"
            }
            Rectangle {
              id: automationsHintCopy
              width: automationsHintCopyLabel.width + Style.space(20)
              height: automationsHintCopyLabel.height + Style.space(8)
              radius: Style.cornerRadius
              color: Util.alpha(Color.accent, automationsHintCopy.activeFocus ? 0.35 : 0.18)
              border.color: Color.accent
              border.width: automationsHintCopy.activeFocus ? 2 : 1
              activeFocusOnTab: true
              Accessible.role: Accessible.Button
              Accessible.name: "Copy the TOML block"
              function copyTOML() {
                automationsHintTOML.selectAll()
                automationsHintTOML.copy()
                automationsHintTOML.deselect()
              }
              Keys.onReturnPressed: automationsHintCopy.copyTOML()
              Keys.onSpacePressed: automationsHintCopy.copyTOML()
              Text {
                id: automationsHintCopyLabel
                anchors.centerIn: parent
                text: "Copy"
                font.family: Style.font.family
                font.pixelSize: Style.font.subtitle
                color: Color.popups.text
              }
              MouseArea { anchors.fill: parent; onClicked: automationsHintCopy.copyTOML() }
            }
          }
        }
      }
    }

    // The Knowledge tab (issue #92): the feed cache as cards — name, mode
    // and cadence, the current value (or "not fetched yet") with its
    // spoken-style age, STALE marked in words, failing-since with the error —
    // and the two operations: Refresh now (knowledge.refresh_now, through
    // the daemon's scheduled-fetch path) and Enable/Disable
    // (knowledge.set_enabled, the surgical config write). A disabled feed's
    // card says so and keeps its last value. The footer hands over the TOML
    // for a new feed — creation stays a hand edit in v1.
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
        visible: win.knowledgeFeeds.length === 0
        anchors.centerIn: parent
        width: parent.width
        text: win.knowledgeEnabled
          ? "No knowledge feeds yet — copy the block below into config.toml to add one."
          : "Knowledge feeds are switched off (knowledge.enabled = false)."
      }

      ListView {
        id: knowledgeList
        visible: win.knowledgeFeeds.length > 0
        anchors.top: parent.top
        anchors.left: parent.left
        anchors.right: parent.right
        anchors.bottom: knowledgeHint.top
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

      // The new-feed hint (issue #92): a collapsed block that unfolds into
      // the exact TOML to copy. Selectable text plus a Copy button — the
      // window offers the block; the paste is the user's, in their editor,
      // because feed definitions are code-adjacent and stay hand-edited.
      Column {
        id: knowledgeHint
        anchors.bottom: parent.bottom
        anchors.left: parent.left
        anchors.right: parent.right
        spacing: Style.space(6)

        Rectangle {
          id: knowledgeHintToggle
          width: knowledgeHintToggleLabel.width + Style.space(20)
          height: knowledgeHintToggleLabel.height + Style.space(8)
          radius: Style.cornerRadius
          color: Util.alpha(Color.popups.text, knowledgeHintToggle.activeFocus ? 0.16 : 0.06)
          border.color: knowledgeHintToggle.activeFocus
            ? Color.accent : Util.alpha(Color.popups.text, 0.4)
          border.width: knowledgeHintToggle.activeFocus ? 2 : 1
          activeFocusOnTab: true
          property bool open: false
          Accessible.role: Accessible.Button
          Accessible.name: open ? "Hide the new-feed TOML" : "Show the TOML for adding a feed"
          Keys.onReturnPressed: knowledgeHintToggle.open = !knowledgeHintToggle.open
          Keys.onSpacePressed: knowledgeHintToggle.open = !knowledgeHintToggle.open
          Text {
            id: knowledgeHintToggleLabel
            anchors.centerIn: parent
            text: knowledgeHintToggle.open ? "Hide the TOML" : "Add a feed…"
            font.family: Style.font.family
            font.pixelSize: Style.font.subtitle
            color: Color.popups.text
          }
          MouseArea { anchors.fill: parent; onClicked: knowledgeHintToggle.open = !knowledgeHintToggle.open }
        }

        Rectangle {
          visible: knowledgeHintToggle.open
          width: parent.width
          height: knowledgeHintBody.height + Style.space(20)
          radius: Style.cornerRadius
          color: Util.alpha(Color.popups.text, 0.06)

          Column {
            id: knowledgeHintBody
            anchors.top: parent.top
            anchors.left: parent.left
            anchors.right: parent.right
            anchors.margins: Style.space(10)
            spacing: Style.space(6)

            Text {
              width: parent.width
              wrapMode: Text.Wrap
              text: "Copy this into config.toml, point the command at something that "
                + "prints the value, then reload — the new card appears here."
              font.family: Style.font.family
              font.pixelSize: Style.font.subtitle
              color: Util.alpha(Color.popups.text, 0.7)
            }
            TextEdit {
              id: knowledgeHintTOML
              width: parent.width
              readOnly: true
              selectByMouse: true
              wrapMode: TextEdit.WrapAnywhere
              text: win.newFeedTOML
              font.family: "monospace"
              font.pixelSize: Style.font.subtitle
              color: Color.popups.text
              selectionColor: Util.alpha(Color.accent, 0.4)
              Accessible.role: Accessible.StaticText
              Accessible.name: "The TOML block for a new feed"
            }
            Rectangle {
              id: knowledgeHintCopy
              width: knowledgeHintCopyLabel.width + Style.space(20)
              height: knowledgeHintCopyLabel.height + Style.space(8)
              radius: Style.cornerRadius
              color: Util.alpha(Color.accent, knowledgeHintCopy.activeFocus ? 0.35 : 0.18)
              border.color: Color.accent
              border.width: knowledgeHintCopy.activeFocus ? 2 : 1
              activeFocusOnTab: true
              Accessible.role: Accessible.Button
              Accessible.name: "Copy the TOML block"
              function copyTOML() {
                knowledgeHintTOML.selectAll()
                knowledgeHintTOML.copy()
                knowledgeHintTOML.deselect()
              }
              Keys.onReturnPressed: knowledgeHintCopy.copyTOML()
              Keys.onSpacePressed: knowledgeHintCopy.copyTOML()
              Text {
                id: knowledgeHintCopyLabel
                anchors.centerIn: parent
                text: "Copy"
                font.family: Style.font.family
                font.pixelSize: Style.font.subtitle
                color: Color.popups.text
              }
              MouseArea { anchors.fill: parent; onClicked: knowledgeHintCopy.copyTOML() }
            }
          }
        }
      }
    }

    // The Memory tab (issue #92): the fact store from memory.list (ADR 0025)
    // — dates, an expandable supersede trail rendered from the existing
    // `previous` data, filter-as-you-type whose matching is the daemon's own
    // query, and per-fact Forget through the gated tool path: the standard
    // confirmation card appears in Chat (the tab badge points there), and
    // this list refreshes when the daemon's events resolve it.
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
        visible: win.memoryEnabled && (win.memoryFacts.length > 0 || win.memoryQuery !== "")
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

      JarvixEmptyState {
        visible: win.memoryFacts.length === 0
        anchors.centerIn: parent
        width: parent.width
        text: !win.memoryEnabled
          ? "Memory is switched off (memory.enabled = false)."
          : win.memoryQuery.trim() !== ""
            ? "No remembered fact matches “" + win.memoryQuery + "” — clear the box to see everything."
            : "Nothing remembered yet — say “remember …” and it will be kept here."
      }

      Text {
        id: memoryCountLine
        visible: win.memoryFacts.length > 0
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

      ListView {
        id: memoryList
        visible: win.memoryFacts.length > 0
        anchors.top: memoryCountLine.bottom
        anchors.topMargin: Style.space(8)
        anchors.left: parent.left
        anchors.right: parent.right
        anchors.bottom: parent.bottom
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
            actionLabel: "Forget"
            actionName: "Forget: " + factDelegate.modelData.content
            onActionTriggered: win.forgetFact(String(factDelegate.modelData.id))
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

        Text {
          text: model.role === "user" ? "You"
            : model.role === "confirmation" ? "Jarvix asks permission" : "Jarvix"
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
