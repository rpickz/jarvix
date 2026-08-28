package desktop

import (
	"os"
	"strings"
	"testing"
)

// The client half of #164: the last families that needed a text editor now
// have a section in the window, and — the property worth a test — every one of
// them reaches the daemon through the SAME generic verbs the routines form has
// used since #99. A section that grew a verb of its own would be the new CRUD
// the ticket's architecture requirement forbids, and it would be invisible to
// every other test in this package.
//
// A text scan, like every other QML guard here (ADR 0013 keeps the decisions in
// Go; these tests keep the QML from growing any).

// stripQMLComments drops `//` lines so a scan can be about what the file DOES
// rather than about what its comments explain — the answerConfirmationBody
// precedent, generalised.
func stripQMLComments(qml string) string {
	var code []string
	for _, line := range strings.Split(qml, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code = append(code, line)
	}
	return strings.Join(code, "\n")
}

func readPlugin(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(pluginFilePath(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestSpokenCommandsUseTheGenericEntryVerbs: the Automations tab's third
// collection is a registry row and a section, not a surface.
func TestSpokenCommandsUseTheGenericEntryVerbs(t *testing.T) {
	qml := readPlugin(t, "JarvixWindow.qml")
	for _, want := range []string{
		`params: { family: "intents.custom" }`,
		`params: { family: "intents.custom", name: match }`,
		`family: "intents.custom", name: spokenFormOriginalMatch`,
		// The phrase, the command and the acknowledgement, each pinning the
		// daemon's problem for its own key.
		`win.spokenProblemFor("match")`,
		`win.spokenProblemFor("run")`,
		`win.spokenProblemFor("say")`,
	} {
		if !strings.Contains(qml, want) {
			t.Errorf("JarvixWindow.qml is missing %q", want)
		}
	}
	// No family-specific verb: everything goes through the four the entry
	// registry serves.
	for _, banned := range []string{`method: "intents.`, `method: "config.set_intent`} {
		if strings.Contains(qml, banned) {
			t.Errorf("JarvixWindow.qml calls %q — the family is a registry row, not a surface", banned)
		}
	}
}

// TestTheLexiconSectionUsesTheGenericEntryVerbs: the same claim for the third
// document shape, on the settings screen's own socket.
func TestTheLexiconSectionUsesTheGenericEntryVerbs(t *testing.T) {
	qml := readPlugin(t, "JarvixSettings.qml")
	for _, want := range []string{
		`method: "config.list_entries", params: { family: "tts.lexicon" }`,
		`method: "config.get_entry"`,
		`method: "config.validate_entry"`,
		`method: "config.upsert_entry"`,
		`method: "config.delete_entry"`,
		// The daemon's NOTE — the common-word warning — is rendered as a
		// consequence, not as an error, and never composed here.
		`settings.lexiconNoteFor("name")`,
		`text: "Note: " + settings.lexiconNoteFor("name")`,
	} {
		if !strings.Contains(qml, want) {
			t.Errorf("JarvixSettings.qml is missing %q", want)
		}
	}
	// The warning's judgement is the daemon's. A screen that knew which words
	// are ordinary English would be a second list to keep in step — so the
	// scan is over the CODE, with the comments stripped, because the comment
	// explaining why the list is not here necessarily describes the list.
	code := stripQMLComments(qml)
	for _, banned := range []string{"commonWords", "commonEnglish", "ordinary English"} {
		if strings.Contains(code, banned) {
			t.Errorf("JarvixSettings.qml mentions %q — the daemon decides which words are "+
				"ordinary, and a copy here is a second list to drift", banned)
		}
	}
}

// TestTheReminderFormPreviewsBeforeItSaves pins the one thing the form adds
// over the spoken path, and pins that it adds nothing else: no clock
// arithmetic, no time parsing, no wording of a moment.
func TestTheReminderFormPreviewsBeforeItSaves(t *testing.T) {
	qml := readPlugin(t, "JarvixWindow.qml")
	for _, want := range []string{
		`method: "reminders.preview", params: { when: reminderDraftWhen }`,
		`method: "reminders.create"`,
		// The resolved moment is the daemon's sentence, read off the reply.
		`reminderPreview = result.valid === true ? String(result.due_spoken || "") : ""`,
		`text: "Fires " + win.reminderPreview + "."`,
	} {
		if !strings.Contains(qml, want) {
			t.Errorf("JarvixWindow.qml is missing %q", want)
		}
	}
	for _, banned := range []string{"new Date(", "getHours(", "setMinutes(", "parseWhen"} {
		if strings.Contains(qml, banned) {
			t.Errorf("JarvixWindow.qml uses %q — the daemon resolves the moment (ADR 0013)", banned)
		}
	}
}

// TestTheFocusFormSendsOneWholeDraft: the whole thread in one request, because
// four sequential verbs would be four writes and a failure between two of them
// would leave a half-configured thread.
func TestTheFocusFormSendsOneWholeDraft(t *testing.T) {
	qml := readPlugin(t, "JarvixFocusTab.qml")
	for _, want := range []string{
		`readonly property int saveRequestId: 504`,
		`method: "focus.save", params: params`,
		`params.remind_every_min`,
		`if (draftReanchor) params.anchors = draftAnchors`,
		`focusTab.problemFor("recap")`,
	} {
		if !strings.Contains(qml, want) {
			t.Errorf("JarvixFocusTab.qml is missing %q", want)
		}
	}
	// One save call site, so the "one write" claim cannot be quietly undone by
	// a second path that sets one field at a time.
	if got := strings.Count(qml, `method: "focus.save"`); got != 1 {
		t.Errorf("focus.save call sites = %d, want exactly 1", got)
	}
	for _, banned := range []string{`method: "focus.remind"`, `method: "focus.create"`} {
		if strings.Contains(qml, banned) {
			t.Errorf("JarvixFocusTab.qml calls %q — the form saves a whole draft", banned)
		}
	}
}

// TestTheNewSurfacesKeepTheirRequestIdRanges: every surface on a shared socket
// carves out a private range so a reply can be matched to exactly the request
// that asked for it. The two new ones on the window's socket reuse the ranges
// their features already own (reminders 850–899, approvals 900–949) rather than
// inventing a third, and the focus tab's new id sits inside its own 500–599.
func TestTheNewSurfacesKeepTheirRequestIdRanges(t *testing.T) {
	window := readPlugin(t, "JarvixWindow.qml")
	for _, want := range []string{
		"property int nextReminderRequestId: 850",
		"nextReminderRequestId >= 899 ? 850",
		"property int nextApprovalsRequestId: 900",
		"nextApprovalsRequestId >= 949 ? 900",
		// The reminder form and the approvals add form draw from those, not
		// from the window's dynamic counter.
		"reminderPreviewRequestId = takeReminderRequestId()",
		"reminderCreateRequestId = takeReminderRequestId()",
		"approvalsAddRequestId = takeApprovalsRequestId()",
	} {
		if !strings.Contains(window, want) {
			t.Errorf("JarvixWindow.qml is missing %q", want)
		}
	}
	focus := readPlugin(t, "JarvixFocusTab.qml")
	for _, id := range []string{"500", "501", "502", "503", "504", "510"} {
		if !strings.Contains(focus, ": "+id) {
			t.Errorf("JarvixFocusTab.qml lost request id %s", id)
		}
	}
}
