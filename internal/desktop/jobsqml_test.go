package desktop

import (
	"strings"
	"testing"
)

// The client half of #221: the Jobs tab renders work still in flight and
// decides none of it.
//
// A text scan, like every other QML guard in this package. tst_jobs.qml
// executes the tab and requires that nothing appears which the daemon did not
// supply, which is the stronger check for the payloads it drives; this is the
// general form, and it covers the payloads no test drives — which is precisely
// where a well-meaning refactor reaches for a phrase of its own because the
// daemon "obviously" meant it.
//
// The stakes here are the sharpest in the window. This surface says what Jarvix
// is doing on a machine while nobody is watching, whether a piece of unattended
// work may be approved, and what it has already done. Every one of those is a
// claim the window cannot check: the state is on disk, the eligibility is the
// same offer the verb refuses with, the elapsed time is the daemon's clock, and
// the report is read back from a ledger written as each step finished. A window
// that worded any of them would be reassuring somebody about work it never
// looked at — which is #71's scar with hours of unsupervised action underneath.

// TestTheJobsTabWordsNothingItself.
func TestTheJobsTabWordsNothingItself(t *testing.T) {
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))
	tab := jobsTab(t, qml)

	// The standings internal/daemon composes. A copy here is a second place for
	// them to drift, and drift towards claiming a job is finished when it is
	// waiting — or waiting when it has stopped — is the failure that matters.
	for _, phrase := range []string{
		"is waiting on you", "has stopped and needs you", "is queued to carry on",
		"is running", "finished", "failed", "stopped",
	} {
		if strings.Contains(tab, phrase) {
			t.Errorf("the Jobs tab words a job's standing itself: %q — render `state` instead", phrase)
		}
	}
	if !strings.Contains(tab, "String(jobEntry.modelData.state") {
		t.Error("the job rows do not render the daemon's standing sentence")
	}

	// Time. The window reads this over a socket; measuring another machine's
	// clock with its own is not a formatting choice. The phrase is inside
	// `state`, which is why nothing here renders a `when` of its own.
	for _, invented := range []string{
		"minutes ago", "hours ago", "yesterday", "just now", "weeks ago",
	} {
		if strings.Contains(tab, invented) {
			t.Errorf("the Jobs tab phrases an elapsed time itself: %q — "+
				"the daemon puts it inside `state`", invented)
		}
	}

	// The state vocabulary is closed and the daemon's (internal/jobs). A tab
	// that compared against one of its words would be deciding what a job's
	// standing means, and would then have to word the answer.
	for _, keyed := range []string{
		`"parked"`, `"running"`, `"ready"`, `"done"`, `"stopped"`, `"failed"`,
		"modelData.state ===",
	} {
		if strings.Contains(tab, keyed) {
			t.Errorf("the Jobs tab reads a job's state as a value (%q); "+
				"`state` is a sentence to render, not a code to switch on", keyed)
		}
	}

	// The goal is the user's own words and travels inside a sentence the daemon
	// wrote. A tab that introduced it itself would be supplying a lead-in, and
	// a lead-in is wording (ADR 0066).
	if !strings.Contains(tab, "String(jobEntry.modelData.goal") {
		t.Error("the job rows do not render the goal in the daemon's sentence")
	}
	if !strings.Contains(tab, "String(jobEntry.modelData.scope") {
		t.Error("the job rows do not state the boundary the job is held to")
	}
	if !strings.Contains(tab, "String(jobEntry.modelData.report") {
		t.Error("a finished job's ledger-derived report is not rendered")
	}

	// The empty listing and the bounds are one promise each, worded once in the
	// daemon and rendered verbatim here — the account's rule (ADR 0066).
	if !strings.Contains(tab, "text: win.jobsEmpty") {
		t.Error("the Jobs tab words an empty listing itself")
	}
	if !strings.Contains(tab, "win.jobsDisclosure") {
		t.Error("the Jobs tab does not disclose its bounds in the daemon's sentence")
	}

	// One call site per verb, so nothing can put a second, unchecked path to a
	// job into this file.
	for verb, want := range map[string]int{
		`method: "jobs.list"`: 1,
		`"jobs.stop"`:         1,
		`"jobs.answer"`:       1,
	} {
		if got := strings.Count(qml, verb); got != want {
			t.Errorf("%s call sites = %d, want %d", verb, got, want)
		}
	}
}

// The controls are the daemon's verdict, not the window's reading of a state.
//
// This is ADR 0066's lesson carried over: eligibility is decided daemon-side
// and worded once, so a control that is withheld and an action that is refused
// cannot explain the same policy differently. The listing draws exactly the
// controls `jobs.list` offered, in the order it offered them, with the labels
// and accessible names it wrote — and a job with none has no buttons at all.
func TestTheJobsTabOffersOnlyWhatTheDaemonSaidItCould(t *testing.T) {
	tab := jobsTab(t, stripQMLComments(readPlugin(t, "JarvixWindow.qml")))

	for _, want := range []string{
		"jobEntry.controls[0].label", "jobEntry.controls[1].label", "jobEntry.controls[2].label",
		"jobEntry.controls[0].name", "jobEntry.controls[1].name", "jobEntry.controls[2].name",
	} {
		if !strings.Contains(tab, want) {
			t.Errorf("a row's control does not take %s from the daemon's list", want)
		}
	}
	// No labels of its own. "Approve", "Say no" and "Stop" are the daemon's
	// words, and the split between Approve and Send-your-answer is the parked
	// question's own — a gate approval is a yes about an action already shown,
	// a planner's question is not.
	for _, worded := range []string{`"Approve"`, `"Say no"`, `"Stop"`, `"Send your answer"`} {
		if strings.Contains(tab, worded) {
			t.Errorf("the Jobs tab labels a control itself: %q — render the "+
				"daemon's `label` and `name`", worded)
		}
	}
	// A control that is not offered is absent rather than dimmed: an empty
	// label is what JarvixCollectionRow already skips in the focus chain, so
	// this is keyboard reachability as well as appearance.
	for _, dimmed := range []string{"enabled: ", "opacity: "} {
		if strings.Contains(tab, dimmed) {
			t.Errorf("the Jobs tab dims a control (%q) instead of withholding it; "+
				"a control that cannot act must not be present to press", dimmed)
		}
	}
	// Whether a job needs typed words is the daemon's mark on the control, not
	// something worked out from the job's state.
	if !strings.Contains(tab, "win.jobWordsControl(jobEntry.controls)") {
		t.Error("the answer field is not driven by the control the daemon marked")
	}
	if !strings.Contains(tab, "String(jobEntry.wordsControl.field_label") {
		t.Error("the answer field labels itself instead of rendering the daemon's label")
	}
}

