// Package tools implements the assistant's tool boundary (ADR 0006): the
// interface, a registry, and V1's single tool, shell.run. Tools execute
// under the session context, so cancelling a session kills any command it
// is running.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/rpickz/jarvix/internal/ai"
)

// Tool is one capability the assistant can invoke.
type Tool interface {
	Name() string
	Description() string
	// Schema is the JSON Schema of the tool's input object.
	Schema() json.RawMessage
	// Execute runs the tool. The returned string goes back to the model as
	// the tool result; err is for infrastructure failures only — a command
	// that ran and failed reports that in the result, not as err.
	Execute(ctx context.Context, input json.RawMessage) (string, error)
}

// Progressive is optionally implemented by tools whose work can outlast the
// user's patience. A voice session gives no other sign of life: without this,
// a two-minute advisor consultation is indistinguishable from a hang.
type Progressive interface {
	// Activity describes one pending call for humans: label is the short
	// present-tense phrase the overlay shows for the duration ("Consulting
	// claude…"), and waiting is the single sentence Jarvix speaks if the call
	// is still running when the progress threshold passes. ok is false when
	// this particular call needs neither — nothing slow is happening.
	Activity(input json.RawMessage) (label, waiting string, ok bool)
}

// Confirmable is optionally implemented by a tool whose ask-tier confirmation
// must name something only the tool can know. The gate decides the *tier* from
// configuration, as it always does; this decides the *words*, and it decides
// them from the tool's own view of the world — the live window inventory, not
// the model's description of it.
//
// Without it, "close my browser" is confirmed as "I want to use the
// desktop.close_window tool. Should I go ahead?", which asks the user to
// approve a tool rather than an action. With it, they are asked about the
// window that is actually about to close, generated daemon-side, so a model
// cannot describe closing their editor as closing a scratch window.
//
// ok is false when this particular call has nothing better to offer than the
// generic question — unparseable arguments, or nothing that resolves.
type Confirmable interface {
	// Confirmation returns the exact action being judged (published verbatim,
	// and what a remembered approval is keyed on) and the one-sentence spoken
	// question.
	Confirmation(input json.RawMessage) (command, summary string, ok bool)
}

// Refusing is optionally implemented by a tool part of whose argument space
// is structurally off limits — not deny-by-default but unreachable, whatever
// the [tools] policy says (issue #105, ADR 0036). The gate consults it before
// the policy, so a refusal wins over an explicit allow, a global allow, and
// even over having no policy installed at all: there is nothing a user can
// write in configuration that softens it, which is the point — the excluded
// space is the configuration that governs the assistant itself, and a gate
// must not be able to loosen itself on request.
//
// The reason is spoken-ready and precise: it is the rule on the tool.denied
// event and the sentence the model relays, so "I can't do that" always says
// what exactly is off limits. ok is false for the ordinary case — arguments
// entirely inside the tool's writable space — where the policy decides as
// usual. The tool's Execute re-checks the same wall (nothing is written even
// if a refusing call is somehow executed), but the gate is where the refusal
// is visible, audited, and cheap.
type Refusing interface {
	// Refuse reports that this call addresses configuration no policy may
	// expose, and why.
	Refuse(input json.RawMessage) (reason string, ok bool)
}

// Escalating is optionally implemented by a tool that can tell, from live
// state, that *this* call is more dangerous than its configured tier assumes.
//
// It exists because some risk is not a property of the tool or its arguments.
// "May Jarvix type?" is a question configuration can answer; "may Jarvix type
// into a shell?" is not, because whether the focused window is a terminal
// depends on where focus happens to be at this instant, and only the tool can
// see that (#37).
//
// The direction is one-way and enforced by the caller: an implementation may
// turn allow into ask, never ask or deny into allow. A tool can therefore only
// ever make the gate stricter than the user configured it, which is the only
// direction in which trusting a tool's own judgement is safe.
type Escalating interface {
	// Escalate reports that this call must be confirmed despite an allow tier,
	// and names the rule for the logs and the audit trail. ok is false for the
	// ordinary case: nothing about this call raises it.
	Escalate(input json.RawMessage) (rule string, ok bool)
}

