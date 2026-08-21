package doctor

import (
	"context"
	"fmt"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/ai/openaicompat"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/memory"
)

// This file is the context-floor check (issue #71): does the served model's
// context window actually hold what Jarvix sends before the user has said a
// word? When it does not, the provider truncates silently — and what gets
// truncated first is the tool definitions, at which point the model degrades
// to narrating actions it can no longer take. The live incident ran
// qwen2.5:7b at a 4096-token window under a larger prompt, and every other
// doctor check was green.
//
// The floor is an estimate (bytes/4, the same rule the memory budget uses),
// so the comparison keeps a deliberate margin: the check exists to catch a
// window that is *clearly* too small, not to referee a close call.

// PromptBudget itemises the estimated tokens one turn spends before the user
// has said anything: the composed system prompt, every registered tool
// schema, the configured memory and desktop-context budgets, and headroom
// for conversation history plus the answer itself.
type PromptBudget struct {
	SystemPrompt int
	ToolSchemas  int
	Memory       int
	Context      int
	Headroom     int
}

// Floor is the smallest context window the budget plausibly fits in.
func (b PromptBudget) Floor() int {
	return b.SystemPrompt + b.ToolSchemas + b.Memory + b.Context + b.Headroom
}

// contextFloorHeadroom (tokens) stands in for everything the floor cannot
// measure statically: carried conversation turns, the user's words, and room
// for the model's own answer. Deliberately modest — the floor is a lower
// bound, and overstating it would turn an honest warning into a nag.
const contextFloorHeadroom = 1024

// OllamaDefaultContext is the window ollama serves a model with when its
// modelfile sets no num_ctx — the default that carried the live incident's
// 4096. Best effort: OLLAMA_CONTEXT_LENGTH can move it, which /api/show does
// not report, so a reading built on this constant says so.
const OllamaDefaultContext = 4096

// EstimatePromptBudget measures the prompt the daemon composes: the system
// prompt as one string, the registered tool schemas as the provider wires
// them (name, description, parameters), and the configured budgets. Token
// counts are the bytes/4 estimate memory.EstimateTokens defines — the daemon
// has no tokenizer for an arbitrary model, and a floor needs a consistent
// ruler more than an exact one.
func EstimatePromptBudget(systemPrompt string, defs []ai.ToolDef, cfg config.Config) PromptBudget {
	b := PromptBudget{
		SystemPrompt: memory.EstimateTokens(systemPrompt),
		Headroom:     contextFloorHeadroom,
	}
	for _, def := range defs {
		b.ToolSchemas += memory.EstimateTokens(def.Name + def.Description + string(def.Schema))
	}
	if cfg.Memory.Enabled {
		b.Memory = cfg.Memory.MaxInjectedTokens
	}
	if len(cfg.Context.EnabledSources()) > 0 {
		// The context budget is a character cap; characters are bytes to the
		// estimate's ruler.
		b.Context = (cfg.Context.MaxChars + 3) / 4
	}
	return b
}

// Report renders the budget for status.get and doctor, floor included.
func (b PromptBudget) Report() map[string]any {
	return map[string]any{
		"system_prompt_tokens": b.SystemPrompt,
		"tool_schema_tokens":   b.ToolSchemas,
		"memory_tokens":        b.Memory,
		"context_tokens":       b.Context,
		"headroom_tokens":      b.Headroom,
		"floor_tokens":         b.Floor(),
	}
}

// BudgetFromReport reads a status.get prompt_budget payload back into a
// budget. ok is false when the payload is missing or from an older daemon.
func BudgetFromReport(v any) (PromptBudget, bool) {
	report, ok := v.(map[string]any)
	if !ok || len(report) == 0 {
		return PromptBudget{}, false
	}
	return PromptBudget{
		SystemPrompt: int(jsonNumber(report["system_prompt_tokens"])),
		ToolSchemas:  int(jsonNumber(report["tool_schema_tokens"])),
		Memory:       int(jsonNumber(report["memory_tokens"])),
		Context:      int(jsonNumber(report["context_tokens"])),
		Headroom:     int(jsonNumber(report["headroom_tokens"])),
	}, true
}

