package doctor

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/ai/openaicompat"
	"github.com/rpickz/jarvix/internal/config"
)

// fixtureDefs is a small registered tool surface with known byte sizes, so
// the floor arithmetic is checked against numbers a reviewer can redo by
// hand (bytes/4, the memory budget's ruler).
func fixtureDefs() []ai.ToolDef {
	return []ai.ToolDef{
		{Name: strings.Repeat("a", 10), Description: strings.Repeat("b", 30),
			Schema: json.RawMessage(strings.Repeat("c", 40))}, // 80 bytes → 20 tokens
		{Name: strings.Repeat("d", 4), Description: strings.Repeat("e", 5),
			Schema: json.RawMessage(strings.Repeat("f", 10))}, // 19 bytes → 5 tokens (rounded up)
	}
}

func TestEstimatePromptBudgetMeasuresEveryComponent(t *testing.T) {
	cfg := config.Default() // memory on (500 tokens), context on (2000 chars)
	prompt := strings.Repeat("p", 400)
	budget := EstimatePromptBudget(prompt, fixtureDefs(), cfg)

	if budget.SystemPrompt != 100 {
		t.Errorf("SystemPrompt = %d, want 400 bytes / 4 = 100", budget.SystemPrompt)
	}
	if budget.ToolSchemas != 25 {
		t.Errorf("ToolSchemas = %d, want 20 + 5 = 25", budget.ToolSchemas)
	}
	if budget.Memory != cfg.Memory.MaxInjectedTokens {
		t.Errorf("Memory = %d, want the configured cap %d", budget.Memory, cfg.Memory.MaxInjectedTokens)
	}
	if budget.Context != 500 {
		t.Errorf("Context = %d, want 2000 chars / 4 = 500", budget.Context)
	}
	if budget.Headroom != contextFloorHeadroom {
		t.Errorf("Headroom = %d, want %d", budget.Headroom, contextFloorHeadroom)
	}
	if got, want := budget.Floor(), 100+25+500+500+1024; got != want {
		t.Errorf("Floor = %d, want %d", got, want)
	}

	// Disabled budgets cost nothing — the floor must not warn about
	// injections a configuration will never make.
	cfg.Memory.Enabled = false
	cfg.Context.Window, cfg.Context.Selection, cfg.Context.Clipboard = false, false, false
	lean := EstimatePromptBudget(prompt, nil, cfg)
	if lean.ToolSchemas != 0 || lean.Memory != 0 || lean.Context != 0 {
		t.Errorf("lean budget = %+v, want zero tools/memory/context", lean)
	}
}

func TestPromptBudgetReportRoundTrips(t *testing.T) {
	budget := PromptBudget{SystemPrompt: 700, ToolSchemas: 1500, Memory: 500,
		Context: 500, Headroom: 1024}
	// Through JSON, the way it actually travels: status.get hands the CLI and
	// doctor float64s, not ints.
	wire, err := json.Marshal(budget.Report())
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	got, ok := BudgetFromReport(decoded)
	if !ok || got != budget {
		t.Errorf("round trip = %+v (ok %v), want %+v", got, ok, budget)
	}
	if _, ok := BudgetFromReport(nil); ok {
		t.Error("an absent payload (older daemon) must not fabricate a budget")
	}
}

func TestContextFloorResult(t *testing.T) {
	// The budget measured in the live incident's ballpark: a floor well above
	// 4096.
	budget := PromptBudget{SystemPrompt: 900, ToolSchemas: 2400, Memory: 500,
		Context: 500, Headroom: 1024} // floor 5324
	tests := []struct {
		name       string
		served     openaicompat.ServedContext
		probeErr   error
		wantStatus Status
		wantIn     []string
	}{
		{name: "the live incident: stock model, default window, floor above it",
			served: openaicompat.ServedContext{NumCtx: 0, MaxCtx: 32768}, wantStatus: Warn,
			wantIn: []string{"~4096", "~5324", "ollama's default", "num_ctx 16384", "truncated"}},
		{name: "a num_ctx variant above the floor is healthy",
			served: openaicompat.ServedContext{NumCtx: 16384, MaxCtx: 32768}, wantStatus: OK,
			wantIn: []string{"~16384", "num_ctx"}},
		{name: "a num_ctx variant below the floor still warns",
			served: openaicompat.ServedContext{NumCtx: 2048, MaxCtx: 32768}, wantStatus: Warn,
			wantIn: []string{"~2048", "~5324"}},
		{name: "an architecture smaller than the default caps the assumption",
			served: openaicompat.ServedContext{NumCtx: 0, MaxCtx: 2048}, wantStatus: Warn,
			wantIn: []string{"~2048", "architecture's maximum"}},
		{name: "a provider that cannot answer degrades silently",
			probeErr: errors.New("connection refused"), wantStatus: OK,
			wantIn: []string{"skipped"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := contextFloorResult("qwen2.5:7b", budget, tt.served, tt.probeErr)
			if r.Status != tt.wantStatus {
				t.Fatalf("status = %v, want %v (%+v)", r.Status, tt.wantStatus, r)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(r.Detail+"\n"+r.Fix, want) {
					t.Errorf("result missing %q:\ndetail: %s\nfix: %s", want, r.Detail, r.Fix)
				}
			}
			if tt.wantStatus == Warn && r.Fix == "" {
				t.Error("a warning must carry the num_ctx fix")
			}
		})
	}
}

func TestCheckContextFloorSkipsQuietlyOffOllama(t *testing.T) {
	// Non-ollama providers cannot be asked what they serve; the check must
	// stay out of the way rather than fail or guess.
	cfg := config.Default()
	cfg.AI.Provider = "openai"
	r := checkContextFloor(cfg, config.DefaultPaths())
	if r.Status != OK || !strings.Contains(r.Detail, "skipped") {
		t.Errorf("non-ollama result = %+v, want a quiet skip", r)
	}
}
