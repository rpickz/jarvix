import QtQuick
import QtTest
import JarvixTest
import "Probe.js" as Probe

// The pending assistant turn (issue #158), executed rather than grepped.
//
// The Go guard next door (pending_test.go) proves the *wording* comes from the
// generated library. It cannot prove the row behaves: that the placeholder
// appears when the daemon starts working, that the first delta takes over the
// same row instead of adding a second bubble beneath it, and that a fast
// answer never makes it flash. Those are claims about a ListModel over time,
// and only running the window can settle them.
JarvixWindowCase {
  id: tc
  name: "PendingTurn"

  // SignalSpy on the transcript model's count. A pending row that is removed
  // and re-appended leaves the count where it started, so counting rows before
  // and after cannot see the flash — only the transitions can. The spy's
  // target is assigned at runtime because the model is found, not declared.
  SignalSpy {
    id: rows
    signalName: "countChanged"
  }

  function test_the_wait_appears_when_the_daemon_starts_working() {
    var win = openWindow({ turns: [] })

    FakeDaemon.event("state.changed", { state: "thinking", since_ms: Date.now() })

    var turns = transcript(win)
    compare(turns.count, 1, "a non-idle state must open exactly one pending row")
    compare(turns.get(0).role, "assistant")
    compare(turns.get(0).pending, true)
    compare(win.pendingTurnIndex, 0, "the pending row is always the last one")
    // The words are the daemon's, compiled from internal/desktop/pending.go.
    // Asserting they are non-empty rather than asserting the string keeps the
    // ownership where pending_test.go already pins it: this test is about the
    // row existing, not about what it says.
    verify(turns.get(0).text !== "", "the wait must say what it is waiting for")

    settle()
    verify(Probe.says(win, turns.get(0).text), "the pending row is not on the screen")
  }

  function test_the_first_delta_adopts_the_wait_in_place() {
    var win = openWindow({ turns: [] })
    FakeDaemon.event("state.changed", { state: "thinking", since_ms: Date.now() })
    var turns = transcript(win)
    compare(turns.count, 1)

    rows.target = turns
    rows.clear()

    FakeDaemon.event("assistant.delta", { content: "Recursion is " })

    // The whole no-double-bubble mechanism, in three assertions: one row, not
    // two; no insertion or removal at all, so the user's eye never sees the
    // placeholder leave; and the row that was the wait is now the answer.
    compare(turns.count, 1, "the first delta appended a second bubble under the wait")
    compare(rows.count, 0,
      "the row count moved, so the placeholder was taken away and put back — that is the flash")
    compare(turns.get(0).pending, false, "the adopted row is still marked as a wait")
    compare(turns.get(0).text, "Recursion is ")
    compare(win.pendingTurnIndex, -1, "the wait is over; nothing should still be counting")

    FakeDaemon.event("assistant.delta", { content: "when a thing calls itself." })
    compare(turns.count, 1, "a later delta must stream into the same row")
    compare(turns.get(0).text, "Recursion is when a thing calls itself.")
  }

  function test_a_fast_answer_never_flashes_a_placeholder() {
    var win = openWindow({ turns: [] })

    // A first cycle, purely to find the model — it has no rows to identify
    // itself by until something lands in it. Returning to idle closes the wait
    // and leaves the transcript empty again, with no snapshot round trip.
    FakeDaemon.event("state.changed", { state: "thinking", since_ms: Date.now() })
    var turns = transcript(win)
    FakeDaemon.event("state.changed", { state: "idle", since_ms: Date.now() })
    compare(turns.count, 0, "returning to idle must close the wait")

    rows.target = turns
    rows.clear()

    // Now the scenario that matters: the answer begins in the same breath as
    // the thinking state, the way a cached or very short reply does.
    FakeDaemon.event("state.changed", { state: "thinking", since_ms: Date.now() })
    FakeDaemon.event("assistant.delta", { content: "Yes." })
    FakeDaemon.event("assistant.finished", { content: "Yes." })

    compare(turns.count, 1, "the fast answer left more than one row behind")
    compare(rows.count, 1,
      "the transcript grew and shrank more than once: the placeholder flashed")
    compare(turns.get(0).text, "Yes.")
    compare(turns.get(0).pending, false)
  }

  function test_a_final_answer_with_no_deltas_adopts_the_wait_too() {
    var win = openWindow({ turns: [] })
    FakeDaemon.event("state.changed", { state: "thinking", since_ms: Date.now() })
    var turns = transcript(win)
    rows.target = turns
    rows.clear()

    // A window the bus dropped while it was slow sees no deltas at all. The
    // final text has to adopt the row exactly as a delta would, or the same
    // double bubble appears for the slowest clients — the ones least able to
    // afford a confusing transcript.
    FakeDaemon.event("assistant.finished", { content: "The whole answer." })

    compare(turns.count, 1, "a delta-less answer appended a second bubble")
    compare(rows.count, 0, "the placeholder flashed on the delta-less path")
    compare(turns.get(0).text, "The whole answer.")
    compare(turns.get(0).pending, false)
  }

  function test_the_users_words_land_above_the_wait() {
    var win = openWindow({ turns: [] })
    FakeDaemon.event("state.changed", { state: "listening", since_ms: Date.now() })
    var turns = transcript(win)
    compare(turns.count, 1)

    FakeDaemon.event("transcript.final", { text: "what is recursion" })

    // Two rows, in the order they were said: the user's turn, then the wait
    // still underneath it. A transcript that put the placeholder above the
    // sentence that caused it would read backwards.
    compare(turns.count, 2)
    compare(turns.get(0).role, "user")
    compare(turns.get(0).text, "what is recursion")
    compare(turns.get(1).pending, true)
    compare(win.pendingTurnIndex, 1, "the wait must still be the last row")
  }

  function test_the_wait_ends_when_the_daemon_goes() {
    var win = openWindow({ turns: [] })
    FakeDaemon.event("state.changed", { state: "thinking", since_ms: Date.now() })
    var turns = transcript(win)
    compare(turns.count, 1)

    FakeDaemon.close()

    // An indicator that keeps counting against a daemon that is not there is
    // the one thing this feature must never do: it turns "gone" into "slow".
    compare(turns.count, 0, "the wait outlived the connection that reported it")
    compare(win.pendingTurnIndex, -1)
    compare(win.pendingElapsedSec, 0)
  }
}
