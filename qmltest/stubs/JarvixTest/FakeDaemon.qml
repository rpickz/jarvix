pragma Singleton

import QtQuick
import "IpcVocabulary.js" as Vocabulary

// The fake daemon surface (issue #174).
//
// It is a bus, not a server: no socket, no process, no thread. Every stub
// Socket a surface declares registers itself here on construction, and this
// object is the only way a test reaches one. That inversion is what makes the
// harness able to drive the *real* JarvixWindow.qml — the window's socket has
// a private `id` no test could address, but it announces itself, so nothing in
// the production QML has to change to be testable.
//
// Everything is synchronous and signal-driven. `event()` delivers straight
// into the parser, which calls the window's handler on the same stack, so a
// test asserts on the next line rather than waiting: there is no queue to
// drain and no reason for a `wait()` anywhere in the suite. That is the "drive
// signals, never sleep" rule made structural rather than remembered.
//
// The vocabulary check is the point of the whole file. Both directions go
// through IpcVocabulary.js, which is generated from the Go tables in
// internal/desktop/ipcvocab.go and checked against the daemon's own Handle()
// registrations and Event publications. A test that sends an event the daemon
// never publishes, or a surface that writes a verb the daemon never
// registered, records a failure that `cleanup()` turns into a red test — so
// the suite cannot quietly become a proof about a message that does not exist.
QtObject {
  id: root

  // Every stub Socket currently alive, in construction order. Plural on
  // purpose: the window hosts one socket and three of its tabs host their
  // own, and a test that only knew about the first would miss the others'
  // traffic entirely.
  property var sockets: []

  // Every frame any surface has written since the last reset(), decoded.
  property var writes: []

  // The first contract breach, in words. Kept rather than thrown: a QML
  // exception inside a signal handler is swallowed by the engine and would
  // turn a broken test into a mysteriously passing one.
  property string failure: ""

  function fail(message) {
    if (root.failure === "") {
      root.failure = String(message)
    }
  }

  function attach(socket) {
    root.sockets.push(socket)
  }

  function detach(socket) {
    var kept = []
    for (var i = 0; i < root.sockets.length; i++) {
      if (root.sockets[i] !== socket) {
        kept.push(root.sockets[i])
      }
    }
    root.sockets = kept
  }

  // reset() clears the recorded traffic but deliberately does NOT drop the
  // sockets: they belong to the surfaces, which outlive a single test
  // function. Tests that want a fresh window create one.
  function reset() {
    root.writes = []
    root.failure = ""
    for (var i = 0; i < root.sockets.length; i++) {
      root.sockets[i].written = []
    }
  }

  // noteWrite is the outbound half of the vocabulary check, called by the stub
  // Socket for every frame a surface writes.
  // Breach messages are plain ASCII. They are the summary line of a red test
  // now, and QTest writes its log in the local 8-bit encoding — an em dash
  // came out as "?" the first time one of these fired in CI, in the middle of
  // the sentence that was supposed to explain the failure.
  function noteWrite(socket, frame) {
    if (frame.method !== undefined && !Vocabulary.knowsMethod(frame.method)) {
      root.fail("a surface sent \"" + frame.method + "\", which the fake daemon's "
        + "vocabulary does not contain, so the real server would answer -32601. "
        + "If the daemon really does register it, add it to daemonMethods in "
        + "internal/desktop/ipcvocab.go by hand and then run "
        + "`go generate ./internal/desktop` - generate only re-renders the Go "
        + "table, it does not discover new verbs. Otherwise fix the caller.")
    }
    root.writes.push(frame)
  }

  // --- driving the surfaces -------------------------------------------------

  // open() is what a real daemon coming up looks like from the window's side.
  // The window reacts by requesting its snapshot and populating every tab, so
  // a test that calls this and then reads `requests()` sees exactly the
  // conversation the real one would have started.
  function open() {
    for (var i = 0; i < root.sockets.length; i++) {
      root.sockets[i].connected = true
    }
  }

  function close() {
    for (var i = 0; i < root.sockets.length; i++) {
      root.sockets[i].connected = false
    }
  }

  // event() pushes a JSON-RPC notification to every connected surface, which
  // is what the real bus does — it fans out to all clients rather than to one.
  function event(method, params) {
    if (!Vocabulary.knowsEvent(method)) {
      root.fail("the test sent event \"" + method + "\", which the fake daemon's "
        + "vocabulary does not contain, so the daemon never publishes it. If it "
        + "really does, add it to daemonEvents in internal/desktop/ipcvocab.go "
        + "by hand and then run `go generate ./internal/desktop`. Otherwise the "
        + "test is driving a message that does not exist.")
      return
    }
    root.deliver({ jsonrpc: "2.0", method: method, params: params || {} })
  }

  // reply()/replyError() answer a request by id. Responses carry no method
  // name, so there is nothing to check against the vocabulary — the id was
  // minted by a request that already passed it.
  function reply(id, result) {
    root.deliver({ jsonrpc: "2.0", id: id, result: result === undefined ? {} : result })
  }

  function replyError(id, code, message, data) {
    var err = { code: code, message: message }
    if (data !== undefined) {
      err.data = data
    }
    root.deliver({ jsonrpc: "2.0", id: id, error: err })
  }

  function deliver(frame) {
    var line = JSON.stringify(frame)
    for (var i = 0; i < root.sockets.length; i++) {
      if (root.sockets[i].connected) {
        root.sockets[i].deliverLine(line)
      }
    }
  }

  // --- reading what the surfaces said ---------------------------------------

  function requests(method) {
    var out = []
    for (var i = 0; i < root.writes.length; i++) {
      if (root.writes[i].method === method) {
        out.push(root.writes[i])
      }
    }
    return out
  }

  function lastRequest(method) {
    var all = root.requests(method)
    return all.length === 0 ? null : all[all.length - 1]
  }

  // The window reserves id 1 for the conversation snapshot and queues every
  // event until it lands, so almost every test has to serve one first. Passing
  // an empty result is the honest "no history, idle" answer.
  function serveSnapshot(result) {
    root.reply(1, result === undefined ? { turns: [] } : result)
  }
}
