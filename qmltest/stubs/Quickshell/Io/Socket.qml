import QtQuick
import JarvixTest

// Socket — the stub that stands in for Quickshell's Unix-socket client.
//
// It opens nothing. `connected` is a plain property the test drives, exactly
// as the production QML drives it (`daemon.connected = true`), and every byte
// written goes to the FakeDaemon bus instead of a file descriptor. That is
// the NFR from #174 made structural: there is no code path here that could
// reach a real daemon even if XDG_RUNTIME_DIR pointed at one.
//
// Registration is by construction rather than by wiring: the surfaces under
// test declare their sockets with private `id`s inside a window a test cannot
// reach into, so the socket announces itself to the bus on completion and the
// test addresses it through the bus. That also means a surface that grows a
// second socket is visible to the test rather than silently ignored.
QtObject {
  id: root

  property string path: ""

  // Quickshell exposes the connection as a settable bool and emits its own
  // signal name for the change — not the auto-generated `connectedChanged` —
  // because the real property transitions through connecting. The production
  // QML attaches `onConnectionStateChanged`, so the stub must emit that name
  // or the window's whole connect path (request the snapshot, populate every
  // tab) would never run and the tests would be exercising an empty shell.
  property bool connected: false
  signal connectionStateChanged()
  onConnectedChanged: root.connectionStateChanged()

  property var parser: null

  // Every frame the surface wrote, as a decoded object. Kept whole rather
  // than as strings so a test asserts on the payload the daemon would parse,
  // which is the thing that matters — "sends a scope word, never a pattern"
  // is a claim about params, not about the bytes' formatting.
  property var written: []

  function write(data) {
    var line = String(data)
    var frame = null
    try {
      frame = JSON.parse(line)
    } catch (e) {
      // A surface that writes something that is not JSON has broken the wire
      // contract; failing here names the frame rather than letting the test
      // fail later with an empty `written` list.
      FakeDaemon.fail("a surface wrote a line that is not JSON: " + line)
      return
    }
    FakeDaemon.noteWrite(root, frame)
    root.written.push(frame)
    root.writtenChanged()
  }

  // deliver pushes one daemon line into this socket's parser, the way the
  // real Socket would when bytes arrive. Tests go through FakeDaemon rather
  // than calling this, so the vocabulary check cannot be skipped.
  function deliverLine(line) {
    if (root.parser === null) {
      FakeDaemon.fail("a line was delivered to a socket with no parser: " + line)
      return
    }
    root.parser.feed(line + "\n")
  }

  Component.onCompleted: FakeDaemon.attach(root)
  Component.onDestruction: FakeDaemon.detach(root)
}
