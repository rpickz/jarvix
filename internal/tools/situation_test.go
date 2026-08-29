package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// The model's path to the situation report (#196, ADR 0061).

type fakeSituating struct {
	spoken string
	err    error
	calls  int
}

func (f *fakeSituating) Situation(context.Context) (string, error) {
	f.calls++
	return f.spoken, f.err
}

func newSituationTool(svc Situating) *SituationGet {
	return NewSituation(SituationOptions{Service: svc, Log: slog.New(slog.DiscardHandler)})
}

// TestSituationToolRelaysTheReportUnchanged. The tool adds nothing: the report
// arrives composed, bounded, and already checked against its own
// no-extrapolation contract, and anything this layer put around it would be
// outside that contract's reach.
func TestSituationToolRelaysTheReportUnchanged(t *testing.T) {
	svc := &fakeSituating{spoken: "Right now: one waiting on you. " +
		"The AI session on the deploy is waiting on you."}
	out, err := newSituationTool(svc).Execute(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != svc.spoken {
		t.Errorf("Execute = %q, want the report verbatim", out)
	}
	if svc.calls != 1 {
		t.Errorf("the service was asked %d times", svc.calls)
	}
}

// TestSituationToolSpeaksItsDisappointments. Every way this can go wrong is a
// sentence the assistant can say in one breath — err is reserved for a
// malformed call, which a no-argument tool cannot receive.
func TestSituationToolSpeaksItsDisappointments(t *testing.T) {
	out, err := newSituationTool(nil).Execute(t.Context(), nil)
	if err != nil {
		t.Fatalf("a missing service was reported as an error: %v", err)
	}
	if !strings.Contains(out, "not available on this daemon") {
		t.Errorf("Execute with no service = %q", out)
	}

	svc := &fakeSituating{err: errors.New("no assistant provider is configured")}
	out, err = newSituationTool(svc).Execute(t.Context(), nil)
	if err != nil {
		t.Fatalf("a service failure was reported as an error: %v", err)
	}
	if !strings.Contains(out, "no assistant provider is configured") {
		t.Errorf("Execute over a failing service = %q", out)
	}
}

// TestSituationToolForbidsEmbellishment. The tool's description is the whole
// anti-confabulation contract for this path: the report's own guard cannot
// reach what an assistant says in reply to a tool result, so the instruction
// not to add to it has to be in the description a model actually reads (#71).
func TestSituationToolForbidsEmbellishment(t *testing.T) {
	desc := newSituationTool(nil).Description()
	for _, want := range []string{
		"Read the result back as it is written",
		"must not add anything to it",
		"guess at anything it does not say",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("the description does not say %q", want)
		}
	}
}

// TestSituationToolIsAllowTierByDefault. It is a read with no arguments: the
// widest thing it can do is tell the user about the state of the machine they
// are sitting at, in answer to their own question. Asking for permission to do
// that would train the user to approve without reading.
func TestSituationToolIsAllowTierByDefault(t *testing.T) {
	if got := builtinToolDefaults[SituationToolName]; got != PolicyAllow {
		t.Errorf("built-in tier = %q, want %q", got, PolicyAllow)
	}
}
