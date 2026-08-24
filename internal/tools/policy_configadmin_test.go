package tools

import (
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
)

// The self-configuration tier tests (issue #105, ADR 0036). The entry write
// verbs carry script.run's floor — ask by default, unreachable by a global
// allow, reachable by a global deny, silenced only by naming the tool — and
// each case here is the mutation check for one arm of that asymmetry. The
// exclusion wall's own tests live in configadmin_test.go beside the tools.

func decideConfigWrite(t *testing.T, cfg PolicyConfig, tool string) Verdict {
	t.Helper()
	p, err := NewPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p.Decide(ai.ToolCall{Name: tool, Arguments: `{"family":"scripts","entry":{"name":"x"}}`})
}

func TestConfigWriteVerbsDefaultToAsk(t *testing.T) {
	for _, tool := range []string{ConfigWriteEntryToolName, ConfigDeleteEntryToolName} {
		v := decideConfigWrite(t, PolicyConfig{}, tool)
		if v.Decision != PolicyAsk {
			t.Errorf("%s decision = %q, want ask", tool, v.Decision)
		}
	}
}

// TestConfigWriteVerbsIgnoreAGlobalAllow: everything these verbs write will
// later run — a script's path, a feed's argv, a routine's launches — so a
// global allow must not reach them. This is the test that fails if
// ToolDecision's authoring-floor branch is removed.
func TestConfigWriteVerbsIgnoreAGlobalAllow(t *testing.T) {
	for _, tool := range []string{ConfigWriteEntryToolName, ConfigDeleteEntryToolName} {
		v := decideConfigWrite(t, PolicyConfig{Default: PolicyAllow}, tool)
		if v.Decision != PolicyAsk {
			t.Errorf("a global allow reached %s: %q (%s)", tool, v.Decision, v.Rule)
		}
		if !strings.Contains(v.Rule, "unless the configuration names it") {
			t.Errorf("%s rule %q does not state the floor", tool, v.Rule)
		}
	}
}

// TestConfigWriteVerbsHonourAGlobalDeny: the exception runs one way —
// tightening always wins.
func TestConfigWriteVerbsHonourAGlobalDeny(t *testing.T) {
	for _, tool := range []string{ConfigWriteEntryToolName, ConfigDeleteEntryToolName} {
		if v := decideConfigWrite(t, PolicyConfig{Default: PolicyDeny}, tool); v.Decision != PolicyDeny {
			t.Errorf("a global deny did not reach %s: %q", tool, v.Decision)
		}
	}
}

// TestConfigWriteVerbsExplicitEntriesWin: naming the tool is the sentence a
// user has to mean, and it works in every direction.
func TestConfigWriteVerbsExplicitEntriesWin(t *testing.T) {
	for _, tool := range []string{ConfigWriteEntryToolName, ConfigDeleteEntryToolName} {
		for _, tier := range []PolicyDecision{PolicyAllow, PolicyAsk, PolicyDeny} {
			v := decideConfigWrite(t, PolicyConfig{
				Tools: map[string]PolicyDecision{tool: tier},
			}, tool)
			if v.Decision != tier {
				t.Errorf("%s explicit %q → decision %q", tool, tier, v.Decision)
			}
		}
	}
}

// TestConfigReadVerbsDefaultToAllow: the reads look at the user's own
// config.toml and change nothing; a toll booth before the read the write
// discipline requires would be the wrong default.
func TestConfigReadVerbsDefaultToAllow(t *testing.T) {
	p, err := NewPolicy(PolicyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{ConfigListEntriesToolName, ConfigGetEntryToolName, ConfigReadSettingsToolName} {
		if got := p.ToolDecision(tool); got != PolicyAllow {
			t.Errorf("%s default = %q, want allow", tool, got)
		}
	}
}

// TestConfigWriteSettingFollowsTheNormalPolicy: the settings verb has no
// blanket floor of its own — the shipped default (ask) asks, a global allow
// reaches BENIGN keys — because the always-confirm floor for dangerous keys
// is per-key, applied by escalation (configadmin_test.go proves that arm).
func TestConfigWriteSettingFollowsTheNormalPolicy(t *testing.T) {
	p, err := NewPolicy(PolicyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.ToolDecision(ConfigWriteSettingToolName); got != PolicyAsk {
		t.Errorf("shipped default for %s = %q, want ask", ConfigWriteSettingToolName, got)
	}
	p, err = NewPolicy(PolicyConfig{Default: PolicyAllow})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.ToolDecision(ConfigWriteSettingToolName); got != PolicyAllow {
		t.Errorf("global allow for %s = %q, want allow (dangerous keys escalate per call)",
			ConfigWriteSettingToolName, got)
	}
}
