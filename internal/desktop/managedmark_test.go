package desktop

import (
	"os"
	"strings"
	"testing"
)

// The managed-window mark (#197, ADR 0062) has two properties that are the
// feature's contract rather than its styling, and both live in files no Go
// test can execute. Text scans, like the overlay surface's own guards
// (windowoverlays_test.go) — deliberately narrow, watching for the specific
// regressions that would make the mark unreadable or unfindable.
func TestTheManagedMarkIsAShapeAndNotAColour(t *testing.T) {
	source, err := os.ReadFile(pluginFilePath(t, "JarvixWindowOverlays.qml"))
	if err != nil {
		t.Fatalf("reading JarvixWindowOverlays.qml: %v", err)
	}
	qml := string(source)

	// The feed's flag has to reach the surface at all. `managed` is absent
	// rather than false for an unmanaged window, so the surface reads it as a
	// truthiness test — a daemon that predates #197 draws no mark.
	if !strings.Contains(qml, "modelData.managed") {
		t.Error("JarvixWindowOverlays.qml no longer reads the row's managed flag; a window " +
			"handed to Jarvix would carry no mark (#197)")
	}

	// The three marks a chip can carry must be told apart WITHOUT colour: a
	// circle for the thread badge, a glyph for the AI-session state, a square
	// for management. The badge's own guard is its `radius: width / 2`; this
	// one is that the managed mark does not borrow it.
	if !strings.Contains(qml, "chip.managed") {
		t.Error("JarvixWindowOverlays.qml has no managed-mark element bound to the row's flag")
	}
	managed := qml[strings.Index(qml, "visible: chip.managed"):]
	if end := strings.Index(managed, "// The thread badge"); end > 0 {
		managed = managed[:end]
	}
	if strings.Contains(managed, "radius: width / 2") {
		t.Error("the managed mark is drawn as a circle; it must be a different silhouette from " +
			"the thread badge, because the two must be distinguishable without colour (#197)")
	}
	if !strings.Contains(managed, "border.color") {
		t.Error("the managed mark has no outline; its shape is the whole of its meaning")
	}
}

// The window's own admin surface: the list of managed windows and the one
// ungated verb that takes one back. Losing either would ship a grant the
// user can make by voice and cannot find or undo by hand.
func TestTheWindowListsAndReleasesManagedWindows(t *testing.T) {
	source, err := os.ReadFile(pluginFilePath(t, "JarvixWindow.qml"))
	if err != nil {
		t.Fatalf("reading JarvixWindow.qml: %v", err)
	}
	qml := string(source)
	for _, required := range []string{
		`method: "windows.managed"`,
		`method: "windows.release"`,
		"Windows Jarvix manages",
	} {
		if !strings.Contains(qml, required) {
			t.Errorf("JarvixWindow.qml no longer contains %q; a managed window would be "+
				"unfindable and unreleasable in the window (#197)", required)
		}
	}
	// There is deliberately no acquisition verb on this surface: taking a
	// window over is a grant, and a grant is made out loud on a card that
	// names the window. A one-click Manage button would be the same grant
	// with none of the naming.
	if strings.Contains(qml, `method: "windows.manage"`) {
		t.Error("JarvixWindow.qml offers a one-click acquisition; handing a window over is a " +
			"grant and must go through the confirmation that names it (ADR 0062)")
	}
}
