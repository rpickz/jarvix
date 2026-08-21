package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
)

func advisorCall(name string) ai.ToolCall {
	args, _ := json.Marshal(map[string]string{"advisor": name, "question": "review my architecture"})
	return ai.ToolCall{ID: "c1", Name: AdvisorToolName, Arguments: string(args)}
}

// advisorTiers is the shape the daemon derives from configuration: an
// advisor on an untouched read-only preset is allow, everything else asks.
var advisorTiers = map[string]PolicyDecision{
	"claude": PolicyAllow, // shipped read-only preset, unchanged
	"aider":  PolicyAsk,   // coding agent: can change the machine
	"custom": PolicyAsk,   // hand-written argv: unaudited
}

func TestAdvisorTierComesFromConfiguration(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{Advisors: advisorTiers})

	if v := p.Decide(advisorCall("claude")); v.Decision != PolicyAllow {
		t.Errorf("read-only advisor: decision = %s (%s)", v.Decision, v.Rule)
	}
	for _, name := range []string{"aider", "custom"} {
		v := p.Decide(advisorCall(name))
		if v.Decision != PolicyAsk {
			t.Errorf("%s: decision = %s (%s)", name, v.Decision, v.Rule)
		}
		if !strings.Contains(v.Summary, name) {
			t.Errorf("%s: summary must name the advisor: %q", name, v.Summary)
		}
		// The confirmation is keyed on the advisor, never on the question,
		// so a remembered approval cannot spread to a different advisor.
		if v.Command != name {
			t.Errorf("%s: command = %q", name, v.Command)
		}
	}
	// An advisor nobody configured (a hallucinated name) can never be the
	// silent case.
	if v := p.Decide(advisorCall("skynet")); v.Decision != PolicyAsk {
		t.Errorf("unknown advisor: decision = %s (%s)", v.Decision, v.Rule)
	}
}

func TestAdvisorToolLevelOverrides(t *testing.T) {
	deny := mustPolicy(t, PolicyConfig{
		Advisors: advisorTiers,
		Tools:    map[string]PolicyDecision{AdvisorToolName: PolicyDeny},
	})
	if v := deny.Decide(advisorCall("claude")); v.Decision != PolicyDeny {
		t.Errorf("deny must disable delegation entirely: %s", v.Decision)
	}

	// "allow" is the user saying they trust delegation wholesale — the
	// shell.run escape hatch, applied to advisors.
	allow := mustPolicy(t, PolicyConfig{
		Advisors: advisorTiers,
		Tools:    map[string]PolicyDecision{AdvisorToolName: PolicyAllow},
	})
	if v := allow.Decide(advisorCall("aider")); v.Decision != PolicyAllow {
		t.Errorf("explicit allow must trust every advisor: %s (%s)", v.Decision, v.Rule)
	}

	// "ask" is the opposite: confirm every consultation, even a read-only one.
	ask := mustPolicy(t, PolicyConfig{
		Advisors: advisorTiers,
		Tools:    map[string]PolicyDecision{AdvisorToolName: PolicyAsk},
	})
	if v := ask.Decide(advisorCall("claude")); v.Decision != PolicyAsk {
		t.Errorf("explicit ask must confirm every advisor: %s (%s)", v.Decision, v.Rule)
	}
}

func TestAdvisorUnparseableArgumentsAsk(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{Advisors: advisorTiers})
	for _, args := range []string{`not json`, `{}`, `{"advisor":"  "}`} {
		v := p.Decide(ai.ToolCall{Name: AdvisorToolName, Arguments: args})
		if v.Decision != PolicyAsk {
			t.Errorf("arguments %q: decision = %s", args, v.Decision)
		}
		if v.Summary == "" {
			t.Errorf("arguments %q: ask tier needs a spoken summary", args)
		}
	}
}

func TestAdvisorPolicyValidation(t *testing.T) {
	_, err := NewPolicy(PolicyConfig{Advisors: map[string]PolicyDecision{"claude": "maybe"}})
	if err == nil || !strings.Contains(err.Error(), "claude") {
		t.Errorf("invalid advisor decision must be rejected with an actionable message: %v", err)
	}
	// With no advisors configured at all the tool still resolves to a tier,
	// so a call that somehow arrives cannot run silently.
	p := mustPolicy(t, PolicyConfig{})
	if p.ToolDecision(AdvisorToolName) != PolicyAsk {
		t.Error("advisor.ask must default to ask (classify)")
	}
	if v := p.Decide(advisorCall("claude")); v.Decision != PolicyAsk {
		t.Errorf("unconfigured advisor: decision = %s", v.Decision)
	}
}
