package desktop

import (
	"strings"
	"testing"
)

// These are text guards over the checked-in QML for the screen-name surface
// (#180, ADR 0057), on the same terms as the other *qml_test.go files: QML
// cannot be parsed by anything in this module, so a scan of what the file
// DOES is what a Go test can hold it to.
//
// What they are guarding against is specific. The window is a thin client
// (ADR 0013): every rule about screen names — which are plugged in, which
// words are reserved, what a collision says — is the daemon's answer, and the
// moment the QML composes one of those itself the two surfaces can disagree
// while both look right.

// monitorSection returns the Screens section of the Automations tab.
func monitorSection(t *testing.T) string {
	t.Helper()
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))
	start := strings.Index(qml, `text: "Screens"`)
	if start < 0 {
		t.Fatal("the Automations tab has no Screens section")
	}
	end := strings.Index(qml[start:], "JarvixDetailPane")
	if end < 0 {
		t.Fatal("the Screens section runs to the end of the file")
	}
	return qml[start : start+end]
}

// TestTheScreenSectionSpeaksOnlyTheDaemonsVerbs: the four monitors.* verbs,
// each called from exactly one place. A second call site is how one of them
// comes to send different parameters than the other.
func TestTheScreenSectionSpeaksOnlyTheDaemonsVerbs(t *testing.T) {
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))
	for _, verb := range []string{"monitors.list", "monitors.name", "monitors.repoint", "monitors.forget"} {
		if n := strings.Count(qml, `"`+verb+`"`); n != 1 {
			t.Errorf("%s is called from %d places; one contract, one call site", verb, n)
		}
	}
	// Naming a NEW screen and moving an existing name are different verbs,
	// and the form has to choose between them rather than always sending one
	// — moving a name changes what every routine using it does.
	if !strings.Contains(qml, `monitorFormExisting === "" ? "monitors.name" : "monitors.repoint"`) {
		t.Error("the screen-name form does not distinguish naming from re-pointing")
	}
}

// TestTheScreenSectionInventsNothing: no connector name, no reserved word and
// no size is spelled in QML. Every one of them arrives in the reply.
func TestTheScreenSectionInventsNothing(t *testing.T) {
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))
	for _, banned := range []string{"HDMI-A-1", "DP-2", `"primary"`, "eDP"} {
		if strings.Contains(qml, banned) {
			t.Errorf("the window spells %q itself; connector names and reserved words "+
				"come from monitors.list", banned)
		}
	}
	// The reserved words are rendered from the reply, never from a literal.
	if !strings.Contains(qml, "win.monitorReserved.join(") {
		t.Error("the form does not render the daemon's reserved-word list")
	}
}

// TestAnUnpluggedScreenIsVisibleAndSaidInWords: the state this whole feature
// exists to stop being mysterious. It must be listed, flagged, and explained
// in words rather than by colour alone (the settings screen's rule).
func TestAnUnpluggedScreenIsVisibleAndSaidInWords(t *testing.T) {
	section := monitorSection(t)
	if !strings.Contains(section, "win.monitorNicknames") {
		t.Error("the section lists something other than the stored names, so a name whose " +
			"screen is unplugged would vanish from the list")
	}
	if !strings.Contains(section, "flagged: modelData.present !== true") {
		t.Error("an absent screen is not flagged")
	}
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))
	if !strings.Contains(qml, "not plugged in right now") {
		t.Error("nothing says in words that a screen is not plugged in")
	}
}

// TestTheScreenRowsUseTheSharedRow: a screen name reads exactly like a fact
// or a feed does in its own tab, and gets the row's keyboard reachability and
// accessible naming rather than reimplementing them.
func TestTheScreenRowsUseTheSharedRow(t *testing.T) {
	section := monitorSection(t)
	if !strings.Contains(section, "JarvixCollectionRow") {
		t.Error("the Screens section does not use JarvixCollectionRow")
	}
	for _, want := range []string{`actionLabel: "Edit"`, `action2Label: "Forget"`} {
		if !strings.Contains(section, want) {
			t.Errorf("the row does not offer %s", want)
		}
	}
	if !strings.Contains(section, "win.monitorPath") {
		t.Error("the section does not tell the user which file the names live in")
	}
}

