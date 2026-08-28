package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// The model's path to the return briefing (#150, ADR 0050).

type fakeBriefer struct {
	spoken string
	err    error
	calls  int
}

func (f *fakeBriefer) Briefing(context.Context) (string, error) {
	f.calls++
	return f.spoken, f.err
}

func newBriefingTool(svc Briefer) *BriefingGet {
	return NewBriefing(BriefingOptions{Service: svc, Log: slog.New(slog.DiscardHandler)})
}

// TestBriefingToolRelaysTheAccountUnchanged. The tool adds nothing: the
// account arrives composed, bounded, and already checked against its own
// contract, and anything this layer put around it would be outside that
// contract's reach.
func TestBriefingToolRelaysTheAccountUnchanged(t *testing.T) {
	svc := &fakeBriefer{spoken: "Since you were last here nine hours ago: one finished."}
	out, err := newBriefingTool(svc).Execute(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != svc.spoken {
		t.Errorf("Execute = %q, want the account verbatim", out)
	}
	if svc.calls != 1 {
		t.Errorf("the service was asked %d times", svc.calls)
	}
}

// TestBriefingToolSpeaksItsDisappointments. Every way this can go wrong is a
// sentence the assistant can say in one breath — err is reserved for a
// malformed call, which a no-argument tool cannot receive.
func TestBriefingToolSpeaksItsDisappointments(t *testing.T) {
	out, err := newBriefingTool(nil).Execute(t.Context(), nil)
	if err != nil {
		t.Fatalf("a missing service was reported as an error: %v", err)
	}
	if !strings.Contains(out, "not available on this daemon") {
		t.Errorf("Execute with no service = %q", out)
	}

	svc := &fakeBriefer{err: errors.New("no assistant provider is configured")}
	out, err = newBriefingTool(svc).Execute(t.Context(), nil)
	if err != nil {
		t.Fatalf("a failed compose was reported as an error: %v", err)
	}
	if !strings.Contains(out, "no assistant provider is configured") {
		t.Errorf("Execute with a failing service = %q", out)
	}
}

// TestBriefingToolForbidsEmbellishment. The description is the only place the
// model is told not to expand what it is handed, and this tool is the one
// path to a briefing that does not pass through internal/briefing's own
// contract on the way to the ear.
func TestBriefingToolForbidsEmbellishment(t *testing.T) {
	desc := newBriefingTool(nil).Description()
	for _, want := range []string{
		"Read the result back as it is written",
		"every claim in it came from a record",
		"you must not add anything to it",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("the tool description no longer says %q:\n%s", want, desc)
		}
	}
}

// TestBriefingToolIsAllowTierByDefault. A read of the user's own work,
// answered to the user, at their own machine: pausing for a permission card
// would make "what did I miss?" a question with a question in front of it.
func TestBriefingToolIsAllowTierByDefault(t *testing.T) {
	if d, ok := builtinToolDefaults[BriefingToolName]; !ok || d != PolicyAllow {
		t.Errorf("builtin default for %s = %q (present %v), want allow",
			BriefingToolName, d, ok)
	}
	// A built-in allow resolves ahead of the gate-wide default, exactly as
	// it does for every other family in that table — pinned here so a change
	// to the resolution order is a decision rather than an accident.
	deny, err := NewPolicy(PolicyConfig{Default: "deny"})
	if err != nil {
		t.Fatal(err)
	}
	if d := deny.ToolDecision(BriefingToolName); d != PolicyAllow {
		t.Errorf("deny-default ToolDecision = %q; builtinToolDefaults resolve before the gate default", d)
	}
}
