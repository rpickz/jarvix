package desktop

import (
	"os"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/provenance"
)

// The client half of issue #168's central property: the window shows what
// went into an answer and decides none of it. Which sources exist, what each
// one is called, whether it is still there and what can be done with it are
// all the daemon's answers (ADR 0013) — so the QML must never contain the
// vocabulary that would let it word one itself.
//
// A text scan, like every other QML guard in this package.
//
// Kept after the QML suite landed (#174). tst_provenance.qml executes this
// panel and requires that nothing appears which the daemon did not supply,
// which is the stronger check for the payloads it drives. It cannot enumerate
// the payloads it does not drive, and these phrases are precisely the ones a
// well-meaning refactor reaches for when the daemon sent nothing. The scan is
// keyed off provenance.AvailablePhrase and friends, so it never goes stale.
func TestTheProvenancePanelWordsNothingItself(t *testing.T) {
	raw, err := os.ReadFile(pluginFilePath(t, "JarvixWindow.qml"))
	if err != nil {
		t.Fatal(err)
	}
	qml := string(raw)

	// The two strengths are the honesty line of the whole feature, and they
	// are worded once, in Go, beside the constants they describe. A copy here
	// is a second place for them to drift — and drift in the direction of
	// overstating what Jarvix knows is exactly the failure this guards.
	for _, phrase := range []string{provenance.AvailablePhrase, provenance.ReturnedPhrase} {
		if strings.Contains(qml, phrase) {
			t.Errorf("the window words a strength itself: %q — render strength_phrase instead", phrase)
		}
	}
	if !strings.Contains(qml, "modelData.strength_phrase") {
		t.Error("the panel does not render the daemon's strength wording")
	}

	// Liveness is the daemon's answer too: it looks in the live stores, and
	// the window renders `gone` and `note`. A window that decided a source
	// had vanished would be deciding it from data it does not have.
	for _, phrase := range []string{
		"has since been forgotten", "has been deleted", "no longer on disk",
		"this thread has ended", "no longer taught",
	} {
		if strings.Contains(qml, phrase) {
			t.Errorf("the window words a missing source itself: %q", phrase)
		}
	}
	if !strings.Contains(qml, "modelData.gone") || !strings.Contains(qml, "modelData.note") {
		t.Error("the panel does not render the daemon's liveness answer")
	}

	// One call site per verb. A second would be a second place for the
	// contract to drift, and for resolve in particular a second one could
	// leave a panel showing a list nobody re-checked.
	for verb, want := range map[string]int{
		`method: "provenance.resolve"`: 1,
		`method: "provenance.open"`:    1,
	} {
		if got := strings.Count(qml, verb); got != want {
			t.Errorf("%s call sites = %d, want %d", verb, got, want)
		}
	}
}

// The panel reuses the shared collection row rather than inventing a row of
// its own, so a source reads exactly like a fact or a feed does in its own
// tab — and gets the row's keyboard reachability and accessible naming for
// free rather than reimplementing them.
func TestTheProvenancePanelUsesTheSharedRow(t *testing.T) {
	raw, err := os.ReadFile(pluginFilePath(t, "JarvixWindow.qml"))
	if err != nil {
		t.Fatal(err)
	}
	qml := string(raw)
	panel := qml[strings.Index(qml, "id: provenancePanel"):]
	panel = panel[:strings.Index(panel, "// The confirmation card")]
	if !strings.Contains(panel, "JarvixCollectionRow") {
		t.Error("the provenance panel does not use JarvixCollectionRow")
	}
	// Collapsed by default and reachable from the keyboard: the control is a
	// focusable button with an accessible name, and the rows are instantiated
	// only once it is open.
	if !strings.Contains(panel, "activeFocusOnTab: true") {
		t.Error("the provenance control is not keyboard reachable")
	}
	if !strings.Contains(panel, "Keys.onReturnPressed") || !strings.Contains(panel, "Keys.onSpacePressed") {
		t.Error("the provenance control does not answer Enter and Space")
	}
	if !strings.Contains(panel, "provenancePanel.open ? win.provenanceItems : []") {
		t.Error("the panel instantiates its rows while collapsed")
	}
	// Nothing at all on a turn that consumed nothing: absence is information.
	if !strings.Contains(panel, "win.provenanceCount(model.provJson) > 0") {
		t.Error("the panel is not hidden on a turn that consumed nothing")
	}
	// Text, not colour: the strength and any note are words on the row, and
	// `flagged` only ever accompanies them (the row's own contract).
	if !strings.Contains(panel, "flagged: Boolean(modelData.gone)") {
		t.Error("a gone source is not flagged beside its words")
	}
}
