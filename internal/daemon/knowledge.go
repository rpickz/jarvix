package daemon

// This file wires the feed cache (ADR 0030) into jarvixd: the config → spec
// conversion, the knowledge.status IPC method (doctor's window into the
// scheduler, and the read-only feed listing — feeds are hand-edited TOML like
// [[routines]], outside the config.set surface), and the reload hook that
// rebuilds the schedules when the tables change.
//
// Values travel over the status method deliberately: it is the user's own
// data, asked for over their own 0600 socket — the memory.list precedent.
// Events and logs still carry names and counts only.

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/knowledge"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/tools"
)

// feedSpecs converts the [[knowledge.feeds]] tables into the knowledge
// package's specs. The tool package deliberately does not import config —
// same as advisorSpecs.
func feedSpecs(cfg config.Config) []knowledge.Feed {
	specs := make([]knowledge.Feed, 0, len(cfg.Knowledge.Feeds))
	for _, f := range cfg.Knowledge.Feeds {
		mode := knowledge.ModeEager
		if f.Mode == config.FeedModeLazy {
			mode = knowledge.ModeLazy
		}
		specs = append(specs, knowledge.Feed{
			Name:        f.Name,
			Description: f.Description,
			Argv:        append([]string(nil), f.Command...),
			Mode:        mode,
			Interval:    time.Duration(f.IntervalSec) * time.Second,
			TTL:         time.Duration(f.TTLSec) * time.Second,
			Timeout:     time.Duration(f.TimeoutSec) * time.Second,
			Inject:      f.Inject,
		})
	}
	return specs
}

// newKnowledgeService builds the feed service from configuration, or nil
// when no feeds are configured — disabled means absent, like memory. The
// gate is consulted here, once: the tools section is restart-class, so the
// background-refresh decision holds for the daemon's life.
func newKnowledgeService(cfg config.Config, paths config.Paths, policy *tools.Policy,
	log *slog.Logger) *knowledge.Service {
	if len(cfg.Knowledge.Feeds) == 0 {
		return nil
	}
	refreshAllowed := policy.ToolDecision(tools.KnowledgeRefreshToolName) == tools.PolicyAllow
	svc := knowledge.NewService(paths.FeedsFile(), knowledge.Options{
		Feeds:             feedSpecs(cfg),
		MaxInjectedTokens: cfg.Knowledge.MaxInjectedTokens,
		RefreshAllowed:    refreshAllowed,
		ScrubEnv:          providerKeyEnvNames(cfg),
	}, log)
	log.Info("knowledge feeds enabled", "component", "knowledge",
		"feeds", cfg.KnowledgeFeedNames(), "path", paths.FeedsFile(),
		"refresh_allowed", refreshAllowed)
	return svc
}

// knowledgeInjector adapts the feed service for the engine, or leaves the
// option nil when feeds are disabled. Same typed-nil trap as memoryInjector:
// disabled must mean absent.
func knowledgeInjector(svc *knowledge.Service) session.KnowledgeInjector {
	if svc == nil {
		return nil
	}
	return svc
}

// knowledgeDrain and knowledgeInFlight adapt the (possibly absent) service
// to the shutdown stage table: with no feeds configured the stage is already
// quiescent.
func (d *Daemon) knowledgeDrain(ctx context.Context) error {
	if d.knowledge == nil {
		return nil
	}
	return d.knowledge.Drain(ctx)
}

func (d *Daemon) knowledgeInFlight() int {
	if d.knowledge == nil {
		return 0
	}
	return d.knowledge.InFlight()
}

// registerKnowledgeMethods adds the feed status IPC surface. Registered even
// with feeds disabled, answering enabled=false, so a client can tell
// "switched off" from "old daemon" — the memory.* precedent.
func (d *Daemon) registerKnowledgeMethods() {
	d.server.Handle("knowledge.status", func(json.RawMessage) (any, error) {
		if d.knowledge == nil {
			return map[string]any{"enabled": false}, nil
		}
		statuses := d.knowledge.Status()
		feeds := make([]map[string]any, 0, len(statuses))
		for _, st := range statuses {
			entry := map[string]any{
				"name":      st.Name,
				"mode":      string(st.Mode),
				"inject":    st.Inject,
				"has_value": st.HasValue,
				"stale":     st.Stale,
				"failing":   st.Failing,
			}
			if st.HasValue {
				entry["value"] = st.Value
				entry["fetched"] = st.FetchedAt.Format(time.RFC3339)
				entry["age_sec"] = int(st.Age / time.Second)
			}
			if st.Failing {
				entry["failing_since"] = st.FailingSince.Format(time.RFC3339)
				entry["attempts"] = st.Attempts
				entry["last_error"] = st.LastErr
			}
			feeds = append(feeds, entry)
		}
		return map[string]any{
			"enabled": true,
			"path":    d.knowledge.Path(),
			"feeds":   feeds,
		}, nil
	})
}
