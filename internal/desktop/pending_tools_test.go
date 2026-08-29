package desktop_test

// An external test package for the same reason activity_tools_test.go is one:
// internal/tools imports internal/desktop, so the desktop package cannot name
// the tool constants and the phrase table matches on string literals instead.
// This file may import both sides, and pins each literal to its constant.
//
// The drift this catches is quiet and expensive. Rename a tool without
// updating the table and the permission gate stops asking "May I run a shell
// command?" and starts asking "May I use the shell.exec tool?", while the
// conversation's pending turn stops saying what a round is doing and starts
// naming an identifier — both technically honest, both a regression in exactly
// the way nobody notices until a user reports it.

import (
	"testing"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/tools"
)

func TestToolActionPhrasesMatchRealToolNames(t *testing.T) {
	cases := []struct {
		tool  string
		ask   string
		doing string
	}{
		{"shell.run", "run a shell command", "Running a shell command"},
		{tools.IntentToolName, "run your custom command", "Running your custom command"},
		{tools.ScriptToolName, "run one of your scripts", "Running one of your scripts"},
		{tools.RoutineToolName, "run one of your routines", "Running one of your routines"},
		{tools.AdvisorToolName, "consult another assistant", "Consulting another assistant"},
		{tools.DeepToolName, "ask the stronger model", "Thinking deeply"},
		{tools.KnowledgeRefreshToolName, "refresh one of your feeds", "Refreshing one of your feeds"},
		{tools.TypeTextToolName, "type on your keyboard", "Typing on your keyboard"},
		{tools.PressKeyToolName, "type on your keyboard", "Typing on your keyboard"},
		{tools.MemoryForgetToolName, "forget one of your saved facts", "Forgetting one of your saved facts"},
		{tools.ConfigWriteSettingToolName, "change one of your settings", "Changing one of your settings"},
		{tools.ConfigWriteEntryToolName, "save a configuration entry", "Saving a configuration entry"},
		{tools.ConfigDeleteEntryToolName, "delete a configuration entry", "Deleting a configuration entry"},
	}
	for _, c := range cases {
		if got := desktop.ToolActionAsk(c.tool); got != c.ask {
			t.Errorf("ToolActionAsk(%q) = %q, want %q", c.tool, got, c.ask)
		}
		if got := desktop.ToolActionDoing(c.tool); got != c.doing {
			t.Errorf("ToolActionDoing(%q) = %q, want %q", c.tool, got, c.doing)
		}
	}
	// The shell tool's name is not exported (its constant is unexported in
	// internal/tools), so the literal above is pinned through the gate's own
	// registry name instead — see internal/session's gate tests, which drive
	// the real policy. Here it is enough to prove the table has not silently
	// lost the most-used tool of all.
	if desktop.ToolActionDoing("shell.run") == "Running shell.run" {
		t.Error("shell.run fell out of the phrase table and is naming itself")
	}
}
