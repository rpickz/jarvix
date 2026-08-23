package config

import (
	"strings"
	"testing"
)

// parseKnowledge runs a document through the real load path — parse plus
// defaulting — so tests see exactly what the daemon would.
func parseKnowledge(t *testing.T, doc string) Config {
	t.Helper()
	cfg, err := parse([]byte(doc), Default())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

func TestKnowledgeFeedDefaults(t *testing.T) {
	cfg := parseKnowledge(t, `
[[knowledge.feeds]]
name = "amd"
description = "AMD share price"
command = ["/home/me/bin/amd-price"]
`)
	if len(cfg.Knowledge.Feeds) != 1 {
		t.Fatalf("feeds = %d, want 1", len(cfg.Knowledge.Feeds))
	}
	f := cfg.Knowledge.Feeds[0]
	if f.Mode != FeedModeEager {
		t.Errorf("mode = %q, want eager by default — being ready is the point", f.Mode)
	}
	if f.IntervalSec != DefaultFeedIntervalSec {
		t.Errorf("interval_sec = %d, want the %d default", f.IntervalSec, DefaultFeedIntervalSec)
	}
	if f.TTLSec != 2*DefaultFeedIntervalSec {
		t.Errorf("ttl_sec = %d, want twice the interval for an eager feed", f.TTLSec)
	}
	if f.TimeoutSec != DefaultFeedTimeoutSec {
		t.Errorf("timeout_sec = %d, want the %d default", f.TimeoutSec, DefaultFeedTimeoutSec)
	}
	if f.Inject {
		t.Error("inject defaulted on; per-turn injection must be opted into")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a minimal feed table failed validation: %v", err)
	}
	if f.Enabled == nil || !*f.Enabled {
		t.Error("enabled did not default to true — the [[family]] convention (issue #92)")
	}
}

// TestKnowledgeFeedEnabledSwitch: `enabled = false` parks a feed, the entry
// stays fully validated, and a disabled feed still fails validation when it
// is broken — re-enabling must never surprise.
func TestKnowledgeFeedEnabledSwitch(t *testing.T) {
	cfg := parseKnowledge(t, `
[[knowledge.feeds]]
name = "amd"
description = "AMD share price"
command = ["/home/me/bin/amd-price"]
enabled = false
`)
	if cfg.Knowledge.Feeds[0].IsEnabled() {
		t.Error("enabled = false was not honoured")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a disabled feed failed validation: %v", err)
	}
	broken := parseKnowledge(t, `
[[knowledge.feeds]]
name = "amd"
command = []
enabled = false
`)
	if err := broken.Validate(); err == nil {
		t.Error("a disabled feed skipped validation; re-enabling it could then surprise")
	}
}

func TestKnowledgeLazyTTLDefault(t *testing.T) {
	cfg := parseKnowledge(t, `
[[knowledge.feeds]]
name = "weather"
description = "local weather"
command = ["/home/me/bin/weather"]
mode = "lazy"
`)
	if ttl := cfg.Knowledge.Feeds[0].TTLSec; ttl != DefaultFeedTTLSec {
		t.Errorf("lazy ttl_sec = %d, want the %d default", ttl, DefaultFeedTTLSec)
	}
}

func TestKnowledgeValidationProblems(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		want string
	}{
		{"missing name", `
[[knowledge.feeds]]
description = "x"
command = ["/bin/x"]
`, "name is empty"},
		{"bad mode", `
[[knowledge.feeds]]
name = "amd"
command = ["/bin/x"]
mode = "sometimes"
`, `mode "sometimes" is not supported`},
		{"empty command", `
[[knowledge.feeds]]
name = "amd"
mode = "lazy"
`, "command is empty"},
		{"duplicate names", `
[[knowledge.feeds]]
name = "amd"
command = ["/bin/x"]
[[knowledge.feeds]]
name = "AMD"
command = ["/bin/y"]
`, "duplicate feed name"},
		{"interval too fast", `
[[knowledge.feeds]]
name = "amd"
command = ["/bin/x"]
interval_sec = 5
`, "must not refresh faster"},
		{"ttl below interval", `
[[knowledge.feeds]]
name = "amd"
command = ["/bin/x"]
interval_sec = 600
ttl_sec = 60
`, "stale before its refresh"},
		{"negative timeout", `
[[knowledge.feeds]]
name = "amd"
command = ["/bin/x"]
timeout_sec = -1
`, "timeout_sec must be positive"},
		{"injection budget too small", `
[knowledge]
max_injected_tokens = 10
[[knowledge.feeds]]
name = "amd"
command = ["/bin/x"]
`, "max_injected_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := parseKnowledge(t, tc.doc)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("validation passed, want a problem")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("problems do not name the issue:\n%v\nwant %q", err, tc.want)
			}
		})
	}
}

func TestKnowledgeSystemPromptAppendsWithFeeds(t *testing.T) {
	cfg := Default()
	if strings.Contains(AssistantSystemPrompt(cfg), "knowledge.get") {
		t.Error("the feed prompt is present with no feeds configured")
	}
	cfg.Knowledge.Feeds = []KnowledgeFeed{{Name: "amd", Command: Command{"/bin/x"}}}
	if !strings.Contains(AssistantSystemPrompt(cfg), "knowledge.get") {
		t.Error("the feed prompt is missing with a feed configured")
	}
}
