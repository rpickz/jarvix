import QtQuick

// SplitParser — the newline splitter Quickshell hangs off a Socket. The real
// one turns a byte stream into one `read(line)` per delimiter; the fake
// daemon hands whole frames over, so all this needs to be is the signal the
// production QML attaches `onRead:` to.
QtObject {
  id: root

  property string splitMarker: "\n"

  signal read(string data)

  // feed() exists for the fake daemon rather than for the window: it is how a
  // test delivers a line without owning a socket. Splitting here rather than
  // in the bus keeps the seam in the same place the real one is — a test that
  // hands over two frames in one string must see two reads, exactly as the
  // window would over a real socket.
  function feed(chunk) {
    var parts = String(chunk).split(splitMarker)
    for (var i = 0; i < parts.length; i++) {
      if (parts[i] !== "") {
        root.read(parts[i])
      }
    }
  }
}