// checkContextFloor compares the running daemon's prompt budget against the
// context window ollama will actually serve the configured model with.
func checkContextFloor(cfg config.Config, paths config.Paths) Result {
	const name = "model context fits the prompt"
	if cfg.AI.Provider != "ollama" {
		// Only ollama exposes what it serves; other providers manage their
		// own windows. Degrade silently, as the reliability rule demands.
		return Result{Status: OK, Name: name,
			Detail: "skipped (only ollama reports a served context length)"}
	}
	budget, ok := daemonPromptBudget(paths)
	if !ok {
		return Result{Status: OK, Name: name,
			Detail: "skipped: jarvixd is not running, so the registered tool surface cannot be measured"}
	}
	ep, ok := cfg.Endpoint()
	if !ok {
		return Result{Status: OK, Name: name, Detail: "skipped: no endpoint configured"}
	}
	client := openaicompat.New(cfg.AI.Provider, ep.BaseURL, ep.Key())
	served, err := client.OllamaServedContext(context.Background(), cfg.AI.Model)
	return contextFloorResult(cfg.AI.Model, budget, served, err)
}

// daemonPromptBudget asks the running daemon what its composed prompt and
// registered tool schemas cost — the daemon's registry is the truth about
// what a turn sends, where a config-only estimate would miss the tools that
// registration actually produced.
func daemonPromptBudget(paths config.Paths) (PromptBudget, bool) {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return PromptBudget{}, false
	}
	defer func() { _ = client.Close() }()
	var status map[string]any
	if err := client.Call("status.get", nil, &status); err != nil {
		return PromptBudget{}, false
	}
	return BudgetFromReport(status["prompt_budget"])
}

// contextFloorResult is the pure comparison, split from the probing so the
// verdict is testable against fixture budgets and canned ollama readings.
func contextFloorResult(model string, budget PromptBudget,
	served openaicompat.ServedContext, probeErr error) Result {
	const name = "model context fits the prompt"
	if probeErr != nil {
		// Best-effort introspection: a provider that cannot answer is not a
		// finding, and the short timeout has already kept doctor from hanging.
		return Result{Status: OK, Name: name,
			Detail: "skipped: ollama did not report a context length (" + probeErr.Error() + ")"}
	}
	window, source := served.NumCtx, "its modelfile's num_ctx"
	if window == 0 {
		// No num_ctx on the model: ollama falls back to its own default
		// (unless OLLAMA_CONTEXT_LENGTH moved it, which /api/show cannot
		// see). Capped by the architecture when the report names one.
		window, source = OllamaDefaultContext, "ollama's default (no num_ctx set)"
		if served.MaxCtx > 0 && served.MaxCtx < window {
			window, source = served.MaxCtx, "the architecture's maximum"
		}
	}
	detail := fmt.Sprintf("%s serves ~%d tokens (%s); the prompt needs ~%d "+
		"(system prompt ~%d + tool schemas ~%d + memory %d + desktop context ~%d + headroom %d)",
		model, window, source, budget.Floor(), budget.SystemPrompt, budget.ToolSchemas,
		budget.Memory, budget.Context, budget.Headroom)
	if window >= budget.Floor() {
		return Result{Status: OK, Name: name, Detail: detail}
	}
	// Below the floor the provider truncates silently, the tool definitions
	// go first, and the model degrades to narrating actions it cannot take —
	// the failure the honesty rule in the prompt can name but not prevent.
	return Result{Status: Warn, Name: name,
		Detail: detail + " — the prompt is being truncated, tool definitions first",
		Fix: "Serve the model with a larger window (the jarvix-qwen7b pattern):\n" +
			"  ollama show " + model + " --modelfile > Modelfile\n" +
			"  echo 'PARAMETER num_ctx 16384' >> Modelfile\n" +
			"  ollama create " + model + "-16k -f Modelfile\n" +
			"then set ai.model = \"" + model + "-16k\" and reload"}
}
