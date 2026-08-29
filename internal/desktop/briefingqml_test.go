package desktop

import (
	"os"
	"strings"
	"testing"
)

// The Focus tab hosts the return briefing's full version (#150, ADR 0050).
// This is a text scan because that is all a Go test can do to QML (see
// composer_test.go and windowkill_test.go), and it watches the pieces that
// are load-bearing rather than the layout, which is allowed to change.
//
// Kept after the QML suite landed (#174), for the same reason as the
// situation tab's guard: a running test can show that the payload it sends is
// rendered, never that the tab has no sentence of its own for the payloads it
// does not send. The banned headings are internal/briefing's.
func TestFocusTabRendersTheBriefingWithoutComposingIt(t *testing.T) {
	source, err := os.ReadFile(pluginFilePath(t, "JarvixFocusTab.qml"))
	if err != nil {
		t.Fatalf("reading JarvixFocusTab.qml: %v", err)
	}
	qml := string(source)

	for _, want := range []struct {
		fragment string
		why      string
	}{
		{`method: "briefing.get"`,
			"the tab no longer asks the daemon for the briefing"},
		{"readonly property int briefingRequestId: 510",
			"the briefing's reply id left the tab's reserved 500–599 range, where it " +
				"cannot be confused with the window's dynamic ids"},
		{"focusTab.briefing.headline",
			"the tab stopped rendering the daemon's headline, which is the only " +
				"place the opening sentence is composed (ADR 0013)"},
		{"focusTab.briefing.sections",
			"the tab stopped rendering the daemon's sections, so the ordering would " +
				"have to be invented client-side"},
		{"modelData.title",
			"the section headings are no longer taken from the payload"},
	} {
		if !strings.Contains(qml, want.fragment) {
			t.Errorf("%s (looking for %q)", want.why, want.fragment)
		}
	}

	// The category headings are daemon vocabulary (internal/briefing). A tab
	// that spells one out has started composing, which is the failure ADR
	// 0013 exists to prevent — and the one that would let the two surfaces
	// disagree about what "waiting for you" means.
	for _, heading := range []string{
		`"Waiting for you"`, `"Still going"`, `"I couldn't check"`,
		`"Nothing while you were away`,
	} {
		if strings.Contains(qml, heading) {
			t.Errorf("the tab composes %s itself; those sentences belong to internal/briefing", heading)
		}
	}
}
