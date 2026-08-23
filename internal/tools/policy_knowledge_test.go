package tools

import (
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
)

// The knowledge.refresh identity (ADR 0031): default allow because the user
// authored every feed command, per-identity overrides both ways, and deny
// always wins — checked against the same Decide path the engine uses.

func knowledgeCall(feed string) ai.ToolCall {
	return ai.ToolCall{Name: KnowledgeGetToolName, Arguments: `{"feed":"` + feed + `"}`}
}

func TestKnowledgeDefaultsToAllowUnderTheShippedPolicy(t *testing.T) {
	p, err := NewPolicy(PolicyConfig{Default: PolicyAsk})
	if err != nil {
		t.Fatal(err)
	}
	v := p.Decide(knowledgeCall("amd"))
	if v.Decision != PolicyAllow {
		t.Fatalf("verdict = %+v, want allow: the user authored the feed's command", v)
	}
	if v.Tool != KnowledgeRefreshToolName || v.Command != "amd" {
		t.Errorf("audit identity = %q command %q, want knowledge.refresh over the feed name", v.Tool, v.Command)
	}
}

func TestKnowledgeExplicitDenyWins(t *testing.T) {
	p, err := NewPolicy(PolicyConfig{Default: PolicyAllow,
		Tools: map[string]PolicyDecision{KnowledgeRefreshToolName: PolicyDeny}})
	if err != nil {
		t.Fatal(err)
	}
	if v := p.Decide(knowledgeCall("amd")); v.Decision != PolicyDeny {
		t.Fatalf("verdict = %+v, want deny — deny always wins", v)
	}
	if d := p.ToolDecision(KnowledgeRefreshToolName); d != PolicyDeny {
		t.Errorf("ToolDecision = %q, want deny for the scheduler's gate check too", d)
	}
}

func TestKnowledgeExplicitAskNamesTheFeed(t *testing.T) {
	p, err := NewPolicy(PolicyConfig{Default: PolicyAllow,
		Tools: map[string]PolicyDecision{KnowledgeRefreshToolName: PolicyAsk}})
	if err != nil {
		t.Fatal(err)
	}
	v := p.Decide(knowledgeCall("amd"))
	if v.Decision != PolicyAsk {
		t.Fatalf("verdict = %+v, want ask", v)
	}
	if !strings.Contains(v.Summary, "amd feed") {
		t.Errorf("summary = %q, want the feed named in the question", v.Summary)
	}
}

func TestKnowledgeGetResolvesThroughTheRefreshIdentity(t *testing.T) {
	// ToolDecision on the tool's registry name must give the same answer the
	// Decide path gives, or status reporting would lie about the gate.
	p, err := NewPolicy(PolicyConfig{Default: PolicyAsk,
		Tools: map[string]PolicyDecision{KnowledgeRefreshToolName: PolicyDeny}})
	if err != nil {
		t.Fatal(err)
	}
	if d := p.ToolDecision(KnowledgeGetToolName); d != PolicyDeny {
		t.Errorf("ToolDecision(knowledge.get) = %q, want the knowledge.refresh tier", d)
	}
}
