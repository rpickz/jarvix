package desktop

import (
	"strings"
	"testing"
)

// Text guards over the routine form's launching half (#175), on the same
// terms as the other *qml_test.go files: QML cannot be parsed by anything in
// this module, so a scan of what the file DOES is what a Go test can hold it
// to.
//
// The rule being guarded is ADR 0013's: the window renders fields, ships the
// draft, and pins the daemon's problems to inputs. It decides nothing about
// what a step may launch — not which programs take an identity flag, not
// whether an entry exists, not what an argument may contain — because a
// second copy of any of those would be the one that disagrees.

// stepFormSection returns the step editor of the routine form.
func stepFormSection(t *testing.T) string {
	t.Helper()
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))
	start := strings.Index(qml, `text: "Steps (run in order)"`)
	if start < 0 {
		t.Fatal("the routine form has no steps section")
	}
	end := strings.Index(qml[start:], `label: "Add step"`)
	if end < 0 {
		t.Fatal("the steps section runs to the end of the file")
	}
	return qml[start : start+end]
}

// TestTheStepFormOffersTheWholeLaunchingHalf: every key a step can carry has
// an input. A key editable only by hand is a key the standing no-config-files
// rule says should not exist.
func TestTheStepFormOffersTheWholeLaunchingHalf(t *testing.T) {
	section := stepFormSection(t)
	for _, key := range []string{"app", "desktop_entry", "args", "identity", "match", "launch"} {
		if !strings.Contains(section, "]."+key) && !strings.Contains(section, "."+key+" =") {
			t.Errorf("the step form has no control bound to %q", key)
		}
	}
}

// TestTheStepFormPinsEveryLaunchingProblemToItsField: each control asks the
// daemon for the problem on its own key, so a validation message written once
// lands on the input the user has to change — including one argument of a
// list, which is keyed by position exactly as a phrase is.
func TestTheStepFormPinsEveryLaunchingProblemToItsField(t *testing.T) {
	// Line breaks are normalised away first: a control whose binding wraps is
	// still one expression, and a guard that only matched unwrapped lines
	// would reward formatting rather than behaviour.
	section := strings.Join(strings.Fields(stepFormSection(t)), " ")
	for _, key := range []string{"app", "desktop_entry", "identity", "launch", "match", "args"} {
		want := `automationProblemFor("steps[" + index + "].` + key + `"`
		alt := `automationProblemFor( "steps[" + argsColumn.stepIndex + "].` + key + `"`
		if !strings.Contains(section, want) && !strings.Contains(section, alt) {
			t.Errorf("no control pins the daemon's problem for the step's %q key", key)
		}
	}
	if !strings.Contains(section, `"].args[" + index + "]"`) {
		t.Error("an args problem is not pinned to the argument row it names")
	}
}

// TestTheStepFormShowsACautionItCanSaveThrough: "that program is not
// installed here" is a NOTE, not a problem, and the form must render it as
// one. Shown in the problem channel it would read as a refusal on a draft the
// daemon will happily save — which is how a user concludes they cannot write
// a routine for something they are about to install.
func TestTheStepFormShowsACautionItCanSaveThrough(t *testing.T) {
	section := strings.Join(strings.Fields(stepFormSection(t)), " ")
	if !strings.Contains(section, "automationStepNoteFor(index)") {
		t.Error("the step editor never renders the daemon's notes")
	}
	if strings.Contains(section, `"Problem: " + win.automationStepNoteFor`) {
		t.Error("a note is rendered in the problem channel; a caution must not read as a refusal")
	}
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))
	if !strings.Contains(qml, "automationFormNotes = result.notes || []") {
		t.Error("the validate reply's notes are never stored, so the form can never show one")
	}
	// Cleared when a form opens, like the problems beside them: a caution
	// left over from the last entry would be attached to the wrong routine.
	if strings.Count(qml, "automationFormNotes = []") < 2 {
		t.Error("the notes are not cleared when a form opens")
	}
}

// TestTheStepFormDecidesNothingAboutLaunching is the ADR 0013 guard proper.
// The window must not spell a class flag, a launch-policy list it composed
// itself, or any rule about what an argument may contain — those are the
// daemon's, and a copy here would be the one that goes stale.
//
// Kept after the QML suite landed (#174). The banned strings are the
// daemon's launching policy — --class, --app-id, a .desktop lookup, splitting
// an argument on spaces. A window that re-derived them would send an argv the
// daemon accepts, so every executed test would pass while the two
// implementations quietly diverged.
func TestTheStepFormDecidesNothingAboutLaunching(t *testing.T) {
	section := stepFormSection(t)
	for _, banned := range []string{"--class", "--app-id", "LookPath", ".desktop\"", "if_missing"} {
		if strings.Contains(section, banned) {
			t.Errorf("the step form spells %q itself; the daemon owns that decision", banned)
		}
	}
	// Arguments are sent as typed. A form that trimmed or split them would
	// quietly break the case the whole feature exists for — a profile name
	// with a space in it — so the only thing done to an argument row is
	// dropping an empty one.
	qml := stripQMLComments(readPlugin(t, "JarvixWindow.qml"))
	if !strings.Contains(qml, `var arg = String(drafted_args[a] || "")`) {
		t.Error("the draft serialiser does not pass arguments through as typed")
	}
	if strings.Contains(qml, `drafted_args[a] || "").trim()`) ||
		strings.Contains(qml, `arg.split(" ")`) {
		t.Error("the form trims or splits an argument; the daemon receives an argv, not a command line")
	}
}
