package desktop

import (
	"os"
	"strings"
	"testing"
)

// The client half of issue #162's central safety property: a surface may ask
// for a SCOPE, and may never name a rule. The daemon derives the pattern from
// the confirmation it published, so a client — or anything that can reach one
// — has no channel through which to choose what gets granted.
//
// A text scan, like every other QML guard in this package (ADR 0013 keeps the
// decisions in Go; these tests keep the QML from growing any).
func TestConfirmationSurfacesSendAScopeNeverAPattern(t *testing.T) {
	for _, name := range []string{"JarvixWindow.qml", "JarvixOverlay.qml"} {
		raw, err := os.ReadFile(pluginFilePath(t, name))
		if err != nil {
			t.Fatal(err)
		}
		qml := string(raw)

		// One session.confirm call site per surface, still — the third answer
		// went through the existing one rather than opening a second door.
		if got := strings.Count(qml, `method: "session.confirm"`); got != 1 {
			t.Errorf("%s: session.confirm call sites = %d, want exactly 1", name, got)
		}
		// The scope words appear as literals; a pattern never does.
		for _, literal := range []string{`"always"`, `params.remember = remember`} {
			if !strings.Contains(qml, literal) {
				t.Errorf("%s: expected the scope to be passed literally (%s)", name, literal)
			}
		}
		// The outgoing params object must carry nothing that could name a
		// rule. Scanned inside answerConfirmation's body only — reading
		// `params.remember_pattern` off an INCOMING event is the card
		// learning what the daemon decided, which is the opposite direction
		// and entirely fine.
		body := answerConfirmationBody(t, name, qml)
		for _, forbidden := range []string{"pattern", "rule", "command"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s: answerConfirmation mentions %q — a client must never name "+
					"the rule it is granting:\n%s", name, forbidden, body)
			}
		}
	}
}

// answerConfirmationBody extracts the function that issues session.confirm,
// so the scan above can be about what a surface SENDS rather than what it
// reads.
func answerConfirmationBody(t *testing.T, name, qml string) string {
	t.Helper()
	const marker = "function answerConfirmation("
	start := strings.Index(qml, marker)
	if start < 0 {
		t.Fatalf("%s: no answerConfirmation function", name)
	}
	rest := qml[start:]
	end := strings.Index(rest, "\n  }")
	if end < 0 {
		t.Fatalf("%s: answerConfirmation has no recognisable end", name)
	}
	// Comments are stripped: this scan is about the code, and the comment
	// above the call site necessarily says the words "pattern" and "rule" to
	// explain why neither is sent.
	var code []string
	for _, line := range strings.Split(rest[:end], "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code = append(code, line)
	}
	return strings.Join(code, "\n")
}

// The card shows the exact rule before the user commits, and shows the
// refusal when there is none. Both come off the socket; neither is derived
// here.
func TestTheCardShowsTheRuleItWouldAdd(t *testing.T) {
	raw, err := os.ReadFile(pluginFilePath(t, "JarvixWindow.qml"))
	if err != nil {
		t.Fatal(err)
	}
	qml := string(raw)
	for _, want := range []string{
		// Read off the daemon's own event and snapshot keys.
		"params.remember_pattern",
		"params.remember_reason",
		"result.confirmation.remember_pattern",
		"result.confirmation.remember_reason",
		// Rendered verbatim on the control, and as a sentence when refused.
		`text: "Approve and don\u2019t ask again: " + win.confirmRememberPattern`,
		`text: "Can\u2019t be remembered: " + win.confirmRememberReason`,
		// Both scopes reachable, and the scope stated in words.
		`win.answerConfirmation(true, "always")`,
		`win.answerConfirmation(true, "conversation")`,
		"never written to disk",
		// Approve-once keeps its plain call, so it stays the primary action
		// and the existing guards still find it.
		"win.answerConfirmation(true)",
		"win.answerConfirmation(false)",
	} {
		if !strings.Contains(qml, want) {
			t.Errorf("JarvixWindow.qml is missing %q", want)
		}
	}
}

// The Approvals tab exists, lists, and revokes — and offers no way to add.
func TestApprovalsTabListsAndRevokesOnly(t *testing.T) {
	raw, err := os.ReadFile(pluginFilePath(t, "JarvixWindow.qml"))
	if err != nil {
		t.Fatal(err)
	}
	qml := string(raw)
	for _, want := range []string{
		`{ id: "approvals", label: "Approvals" }`,
		`method: "approvals.list"`,
		`method: "approvals.forget"`,
		`case "approvals.changed":`,
	} {
		if !strings.Contains(qml, want) {
			t.Errorf("JarvixWindow.qml is missing %q", want)
		}
	}
	if strings.Contains(qml, `method: "approvals.add"`) {
		t.Error("the window offers a way to add a rule outside the confirmation card")
	}
}
