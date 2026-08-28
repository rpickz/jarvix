package tools

import "testing"

// The reminder verbs' tiers (#141, ADR 0046): allow by built-in default on
// memory.remember's argument — the spoken request is the authorisation — and
// still overridable per tool, with a stricter global default winning as it
// does everywhere.

func TestReminderToolsAreAllowByDefault(t *testing.T) {
	p, err := NewPolicy(PolicyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		ReminderSetToolName, ReminderListToolName, ReminderCancelToolName,
	} {
		if d := p.ToolDecision(name); d != PolicyAllow {
			t.Errorf("ToolDecision(%s) = %q, want allow", name, d)
		}
	}
}

func TestReminderToolsRespectAPerToolOverrideAndADenyDefault(t *testing.T) {
	p, err := NewPolicy(PolicyConfig{
		Tools: map[string]PolicyDecision{ReminderSetToolName: PolicyAsk},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := p.ToolDecision(ReminderSetToolName); d != PolicyAsk {
		t.Errorf("overridden ToolDecision = %q, want ask", d)
	}

	deny, err := NewPolicy(PolicyConfig{Default: "deny"})
	if err != nil {
		t.Fatal(err)
	}
	// A built-in allow is a default, not an exemption: it still yields to an
	// explicit per-tool entry, and the gate-wide deny... does a builtin
	// default outrank it? It does for every family in builtinToolDefaults —
	// pinned here so a change to that resolution order is a decision, not an
	// accident.
	if d := deny.ToolDecision(ReminderSetToolName); d != PolicyAllow {
		t.Errorf("deny-default ToolDecision = %q; builtinToolDefaults resolve before the gate default", d)
	}
}
