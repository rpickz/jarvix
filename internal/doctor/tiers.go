package doctor

import (
	"context"
	"fmt"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/ai/openaicompat"
	"github.com/rpickz/jarvix/internal/config"
)

// This file reports the model tiers (issue #159, ADR 0063): one row per
// configured tier, saying by name and by endpoint whether it can answer.
//
// It is a *real* probe, on the terms #113/#114 established for the speech
// engines and the provider endpoint. The lesson those two paid for was that
// "configured" and "working" are different questions, and that a diagnostic
// answering the first while appearing to answer the second is worse than no
// diagnostic at all: doctor printed "[OK] whisper.cpp installed" for two days
// while every session died at transcription. A tier is exactly that shape
// again — a table in a file naming a machine that may or may not be listening
// — and the failure it would otherwise produce is the one this feature must
// never have: a user asks for the deep model, waits, and hears an answer that
// quietly came from somewhere else. **A tier that cannot answer fails here,
// not mid-conversation.**
//
// The advisor half is the exception, and it is the same exception
// advisorChecks already argues (ADR 0016): presence is checked with
// exec.LookPath and nothing else. Invoking an assistant CLI to see whether it
// works would spend the user's own budget every time they ran `jarvix doctor`,
// and the failure mode a probe would catch — a CLI that is installed but not
// authenticated — is one the CLI reports for itself the first time it is used.

// tierProbeTimeout bounds one tier's endpoint probe. The same budget the
// provider endpoint's own probe uses (#114): the request is identical, so
// waiting a different length of time for it would be a second opinion about
// the same question.
const tierProbeTimeout = 10 * time.Second

// tierChecks reports one result per configured model tier. It returns nothing
// at all when no tiers are configured: a row saying "tiers: none" on every
// machine that has never heard of the feature is noise in a report whose value
// is that every line is worth reading.
func tierChecks(cfg config.Config) []Result {
	if !cfg.AI.Tiers.Enabled() {
		return nil
	}
	var results []Result
	for _, tier := range ai.TierOrder() {
		table, present := cfg.AI.Tiers.Tiers[string(tier)]
		if !present {
			continue
		}
		results = append(results, tierResult(cfg, tier, table, tierProbeTimeout))
	}
	return results
}

// tierResult probes one tier. The timeout is a parameter for #114's reason:
// the budget is part of what the check promises, so it has to be testable
// without waiting it out.
func tierResult(cfg config.Config, tier ai.Tier, table config.AITier, timeout time.Duration) Result {
	name := fmt.Sprintf("%s tier answers", ai.TierLabel(tier))

	if table.Advisor != "" {
		advisor, ok := cfg.Advisors[table.Advisor]
		if !ok {
			return Result{Status: Fail, Name: name,
				Detail: fmt.Sprintf("advisor %q is not configured", table.Advisor),
				Fix: fmt.Sprintf("Add an [advisors.%s] table, or point ai.tiers.%s at another advisor",
					table.Advisor, tier)}
		}
		path, err := lookAdvisor(advisor.Binary)
		if err != nil {
			return Result{Status: Fail, Name: name,
				Detail: fmt.Sprintf("advisor %s: %s not found (%v)", table.Advisor, advisor.Binary, err),
				Fix: fmt.Sprintf("Install %s, or point ai.tiers.%s at an endpoint instead",
					table.Advisor, tier)}
		}
		return Result{Status: OK, Name: name,
			Detail: fmt.Sprintf("advisor %s → %s", table.Advisor, path)}
	}

	ep, ok := cfg.AI.Endpoints[table.Provider]
	if !ok {
		return Result{Status: Fail, Name: name,
			Detail: fmt.Sprintf("provider %q has no endpoint", table.Provider),
			Fix:    "Add an [ai." + table.Provider + "] table with a base_url"}
	}
	where := fmt.Sprintf("%s → %s (model %s)", table.Provider, ep.BaseURL, table.Model)
	report := openaicompat.New(table.Provider, ep.BaseURL, ep.Key()).
		ProbeEndpoint(context.Background(), timeout)
	switch report.Outcome {
	case openaicompat.ProbeReachable:
		return Result{Status: OK, Name: name, Detail: where}
	case openaicompat.ProbeUnauthorised:
		// Told apart from unreachable because the fixes are different and the
		// user can act on the distinction: the address is right, the key is
		// not. The credential itself is never read, quoted, or named beyond
		// the variable it should live in.
		fix := "Check the credential for [ai." + table.Provider + "]"
		if ep.APIKeyEnv != "" {
			fix = "Export " + ep.APIKeyEnv + " in your environment (and in jarvixd's:\n" +
				"systemctl --user set-environment " + ep.APIKeyEnv + "=... && systemctl --user restart jarvixd)"
		}
		return Result{Status: Fail, Name: name,
			Detail: where + " answered, but rejected the credentials", Fix: fix}
	}
	return Result{Status: Fail, Name: name,
		Detail: where + " — " + report.Detail,
		Fix: fmt.Sprintf("Start or fix that endpoint, or remove the [ai.tiers.%s] table so "+
			"Jarvix stops offering a level it cannot serve", tier)}
}
