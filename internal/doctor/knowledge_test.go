package doctor

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
)

// The offline half of the feed checks (ADR 0031): command presence and the
// no-daemon degradation. The live summary's numbers come from
// knowledge.status, whose composition is the daemon's and is tested there.

func feedsConfig(command string) config.Config {
	cfg := config.Config{}
	cfg.Knowledge.Feeds = []config.KnowledgeFeed{{
		Name: "amd", Description: "AMD share price",
		Command: config.Command{command},
	}}
	return cfg
}

func TestKnowledgeChecksWithNoFeeds(t *testing.T) {
	results := knowledgeChecks(config.Config{}, config.Paths{})
	if len(results) != 1 || results[0].Status != OK {
		t.Fatalf("results = %+v, want one OK 'none configured'", results)
	}
	if !strings.Contains(results[0].Detail, "none configured") {
		t.Errorf("detail = %q, want the feature named as absent", results[0].Detail)
	}
}

func TestKnowledgeChecksFlagAMissingCommand(t *testing.T) {
	paths := config.Paths{Socket: filepath.Join(t.TempDir(), "no-daemon.sock")}
	results := knowledgeChecks(feedsConfig("jarvix-not-installed-anywhere"), paths)

	cmd, found := resultNamed(results, `feed "amd" command`)
	if !found || cmd.Status != Warn {
		t.Fatalf("command check = %+v, want a Warn for the missing binary", cmd)
	}
	if !strings.Contains(cmd.Fix, "amd") {
		t.Errorf("fix = %q, want the feed named", cmd.Fix)
	}
}

func TestKnowledgeChecksReportPresentCommandAndDeadDaemon(t *testing.T) {
	present := stubBinary(t, "amd-price")
	paths := config.Paths{Socket: filepath.Join(t.TempDir(), "no-daemon.sock")}
	results := knowledgeChecks(feedsConfig(present), paths)

	cmd, found := resultNamed(results, `feed "amd" command`)
	if !found || cmd.Status != OK || cmd.Detail != present {
		t.Fatalf("command check = %+v, want OK with the resolved path", cmd)
	}
	live, found := resultNamed(results, "knowledge feeds")
	if !found || live.Status != Warn {
		t.Fatalf("live check = %+v, want a Warn with no daemon to ask", live)
	}
	if !strings.Contains(live.Detail, "not running") {
		t.Errorf("detail = %q, want the dead daemon named", live.Detail)
	}
}
