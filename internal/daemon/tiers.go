package daemon

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/ai/openaicompat"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/tools"
)

// This file turns `[ai.tiers]` into the engine's bindings (issue #159, ADR
// 0062). It is the one place a tier stops being configuration and becomes a
// client with a model name on it.
//
// Nothing here builds a second HTTP client type or a second way of reaching an
// assistant CLI. An endpoint-backed tier is openaicompat.New — the same
// constructor the single-brain path has always used, pointed at the endpoint
// the tier names — and an advisor-backed tier is the ADR 0016 bridge wearing
// the ai.Provider interface. That is the "one routing seam, no per-tier
// special-casing at call sites" requirement stated as code: by the time the
// engine sees a tier it is a Provider and a string.

// tierSet builds the engine's tier bindings.
//
// brain is the [ai] provider the daemon already built — the model this
// configuration has always used. It is what medium binds to when there is no
// [ai.tiers.medium] table, which is the whole of the backwards-compatibility
// promise: an existing config that adds only [ai.tiers.instant] keeps
// answering ordinary turns from exactly the model it answered them from
// yesterday.
//
// A tier whose endpoint or advisor cannot be resolved is left unbound rather
// than bound to something else. Validation has already refused such a document
// at the save that wrote it, so this is the hand-edited-file path; binding it
// to the brain would make "deep" quietly mean "medium", which is precisely the
// silent downgrade this feature exists to not do.
func tierSet(cfg config.Config, brain ai.Provider, log *slog.Logger) session.TierSet {
	if !cfg.AI.Tiers.Enabled() {
		return session.TierSet{}
	}
	set := session.TierSet{
		Default:  ai.TierMedium,
		Bindings: map[ai.Tier]session.TierBinding{},
	}
	if d, ok := ai.ParseTier(cfg.AI.Tiers.Default); ok {
		set.Default = d
	}

	// One bridge for every advisor-backed tier, built from the same specs and
	// with the same credential scrubbing as the advisor.ask tool, so a tier
	// and a consultation can never disagree about how an advisor is run.
	var bridge *tools.Advisor
	if len(cfg.Advisors) > 0 {
		bridge = &tools.Advisor{
			Advisors: advisorSpecs(cfg),
			ScrubEnv: providerKeyEnvNames(cfg),
			Log:      log,
		}
	}

	for _, tier := range ai.TierOrder() {
		table, present := cfg.AI.Tiers.Tiers[string(tier)]
		switch {
		case present && table.Advisor != "":
			if bridge == nil {
				continue
			}
			set.Bindings[tier] = session.TierBinding{
				Provider:     &tools.AdvisorProvider{Advisor: bridge, AdvisorName: table.Advisor},
				Advisor:      table.Advisor,
				HistoryTurns: table.HistoryTurns,
			}
		case present && table.Provider != "":
			ep, ok := cfg.AI.Endpoints[table.Provider]
			if !ok {
				continue
			}
			set.Bindings[tier] = session.TierBinding{
				Provider:     openaicompat.New(table.Provider, ep.BaseURL, ep.Key()),
				Model:        table.Model,
				HistoryTurns: table.HistoryTurns,
			}
		case tier == ai.TierMedium:
			// Medium with no table of its own is the [ai] brain. Not a
			// fallback for the other two: an absent instant or deep does not
			// exist, and the router says so rather than serving this.
			set.Bindings[tier] = session.TierBinding{Provider: brain, Model: cfg.AI.Model}
		}
	}
	if _, ok := set.Bindings[set.Default]; !ok {
		set.Default = ai.TierMedium
	}
	return set
}

// registerThinkingMethods adds the thinking-level surface (#159): read the
// level and the levels this machine actually has, and move it.
//
// Two methods rather than a field on config.set, because the level is not
// configuration. It lives for one conversation, it is moved by a control next
// to the composer and by a spoken phrase, and writing it into config.toml
// would make "quick answer, just this once" a permanent decision about every
// future conversation.
func (d *Daemon) registerThinkingMethods() {
	d.server.Handle("thinking.get", func(json.RawMessage) (any, error) {
		return d.thinkingReport(), nil
	})
	d.server.Handle("thinking.set", func(params json.RawMessage) (any, error) {
		var p struct {
			Thinking string `json:"thinking"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "invalid thinking.set params: %v", err)
			}
		}
		tier, ok := ai.ParseTier(strings.TrimSpace(p.Thinking))
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"thinking must be one of instant, medium or deep")
		}
		if _, err := d.engine.SetThinking(tier); err != nil {
			// A level this machine cannot serve is a refusal the control shows
			// where it stands, not a failure at answer time — which is the
			// whole point of asking here rather than finding out mid-turn.
			return nil, ipc.Errorf(ipc.CodeSessionError, "%s", err.Error())
		}
		return d.thinkingReport(), nil
	})
}

// thinkingReport is the shape both thinking methods answer with, so a set and
// a get can never describe the level two different ways.
func (d *Daemon) thinkingReport() map[string]any {
	level := d.engine.Thinking()
	return map[string]any{
		"thinking":       string(level),
		"thinking_label": ai.TierLabel(level),
		"levels":         d.thinkingLevels(),
	}
}

// thinkingLevels describes the levels this machine can actually serve, in
// TierOrder. Every level is listed, including the ones with no tier
// configured: a control that silently omitted "Deep" would leave the user
// wondering whether the feature exists, where one that shows it as
// unavailable tells them what to configure. `available` is the field the
// control disables on, and it is the same answer thinking.set would give.
func (d *Daemon) thinkingLevels() []map[string]any {
	// Asked of the engine rather than rebuilt from config: the engine holds
	// the bindings a reload actually applied, and a control drawn from the
	// file could offer a level the running daemon has not been given yet.
	available := map[ai.Tier]bool{}
	for _, tier := range d.engine.AvailableTiers() {
		available[tier] = true
	}
	levels := make([]map[string]any, 0, len(ai.TierOrder()))
	for _, tier := range ai.TierOrder() {
		ok := available[tier]
		levels = append(levels, map[string]any{
			"tier":        string(tier),
			"label":       ai.TierLabel(tier),
			"description": ai.TierDescription(tier),
			"available":   ok,
		})
	}
	return levels
}
