package desktop_test

// An external test package on purpose: internal/tools imports
// internal/desktop, so the desktop package cannot name the tool constants —
// SummariseToolArgs matches on string literals instead. This file may import
// both sides, and pins each literal to its constant: rename a tool without
// updating the summary table and the tool silently becomes "unknown", whose
// summary is nothing — safe for privacy, wrong for the feed, and exactly the
// drift this test exists to catch.

import (
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/tools"
)

func TestActivityToolSummariesMatchRealToolNames(t *testing.T) {
	cases := []struct {
		tool string
		args string
		want string
	}{
		{tools.AdvisorToolName, `{"advisor":"claude"}`, "advisor claude"},
		{tools.LaunchAppToolName, `{"app":"firefox"}`, "firefox"},
		{tools.FocusWindowToolName, `{"window":"the browser"}`, "the browser"},
		{tools.FocusWindowToolName, `{}`, "the focused window"},
		{tools.CloseWindowToolName, `{"window":"kitty"}`, "kitty"},
		{tools.MoveWindowToolName, `{"window":"slack","workspace":3}`, "slack · workspace 3"},
		{tools.ListWindowsToolName, `{}`, ""},
		{tools.TypeTextToolName, `{"text":"ab"}`, "2 characters (text not shown)"},
		{tools.PressKeyToolName, `{"key":"enter"}`, "enter"},
		{tools.MemoryRememberToolName, `{"content":"abc"}`, "a fact of 3 characters (content not shown)"},
		{tools.MemoryRecallToolName, `{"query":"abc"}`, "query not shown"},
		{tools.MemoryForgetToolName, `{"query":"abc"}`, "query not shown"},
		{tools.ConversationsSearchToolName, `{"query":"abcd"}`, "query of 4 characters (not shown)"},
	}
	for _, c := range cases {
		if got := desktop.SummariseToolArgs(c.tool, c.args); got != c.want {
			t.Errorf("SummariseToolArgs(%q, %s) = %q, want %q", c.tool, c.args, got, c.want)
		}
	}
	// The default is silence: model-authored arguments for a tool the table
	// does not know never reach a row.
	if got := desktop.SummariseToolArgs("future.tool", `{"secret":"content"}`); got != "" {
		t.Errorf("unknown tool summarised as %q, want nothing", got)
	}
	if got := desktop.SummariseToolArgs("shell.run", `not json`); got != "" {
		t.Errorf("unparseable arguments summarised as %q, want nothing", got)
	}
	if !strings.HasPrefix(tools.AdvisorToolName, "advisor.") {
		t.Errorf("advisor tool renamed to %q; update the summary table", tools.AdvisorToolName)
	}
}
