package desktop

import (
	"os"
	"strings"
	"testing"
)

// The thinking control (issue #159) must stay a view of daemon state. QML is
// the one place in this project that cannot be tested (ADR 0013), so nothing
// worth testing may live there — and the specific thing that must not migrate
// here is the vocabulary: the three words a person reads for the three tiers,
// and the decision about which of them is showing.
//
// This is a text scan because that is all a Go test can do to QML, and it is
// deliberately narrow: it watches for the window growing its own copy of the
// level table or its own idea of what the current level is.

func TestTheThinkingControlReadsTheDaemonRatherThanDecidingForItself(t *testing.T) {
	source, err := os.ReadFile(pluginFilePath(t, "JarvixWindow.qml"))
	if err != nil {
		t.Fatal(err)
	}
	qml := string(source)

	// The control is drawn from the daemon's own list of levels, and moved
	// through the daemon's own verb. Both halves matter: a window that
	// hard-coded three buttons would offer a level this machine cannot serve,
	// and one that set a local property would leave the spoken phrases and the
	// click disagreeing about what the level is.
	for _, want := range []string{
		`method: "thinking.get"`,
		`method: "thinking.set"`,
		"win.thinkingLevels",
		`case "thinking.changed":`,
	} {
		if !strings.Contains(qml, want) {
			t.Errorf("JarvixWindow.qml is missing %q", want)
		}
	}

	// The pending turn's tier note comes from the generated library, so the
	// separator and the labels have exactly one definition (internal/desktop
	// /pending.go). A window that built " · Deep" itself would be a second,
	// untested copy of the tier vocabulary.
	if !strings.Contains(qml, "BarState.pendingTurnTierNote(") {
		t.Error("the pending turn's tier is not read from the generated library")
	}

	// The labels themselves must not appear as QML literals. This is the
	// regression that matters: rename a level in Go and the window would go on
	// saying the old word, in a place no test looks. Comments are stripped
	// first — a comment naming the levels is documentation, and a guard that
	// could not tell the two apart would be one nobody could write around.
	code := stripQMLComments(qml)
	for _, label := range []string{`"Quick"`, `"Balanced"`, `"Deep"`} {
		if strings.Contains(code, label) {
			t.Errorf("JarvixWindow.qml spells the tier label %s itself; it must come from the daemon", label)
		}
	}

	// The current level is legible as text, not by colour alone — an
	// accessibility requirement the segmented control cannot meet on its own.
	if !strings.Contains(qml, `"Thinking: "`) {
		t.Error("the current thinking level is not stated in words beside the control")
	}
}

// The generated library carries the whole tier vocabulary, so QML never has to
// know a tier's name to render one.
func TestBarStateCarriesTheTierLabels(t *testing.T) {
	source, err := os.ReadFile(pluginFilePath(t, "BarState.js"))
	if err != nil {
		t.Fatal(err)
	}
	js := string(source)
	for _, want := range []string{"tierLabels", "pendingTurnTierNote", `"instant": "Quick"`,
		`"medium": "Balanced"`, `"deep": "Deep"`} {
		if !strings.Contains(js, want) {
			t.Errorf("BarState.js is missing %q — run `go generate ./internal/desktop`", want)
		}
	}
}

func TestPendingTurnTierNote(t *testing.T) {
	if got := PendingTurnTierNote("deep"); got != " · Deep" {
		t.Errorf("note = %q, want the label with its separator", got)
	}
	// Every turn of a configuration with no tiers: the pending line is exactly
	// the line it has always been.
	for _, tier := range []string{"", "turbo", "Deep"} {
		if got := PendingTurnTierNote(tier); got != "" {
			t.Errorf("PendingTurnTierNote(%q) = %q, want nothing", tier, got)
		}
	}
}