// Registry holds the enabled tools.
type Registry struct {
	tools map[string]Tool
	order []string
	log   *slog.Logger
	// policy is the permission gate consulted before every Execute (ADR
	// 0014). Nil means no gate — a construction-time choice for tests, never
	// the daemon's: the daemon always installs a policy.
	//
	// Atomic since issue #162, because the gate is no longer written once at
	// construction: remembering a pattern recompiles the policy and swaps it
	// in while sessions may be reading it, and so does a config.reload that
	// revokes one. A plain field would make every pre-approved command a data
	// race against the rule that allowed it — and a race whose losing side is
	// a permission decision is not a race anyone gets to run in production.
	// The pointer is swapped whole; a Policy is immutable once compiled, so a
	// reader either sees the old gate entirely or the new gate entirely and
	// never a half-applied one.
	policy atomic.Pointer[Policy]
}

// NewRegistry builds a registry. logger may be nil.
func NewRegistry(logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{tools: make(map[string]Tool), log: logger}
}

// Register adds a tool.
func (r *Registry) Register(t Tool) {
	if _, exists := r.tools[t.Name()]; !exists {
		r.order = append(r.order, t.Name())
	}
	r.tools[t.Name()] = t
}

// SetPolicy installs the permission gate, replacing any previous one. Safe to
// call while the registry is serving traffic (see Registry.policy): a call in
// flight either finishes under the gate it started with or, if it has not
// reached the check yet, is judged by the new one — both honest readings, and
// neither a torn one.
func (r *Registry) SetPolicy(p *Policy) { r.policy.Store(p) }

// Policy returns the installed permission gate (nil when none).
func (r *Registry) Policy() *Policy { return r.policy.Load() }

// Check classifies one tool call against the permission gate, without
// executing anything. With no policy installed everything is allowed —
// the pre-gate behaviour tests rely on.
func (r *Registry) Check(call ai.ToolCall) Verdict {
	return r.CheckWithGrants(call, nil)
}

// CheckWithGrants is Check plus the conversation-scoped allow patterns of
// issue #162, applied by the policy exactly where the configured allow list
// is applied. The structural wall and the escalation hooks below are
// unchanged by a grant: a tool that refuses a call still refuses it, and a
// tool that tightens its own tier still tightens it — a grant can only ever
// answer the question "is this shell command on a list the user wrote".
func (r *Registry) CheckWithGrants(call ai.ToolCall, grants [][]string) Verdict {
	policy := r.policy.Load()
	// The structural wall first, before any policy is consulted — including
	// the no-policy case below, because "no gate installed" must not mean
	// "the excluded configuration became writable" (Refusing).
	if t, registered := r.tools[call.Name]; registered {
		if ref, refusing := t.(Refusing); refusing {
			if reason, refuse := ref.Refuse(json.RawMessage(call.Arguments)); refuse {
				r.log.Info("tool call refused by exclusion", "component", "tools",
					"tool", call.Name, "rule", reason)
				return Verdict{Decision: PolicyDeny, Tool: call.Name, Rule: reason}
			}
		}
	}
	if policy == nil {
		return Verdict{Decision: PolicyAllow, Tool: call.Name, Rule: "no policy installed"}
	}
	verdict := policy.DecideWithGrants(call, grants)
	tool, registered := r.tools[call.Name]
	// A tool that can see something the configuration could not may tighten
	// the tier — allow becomes ask, and only in that direction (Escalating).
	// Checked before the question is built, so an escalated call gets the same
	// generated sentence a configured ask would.
	if registered && verdict.Decision == PolicyAllow {
		if e, escalating := tool.(Escalating); escalating {
			if rule, ok := e.Escalate(json.RawMessage(call.Arguments)); ok {
				verdict.Decision, verdict.Rule = PolicyAsk, rule
				// A fallback question, so an escalation is never silent even if
				// the tool then declines to word it. Confirmable overwrites it
				// below in every case that has something better to say.
				verdict.Summary = fmt.Sprintf("I want to use the %s tool, and %s. Should I go ahead?",
					call.Name, rule)
				r.log.Info("tool call escalated to ask", "component", "tools",
					"tool", call.Name, "rule", rule)
			}
		}
	}
	// The gate settled the tier; a Confirmable tool now supplies the sentence
	// the user actually hears, from what it can see (Confirmable). Only for
	// the ask tier: nothing else asks a question.
	if registered && verdict.Decision == PolicyAsk {
		if c, confirmable := tool.(Confirmable); confirmable {
			if command, summary, ok := c.Confirmation(json.RawMessage(call.Arguments)); ok {
				verdict.Command, verdict.Summary = command, summary
			}
		}
	}
	return verdict
}

