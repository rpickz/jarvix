package daemon

// This file declares the two map-shaped families the Providers section edits
// (issue #163): the endpoints Jarvix thinks with and the advisor CLIs it
// consults. Both are registry ROWS — no CRUD of their own, no second write
// path — which is the whole architectural claim of ADR 0033 restated for a
// second document shape.
//
// What each row has to say beyond its keys is what makes these two families
// different from a routine:
//
//	[ai.<name>]      carries a credential (so it declares one), can be tested
//	                 against the real service (so it declares a probe), and
//	                 cannot be deleted while `ai.provider` names it (so it
//	                 declares a delete guard). It shares [ai] with the
//	                 section's own scalars, so it declares them reserved.
//	[advisors.<name>] earns a permission tier from its own argv (ADR 0016), so
//	                 it declares a note that says which — before the save, on
//	                 the field that decides it — and it cannot go live without
//	                 a restart, so it declares that too.
//
// Neither is reachable by the assistant. Both carry assistantReason, which is
// the entry half of #109's exclusion wall: the model's config tools resolve
// families through a view that these are absent from, for the same reason the
// [ai] and [advisors] SETTINGS are absent from AssistantSettings — a gate must
// not be able to loosen itself, and a brain must not be able to choose its own.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/ai/openaicompat"
	"github.com/rpickz/jarvix/internal/config"
)

// endpointProbeTimeout bounds the Test action. Ten seconds is long enough for
// a cold cloud endpoint over a slow link and short enough that a wrong host
// fails while the user is still looking at the form — the doctor's budget for
// the same request (#114).
const endpointProbeTimeout = 10 * time.Second

// aiEndpointFamily is the [ai.<name>] row: one OpenAI-compatible endpoint.
var aiEndpointFamily = entryFamilySpec{
	family: "ai", kind: "endpoint", shape: entryShapeKeyed,
	keys: map[string]entryKeyKind{
		"name": entryKeyString, "base_url": entryKeyString,
		"api_key_env": entryKeyString,
	},
	keyOrder: []string{"name", "base_url", "api_key_env"},
	reserved: config.ReservedAIKeys(),
	// api_key is deliberately NOT in keys: it is declared here instead, which
	// is what makes it unwritable through the entry and unreadable through
	// any reply (entry_admin_secrets.go).
	secrets: []entrySecretSpec{{
		key: "api_key", envKey: "api_key_env", label: "API key",
	}},
	nameProblems: []string{"endpoint name "},
	guardDelete: func(cfg config.Config, name string) string {
		if !strings.EqualFold(strings.TrimSpace(cfg.AI.Provider), strings.TrimSpace(name)) {
			return ""
		}
		return fmt.Sprintf(
			"[ai.%s] is the endpoint ai.provider currently names, so removing it would "+
				"leave Jarvix with nothing to think with; point ai.provider at another "+
				"endpoint in Settings first, then delete this one", name)
	},
	probe: probeEndpoint,
	assistantReason: "the assistant may not add, change, or remove AI endpoints; " +
		"[ai.<name>] tables are edited in the window's Providers section",
}

// advisorFamily is the [advisors.<name>] row: one assistant CLI (ADR 0016).
var advisorFamily = entryFamilySpec{
	family: "advisors", kind: "advisor", shape: entryShapeKeyed,
	keys: map[string]entryKeyKind{
		"name": entryKeyString, "binary": entryKeyString,
		"args": entryKeyStringList, "timeout_sec": entryKeyInt,
		"description": entryKeyString,
	},
	keyOrder:     []string{"name", "binary", "args", "timeout_sec", "description"},
	nameProblems: []string{"advisor name "},
	notes:        advisorTierNotes,
	// The advisor tool and its per-advisor permission tiers are built once,
	// at construction, from the configuration the daemon booted with — the
	// tool registry is not among the collaborators a reload rebuilds. So an
	// advisor edit is written, validated, and true of the file, and is not
	// yet true of this daemon. Saying so is the whole point of the field.
	pending: func(*Daemon) string {
		return "advisor changes need a daemon restart before Jarvix will use them " +
			"(restart jarvixd); the configuration is saved either way"
	},
	assistantReason: "the assistant may not add, change, or remove advisors; " +
		"[advisors.<name>] tables are edited in the window's Providers section",
}

