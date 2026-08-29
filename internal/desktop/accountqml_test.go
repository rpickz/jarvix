package desktop

import (
	"strings"
	"testing"
)

// The client half of #210: the Account tab renders the record of what Jarvix
// changed and decides none of it.
//
// A text scan, like every other QML guard in this package. tst_account.qml
// executes the tab and requires that nothing appears which the daemon did not
// supply, which is the stronger check for the payloads it drives; this is the
// general form, and it covers the payloads no test drives — which is precisely
// where a well-meaning refactor reaches for a phrase of its own because the
// daemon "obviously" meant it.
//
// The stakes are not cosmetic. This surface says whether something can be put
// back, when it happened, and whether it already was. Every one of those is a
// claim about the machine that the window cannot check: the eligibility is the
// permission gate's, the elapsed time is measured on the daemon's clock, and
// the reversal is the account's own record. A window that worded any of them
// would be reassuring somebody about work it never looked at.

// TestTheAccountTabWordsNothingItself.
func TestTheAccountTabWordsNothingItself(t *testing.T) {
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))

	// The three sentences internal/daemon composes for a row's standing. A
	// copy here is a second place for them to drift, and drift towards
	// claiming a reversal happened is the failure that matters.
	for _, phrase := range []string{
		"I can put this back", "I can't put this back", "I put this back",
		"already put that back", "turned off",
	} {
		if strings.Contains(qml, phrase) {
			t.Errorf("the window words a row's standing itself: %q — render `state` instead", phrase)
		}
	}
	if !strings.Contains(qml, "String(accountEntry.modelData.state") {
		t.Error("the account rows do not render the daemon's standing sentence")
	}

	// Time. The window reads the account over a socket; measuring another
	// machine's clock with its own is not a formatting choice, it is an
	// arithmetic nobody asked for. `when` arrives as a phrase.
	for _, invented := range []string{
		"minutes ago", "hours ago", "yesterday", "just now", "weeks ago",
	} {
		if strings.Contains(qml, invented) {
			t.Errorf("the window phrases an elapsed time itself: %q — render `when` instead", invented)
		}
	}
	if !strings.Contains(qml, `parts.push(String(a.when))`) {
		t.Error("the account rows do not render the daemon's own phrase for when something happened")
	}

	// The bound and the empty account are one promise each, worded once, in
	// the daemon (ADR 0064). Rendered verbatim here.
	if !strings.Contains(qml, "win.accountDisclosure") {
		t.Error("the Account tab does not disclose the bound in the daemon's sentence")
	}
	if !strings.Contains(qml, "text: win.accountEmpty") {
		t.Error("the Account tab words an empty account itself")
	}

	// One call site per verb, so nothing can put a second, unchecked path to
	// the account or to a reversal into this file.
	for verb, want := range map[string]int{
		`method: "undo.list"`:  1,
		`method: "undo.apply"`: 2, // by id, and by job
	} {
		if got := strings.Count(qml, verb); got != want {
			t.Errorf("%s call sites = %d, want %d", verb, got, want)
		}
	}
}

// The offer is the daemon's verdict, not the window's reading of the record.
//
// `reversible` says the action left something behind that would restore it;
// `can_undo` says the offer stands right now, which the permission gate can
// withhold from a record that is perfectly reversible. A window that drew its
// button from the first would offer a control that refuses when pressed — the
// dead affordance ADR 0055 already refused once, for provenance, and the same
// rule applies to a reversal.
func TestTheAccountOffersOnlyWhatTheDaemonSaidItCould(t *testing.T) {
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))
	tab := accountTab(t, qml)

	if !strings.Contains(tab, "accountEntry.modelData.can_undo === true ?") {
		t.Error("the row's control is not gated on the daemon's can_undo verdict")
	}
	if strings.Contains(tab, "modelData.reversible") {
		t.Error("the Account tab reads `reversible` — that is the record's property, " +
			"not the offer; the gate can withhold a reversal of a reversible record")
	}
	// A row that cannot go back has no button at all rather than a disabled
	// one: an empty actionLabel is what JarvixCollectionRow already skips in
	// the focus chain, so this is reachability as well as appearance. Nothing
	// here may reach for `enabled` or `opacity` to make a dead control merely
	// look dead.
	if !strings.Contains(tab, `accountEntry.modelData.can_undo === true ? "Put it back" : ""`) {
		t.Error("the row does not withhold its label outright when the offer does not stand")
	}
	for _, dimmed := range []string{"enabled: ", "opacity: "} {
		if strings.Contains(tab, dimmed) {
			t.Errorf("the Account tab dims a control (%q) instead of withholding it; "+
				"a control that cannot act must not be present to press", dimmed)
		}
	}
	// The same rule one level up, for the whole-job control.
	if !strings.Contains(tab, "accountGroup.modelData.can_undo === true") {
		t.Error("the whole-job control is not gated on the daemon's verdict")
	}
	if !strings.Contains(tab, "accountGroup.modelData.why") {
		t.Error("a job that cannot be put back does not say why in the daemon's words")
	}
}

