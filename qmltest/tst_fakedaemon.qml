import QtQuick
import QtTest
import Quickshell.Io
import JarvixTest
import "stubs/JarvixTest/IpcVocabulary.js" as Vocabulary

// The fake daemon's own guard.
//
// Every other file in this directory trusts the fake to be a fair stand-in for
// jarvixd. That trust has to be earned somewhere, and this is where: the
// vocabulary is generated from internal/desktop/ipcvocab.go, checked against
// the daemon's own Handle() registrations and Event publications by
// ipcvocab_test.go, and enforced here at runtime in both directions.
//
// Without this, the failure mode is the worst kind: a suite that is green
// because it is testing a conversation neither side ever has.
TestCase {
  id: tc
  name: "FakeDaemonSurface"

  when: windowShown
  visible: true

  // A socket of the same stub type the surfaces declare, so the outbound check
  // is exercised through the same path a real surface takes.
  Socket {
    id: probe
    parser: SplitParser {
      onRead: function (line) { tc.received.push(JSON.parse(line)) }
    }
  }

  property var received: []

  function init() {
    FakeDaemon.reset()
    tc.received = []
    probe.connected = true
  }

  function cleanup() {
    probe.connected = false
  }

  function test_an_event_the_daemon_never_publishes_is_refused() {
    FakeDaemon.event("assistant.pondering", { content: "hmm" })

    verify(FakeDaemon.failure !== "",
      "the fake delivered an event the daemon has no word for; a test could then "
      + "prove a behaviour nothing in production can ever trigger")
    verify(FakeDaemon.failure.indexOf("assistant.pondering") >= 0,
      "the refusal does not name the invented event")
    compare(tc.received.length, 0, "the invented event was delivered anyway")
  }

  function test_an_event_the_daemon_does_publish_goes_through() {
    FakeDaemon.event("state.changed", { state: "thinking", since_ms: 0 })

    compare(FakeDaemon.failure, "", "a real event was refused")
    compare(tc.received.length, 1)
    compare(tc.received[0].method, "state.changed")
    compare(tc.received[0].params.state, "thinking")
  }

  function test_a_verb_the_daemon_never_registers_is_refused() {
    probe.write(JSON.stringify({ jsonrpc: "2.0", id: 7, method: "session.ponder" }) + "\n")

    verify(FakeDaemon.failure !== "",
      "a surface sent a verb the daemon does not register and the fake said nothing; "
      + "the real server would answer -32601")
    verify(FakeDaemon.failure.indexOf("session.ponder") >= 0,
      "the refusal does not name the unknown verb")
  }

  function test_a_verb_the_daemon_does_register_goes_through() {
    probe.write(JSON.stringify({
      jsonrpc: "2.0", id: 7, method: "session.text", params: { text: "hello" }
    }) + "\n")

    compare(FakeDaemon.failure, "", "a real verb was refused")
    compare(FakeDaemon.requests("session.text").length, 1)
    compare(FakeDaemon.lastRequest("session.text").params.text, "hello")
  }

  function test_a_response_carries_no_method_and_needs_no_checking() {
    FakeDaemon.reply(1, { turns: [] })

    compare(FakeDaemon.failure, "")
    compare(tc.received.length, 1)
    compare(tc.received[0].id, 1)
    compare(tc.received[0].method, undefined)
  }

  // The generated vocabulary is only useful if it is the *whole* vocabulary.
  // A spot check of both halves, so a generator that silently emitted an empty
  // table would fail here rather than making every other file's guard a no-op.
  function test_the_vocabulary_is_populated_in_both_directions() {
    verify(Vocabulary.knowsMethod("conversation.get"))
    verify(Vocabulary.knowsMethod("session.confirm"))
    verify(Vocabulary.knowsMethod("provenance.resolve"))
    verify(!Vocabulary.knowsMethod("conversation.getx"))
    verify(!Vocabulary.knowsMethod(""))

    verify(Vocabulary.knowsEvent("assistant.delta"))
    verify(Vocabulary.knowsEvent("tool.confirmation_required"))
    verify(Vocabulary.knowsEvent("error"))
    verify(!Vocabulary.knowsEvent("assistant.deltas"))
    verify(!Vocabulary.knowsEvent(""))
  }

  // A closed socket receives nothing. The fixture in JarvixWindowCase relies
  // on this to keep a window that is on its way out of one test from picking
  // up the next test's events.
  function test_a_closed_socket_hears_nothing() {
    probe.connected = false

    FakeDaemon.event("state.changed", { state: "idle", since_ms: 0 })

    compare(tc.received.length, 0, "a disconnected socket still received an event")
    compare(FakeDaemon.failure, "")
  }
}
