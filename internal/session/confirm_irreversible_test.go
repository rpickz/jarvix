package session

import (
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"

	"github.com/rpickz/jarvix/internal/tools"
)

// Which decisions are one-way, said before approval (#201, ADR 0064).
//
// This is the acceptance criterion that is easy to satisfy in the wrong
// place. Recording irreversibility after the fact is the obvious feature and
// changes nothing: a manager who learns a decision was one-way when they read
// the account has learned something useless. What matters is the moment — the
// warning has to be on the card the user is answering, and on the question
// they are hearing, before they say yes.

// TestTheCardSaysAnActionIsOneWayBeforeApproval drives the real gate: a
// risky shell command reaches the ask tier, and the event the card and the
// overlay both render carries the warning inside the summary they already
// show. Riding the summary rather than a new field is deliberate — see ADR
// 0064 — because a new field reaches whichever surface was updated for it and
// leaves the others silent about the thing that matters most.
func TestTheCardSaysAnActionIsOneWayBeforeApproval(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "Understood.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	required := h.waitFor(t, "tool.confirmation_required")

	summary, _ := required.Data["summary"].(string)
	if !strings.Contains(summary, "This can't be undone") {
		t.Errorf("the card's summary %q does not say the command cannot be taken back", summary)
	}
	if !strings.Contains(summary, "a command that has run has run") {
		t.Errorf("the card's summary %q says it is one-way without saying why", summary)
	}
	// The verbatim command is untouched by the annotation: ADR 0014's display
	// doctrine is not something this feature gets to bend.
	if required.Data["command"] != "rm -rf ./build" {
		t.Errorf("event command = %v, want it verbatim", required.Data["command"])
	}
	// And the same fact in a form a surface can act on rather than only
	// print, set only when the summary actually carries the clause so the two
	// can never disagree about whether the user was told.
	if irreversible, _ := required.Data["irreversible"].(bool); !irreversible {
		t.Error("the event does not flag the action as irreversible")
	}

	_ = h.engine.Confirm(false)
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
}

// TestAReversibleActionsCardClaimsNothing is the other half, and the reason
// the warning is worth reading at all: a card that said something about every
// action would be a card nobody reads. A config write is reversible, so its
// question carries no clause and no flag.
func TestAReversibleActionsCardClaimsNothing(t *testing.T) {
	rec := &namedTool{name: tools.ConfigWriteSettingToolName, result: "saved"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{
		Tools: map[string]tools.PolicyDecision{tools.ConfigWriteSettingToolName: tools.PolicyAsk},
	})
	h.provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: tools.ConfigWriteSettingToolName,
			Arguments: `{"key":"tts.voice","value":"jenny"}`}},
	}
	h.provider.Response = "Understood."

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("change my voice")
	required := h.waitFor(t, "tool.confirmation_required")

	summary, _ := required.Data["summary"].(string)
	if strings.Contains(summary, "can't be undone") {
		t.Errorf("a reversible action's card says %q", summary)
	}
	if _, present := required.Data["irreversible"]; present {
		t.Error("a reversible action's event carries the irreversible flag")
	}

	_ = h.engine.Confirm(false)
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
}