// The tab reuses the window's established furniture rather than inventing a
// listing of its own: the shared collection row, the shared button, and the
// shared empty state. A new surface with its own layout is how a window stops
// reading like one window.
func TestTheAccountTabUsesTheSharedFurniture(t *testing.T) {
	tab := accountTab(t, stripQMLComments(readPlugin(t, "JarvixWindow.qml")))

	for _, want := range []string{
		"JarvixCollectionRow", "JarvixFormButton", "JarvixEmptyState",
	} {
		if !strings.Contains(tab, want) {
			t.Errorf("the Account tab does not use %s", want)
		}
	}
	// Provenance clicks through on the verbs the answer panel already uses,
	// and the asking happens in one shared function — the tab never writes a
	// provenance verb of its own.
	if !strings.Contains(tab, "win.toggleActionProvenance") {
		t.Error("an action's sources are not reachable from the account")
	}
	if strings.Contains(tab, "provenance.resolve") || strings.Contains(tab, "provenance.open") {
		t.Error("the Account tab asks about provenance directly instead of through " +
			"the window's single call site")
	}
	// The arrangement is the daemon's. A tab that read `job` off a row and
	// worked out where the headings go would be deciding what grouping means.
	if !strings.Contains(tab, "model: win.accountGroups") {
		t.Error("the Account tab does not render the daemon's arrangement")
	}
	if strings.Contains(tab, "modelData.job ===") {
		t.Error("the Account tab compares job ids — grouping is the daemon's decision")
	}
}

// The account is driven by its event rather than by a poll, so an action
// recorded anywhere — this window, the CLI, a job running unattended —
// re-reads it in every open window.
func TestTheAccountIsDrivenByItsEvent(t *testing.T) {
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))
	at := strings.Index(qml, `case "undo.changed":`)
	if at < 0 {
		t.Fatal("the window ignores undo.changed and would show a stale account")
	}
	arm := qml[at:]
	if end := strings.Index(arm, "break"); end >= 0 {
		arm = arm[:end]
	}
	if !strings.Contains(arm, "requestAccount()") {
		t.Errorf("the undo.changed arm does not re-read the account: %q", arm)
	}
	// Nothing may put the read on a clock. A Timer would turn "the account is
	// current" into "the account is at most N seconds out of date", which is a
	// different and weaker claim than the one this tab makes.
	for _, polled := range []string{"triggeredOnStart", "repeat: true"} {
		if strings.Contains(accountTab(t, qml), polled) {
			t.Errorf("the Account tab carries %q — the account is driven by "+
				"undo.changed, not polled", polled)
		}
	}
	// And on the reply as well, because undo.changed is published when the
	// reversal's own row is written — an instant before the row it reversed is
	// marked — so the reply is the moment both halves are certainly on disk.
	if !strings.Contains(qml, "function handleUndoApplyReply") ||
		strings.Count(qml, "requestAccount()") < 3 {
		t.Error("the account is not re-read after a reversal; the window would be " +
			"showing a row's standing it worked out for itself")
	}
}

// accountTab slices the Account tab's own screen out of the window, so the
// scans above are about this surface rather than about the eight others in the
// same file.
func accountTab(t *testing.T, qml string) string {
	t.Helper()
	start := strings.Index(qml, "id: accountScreen")
	if start < 0 {
		t.Fatal("JarvixWindow.qml has no Account tab")
	}
	end := strings.Index(qml[start:], "id: approvalFormBody")
	if end < 0 {
		t.Fatal("the Account tab has no end; the slice would cover the rest of the window")
	}
	return qml[start : start+end]
}
