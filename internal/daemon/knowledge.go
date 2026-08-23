package daemon

// This file wires the feed cache (ADR 0031) into jarvixd: the config → spec
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
	"os"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
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
			Enabled:     f.IsEnabled(),
		})
	}
	return specs
}

// newKnowledgeService builds the feed service from configuration, or nil
// when no feeds are configured — disabled means absent, like memory. The
// gate is consulted here, once: the tools section is restart-class, so the
// background-refresh decision holds for the daemon's life.
func newKnowledgeService(cfg config.Config, paths config.Paths, policy *tools.Policy,
	bus *session.Bus, log *slog.Logger) *knowledge.Service {
	if len(cfg.Knowledge.Feeds) == 0 {
		return nil
	}
	refreshAllowed := policy.ToolDecision(tools.KnowledgeRefreshToolName) == tools.PolicyAllow
	svc := knowledge.NewService(paths.FeedsFile(), knowledge.Options{
		Feeds:             feedSpecs(cfg),
		MaxInjectedTokens: cfg.Knowledge.MaxInjectedTokens,
		RefreshAllowed:    refreshAllowed,
		ScrubEnv:          providerKeyEnvNames(cfg),
		// Every completed fetch — scheduled or RefreshNow — is announced so
		// open windows refresh their cards. Names only, never values: the
		// value is fetched with knowledge.status, over the socket, on request.
		Notify: func(name string) {
			bus.Publish(session.Event{Type: "knowledge.updated",
				Data: map[string]any{"feed": name}})
		},
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

// registerKnowledgeMethods adds the feed IPC surface: the status listing, and
// the two admin verbs the window's cards call (issue #92) — refresh_now
// (fetch this feed, now, through the scheduled path) and set_enabled (park or
// resume this feed, persisted into config.toml through the surgical entry
// editor). Registered even with feeds disabled, answering enabled=false, so a
// client can tell "switched off" from "old daemon" — the memory.* precedent.
func (d *Daemon) registerKnowledgeMethods() {
	d.server.Handle("knowledge.status", func(json.RawMessage) (any, error) {
		if d.knowledge == nil {
			return map[string]any{"enabled": false}, nil
		}
		// The config file's fingerprint travels with the listing so a
		// set_enabled built from these cards can detect an external edit —
		// the config.get/config.set contract, restated for this surface.
		fp, err := config.FingerprintFile(d.paths.ConfigFile())
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, "read config: %v", err)
		}
		statuses := d.knowledge.Status()
		feeds := make([]map[string]any, 0, len(statuses))
		for _, st := range statuses {
			entry := map[string]any{
				"name":         st.Name,
				"mode":         string(st.Mode),
				"enabled":      st.Enabled,
				"interval_sec": int(st.Interval / time.Second),
				"ttl_sec":      int(st.TTL / time.Second),
				"inject":       st.Inject,
				"has_value":    st.HasValue,
				"stale":        st.Stale,
				"failing":      st.Failing,
			}
			if st.HasValue {
				entry["value"] = st.Value
				entry["fetched"] = st.FetchedAt.Format(time.RFC3339)
				entry["age_sec"] = int(st.Age / time.Second)
				// The spoken-style age every surface shares (ADR 0031's
				// honesty contract, carried to the eyes): worded here so the
				// window never invents its own scale.
				entry["age_spoken"] = st.AgeSpoken
			}
			if st.Failing {
				entry["failing_since"] = st.FailingSince.Format(time.RFC3339)
				entry["attempts"] = st.Attempts
				entry["last_error"] = st.LastErr
			}
			feeds = append(feeds, entry)
		}
		return map[string]any{
			"enabled":     true,
			"path":        d.knowledge.Path(),
			"fingerprint": fp,
			"feeds":       feeds,
		}, nil
	})

	// knowledge.refresh_now: an immediate fetch through the exact scheduled
	// path — same runner, same single-flight latch per feed, same gate
	// identity (knowledge.refresh must be allow, the decision the scheduler
	// itself runs under). The fetch is asynchronous; completion arrives as
	// the knowledge.updated event, like any scheduled fetch's.
	d.server.Handle("knowledge.refresh_now", func(params json.RawMessage) (any, error) {
		if d.knowledge == nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"no knowledge feeds are configured ([[knowledge.feeds]] in config.toml)")
		}
		var p struct {
			Name string `json:"name"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "knowledge.refresh_now params: %v", err)
			}
		}
		if err := d.knowledge.RefreshNow(p.Name); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"started": true}, nil
	})

	// knowledge.set_enabled: park or resume one feed, persisted. The write is
	// the settings discipline end to end (ADR 0015): fingerprint-checked
	// against external edits, applied through the surgical entry editor so
	// comments and sibling entries survive byte-for-byte, validated as a
	// whole configuration before anything lands, written atomically, and
	// picked up by the same reload path a hand edit uses — the scheduler
	// drops or readopts the feed through Reconfigure, values kept.
	d.server.Handle("knowledge.set_enabled", d.handleKnowledgeSetEnabled)
}

// handleKnowledgeSetEnabled is the set_enabled body; see the registration
// comment for the contract.
func (d *Daemon) handleKnowledgeSetEnabled(params json.RawMessage) (any, error) {
	if d.knowledge == nil {
		return nil, ipc.Errorf(ipc.CodeInvalidParams,
			"no knowledge feeds are configured ([[knowledge.feeds]] in config.toml)")
	}
	var p struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
		// Fingerprint is the config fingerprint from the client's
		// knowledge.status (or config.get). When present, a mismatch with
		// the file on disk fails the set — hand edits are never clobbered.
		Fingerprint string `json:"fingerprint"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "knowledge.set_enabled params: %v", err)
		}
	}

	path := d.paths.ConfigFile()
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, ipc.Errorf(ipc.CodeInternalError, "read config: %v", err)
	}
	fp := config.FingerprintMissing
	if raw != nil {
		fp = config.Fingerprint(raw)
	}
	if p.Fingerprint != "" && p.Fingerprint != fp {
		return nil, &ipc.Error{
			Code: ipc.CodeConfigConflict,
			Message: "config.toml changed on disk since it was read; " +
				"the feed list has been refreshed — try the switch again",
			Data: map[string]any{"fingerprint": fp},
		}
	}

	newRaw, err := config.SetEntryField(raw, "knowledge.feeds", p.Name, "enabled", p.Enabled)
	if err != nil {
		return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
	}
	fileCfg, err := config.ParseBytes(newRaw)
	if err != nil {
		return nil, ipc.Errorf(ipc.CodeInternalError, "rewrite config: %v", err)
	}
	fileCfg.Voices = fileCfg.InstalledVoices(d.paths)
	if err := fileCfg.Validate(); err != nil {
		return nil, &ipc.Error{
			Code:    ipc.CodeConfigInvalid,
			Message: "the change was rejected by validation; nothing was written",
			Data:    map[string]any{"problems": validationProblems(err)},
		}
	}
	if err := config.WriteFileAtomic(path, newRaw); err != nil {
		return nil, ipc.Errorf(ipc.CodeInternalError, "write config: %v", err)
	}

	applied, reason := d.applyRuntime(fileCfg)
	newFP := config.Fingerprint(newRaw)
	d.publishConfigChanged(newFP)
	result := map[string]any{
		"fingerprint": newFP,
		"applied":     applied,
	}
	if reason != "" {
		result["reason"] = reason
	}
	return result, nil
}