// Activity reports how a call should be surfaced while it runs, for tools
// that implement Progressive. ok is false for every other tool, which is the
// common case: most calls finish before anyone could wonder.
func (r *Registry) Activity(call ai.ToolCall) (label, waiting string, ok bool) {
	t, registered := r.tools[call.Name]
	if !registered {
		return "", "", false
	}
	p, progressive := t.(Progressive)
	if !progressive {
		return "", "", false
	}
	return p.Activity(json.RawMessage(call.Arguments))
}

// CheckCommand classifies a bare command string under the given tool identity
// — the user-defined intent path (ADR 0017). Like Check, no policy means
// everything is allowed.
func (r *Registry) CheckCommand(tool, command string) Verdict {
	return r.CheckCommandWithGrants(tool, command, nil)
}

// CheckCommandWithGrants is CheckCommand plus conversation-scoped grants.
func (r *Registry) CheckCommandWithGrants(tool, command string, grants [][]string) Verdict {
	policy := r.policy.Load()
	if policy == nil {
		return Verdict{Decision: PolicyAllow, Tool: tool, Command: command, Rule: "no policy installed"}
	}
	return policy.DecideCommandWithGrants(tool, command, grants)
}

// CheckRoutine classifies running one named routine under the routine.run
// identity (ADR 0026). Like Check, no policy means everything is allowed —
// which for routines is also what the shipped default policy says.
func (r *Registry) CheckRoutine(name string) Verdict {
	policy := r.policy.Load()
	if policy == nil {
		return Verdict{Decision: PolicyAllow, Tool: RoutineToolName, Command: name, Rule: "no policy installed"}
	}
	return policy.DecideRoutine(name)
}

// CheckScript classifies running one named script under the script.run
// identity (ADR 0030). Like Check, no policy means everything is allowed —
// but note the session engine never takes that reading for scripts: with no
// registry at all it asks, because an ungated arbitrary executable must not
// run silently.
func (r *Registry) CheckScript(name, path string) Verdict {
	policy := r.policy.Load()
	if policy == nil {
		return Verdict{Decision: PolicyAllow, Tool: ScriptToolName,
			Command: name + " (" + path + ")", Rule: "no policy installed"}
	}
	return policy.DecideScript(name, path)
}

// Names returns registered tool names in registration order.
func (r *Registry) Names() []string {
	return append([]string(nil), r.order...)
}

// Empty reports whether no tools are enabled.
func (r *Registry) Empty() bool { return len(r.tools) == 0 }

// Defs returns provider-facing tool definitions in registration order.
func (r *Registry) Defs() []ai.ToolDef {
	defs := make([]ai.ToolDef, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		defs = append(defs, ai.ToolDef{
			Name:        t.Name(),
			Description: t.Description(),
			Schema:      t.Schema(),
		})
	}
	return defs
}

// Execute runs one tool call and always returns text for the model: unknown
// tools and failures come back as readable errors so the assistant can
// recover or explain, instead of the session dying.
func (r *Registry) Execute(ctx context.Context, call ai.ToolCall) string {
	t, ok := r.tools[call.Name]
	if !ok {
		return fmt.Sprintf("error: unknown tool %q", call.Name)
	}
	result, err := t.Execute(ctx, json.RawMessage(call.Arguments))
	if err != nil {
		r.log.Error("tool failed", "component", "tools", "tool", call.Name, "error", err.Error())
		return fmt.Sprintf("error: %v", err)
	}
	return result
}
