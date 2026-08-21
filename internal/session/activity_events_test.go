package session

import (
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/tools"
)

// The activity feed (issue #70) reads two additive facts off the bus that
// this package publishes: tool.finished carries how long the call took and
// whether the registry could run it, and assistant.finished carries how many
// tool calls the turn requested — zero being the "claimed an action, called
// no tool" incident the feed exists to make visible.

func TestToolFinishedCarriesDurationAndOutcome(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	rec := &recordingTool{result: "3 containers running"}
	h.tools = tools.NewRegistry(nil)
	h.tools.Register(rec)
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, h.tools, nil, bus, nil,
		Options{Model: "m", SpeakResponses: true})
	h.provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "run", Arguments: `{"command":"docker ps"}`}},
	}
	h.provider.Response = "Three containers are running."

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("what's happening in docker")
	seen := h.collectUntil(t, "session.finished")

	finished := seen["tool.finished"].Data
	if finished["outcome"] != "ok" {
		t.Errorf("outcome = %v, want ok", finished["outcome"])
	}
	if ms, ok := finished["duration_ms"].(int64); !ok || ms < 0 {
		t.Errorf("duration_ms = %v (%T), want a non-negative int64", finished["duration_ms"], finished["duration_ms"])
	}
	if calls, ok := seen["assistant.finished"].Data["tool_calls"].(int); !ok || calls != 1 {
		t.Errorf("assistant.finished tool_calls = %v, want 1", seen["assistant.finished"].Data["tool_calls"])
	}
}

// A call the registry itself cannot run — an unknown tool is the shape a
// test can force — is the one failure this layer can attest to on the bus.
func TestToolFinishedReportsRegistryErrorsAsOutcomeError(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	h.tools = tools.NewRegistry(nil) // empty: every call is unknown
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, h.tools, nil, bus, nil,
		Options{Model: "m", SpeakResponses: true})
	h.provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "no.such.tool", Arguments: `{}`}},
	}
	h.provider.Response = "That did not work."

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("try the tool")
	seen := h.collectUntil(t, "session.finished")

	if got := seen["tool.finished"].Data["outcome"]; got != "error" {
		t.Errorf("outcome = %v, want error", got)
	}
}

// The zero that matters: a turn the model answered without requesting a
// single tool says so on the bus, which is what lets the feed mark the turn
// text-only instead of leaving an absence to be noticed.
func TestAssistantFinishedCountsZeroToolCalls(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("explain recursion")
	seen := h.collectUntil(t, "session.finished")
	if calls, ok := seen["assistant.finished"].Data["tool_calls"].(int); !ok || calls != 0 {
		t.Errorf("tool_calls = %v, want 0", seen["assistant.finished"].Data["tool_calls"])
	}
}
