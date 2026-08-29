pragma Singleton

import QtQuick

// The `Quickshell` singleton, stubbed for the headless runner.
//
// It offers one function, because the surfaces under test use one. That is a
// rule and not an accident: a stub that answers more than it is asked grows
// its own behaviour, and the day the production QML starts calling something
// it will find a plausible answer here instead of a loud failure. Keeping the
// stub smaller than the thing it stands in for is what keeps it honest —
// anything new fails as an unknown property, which is a good failure.
QtObject {
  id: root

  // XDG_RUNTIME_DIR is the only lookup the window makes, to build its socket
  // path. The value is deliberately a directory that does not exist: the stub
  // Socket never opens anything, and a test that somehow reached a real
  // connect() must fail rather than find the developer's live daemon. That is
  // #174's "no real daemon and no sockets in the runner" enforced by
  // construction rather than by discipline.
  readonly property var environment: ({ "XDG_RUNTIME_DIR": "/nonexistent/jarvix-qmltest" })

  function env(name) {
    var value = root.environment[name]
    return value === undefined ? "" : value
  }
}
