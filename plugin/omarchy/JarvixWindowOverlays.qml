import QtQuick
import Quickshell
import Quickshell.Io
import Quickshell.Wayland
import qs.Commons
import qs.Ui

// Tiny top-right window overlays (#127): on each window the user has
// deliberately enrolled — anchored to a focus thread, given a nickname, or
// handed over to Jarvix to manage (#197) — a small static chip carrying at
// most a managed mark, a thread badge (filled = active thread, hollow =
// inactive), an AI-session state glyph, and the nickname tag. Unenrolled
// windows carry nothing; `overlays.enabled = false` clears everything.
//
// The managed mark is a SQUARE bracket-cornered outline, and its shape is
// the whole of its meaning. The three marks a chip can carry have to be
// told apart at a glance and without colour discrimination, so each one is
// a different silhouette: the thread badge is a circle (filled or hollow),
// the AI-session state is a Nerd Font glyph, and this is a square outline
// with a solid centre — a "held" mark. It sits leftmost so it reads first,
// which is the order the question is asked in: can Jarvix act in here, and
// then what else is true of this window.
//
// Display-only in the strict ADR 0013 sense: the daemon's overlay feed
// (internal/overlay) decides which windows get a chip, which thread owns a
// shared anchor, when a fullscreen or covering window suppresses one, and
// which workspace is visible at all. This file receives finished rows —
// {x, y, width, height, tag?, badge?{thread, active}, ai_state?} — over
// overlays.get / overlays.changed (docs/ipc.md) and draws them verbatim.
//
// Surface architecture (recorded in ADR 0048): ONE full-output, fully
// click-through layer surface per monitor, with chips positioned inside it —
// not one layer surface per overlaid window. Per-window surfaces would be
// created and destroyed as windows enroll, close, and change workspace, and
// repositioned through the compositor on every move; a static panel per
// monitor never churns, and tracking a window is assigning x/y to an Item.
// The panel sits on the `top` layer (above normal windows, below the
// conversation overlay's `overlay` layer), takes no keyboard focus, reserves
// no space, and masks an EMPTY input region so every click, everywhere,
// passes through to the desktop below — overlays are readable, never
// clickable.
//
// Nothing here animates and nothing ever will: the whole point of the
// surface is calm peripheral state, and the issue's anti-goals ban timers,
// counts, and animation outright. A state change swaps a colour or a glyph
// once. The guard test in internal/desktop pins this file animation-free.
Item {
  id: root

  // --- daemon state -------------------------------------------------------
  // The finished rows, verbatim from the feed. Empty means nothing overlaid
  // — nothing enrolled, overlays disabled, a fullscreen workspace, or no
  // daemon; all four render identically as absence.
  //
  // A row's `managed` flag is absent rather than false for an unmanaged
  // window, so `!!modelData.managed` below is the whole of the reading: a
  // daemon that predates #197 sends no such field and draws no such mark.
  property var rows: []
  property bool socketReady: false

  // The chip's geometry contract with the daemon (overlay.RegionWidth /
  // RegionHeight in internal/overlay/overlay.go): occlusion is judged
  // against a 280x44 strip inside the window's top-right corner, so the
  // drawn chip must stay inside that box. The inset keeps it clear of
  // window borders and rounded corners.
  readonly property int regionWidth: 280
  readonly property int regionHeight: 44
  readonly property int chipInset: 6

  // JSON-RPC ids for this surface's requests: its own private range
  // (800-849), like the mid-screen overlay's confirm range (600-649), so a
  // reply is recognisable as ours and this file's ids stay disjoint from
  // every other surface's by construction.
  property int nextRequestId: 800
  property int pendingGetId: 0

  function requestRows() {
    if (!daemon.connected) return
    pendingGetId = nextRequestId
    nextRequestId = nextRequestId >= 849 ? 800 : nextRequestId + 1
    daemon.write(JSON.stringify({
      jsonrpc: "2.0", id: pendingGetId, method: "overlays.get"
    }) + "\n")
  }

  // --- daemon connection --------------------------------------------------
  // The surface's own socket, like the bar's and the window's: each Jarvix
  // surface is an independent client of the same event stream (ADR 0013).
  // One overlays.get on connect seeds the state — a shell attaching
  // mid-life must not wait for the next change — and overlays.changed keeps
  // it current; the daemon publishes only on change, so a quiet desktop
  // costs no traffic.
  Socket {
    id: daemon
    path: Quickshell.env("XDG_RUNTIME_DIR") + "/jarvix.sock"
    connected: true

    parser: SplitParser {
      onRead: function(line) {
        var frame
        try { frame = JSON.parse(line) } catch (e) { return }
        if (frame.method === "overlays.changed") {
          root.rows = (frame.params && frame.params.rows) || []
        } else if (frame.id === root.pendingGetId && frame.result) {
          root.pendingGetId = 0
          root.rows = frame.result.rows || []
        }
      }
    }

    onConnectionStateChanged: {
      root.socketReady = connected
      if (connected) {
        root.requestRows()
      } else {
        // Chips from a dead daemon are stale by definition: geometry moves
        // on without us, so absence is the only honest rendering.
        root.rows = []
        reconnect.start()
      }
    }
  }

  Timer {
    id: reconnect
    interval: 2000
    repeat: false
    onTriggered: { if (!daemon.connected) daemon.connected = true }
  }

  // --- per-monitor panels -------------------------------------------------
  // Row geometry is global (the compositor's layout coordinates); each
  // panel shows the rows whose top-right corner lands on its own screen and
  // positions them in screen-local coordinates by subtracting its origin.
  Variants {
    model: Quickshell.screens

    PanelWindow {
      id: panel
      required property var modelData
      screen: modelData

      readonly property var screenRows: {
        var mine = []
        var sx = panel.screen ? panel.screen.x : 0
        var sy = panel.screen ? panel.screen.y : 0
        var sw = panel.screen ? panel.screen.width : 0
        var sh = panel.screen ? panel.screen.height : 0
        for (var i = 0; i < root.rows.length; i++) {
          var row = root.rows[i]
          var cornerX = row.x + row.width - 1
          if (cornerX >= sx && cornerX < sx + sw && row.y >= sy && row.y < sy + sh)
            mine.push(row)
        }
        return mine
      }

      visible: root.socketReady && screenRows.length > 0
      anchors { top: true; bottom: true; left: true; right: true }
      color: "transparent"
      WlrLayershell.namespace: "jarvix-window-overlays"
      // The `top` layer: above every normal window (the chips must be
      // readable over the windows they annotate) and below the `overlay`
      // layer the mid-screen indicator uses. The daemon has already
      // suppressed any row a fullscreen or covering window would make a
      // lie of.
      WlrLayershell.layer: WlrLayer.Top
      WlrLayershell.keyboardFocus: WlrKeyboardFocus.None
      exclusionMode: ExclusionMode.Ignore
      // An EMPTY input region: the overlays never intercept a click,
      // anywhere, ever — there is nothing on this surface to click.
      mask: Region {}

      Repeater {
        model: panel.screenRows

        // One chip, pinned inside its window's top-right corner. Width is
        // content-sized but clamped to the daemon's occlusion region so
        // the drawn chip can never outgrow the box occlusion was judged
        // against; a long tag elides.
        Rectangle {
          id: chip
          required property var modelData

          readonly property int maxWidth: root.regionWidth - root.chipInset * 2
          readonly property bool hasBadge: !!modelData.badge
          // Whether Jarvix manages this window (#197): one it opened, or one
          // the user handed over. It is a fact about the window, not about a
          // thread or a name, so it renders on its own and a window carrying
          // nothing else still gets a chip for it.
          readonly property bool managed: !!modelData.managed
          readonly property string tag: modelData.tag || ""
          // The AI-session dot (#124/#137). The daemon admits exactly three
          // states onto the wire; the glyph/colour table below is
          // presentation, not classification (ADR 0013) — an absent or
          // unrecognised state simply renders nothing.
          readonly property string aiState: modelData.ai_state || ""
          // Nerd Font (Material Design) glyphs, written as code-point
          // escapes because a bare glyph is an unreviewable box in most
          // diff viewers (the barstatus.go convention). Each state has
          // its own shape, so the mark never depends on colour alone:
          // working is the bar's busy dots, needs-you its waiting
          // question mark, done a check.
          readonly property var aiLook: {
            switch (aiState) {
            case "working":   return { glyph: "\u{F01D8}", color: Color.accent }     // md-dots_horizontal
            case "needs_you": return { glyph: "\u{F02D7}", color: Color.urgent }     // md-help_circle
            case "done":      return { glyph: "\u{F05E0}", color: Color.foreground } // md-check_circle
            default:          return null
            }
          }

          x: modelData.x + modelData.width - (panel.screen ? panel.screen.x : 0) - width - root.chipInset
          y: modelData.y - (panel.screen ? panel.screen.y : 0) + root.chipInset
          width: Math.min(content.implicitWidth + Style.space(16), maxWidth)
          height: Math.min(content.implicitHeight + Style.space(8), root.regionHeight - root.chipInset)
          radius: Style.cornerRadius
          color: Util.alpha(Color.background, 0.88)
          border.color: Util.alpha(Color.popups.border, 0.6)
          border.width: 1

          Row {
            id: content
            anchors.centerIn: parent
            spacing: Style.space(6)

            // The managed mark: a square outline with a solid centre. Square
            // rather than round so it can never be mistaken for the thread
            // badge beside it, and outline-plus-centre rather than a plain
            // block so it stays legible against either theme. Static, like
            // everything on this surface — a window's management does not
            // change often enough to be worth a person noticing motion.
            Rectangle {
              visible: chip.managed
              width: Style.space(10)
              height: width
              radius: Math.max(1, Style.space(1))
              anchors.verticalCenter: parent.verticalCenter
              color: "transparent"
              border.color: Color.popups.text
              border.width: Math.max(1, Style.space(1))

              Rectangle {
                anchors.centerIn: parent
                width: Math.max(2, parent.width - Style.space(5))
                height: width
                color: Color.popups.text
              }
            }

            // The thread badge: filled when the thread is active, hollow
            // otherwise — a shape-and-fill difference, readable without
            // colour discrimination.
            Rectangle {
              visible: chip.hasBadge
              width: Style.space(9)
              height: width
              radius: width / 2
              anchors.verticalCenter: parent.verticalCenter
              color: chip.hasBadge && chip.modelData.badge.active ? Color.accent : "transparent"
              border.color: Color.accent
              border.width: Math.max(1, Style.space(1))
            }

            // The AI-session state, glyph and colour per the table above.
            Text {
              visible: chip.aiLook !== null
              text: chip.aiLook ? chip.aiLook.glyph : ""
              color: chip.aiLook ? chip.aiLook.color : Color.foreground
              anchors.verticalCenter: parent.verticalCenter
              font.family: Style.font.family
              font.pixelSize: Style.font.caption
            }

            // The nickname tag.
            Text {
              visible: chip.tag !== ""
              text: chip.tag
              color: Color.popups.text
              anchors.verticalCenter: parent.verticalCenter
              font.family: Style.font.family
              font.pixelSize: Style.font.caption
              elide: Text.ElideRight
              // Leave room for the marks beside it inside the clamp.
              width: Math.min(implicitWidth, chip.maxWidth - Style.space(48))
            }
          }
        }
      }
    }
  }
}
