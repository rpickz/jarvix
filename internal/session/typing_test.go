package session

import (
	"encoding/json"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/tools"
)

// TestTypingApprovalIsNeverRemembered: `remember_for_conversation` re-runs an
// approved command without asking again, and that premise does not hold for
// keystrokes.
//
// Remembering is safe when the thing approved is fully described by what was
// asked — the same command, the same advisor. A typing approval is about a
// payload *and* the window that had focus when the question was asked, and the
// second half cannot be carried forward: the user is at their keyboard, and by
// the next call they may be somewhere else entirely. So the same call is asked
// about every time, however the setting is configured.
//
// The tool here is a stand-in that types nothing. What is under test is the
// engine's half of the rule — that an approval for a typing tool is not
// stored, and not reused — and no keystroke is needed to prove it.
func TestTypingApprovalIsNeverRemembered(t *testing.T) {
	rec := &namedTool{name: tools.TypeTextToolName, result: "Typed the text."}
	h := newGateHarness(t, Options{RememberApprovals: true}, rec, tools.PolicyConfig{})
	args, _ := json.Marshal(map[string]string{"text": "dear team"})
	call := ai.ToolCall{ID: "c1", Name: tools.TypeTextToolName, Arguments: string(args)}
	// The model types the same thing twice in one turn.
	h.provider.ToolCallsByRound = [][]ai.ToolCall{{call}, {call}}
	h.provider.Response = "Done."

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("type that into the document, twice")
	h.waitFor(t, "tool.confirmation_required")
	_ = h.engine.Confirm(true)
	// The second call must ask again rather than ride the first approval.
	h.waitFor(t, "tool.confirmation_required")
	_ = h.engine.Confirm(true)
	h.countUntil(t, "session.finished")
	h.waitIdle(t)

	if rec.calls != 2 {
		t.Errorf("tool calls = %d, want 2 — each one separately approved", rec.calls)
	}
}

// TestOtherApprovalsAreStillRemembered guards the exception from becoming the
// rule: the setting still does what it says for everything else.
func TestOtherApprovalsAreStillRemembered(t *testing.T) {
	rec := &namedTool{name: "mystery.op", result: "done"}
	h := newGateHarness(t, Options{RememberApprovals: true}, rec, tools.PolicyConfig{})
	call := ai.ToolCall{ID: "c1", Name: "mystery.op", Arguments: `{}`}
	h.provider.ToolCallsByRound = [][]ai.ToolCall{{call}, {call}}
	h.provider.Response = "Done."

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("do it twice")
	h.waitFor(t, "tool.confirmation_required")
	_ = h.engine.Confirm(true)
	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)

	if rec.calls != 2 {
		t.Errorf("tool calls = %d, want 2", rec.calls)
	}
	if counts["tool.confirmation_required"] != 0 {
		t.Errorf("asked again %d times; this approval should have been remembered",
			counts["tool.confirmation_required"])
	}
}
