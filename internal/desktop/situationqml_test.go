package desktop

import (
	"os"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/situation"
)

// The client half of #196's central property: the window shows the situation
// report and decides none of it. Which sections exist, what order they are in,
// what each line says, whether the thing a line points at is still there and
// what can be done with it are all the daemon's answers (ADR 0013, ADR 0061) —
// so the QML must never contain the vocabulary that would let it word one
// itself.
//
// A text scan, like every other QML guard in this package: QML cannot be parsed
// by anything in this module, so a text scan is all a Go test can do to it.
func TestTheSituationTabWordsNothingItself(t *testing.T) {
	qml := situationTabSource(t)

	// The section headings are the ordering, and the ordering is the feature.
	// A copy here is a second place for it to drift, and drift in the
	// direction of a tab that renders its own order would silently undo the
	// one thing the report is for.
	for _, rank := range situation.Ordered() {
		if strings.Contains(qml, rank.Title()) {
			t.Errorf("the tab words a section heading itself: %q — render section.title instead",
				rank.Title())
		}
	}
	if !strings.Contains(qml, "modelData.title") {
		t.Error("the tab does not render the daemon's section headings")
	}
	if !strings.Contains(qml, "modelData.text") {
		t.Error("the tab does not render the daemon's line wording")
	}

	// The quiet answer and the restart admission are sentences the daemon
	// owns. A tab holding either would be a tab that could say one when the
	// daemon had not.
	for _, phrase := range []string{
		situation.QuietSentence,
		"I restarted", "I couldn't check",
	} {
		if strings.Contains(qml, phrase) {
			t.Errorf("the tab words a daemon sentence itself: %q", phrase)
		}
	}
	if !strings.Contains(qml, "report.headline") || !strings.Contains(qml, "report.caveat") {
		t.Error("the tab does not render the daemon's headline and caveat")
	}

	// Liveness is the daemon's answer too, resolved from the live stores, and
	// the tab renders `gone` and `note`. A tab that decided a thread had ended
	// would be deciding it from data it does not have.
	for _, phrase := range []string{
		"no longer pending", "no longer configured", "this thread has ended",
		"has been deleted", "no longer on disk",
	} {
		if strings.Contains(qml, phrase) {
			t.Errorf("the tab words a missing subject itself: %q", phrase)
		}
	}
	for _, want := range []string{"item.gone", "item.note"} {
		if !strings.Contains(qml, want) {
			t.Errorf("the tab does not render the daemon's liveness answer (%s)", want)
		}
	}

	// The age is pre-worded on the shared spoken scale, so the tab never does
	// clock arithmetic (ADR 0013).
	if !strings.Contains(qml, "report.age_spoken") {
		t.Error("the tab does not render the daemon's pre-worded age")
	}
	for _, arithmetic := range []string{"Date.now", "new Date", "getTime"} {
		if strings.Contains(qml, arithmetic) {
			t.Errorf("the tab does its own clock arithmetic: %q", arithmetic)
		}
	}

	// One call site per verb, in this tab. The conversation window has its own
	// pair on its own socket (provenanceqml_test.go pins those); two surfaces
	// with two sockets is the Focus tab's shape, and a SECOND pair inside one
	// surface would be a second place for the contract to drift.
	for verb, want := range map[string]int{
		`method: "situation.get"`:      1,
		`method: "provenance.resolve"`: 1,
		`method: "provenance.open"`:    1,
	} {
		if got := strings.Count(qml, verb); got != want {
			t.Errorf("%s call sites = %d, want %d", verb, got, want)
		}
	}
}

