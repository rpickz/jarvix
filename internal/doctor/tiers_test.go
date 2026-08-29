package doctor

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/config"
)

// The tier checks are real probes, so their tests are the probe tests' shape:
// a live httptest server stands in for an endpoint, PATH stands in for an
// installed advisor CLI, and no test reaches the network or the machine.

// tierConfig builds a config with one tier pointed at the given endpoint.
func tierConfig(t *testing.T, tier ai.Tier, table config.AITier, baseURL string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.AI.Endpoints["probe"] = config.Endpoint{BaseURL: baseURL}
	cfg.AI.Tiers = config.AITiers{
		Default: "medium",
		Tiers:   map[string]config.AITier{string(tier): table},
	}
	return cfg
}

// tierResultFor runs the one check under test with a short budget, so a
// deliberately dead endpoint costs milliseconds rather than the real ten
// seconds — the #114 discipline of making the budget a parameter.
func tierResultFor(t *testing.T, cfg config.Config, tier ai.Tier) Result {
	t.Helper()
	return tierResult(cfg, tier, cfg.AI.Tiers.Tiers[string(tier)], 200*time.Millisecond)
}

// Nothing at all with no tiers: a row saying "tiers: none" on every machine
// that has never heard of the feature is noise in a report whose value is that
// every line is worth reading.
func TestNoTiersMeansNoTierRows(t *testing.T) {
	if got := tierChecks(config.Default()); len(got) != 0 {
		t.Errorf("tierChecks = %v with no tiers configured, want none", got)
	}
}

func TestAReachableTierPassesAndNamesItsEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)

	cfg := tierConfig(t, ai.TierInstant,
		config.AITier{Provider: "probe", Model: "qwen3-1.7b"}, srv.URL)
	r := tierResultFor(t, cfg, ai.TierInstant)
	if r.Status != OK {
		t.Fatalf("status = %v, want OK (%s)", r.Status, r.Detail)
	}
	if !strings.Contains(r.Name, "Quick") {
		t.Errorf("name = %q, want the level's own word", r.Name)
	}
	for _, want := range []string{"probe", srv.URL, "qwen3-1.7b"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail %q does not name %q", r.Detail, want)
		}
	}
}

// The whole point of the check: a tier that cannot answer fails here, not
// mid-conversation. #113's incident was a diagnostic that said OK while every
// session died, and a tier is that shape again.
func TestAnUnreachableTierFails(t *testing.T) {
	cfg := tierConfig(t, ai.TierDeep,
		config.AITier{Provider: "probe", Model: "big"}, "http://127.0.0.1:1")
	r := tierResultFor(t, cfg, ai.TierDeep)
	if r.Status != Fail {
		t.Fatalf("status = %v, want Fail — an unreachable tier must fail here", r.Status)
	}
	if r.Fix == "" {
		t.Error("no fix offered for an unreachable tier")
	}
}

// Told apart from unreachable, because the fixes are different and the user
// can act on the distinction. The credential itself is never read or quoted.
func TestAnUnauthorisedTierSaysTheKeyIsWrongRatherThanTheAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	cfg := tierConfig(t, ai.TierDeep,
		config.AITier{Provider: "probe", Model: "big"}, srv.URL)
	cfg.AI.Endpoints["probe"] = config.Endpoint{BaseURL: srv.URL, APIKeyEnv: "PROBE_KEY"}
	r := tierResultFor(t, cfg, ai.TierDeep)
	if r.Status != Fail {
		t.Fatalf("status = %v, want Fail", r.Status)
	}
	if !strings.Contains(r.Detail, "rejected the credentials") {
		t.Errorf("detail = %q, want it to name the credential rather than the address", r.Detail)
	}
	if !strings.Contains(r.Fix, "PROBE_KEY") {
		t.Errorf("fix = %q, want it to name the variable to export", r.Fix)
	}
	if strings.Contains(r.Detail+r.Fix, "secret") {
		t.Error("the check quoted something that looks like a credential")
	}
}

// An advisor-backed tier is checked with exec.LookPath and nothing else, for
// advisorChecks' reason: running an assistant CLI to see whether it works
// would spend the user's own budget on every `jarvix doctor`.
func TestAnAdvisorBackedTierIsCheckedByPresence(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	cfg := config.Default()
	cfg.Advisors = map[string]config.Advisor{"claude": {Binary: "claude"}}
	cfg.AI.Tiers = config.AITiers{Default: "medium", Tiers: map[string]config.AITier{
		"deep": {Advisor: "claude"},
	}}
	r := tierResultFor(t, cfg, ai.TierDeep)
	if r.Status != OK {
		t.Fatalf("status = %v, want OK (%s)", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "claude") {
		t.Errorf("detail = %q, want it to name the advisor", r.Detail)
	}

	// And a missing one fails, by name, with a fix.
	t.Setenv("PATH", t.TempDir())
	r = tierResultFor(t, cfg, ai.TierDeep)
	if r.Status != Fail {
		t.Fatalf("status = %v, want Fail for a missing advisor binary", r.Status)
	}
	if !strings.Contains(r.Fix, "claude") {
		t.Errorf("fix = %q, want it to name what to install", r.Fix)
	}
}

// Every configured tier gets a row, in the order every surface prints them.
func TestTierRowsAreOnePerConfiguredTierInOrder(t *testing.T) {
	cfg := config.Default()
	cfg.AI.Endpoints["probe"] = config.Endpoint{BaseURL: "http://127.0.0.1:1"}
	cfg.AI.Tiers = config.AITiers{Default: "medium", Tiers: map[string]config.AITier{
		"deep":    {Provider: "probe", Model: "big"},
		"instant": {Provider: "probe", Model: "small"},
	}}
	rows := tierChecks(cfg)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want one per configured tier", len(rows))
	}
	if !strings.HasPrefix(rows[0].Name, "Quick") || !strings.HasPrefix(rows[1].Name, "Deep") {
		t.Errorf("rows out of order: %q then %q", rows[0].Name, rows[1].Name)
	}
}
