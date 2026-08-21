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
	// waiting on a confirmation; session.confirm would mean the window had
	// read the text itself and decided what "yes" means.
	for _, forbidden := range []string{"session.start", "session.confirm", "session.submit"} {
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
}