// Approving a parked gate question shows the same verbatim detail a session's
// confirmation card shows, in the same treatment (#200, #221).
//
// `detail` is JarvixCollectionRow's monospace block — the one this design
// system reserves for values that must not be reworded — because the detail is
// the exact thing being approved and a paraphrase of it would be the lie the
// confirmation card exists to prevent.
func TestTheJobsTabShowsAnApprovalsVerbatimDetail(t *testing.T) {
	tab := jobsTab(t, stripQMLComments(readPlugin(t, "JarvixWindow.qml")))

	if !strings.Contains(tab, "detail: String(jobEntry.modelData.detail") {
		t.Error("a parked approval's verbatim detail is not rendered in the row's " +
			"monospace block; the user would be approving a paraphrase")
	}
	if !strings.Contains(tab, "subtitle: String(jobEntry.modelData.question") {
		t.Error("the row does not show what the job is waiting for, verbatim")
	}
	// Nothing here reads the step, the tool or the arguments the job parked on.
	// The step is kept whole daemon-side and run as it was shown; a window that
	// named it would be re-describing an action it never judged.
	for _, reached := range []string{"modelData.tool", "modelData.step", "modelData.args"} {
		if strings.Contains(tab, reached) {
			t.Errorf("the Jobs tab reads %q — the step a job parked on is the "+
				"daemon's, kept whole, and this surface renders its detail only", reached)
		}
	}
}

// The tab reuses the window's established furniture rather than inventing a
// listing of its own, so a job reads exactly like a routine or a recorded
// action does in its own tab, and inherits their keyboard reachability and
// accessible naming rather than reimplementing them.
func TestTheJobsTabUsesTheSharedFurniture(t *testing.T) {
	tab := jobsTab(t, stripQMLComments(readPlugin(t, "JarvixWindow.qml")))

	for _, want := range []string{
		"JarvixCollectionRow", "JarvixEmptyState", "JarvixFormField",
	} {
		if !strings.Contains(tab, want) {
			t.Errorf("the Jobs tab does not use %s", want)
		}
	}
	// Reading comfort governs the transcript's message body and not listing
	// chrome (#134, and ADR 0066 records it as the existing rule). A listing
	// that reflowed a hundred rows against a setting written for a paragraph of
	// prose would be extending that rule rather than following it.
	for _, comfort := range []string{"chatLineSpacing", "chatLetterSpacing", "chatTextSize"} {
		if strings.Contains(tab, comfort) {
			t.Errorf("the Jobs tab scales with %s; the reading-comfort settings "+
				"govern the transcript body, not listing chrome", comfort)
		}
	}
}

// The listing is driven by its event rather than by a poll, so a job parking or
// finishing while the tab is open updates it without anybody touching anything
// — which is the acceptance criterion, and the whole reason a manager can leave
// the tab open and trust what it says.
func TestTheJobsListingIsDrivenByItsEvent(t *testing.T) {
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))
	at := strings.Index(qml, `case "jobs.changed":`)
	if at < 0 {
		t.Fatal("the window ignores jobs.changed and would show stale work")
	}
	arm := qml[at:]
	if end := strings.Index(arm, "break"); end >= 0 {
		arm = arm[:end]
	}
	if !strings.Contains(arm, "requestJobs()") {
		t.Errorf("the jobs.changed arm does not re-read the listing: %q", arm)
	}
	// Nothing may put the read on a clock. A Timer would turn "this is what is
	// running" into "this is what was running at most N seconds ago", which is
	// a different and weaker claim than the one this tab makes.
	for _, polled := range []string{"triggeredOnStart", "repeat: true"} {
		if strings.Contains(jobsTab(t, qml), polled) {
			t.Errorf("the Jobs tab carries %q — the listing is driven by "+
				"jobs.changed, not polled", polled)
		}
	}
	// And after an action, because where a job stands once it has been answered
	// is the store's answer: an approved job resumes from its checkpoint and may
	// park again on the very next step.
	if !strings.Contains(qml, "function handleJobActionReply") ||
		strings.Count(qml, "requestJobs()") < 4 {
		t.Error("the listing is not re-read after a stop or an answer; the window " +
			"would be showing a state it worked out for itself")
	}
}

// jobsTab slices the Jobs tab's own screen out of the window, so the scans
// above are about this surface rather than about the twelve others in the same
// file.
func jobsTab(t *testing.T, qml string) string {
	t.Helper()
	start := strings.Index(qml, "id: jobsScreen")
	if start < 0 {
		t.Fatal("JarvixWindow.qml has no Jobs tab")
	}
	end := strings.Index(qml[start:], "id: accountScreen")
	if end < 0 {
		t.Fatal("the Jobs tab has no end; the slice would cover the rest of the window")
	}
	return qml[start : start+end]
}
