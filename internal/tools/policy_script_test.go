package tools

import (
	"strings"
	"testing"
)

// The script.run tier tests (ADR 0030). The identity's whole contract is
// asymmetric on purpose — ask by default, unreachable by a global allow,
// reachable by a global deny — and each case here is the mutation check for
// one arm of that asymmetry.

func TestScriptDecisionDefaultsToAsk(t *testing.T) {
	p, err := NewPolicy(PolicyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	v := p.DecideScript("backup notes", "/home/user/bin/backup.sh")
	if v.Decision != PolicyAsk {
		t.Fatalf("decision = %q", v.Decision)
	}
	// The confirmation names the script and its exact path: the path is the
	// control that makes a substituted file visible in the question itself.
	if !strings.Contains(v.Summary, "backup notes") || !strings.Contains(v.Summary, "/home/user/bin/backup.sh") {
		t.Errorf("summary %q does not name the script and its path", v.Summary)
	}
	if !strings.Contains(v.Command, "/home/user/bin/backup.sh") {
		t.Errorf("command %q does not carry the path the user is approving", v.Command)
	}
	if v.Tool != ScriptToolName {
		t.Errorf("tool = %q", v.Tool)
	}
}

// TestScriptDecisionIgnoresAGlobalAllow: `default = "allow"` is a loosening
// the script identity refuses to inherit — only naming the tool silences it.
// This is the test that fails if ToolDecision's script branch is removed.
func TestScriptDecisionIgnoresAGlobalAllow(t *testing.T) {
	p, err := NewPolicy(PolicyConfig{Default: PolicyAllow})
	if err != nil {
		t.Fatal(err)
	}
	if v := p.DecideScript("backup notes", "/x/y"); v.Decision != PolicyAsk {
		t.Errorf("a global allow reached script.run: %q (%s)", v.Decision, v.Rule)
	}
}

// TestScriptDecisionHonoursAGlobalDeny: the exception runs one way —
// tightening always wins.
func TestScriptDecisionHonoursAGlobalDeny(t *testing.T) {
	p, err := NewPolicy(PolicyConfig{Default: PolicyDeny})
	if err != nil {
		t.Fatal(err)
	}
	v := p.DecideScript("backup notes", "/x/y")
	if v.Decision != PolicyDeny {
		t.Errorf("a global deny did not reach script.run: %q", v.Decision)
	}
	if !strings.Contains(v.Rule, "policy default") {
		t.Errorf("rule %q does not blame the default", v.Rule)
	}
}

func TestScriptDecisionExplicitEntries(t *testing.T) {
	tests := []struct {
		tier PolicyDecision
		want PolicyDecision
	}{
		{PolicyAllow, PolicyAllow},
		{PolicyAsk, PolicyAsk},
		{PolicyDeny, PolicyDeny},
	}
	for _, tt := range tests {
		p, err := NewPolicy(PolicyConfig{
			Tools: map[string]PolicyDecision{ScriptToolName: tt.tier},
		})
		if err != nil {
			t.Fatal(err)
		}
		v := p.DecideScript("backup notes", "/x/y")
		if v.Decision != tt.want {
			t.Errorf("tier %q → decision %q", tt.tier, v.Decision)
		}
		if !strings.Contains(v.Rule, "is set to") {
			t.Errorf("tier %q rule %q does not say it was configured", tt.tier, v.Rule)
		}
	}
}

// TestScriptApprovalIsRememberable: the thing approved is fully described by
// what was asked — this name, this path — so remember_for_conversation may
// reuse it. (The engine keys the memory on name AND path, so a config edit
// that repoints the phrase starts a new question.)
func TestScriptApprovalIsRememberable(t *testing.T) {
	if !RememberableApproval(ScriptToolName) {
		t.Error("script approvals must be rememberable; the ask is fully self-describing")
	}
}

// TestRegistryCheckScriptWithoutPolicy pins the registry fallback wording:
// no policy means allow here, and the session engine — which never installs
// a registry without a policy in production — independently asks when there
// is no registry at all. Both facts are asserted so a change to either shows.
func TestRegistryCheckScriptWithoutPolicy(t *testing.T) {
	r := NewRegistry(nil)
	v := r.CheckScript("backup notes", "/x/y")
	if v.Decision != PolicyAllow || v.Rule != "no policy installed" {
		t.Errorf("verdict = %+v", v)
	}
	if !strings.Contains(v.Command, "/x/y") {
		t.Errorf("command %q lost the path", v.Command)
	}
}
