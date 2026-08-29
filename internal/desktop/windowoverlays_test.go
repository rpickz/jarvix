package desktop

import (
	"os"
	"strings"
	"testing"
)

// The window-overlay surface (#127) has two properties that are the feature's
// contract rather than its styling, and both live in the one file no Go test
// can execute: it never animates, and it never intercepts input. These are
// text scans because that is all a Go test can do to QML (the composer_test
// precedent) — deliberately narrow, watching for the specific regressions
// that would break the issue's anti-goals.
//
// Kept after the QML suite landed (#174). "No animation type appears anywhere
// in this file" cannot be asked of a running window: the headless suite
// renders in software with no frame clock a test can inspect, and a Behavior
// that only fires on a property no test touches would be invisible either
// way. The window-type ban is the same shape as the Quickshell.Wayland one.
func TestWindowOverlaysAreStaticAndClickThrough(t *testing.T) {
	source, err := os.ReadFile(pluginFilePath(t, "JarvixWindowOverlays.qml"))
	if err != nil {
		t.Fatalf("reading JarvixWindowOverlays.qml: %v", err)
	}
	qml := string(source)

	// Nothing animated, ever: no animation type, no Behavior easing a
	// property change. A state change swaps a colour or glyph once (#127's
	// explicit anti-goal — "nothing animated" is an acceptance criterion).
	for _, forbidden := range []string{"Animation", "Behavior on"} {
		if strings.Contains(qml, forbidden) {
			t.Errorf("JarvixWindowOverlays.qml contains %q; the overlay surface is static by "+
				"contract — no timers, no pulses, no eased transitions (#127)", forbidden)
		}
	}

	// Input passthrough: an empty input region, so no click anywhere on the
	// surface can be swallowed. `mask: Region {}` is the shell's established
	// click-through idiom (JarvixOverlay masks to a null item the same way).
	if !strings.Contains(qml, "mask: Region {}") {
		t.Error("JarvixWindowOverlays.qml no longer masks an empty input region; " +
			"the overlays must never intercept a click meant for the window below (#127)")
	}
	if !strings.Contains(qml, "WlrKeyboardFocus.None") {
		t.Error("JarvixWindowOverlays.qml takes keyboard focus; a passive overlay must not")
	}

	// Display-only (ADR 0013): the surface renders the daemon's feed and asks
	// for nothing but the one seed read. overlays.changed is the live source,
	// overlays.get the attach-time snapshot; anything else appearing here
	// would be logic growing in the untestable file.
	for _, required := range []string{`"overlays.changed"`, `method: "overlays.get"`} {
		if !strings.Contains(qml, required) {
			t.Errorf("JarvixWindowOverlays.qml no longer handles %s; the surface renders the "+
				"daemon's feed and must seed itself on attach (docs/ipc.md)", required)
		}
	}

	// Layer surfaces are the right window type here; a FloatingWindow would
	// be a toplevel the compositor tiles and kills (see the Wayland-import
	// guard in barstatus_test.go, which this file must keep passing).
	if strings.Contains(qml, "FloatingWindow") {
		t.Error("JarvixWindowOverlays.qml declares a FloatingWindow; overlays are layer " +
			"surfaces (PanelWindow), never toplevels")
	}
}

// The overlay surface must actually be instantiated: it lives in its own
// file, and the plugin's panel entry point (JarvixOverlay.qml) is what loads
// it. Losing the one instantiation line would ship the feature dark.
func TestOverlayPanelHostsTheWindowOverlays(t *testing.T) {
	source, err := os.ReadFile(pluginFilePath(t, "JarvixOverlay.qml"))
	if err != nil {
		t.Fatalf("reading JarvixOverlay.qml: %v", err)
	}
	if !strings.Contains(string(source), "JarvixWindowOverlays {") {
		t.Error("JarvixOverlay.qml no longer instantiates JarvixWindowOverlays; the " +
			"per-window overlays (#127) never appear without it")
	}
}

// The bar's active-thread chip (#123, deferred from PR #132) is fed by
// focus.changed — which carries the active thread's id, name, and session
// flag on every change precisely so no round trip is needed — and seeded by
// one focus.list on connect. Both halves are load-bearing: without the event
// the chip goes stale on every switch, without the seed a bar loaded
// mid-thread stays blank until the next one.
func TestBarWidgetKeepsTheActiveThreadChipFed(t *testing.T) {
	source, err := os.ReadFile(pluginFilePath(t, "JarvixBar.qml"))
	if err != nil {
		t.Fatalf("reading JarvixBar.qml: %v", err)
	}
	qml := string(source)
	for _, required := range []string{`case "focus.changed":`, `method: "focus.list"`, "activeThreadName"} {
		if !strings.Contains(qml, required) {
			t.Errorf("JarvixBar.qml no longer contains %q; the active-thread chip loses its feed", required)
		}
	}
}
