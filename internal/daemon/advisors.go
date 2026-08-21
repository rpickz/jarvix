package daemon

import (
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/tools"
)

// This file translates `[advisors.<name>]` configuration into what the tool
// layer needs (ADR 0016). It lives apart from the wiring in daemon.go because
// two of the three translations are security decisions, not plumbing: which
// advisors may run without asking, and which environment variables must never
// reach one.

// advisorSpecs builds the tool's advisor list in a stable order, so the tool
// schema the model sees does not change shape between daemon restarts.
func advisorSpecs(cfg config.Config) []tools.AdvisorSpec {
	names := cfg.AdvisorNames()
	specs := make([]tools.AdvisorSpec, 0, len(names))
	for _, name := range names {
		a := cfg.Advisors[name]
		specs = append(specs, tools.AdvisorSpec{
			Name:        name,
			Binary:      a.Binary,
			Args:        a.Args,
			Timeout:     time.Duration(a.TimeoutSec) * time.Second,
			Description: a.Description,
		})
	}
	return specs
}

// advisorPolicyTiers decides how much confirmation each advisor needs.
//
// Consulting an advisor sends the user's question to another program, and
// that program's own powers decide the risk: a CLI in a one-shot answering
// mode reads and replies, which is no more than Jarvix already does with the
// model, so it runs silently. A coding agent that edits files and runs
// commands is an action on the machine, and so is any advisor whose argv the
// user wrote by hand — Jarvix has not audited it and cannot claim it only
// answers. Both ask first.
func advisorPolicyTiers(cfg config.Config) map[string]tools.PolicyDecision {
	tiers := make(map[string]tools.PolicyDecision, len(cfg.Advisors))
	for name, a := range cfg.Advisors {
		tier := tools.PolicyAsk
		if a.ReadOnly {
			tier = tools.PolicyAllow
		}
		tiers[name] = tier
	}
	return tiers
}

// providerKeyEnvNames lists the environment variables Jarvix reads its own
// API keys from. They are scrubbed from every advisor's environment on top of
// the built-in secret-name patterns: an advisor carries its own
// authentication, and Jarvix's credentials are not its to spend.
func providerKeyEnvNames(cfg config.Config) []string {
	names := make([]string, 0, len(cfg.AI.Endpoints))
	for _, ep := range cfg.AI.Endpoints {
		if ep.APIKeyEnv != "" {
			names = append(names, ep.APIKeyEnv)
		}
	}
	return names
}
