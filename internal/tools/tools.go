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

// Registry holds the enabled tools.
type Registry struct {
	tools map[string]Tool
	order []string
	log   *slog.Logger
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
