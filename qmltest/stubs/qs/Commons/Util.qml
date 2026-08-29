pragma Singleton

import QtQuick

// Util — Omarchy's colour helper singleton, stubbed.
//
// One function, because the plugin uses one: `Util.alpha(colour, a)` for the
// translucent fills and focus rings. Qt.rgba is the real implementation's
// behaviour and there is nothing to fake about it, so the stub is honest by
// being the same arithmetic rather than a placeholder.
QtObject {
  function alpha(c, a) {
    return Qt.rgba(c.r, c.g, c.b, a)
  }
}