// TestTheSituationTabFollowsLinksThroughTheProvenanceNavigation is the
// acceptance criterion that a line links to the thing it describes "reusing the
// provenance navigation rather than inventing a second way to get there",
// pinned in the one file that could have invented one.
//
// Two things make it true and both are checked. The tab resolves its links
// through provenance.resolve — it never asks the daemon for names of its own —
// and it follows an action through the same tab/invoke split the conversation
// window's panel makes: a `tab` action is the window's navigation, anything
// else is provenance.open.
func TestTheSituationTabFollowsLinksThroughTheProvenanceNavigation(t *testing.T) {
	qml := situationTabSource(t)

	if !strings.Contains(qml, "params: { sources: sources }") {
		t.Error("the tab does not hand the report's sources to provenance.resolve verbatim")
	}
	// The index is the daemon's, so the tab never pairs a line with a link by
	// counting.
	if !strings.Contains(qml, "line.link") {
		t.Error("the tab does not read the daemon's per-line link index")
	}
	if !strings.Contains(qml, `String(action.tab || "")`) {
		t.Error("the tab does not make the window-navigation/daemon-action split")
	}
	if !strings.Contains(qml, "situationTab.navigate(tab,") {
		t.Error("a tab action is not handed to the window that owns the tabs")
	}
	// And the labels on the buttons are the daemon's, never invented here.
	if !strings.Contains(qml, "action.label") {
		t.Error("the tab does not render the daemon's action labels")
	}
	for _, invented := range []string{`"Show in `, `"Open the `, `"Focus that `} {
		if strings.Contains(qml, invented) {
			t.Errorf("the tab words an action label itself: %q", invented)
		}
	}
}

// TestTheSituationTabUsesTheSharedRowAndItsOwnIdRange. The rows are the shared
// collection row, so a situation line reads exactly like a fact or a feed does
// in its own tab; and the request ids stay inside the range reserved for this
// surface, so its traffic can never be mistaken for the Focus tab's or the
// window's own.
func TestTheSituationTabUsesTheSharedRowAndItsOwnIdRange(t *testing.T) {
	qml := situationTabSource(t)

	if !strings.Contains(qml, "JarvixCollectionRow") {
		t.Error("the situation tab does not use JarvixCollectionRow")
	}
	// 600-699: 500-599 is the Focus tab's, and the window allocates dynamic
	// ids from 100 upwards.
	for _, want := range []string{
		"readonly property int getRequestId: 6",
		"readonly property int resolveRequestId: 6",
		"readonly property int openRequestId: 6",
	} {
		if !strings.Contains(qml, want) {
			t.Errorf("a request id is outside the tab's reserved 600-699 range (%s)", want)
		}
	}
	// The socket only lives while the tab is shown, so a closed tab costs the
	// daemon nothing.
	if !strings.Contains(qml, "bridge.connected = false") {
		t.Error("the tab does not drop its socket when it is hidden")
	}
}

// TestTheWindowPlacesTheSituationTabAndAnswersItsNavigation. The tab is
// self-contained, so the two things the window has to do for it are the two
// only it can: place it in the strip, and perform the navigation a tab action
// asks for — through the same revealIn the conversation panel's links use.
func TestTheWindowPlacesTheSituationTabAndAnswersItsNavigation(t *testing.T) {
	raw, err := os.ReadFile(pluginFilePath(t, "JarvixWindow.qml"))
	if err != nil {
		t.Fatal(err)
	}
	qml := string(raw)
	if !strings.Contains(qml, `{ id: "situation", label: "Situation" }`) {
		t.Error("the Situation tab is not in the tab strip")
	}
	if !strings.Contains(qml, "JarvixSituationTab {") {
		t.Error("the window does not place the Situation tab")
	}
	if !strings.Contains(qml, "onNavigate: function(tab, ref) { win.revealIn(tab, ref) }") {
		t.Error("the window does not answer the tab's navigation through revealIn")
	}
	// The tab actions the daemon can produce for a situation line have to have
	// somewhere to land, so revealIn knows the automations tab too.
	if !strings.Contains(qml, `if (tab === "automations") {`) {
		t.Error("revealIn cannot reach the Automations tab, so a reminder or schedule link dead-ends")
	}
}

func situationTabSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(pluginFilePath(t, "JarvixSituationTab.qml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