// TestThePickerOffersTheCurrentMonitorAndEveryPresentOutput: the acceptance
// criterion for the picker, checked where it is built.
func TestThePickerOffersTheCurrentMonitorAndEveryPresentOutput(t *testing.T) {
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))
	start := strings.Index(qml, "function monitorPickerOptions()")
	if start < 0 {
		t.Fatal("there is no monitor picker data source")
	}
	body := qml[start:]
	if end := strings.Index(body, "\n  }"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, `value: "", label: "the current monitor"`) {
		t.Error("the picker does not offer the current monitor")
	}
	if !strings.Contains(body, "win.monitorScreens") && !strings.Contains(body, "monitorScreens.length") {
		t.Error("the picker does not offer the present outputs")
	}
	for _, want := range []string{"m.describe", "m.nickname"} {
		if !strings.Contains(body, want) {
			t.Errorf("the picker option omits %s, so a screen cannot be told from another", want)
		}
	}
	if !strings.Contains(qml, "JarvixMonitorPicker {") {
		t.Error("the shared picker component is not used by any form")
	}
}

// TestThePickerComponentIsKeyboardReachableAndDisplayOnly: the shared
// component itself — a control the user cannot reach with a keyboard is a
// control half this window's users do not have.
func TestThePickerComponentIsKeyboardReachableAndDisplayOnly(t *testing.T) {
	qml := stripQMLComments(readPlugin(t, "JarvixMonitorPicker.qml"))
	for _, want := range []string{
		"activeFocusOnTab: true",
		"Accessible.role: Accessible.ComboBox",
		"Keys.onSpacePressed",
		"Keys.onLeftPressed",
		"Keys.onRightPressed",
	} {
		if !strings.Contains(qml, want) {
			t.Errorf("the picker is missing %s", want)
		}
	}
	if strings.Contains(qml, "monitors.") {
		t.Error("the picker talks to the daemon; it is display-only (ADR 0013)")
	}
	if !strings.Contains(qml, `signal chosen(string value)`) {
		t.Error("the picker does not signal its choice to the caller")
	}
}

// TestTheScreenFormPinsRefusalsToTheirFields: the collision matrix arrives
// field-keyed and has to land on the control it belongs to, or the user is
// told something is wrong without being told what to change.
func TestTheScreenFormPinsRefusalsToTheirFields(t *testing.T) {
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))
	for _, want := range []string{
		`problem: win.monitorProblemFor("name")`,
		`problem: win.monitorProblemFor("connector")`,
		`win.monitorProblemFor("")`, // the whole-form bucket: no message is ever dropped
	} {
		if !strings.Contains(qml, want) {
			t.Errorf("the screen-name form is missing %s", want)
		}
	}
	if !strings.Contains(qml, "data.problems") {
		t.Error("the form does not read the daemon's field-keyed problems")
	}
}

// TestTheScreenListingRefreshesOnEveryRouteToIt: on connect, and on opening
// the tab. A stale list is how a user comes to believe a name they just
// assigned did not take.
func TestTheScreenListingRefreshesOnEveryRouteToIt(t *testing.T) {
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))
	if n := strings.Count(qml, "requestMonitors()"); n < 4 {
		t.Errorf("requestMonitors is called from %d places; expected the definition plus "+
			"connect, tab-open, and the post-write refreshes", n)
	}
	connect := qml[strings.Index(qml, "win.requestVocabulary()"):]
	if i := strings.Index(connect, "requestTypography()"); i > 0 {
		connect = connect[:i]
	}
	if !strings.Contains(connect, "win.requestMonitors()") {
		t.Error("the screen names are not loaded on connect")
	}
}
