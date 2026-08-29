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
//
// Kept after the QML suite landed (#174). tst_confirmation.qml presses each
// button and reads the frame, proving no pattern goes out on those four
// paths. This proves it on every path, including the ones no test presses:
// counting the session.confirm call sites is how "there is only one way to
// answer" stays true, and a running test cannot count call sites — it can
// only exercise the ones it knows about.
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

// The Approvals tab lists and edits both lists (#164), and — this is the part
// worth pinning — it decides nothing about either.
//
// It replaces an earlier version of this test that banned `approvals.add`
// outright. That ban was the right guard while the card was the only way to
// make a standing grant; #164 gives the view an add form on purpose, and ADR
// 0054 states why the two routes are not the same door. What survives is the
// property that mattered underneath the ban: the window never judges a pattern
// and never words a refusal. It types a rule, sends it, and shows what the
// daemon says — which is what makes "the card's refusal matrix, verbatim" true
// of the screen and not only of the Go code.
//
// Kept after the QML suite landed (#174). The banned literals are the
// daemon's refusal matrix. A running test would have to know the matrix to
// notice a copy of it, and would then be a second copy itself. Absence is the
// only form this check can take.
func TestApprovalsTabEditsBothListsAndJudgesNeither(t *testing.T) {
	raw, err := os.ReadFile(pluginFilePath(t, "JarvixWindow.qml"))
	if err != nil {
		t.Fatal(err)
	}
	qml := string(raw)
	for _, want := range []string{
		`{ id: "approvals", label: "Approvals" }`,
		`method: "approvals.list"`,
		`method: "approvals.forget"`,
		`method: "approvals.add"`,
		`case "approvals.changed":`,
		// Both lists are rendered, so a deny rule is visible as the reason
		// something still asks.
		`win.denials`,
		// The deny removal is the daemon's two-step: the first call carries
		// confirmed:false and the answer comes back as a sentence to show.
		`params: { pattern: pattern, list: "deny", confirmed: confirmed === true }`,
		`win.denyRemovalConfirmation`,
	} {
		if !strings.Contains(qml, want) {
			t.Errorf("JarvixWindow.qml is missing %q", want)
		}
	}
	// The refusal matrix lives in one place (internal/tools/approvals.go) and
	// the window must not grow a copy of it — a client-side "we know docker is
	// risky" would be a second matrix, and the two would drift on the first
	// entry somebody added to only one of them.
	for _, banned := range []string{
		"unrememberable", "riskWords", "docker run", "docker exec",
		"it runs whatever command follows", "cannot be remembered",
	} {
		if strings.Contains(qml, banned) {
			t.Errorf("JarvixWindow.qml mentions %q — the refusal matrix is the daemon's, "+
				"and a copy here is a second matrix to drift", banned)
		}
	}
	// And the sentence a refused add shows is the daemon's own, taken out of
	// the problems it returned rather than composed here.
	if !strings.Contains(qml, `approvalFormProblem = problems.length > 0`) {
		t.Error("the add form should show the daemon's refusal verbatim")
	}
}