// advisorTierNotes states the permission tier the draft earns, on the field
// that decides it (ADR 0016).
//
// The rule it mirrors is applyAdvisorDefaults' and advisorPolicyTiers': only
// an UNSET argv inherits a shipped preset, and only a shipped preset that
// merely reads and answers earns the allow tier. Anything hand-written is
// unaudited by definition and asks first. The form states it because the
// alternative is a user loosening — or, just as surprising, tightening — a
// permission gate as a side effect of typing a flag.
func advisorTierNotes(name string, draft map[string]any) []entryNote {
	preset, known := config.AdvisorPresets[strings.TrimSpace(name)]
	_, argvSet := draft["args"]
	switch {
	case !argvSet && known && preset.ReadOnly:
		return []entryNote{{Field: "args", Message: fmt.Sprintf(
			"Permission: allow — with no arguments of its own this advisor runs the shipped "+
				"%s preset (%s), which answers without changing anything, so Jarvix consults "+
				"it without asking you first. Typing your own arguments moves it to ask.",
			name, strings.Join(preset.Args, " "))}}
	case !argvSet && known:
		return []entryNote{{Field: "args", Message: fmt.Sprintf(
			"Permission: ask — %s can change files and run commands whatever its arguments "+
				"are, so every consultation asks you first.", name)}}
	case !argvSet:
		return []entryNote{{Field: "args", Message: fmt.Sprintf(
			"Permission: ask — Jarvix ships no preset for %q, so it will run the binary with "+
				"no arguments and ask you before every consultation.", name)}}
	default:
		return []entryNote{{Field: "args", Message: "Permission: ask — these are arguments you " +
			"wrote, which Jarvix has not audited, so every consultation asks you first. " +
			"Remove them to fall back to the shipped preset for this advisor."}}
	}
}

// probeEndpoint is the Test action: the cheapest real request that proves both
// halves of an endpoint — that it answers at all, and that it accepts the
// credentials the file gives it. GET /models costs no tokens and returns 401
// rather than 200 for a wrong key, which is exactly the distinction the form
// needs to make.
//
// It reports what happened and never more: a success is a 2xx that actually
// arrived, and a failure carries the service's OWN words (the transport error
// or the body it returned) rather than a guess about what they mean. The key
// is resolved here, server-side, from the endpoint the name resolves —
// config.Endpoint.Key, which prefers the environment — so a client can ask for
// a test without ever holding a credential.
func probeEndpoint(ctx context.Context, cfg config.Config, name string) map[string]any {
	ep, ok := cfg.AI.Endpoints[strings.TrimSpace(name)]
	if !ok {
		return map[string]any{
			"outcome": string(openaicompat.ProbeUnreachable),
			"summary": fmt.Sprintf("There is no endpoint named %q to test.", name),
		}
	}
	report := openaicompat.New(name, ep.BaseURL, ep.Key()).ProbeEndpoint(ctx, endpointProbeTimeout)
	result := map[string]any{
		"outcome":  string(report.Outcome),
		"summary":  endpointProbeSummary(report.Outcome, ep.BaseURL),
		"base_url": ep.BaseURL,
	}
	if report.Status != 0 {
		result["status"] = report.Status
	}
	if report.Detail != "" {
		result["detail"] = report.Detail
	}
	return result
}

// endpointProbeSummary is the one sentence the form leads with, saying what
// the outcome means for the next thing the user should do.
func endpointProbeSummary(outcome openaicompat.ProbeOutcome, base string) string {
	switch outcome {
	case openaicompat.ProbeReachable:
		return "Reachable — " + base + " answered and accepted the credentials."
	case openaicompat.ProbeUnauthorised:
		return "Unauthorised — " + base + " answered, but rejected the credentials. " +
			"The base URL is right; the key is not."
	default:
		return "Unreachable — nothing usable answered at " + base + ". " +
			"Check the base URL and that the service is running."
	}
}
