package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
)

// loadConfigString parses a config fragment exactly as the daemon would,
// defaults and advisor presets included.
func loadConfigString(t *testing.T, body string) (config.Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Load(path)
}

// newTestDaemon wires a daemon over fakes without serving: enough to inspect
// what configuration turned into.
func newTestDaemon(t *testing.T, cfg config.Config) *Daemon {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock")}
	d, err := New(cfg, paths, nil, Deps{
		Provider:    &ai.Fake{},
		Transcriber: &stt.Fake{},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestAdvisorPolicyTiersFollowWhatTheAdvisorCanDo(t *testing.T) {
	cfg := config.Config{Advisors: map[string]config.Advisor{
		"claude": {Binary: "/usr/bin/claude", ReadOnly: true},
		"aider":  {Binary: "/usr/bin/aider"},
	}}
	tiers := advisorPolicyTiers(cfg)
	if tiers["claude"] != tools.PolicyAllow {
		t.Errorf("an advisor that only answers should run silently: %s", tiers["claude"])
	}
	if tiers["aider"] != tools.PolicyAsk {
		t.Errorf("an advisor that can act must be confirmed: %s", tiers["aider"])
	}
}

func TestProviderKeysAreWithheldFromAdvisors(t *testing.T) {
	cfg := testConfig()
	names := strings.Join(providerKeyEnvNames(cfg), " ")
	for _, want := range []string{"OPENAI_API_KEY", "OPENROUTER_API_KEY"} {
		if !strings.Contains(names, want) {
			t.Errorf("%s must be scrubbed from advisor environments (got %q)", want, names)
		}
	}
}

func TestAdvisorSpecsAreOrderedAndComplete(t *testing.T) {
	cfg, err := loadConfigString(t, `
[advisors.zeta]
binary = "/usr/bin/zeta"

[advisors.claude]
binary = "/usr/bin/claude"
`)
	if err != nil {
		t.Fatal(err)
	}
	specs := advisorSpecs(cfg)
	if len(specs) != 2 || specs[0].Name != "claude" || specs[1].Name != "zeta" {
		t.Fatalf("specs = %+v (want a stable, sorted order)", specs)
	}
	if specs[0].Timeout == 0 || len(specs[0].Args) == 0 || specs[0].Description == "" {
		t.Errorf("preset defaults did not reach the tool: %+v", specs[0])
	}
}

// TestAdvisorToolIsRegisteredOnlyWhenConfigured pins the enablement rule:
// configuring an advisor is all it takes, and configuring none leaves the
// tool (and its system prompt) out entirely.
func TestAdvisorToolIsRegisteredOnlyWhenConfigured(t *testing.T) {
	withAdvisor, err := loadConfigString(t, "[advisors.claude]\nbinary = \"/usr/bin/claude\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if names := newTestDaemon(t, withAdvisor).registry.Names(); !contains(names, tools.AdvisorToolName) {
		t.Errorf("advisor.ask should be registered: %v", names)
	}

	none := testConfig()
	if names := newTestDaemon(t, none).registry.Names(); contains(names, tools.AdvisorToolName) {
		t.Errorf("advisor.ask must not be registered with no advisors: %v", names)
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
