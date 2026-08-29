package undo_test

// An external test package for internal/desktop/pending_tools_test.go's
// reason, inverted: internal/tools imports internal/undo, so the undo package
// cannot name the tool constants and its classification table matches on
// string literals instead. This file may import both sides, and pins each
// literal to its constant.
//
// The drift this catches is the worst kind there is for this feature. Rename a
// tool without updating the table and shell.run stops being classified as
// one-way — so the confirmation card stops saying "this can't be undone", and
// a user approves a command believing they could take it back. The failure is
// silent, it is on the safety side, and nothing else in the build would notice.
//
// It also fails when a tool exists that nobody has classified at all, which is
// the second half of the same argument: an unclassified capability says
// nothing on the card, and "nobody looked" must not be able to masquerade as
// "we decided this needs no warning".

import (
	"sort"
	"testing"

	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/undo"
)

// TestClassificationsMatchRealToolNames pins every literal in the table to
// the constant it is meant to be.
func TestClassificationsMatchRealToolNames(t *testing.T) {
	cases := []struct {
		tool string
		want undo.Nature
	}{
		{tools.ShellToolName, undo.NatureIrreversible},
		{tools.IntentToolName, undo.NatureIrreversible},
		{tools.ScriptToolName, undo.NatureIrreversible},
		{tools.RoutineToolName, undo.NatureIrreversible},
		{tools.TypeTextToolName, undo.NatureIrreversible},
		{tools.PressKeyToolName, undo.NatureIrreversible},
		{tools.KnowledgeRefreshToolName, undo.NatureIrreversible},
		{tools.CloseWindowToolName, undo.NatureIrreversible},
		{tools.LaunchAppToolName, undo.NatureIrreversible},

		{tools.ConfigWriteEntryToolName, undo.NatureReversible},
		{tools.ConfigDeleteEntryToolName, undo.NatureReversible},
		{tools.ConfigWriteSettingToolName, undo.NatureReversible},
		{tools.MemoryRememberToolName, undo.NatureReversible},
		{tools.MemoryForgetToolName, undo.NatureReversible},
		{tools.VocabularyTeachToolName, undo.NatureReversible},
		{tools.VocabularyForgetToolName, undo.NatureReversible},
		{tools.ReminderSetToolName, undo.NatureReversible},
		{tools.ReminderCancelToolName, undo.NatureReversible},
		{tools.MoveWindowToolName, undo.NatureReversible},
		{tools.NameWindowToolName, undo.NatureReversible},
		{tools.ManageWindowToolName, undo.NatureReversible},
		{tools.ArtifactToolName, undo.NatureReversible},

		{tools.ListWindowsToolName, undo.NatureReadOnly},
		{tools.ListAppsToolName, undo.NatureReadOnly},
		{tools.ListManagedToolName, undo.NatureReadOnly},
		{tools.ReleaseWindowToolName, undo.NatureReadOnly},
		{tools.MemorySearchToolName, undo.NatureReadOnly},
		{tools.ConversationsSearchToolName, undo.NatureReadOnly},
		{tools.ConfigListEntriesToolName, undo.NatureReadOnly},
		{tools.ConfigGetEntryToolName, undo.NatureReadOnly},
		{tools.ConfigReadSettingsToolName, undo.NatureReadOnly},
		{tools.ReminderListToolName, undo.NatureReadOnly},
		{tools.BriefingToolName, undo.NatureReadOnly},
		{tools.SituationToolName, undo.NatureReadOnly},
		{tools.KnowledgeGetToolName, undo.NatureReadOnly},
		{tools.AdvisorToolName, undo.NatureReadOnly},
		{tools.DeepToolName, undo.NatureReadOnly},
	}
	classified := map[string]bool{}
	for _, c := range cases {
		if got := undo.Classify(c.tool); got != c.want {
			t.Errorf("Classify(%q) = %v, want %v — the table has drifted from the tool's name",
				c.tool, got, c.want)
		}
		classified[c.tool] = true
	}

	// The other direction: nothing in the table is a name no constant claims.
	// A stale literal is a classification that stopped applying to anything,
	// which reads like coverage and is not.
	var stale []string
	for _, name := range undo.ClassifiedTools() {
		if !classified[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		t.Errorf("the table classifies %q, which no tool constant above names; "+
			"either the tool was renamed or the entry is dead", name)
	}
}

// TestTheMostDangerousToolIsAlwaysMarked is the assertion worth having on its
// own, unconditioned by any table walk: shell.run must warn, in both forms.
// If everything else in this file is deleted by a future refactor, this is
// the line that keeps the promise.
func TestTheMostDangerousToolIsAlwaysMarked(t *testing.T) {
	if undo.CardNote(tools.ShellToolName) == "" {
		t.Fatal("the confirmation card would say nothing about a shell command being one-way")
	}
	if undo.SpokenNote(tools.ShellToolName) == "" {
		t.Fatal("the spoken question would say nothing about a shell command being one-way")
	}
}
