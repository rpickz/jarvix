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

// Registry holds the enabled tools.
type Registry struct {
	tools map[string]Tool
	order []string
	log   *slog.Logger
	// policy is the permission gate consulted before every Execute (ADR
	// 0014). Nil means no gate — a construction-time choice for tests, never
	// the daemon's: the daemon always installs a policy.
	policy *Policy
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

// SetPolicy installs the permission gate. Call before the registry serves
// traffic; the registry does not lock around it.
func (r *Registry) SetPolicy(p *Policy) { r.policy = p }

// Policy returns the installed permission gate (nil when none).
func (r *Registry) Policy() *Policy { return r.policy }

// Check classifies one tool call against the permission gate, without
// executing anything. With no policy installed everything is allowed —
// the pre-gate behaviour tests rely on.
func (r *Registry) Check(call ai.ToolCall) Verdict {
	if r.policy == nil {
		return Verdict{Decision: PolicyAllow, Tool: call.Name, Rule: "no policy installed"}
	}
	return r.policy.Decide(call)
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
	if r.policy == nil {
		return Verdict{Decision: PolicyAllow, Tool: tool, Command: command, Rule: "no policy installed"}
	}
	return r.policy.DecideCommand(tool, command)
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
