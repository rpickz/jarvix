package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/tools"
)

// These tests drive the real advisor tool — a real child process, through the
// real permission gate and tool loop — so the whole path from "the model asked
// for an advisor" to "Jarvix speaks its answer" is covered end to end.

// fakeAdvisorBinary writes an executable /bin/sh script standing in for an
// installed assistant CLI.
func fakeAdvisorBinary(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-advisor")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func advisorTool(binary string, timeout time.Duration) *tools.Advisor {
	return &tools.Advisor{Advisors: []tools.AdvisorSpec{{
		Name: "claude", Binary: binary, Timeout: timeout, Description: "the strong one",
	}}}
}

func scriptAdvisorCall(h *harness, question, answer string) {
	args, _ := json.Marshal(map[string]string{"advisor": "claude", "question": question})
	h.provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "advisor.ask", Arguments: string(args)}},
	}
	h.provider.Response = answer
}

func TestAdvisorAnswerBecomesTheSpokenTurn(t *testing.T) {
	bin := fakeAdvisorBinary(t, `printf 'Split the pipeline: build once, publish per target.\n'`)
	h := newGateHarness(t, Options{SpeakResponses: true}, advisorTool(bin, 10*time.Second),
		tools.PolicyConfig{Advisors: map[string]tools.PolicyDecision{"claude": tools.PolicyAllow}})
	scriptAdvisorCall(h, "review my publish pipeline",
		"Claude suggests splitting the pipeline: build once, then publish per target.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("ask claude to review my publish pipeline")
	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)

	// A read-only advisor is consulted without stopping to ask.
	if counts["tool.confirmation_required"] != 0 {
		t.Error("a read-only advisor must not require confirmation")
	}
	if counts["tool.started"] != 1 || counts["tool.finished"] != 1 {
		t.Errorf("started/finished = %d/%d", counts["tool.started"], counts["tool.finished"])
	}
	// The advisor's own words reached the model, and the spoken turn is
	// derived from them.
	result := lastToolResult(t, h)
	if !strings.Contains(result, "build once, publish per target") {
		t.Errorf("advisor output did not reach the model: %q", result)
	}
	if h.tts.Last().Text == "" || !strings.Contains(h.tts.Last().Text, "publish per target") {
		t.Errorf("the answer was not spoken: %q", h.tts.Last().Text)
	}
}

func TestAdvisorThatCanActWaitsForConfirmation(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "advisor-ran")
	bin := fakeAdvisorBinary(t, `echo ran > "`+marker+`"; echo done`)
	h := newGateHarness(t, Options{SpeakResponses: true}, advisorTool(bin, 10*time.Second),
		tools.PolicyConfig{Advisors: map[string]tools.PolicyDecision{"claude": tools.PolicyAsk}})
	scriptAdvisorCall(h, "refactor my repo", "I did not consult Claude.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("ask claude to refactor my repo")

	ev := h.waitFor(t, "tool.confirmation_required")
	// The user confirms the advisor, not the question: that is what a
	// remembered approval would be keyed on.
	if ev.Data["command"] != "claude" {
		t.Errorf("confirmation command = %v, want the advisor's name", ev.Data["command"])
	}
	if summary, _ := ev.Data["summary"].(string); !strings.Contains(summary, "claude") {
		t.Errorf("summary = %q", summary)
	}

	if err := h.engine.Submit("no, don't"); err != nil {
		t.Fatal(err)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a declined consultation must never start the advisor")
	}
	if result := lastToolResult(t, h); !strings.Contains(result, "declined") {
		t.Errorf("model should be told the user declined: %q", result)
	}
}

func TestMissingAdvisorEndsTheSessionCleanly(t *testing.T) {
	h := newGateHarness(t, Options{SpeakResponses: true},
		advisorTool("jarvix-no-such-assistant", 10*time.Second),
		tools.PolicyConfig{Advisors: map[string]tools.PolicyDecision{"claude": tools.PolicyAllow}})
	scriptAdvisorCall(h, "review my architecture",
		"I couldn't reach Claude — it isn't installed.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("ask claude about my architecture")
	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)

	if counts["error"] != 0 {
		t.Error("a missing advisor must not fail the session")
	}
	result := lastToolResult(t, h)
	if !strings.Contains(result, "not installed") || !strings.Contains(result, "one short sentence") {
		t.Errorf("model should be asked for a one-sentence failure: %q", result)
	}
	if !strings.Contains(h.tts.Last().Text, "isn't installed") {
		t.Errorf("the failure was not spoken: %q", h.tts.Last().Text)
	}
}

func TestInterruptingAConsultationKillsTheAdvisor(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "advisor-finished")
	// Records that it survived — the file appears only if the child outlived
	// the interruption.
	bin := fakeAdvisorBinary(t, `sleep 1; echo ran > "`+marker+`"; echo late`)
	h := newGateHarness(t, Options{SpeakResponses: true}, advisorTool(bin, time.Minute),
		tools.PolicyConfig{Advisors: map[string]tools.PolicyDecision{"claude": tools.PolicyAllow}})
	scriptAdvisorCall(h, "think hard", "unused")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("ask claude to think hard")
	h.waitFor(t, "tool.started")

	start := time.Now()
	_ = h.engine.Cancel()
	h.waitIdle(t)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancellation waited for the advisor (%s)", elapsed)
	}
	time.Sleep(1500 * time.Millisecond) // past the advisor's own sleep
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the advisor process survived the interruption")
	}
}
