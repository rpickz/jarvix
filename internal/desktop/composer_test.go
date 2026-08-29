package desktop

import (
	"os"
	"strings"
	"testing"
)

// The conversation window's composer (issue #35) must stay a text field and
// nothing more. Everything Enter could decide — start a turn, interrupt what
// is running, or answer a pending tool confirmation — is decided in the
// daemon behind `session.text` (ADR 0021), for the reason ADR 0013 gives:
// QML is the one place in this project that cannot be tested, so nothing
// worth testing may live there.
//
// This guard is a text scan because that is all a Go test can do to QML. It
// is deliberately narrow: it watches for the window growing its own session
// sequencing back, which is the specific regression that would put a second,
// untested reading of "yes" in front of the permission gate.
//
// Kept after the QML suite landed (#174). tst_keyboard.qml types a question,
// presses Enter and reads the session.text frame, which proves the happy
// path. It cannot prove the absence of a *second* yes/no vocabulary in this
// file: a window that interpreted "yes" itself would answer identically for
// every phrasing a test tries, and differently for the one it does not.
func TestConversationWindowLeavesTypedTurnDecisionsToTheDaemon(t *testing.T) {
	source, err := os.ReadFile(pluginFilePath(t, "JarvixWindow.qml"))
	if err != nil {
		t.Fatalf("reading JarvixWindow.qml: %v", err)
	}
	qml := string(source)

	if !strings.Contains(qml, `method: "session.text"`) {
		t.Error("the window no longer submits typed turns through session.text")
	}
	// session.start would interrupt a session the window cannot know is
	// waiting on a confirmation.
	for _, forbidden := range []string{"session.start", "session.submit"} {
		if strings.Contains(qml, forbidden) {
			t.Errorf("the window calls %s: typed turns go through session.text, "+
				"which makes that choice daemon-side under the session lock (ADR 0021)", forbidden)
		}
	}
	// The affirmative vocabulary lives in internal/session/confirm.go, once.
	// A copy here would be a second reading of the same word, in the one file
	// no test can exercise.
	if strings.Contains(qml, "affirmative") || strings.Contains(qml, "isAffirmative") {
		t.Error("the window appears to interpret yes/no itself; confirm.go owns that vocabulary")
	}

	// The confirmation card's buttons (issue #76) are the one legal use of
	// session.confirm: a click (or Y/N on the focused card) that carries a
	// literal boolean the user chose. That is not a second yes/no vocabulary —
	// no text is read — so the guard pins the shape rather than banning the
	// method: exactly one call site, reached only with literal booleans.
	if got := strings.Count(qml, `method: "session.confirm"`); got != 1 {
		t.Errorf("session.confirm call sites = %d, want exactly 1 (the card's "+
			"answerConfirmation function); more would scatter the gate's answer path", got)
	}
	for _, literal := range []string{"answerConfirmation(true)", "answerConfirmation(false)"} {
		if !strings.Contains(qml, literal) {
			t.Errorf("the card never calls %s: approve/decline must be literal booleans "+
				"from a click or key, never derived from text", literal)
		}
	}
	// Typed input must never feed the confirm path: the composer's text goes
	// to session.text and is interpreted daemon-side, buttons or no buttons.
	if strings.Contains(qml, "answerConfirmation(composerInput") {
		t.Error("the composer's text reaches answerConfirmation; typed answers belong to session.text")
	}
}
