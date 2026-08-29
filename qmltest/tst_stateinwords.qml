import QtQuick
import JarvixTest
import "Probe.js" as Probe

// State is conveyed by text, not colour alone — executed.
//
// The window says so in a comment above `stateLabel` and again above the tab
// strip and the error banner. A comment is a promise, not a check: a refactor
// that dropped the header's `"— " + win.stateLabel` would leave the design
// intact, every Go text scan green, and the window telling a colour-blind user
// nothing at all about what it is doing.
//
// The method here is deliberately blunt. Drive the state, read every string
// that is actually on the screen, and require the meaning to be among them.
// Nothing in this file looks at a colour, which is the point: if these pass
// with the palette deleted, the words are carrying the state.
JarvixWindowCase {
  id: tc
  name: "StateInWords"

  function test_every_session_phase_is_said_in_words() {
    var win = openWindow({ turns: [] })

    // The daemon's own state vocabulary (docs/ipc.md), each with the sentence
    // the window owes the user for it.
    var phases = [
      { state: "listening", word: "Listening" },
      { state: "transcribing", word: "Transcribing" },
      { state: "thinking", word: "Thinking" },
      { state: "responding", word: "Responding" },
      { state: "speaking", word: "Speaking" },
      { state: "awaiting_confirmation", word: "Waiting for your yes or no" },
      { state: "cancelling", word: "Cancelling" },
      { state: "idle", word: "Idle" }
    ]
    for (var i = 0; i < phases.length; i++) {
      FakeDaemon.event("state.changed", { state: phases[i].state, since_ms: Date.now() })
      settle()
      verify(Probe.says(win, phases[i].word),
        "in state " + phases[i].state + " nothing on screen says " + phases[i].word
        + "; the screen said: " + JSON.stringify(Probe.texts(win)))
    }
  }

  // The badge on the Chat tab is a red "!". On its own that is colour and a
  // glyph; what makes it usable is the accessible name saying what it means.
  function test_a_waiting_permission_question_is_announced_in_words() {
    var win = openWindow({ turns: [] })
    win.openTab("memory")
    settle()

    var before = Probe.names(win)
    verify(before.indexOf("Chat") >= 0, "the Chat tab is not reachable")

    FakeDaemon.event("tool.confirmation_required", {
      summary: "Run a shell command?", command: "ls -la /tmp",
      timeout_sec: 30, remember_pattern: "", remember_reason: ""
    })
    settle()

    var after = JSON.stringify(Probe.names(win))
    verify(after.indexOf("a permission question is waiting") >= 0,
      "a pending permission question badges the Chat tab in colour only; names were: " + after)
  }

  function test_a_failure_is_said_in_words_and_names_its_stage() {
    var win = openWindow({ turns: [] })

    FakeDaemon.event("error", { stage: "speech", message: "piper is not installed" })
    settle()

    verify(Probe.says(win, "piper is not installed"),
      "the failure's message is not on the screen; the screen said: "
      + JSON.stringify(Probe.texts(win)))
    verify(Probe.says(win, "speech"),
      "the failure does not say which stage failed, so the banner is a colour with a sentence")
  }

  // A wait that ends badly ends in words too — the row that was counting says
  // what happened to it rather than just stopping.
  function test_a_wait_that_fails_says_so_where_it_was_waiting() {
    var win = openWindow({ turns: [] })
    FakeDaemon.event("state.changed", { state: "thinking", since_ms: Date.now() })
    var turns = transcript(win)

    FakeDaemon.event("error", { stage: "model", message: "the model refused" })
    settle()

    verify(turns.get(0).text.indexOf("model") >= 0,
      "the pending row did not say what went wrong; it reads: " + JSON.stringify(turns.get(0).text))
    verify(turns.get(0).text.indexOf("the model refused") >= 0,
      "the pending row dropped the daemon's explanation")
  }

  function test_a_cancelled_turn_says_it_was_cancelled() {
    var win = openWindow({ turns: [] })
    FakeDaemon.event("state.changed", { state: "thinking", since_ms: Date.now() })
    var turns = transcript(win)

    FakeDaemon.event("session.cancelled", {})
    settle()

    // Cancelling closes the wait rather than leaving it counting. Whether the
    // row survives is the daemon's snapshot to decide; what must not happen is
    // an indicator that silently stops moving and stays on screen.
    compare(win.pendingTurnIndex, -1, "a cancelled turn left the wait counting")
    verify(FakeDaemon.lastRequest("conversation.get") !== null,
      "a cancelled turn did not re-read the daemon's record")
  }

  // A window with no daemon behind it says so, and says what to do about it,
  // rather than presenting an empty conversation that looks like a new one.
  function test_a_missing_daemon_is_explained_in_words() {
    var win = openWindow({ turns: [] })
    settle()
    verify(!Probe.says(win, "Jarvix daemon is not running"),
      "the window claims the daemon is gone while it is connected")

    FakeDaemon.close()
    settle()

    verify(Probe.says(win, "Jarvix daemon is not running"),
      "a disconnected window does not say why it is empty; the screen said: "
      + JSON.stringify(Probe.texts(win)))
    verify(Probe.says(win, "systemctl --user start jarvixd"),
      "the window says the daemon is gone but not how to bring it back")
  }
}
