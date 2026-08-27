package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/tools"
)

// namedTool is a recording fake tool: it counts executions so tests can
// prove that a declined or denied call never reached it.
type namedTool struct {
	name   string
	result string
	calls  int
}

func (n *namedTool) Name() string            { return n.name }
func (n *namedTool) Description() string     { return "fake tool" }
func (n *namedTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (n *namedTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	n.calls++
	return n.result, nil
}

// newGateHarness builds a harness whose registry carries the given tool and
// a compiled policy — the full permission gate, over fakes.
func newGateHarness(t *testing.T, opts Options, tool tools.Tool, cfg tools.PolicyConfig) *harness {
	t.Helper()
	h := newHarness(t, opts)
	policy, err := tools.NewPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.tools = tools.NewRegistry(nil)
	h.tools.Register(tool)
	h.tools.SetPolicy(policy)
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	t.Cleanup(h.cancel)
	if opts.Model == "" {
		opts.Model = "m"
	}
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, h.tools, nil, bus, nil, opts)
	return h
}

// scriptShellCall makes the fake provider request one shell.run call in its
// first round and answer with text in the second.
func scriptShellCall(h *harness, command, answer string) {
	args, _ := json.Marshal(map[string]string{"command": command})
	h.provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "shell.run", Arguments: string(args)}},
	}
	h.provider.Response = answer
}

// countUntil drains events until terminal arrives, counting occurrences per
// type — unlike collectUntil, duplicates are visible.
func (h *harness) countUntil(t *testing.T, terminal string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-h.events:
			counts[ev.Type]++
			if ev.Type == terminal {
				return counts
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q; saw %v", terminal, counts)
		}
	}
}

// lastToolResult returns the most recent RoleTool message the provider saw.
func lastToolResult(t *testing.T, h *harness) string {
	t.Helper()
	last := h.provider.Requests[len(h.provider.Requests)-1]
	for i := len(last.Messages) - 1; i >= 0; i-- {
		if last.Messages[i].Role == ai.RoleTool {
			return last.Messages[i].Content
		}
	}
	t.Fatal("provider never saw a tool result")
	return ""
}

func TestAllowListedCommandRunsWithoutConfirmation(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "3 containers"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "docker ps", "Three containers are running.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("what's in docker")
	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)

	if rec.calls != 1 {
		t.Errorf("tool calls = %d, want 1", rec.calls)
	}
	if counts["tool.confirmation_required"] != 0 {
		t.Error("read-only command must not require confirmation")
	}
	if counts["tool.started"] != 1 || counts["tool.finished"] != 1 {
		t.Errorf("started/finished = %d/%d", counts["tool.started"], counts["tool.finished"])
	}
}

func TestRiskyCommandWaitsAndConfirmRunsIt(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "Build directory removed.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	ev := h.waitFor(t, "tool.confirmation_required")
	if ev.Data["command"] != "rm -rf ./build" {
		t.Errorf("event command = %v; the exact command must be published", ev.Data["command"])
	}
	if s, _ := ev.Data["summary"].(string); !strings.Contains(s, "rm -rf ./build") {
		t.Errorf("summary %v does not quote the command", ev.Data["summary"])
	}
	if state, _ := h.engine.State(); state != StateAwaitingConfirmation {
		t.Errorf("state = %s, want awaiting_confirmation", state)
	}
	if rec.calls != 0 {
		t.Fatal("tool ran before confirmation")
	}

	if err := h.engine.Confirm(true); err != nil {
		t.Fatal(err)
	}
	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)
	if rec.calls != 1 {
		t.Errorf("tool calls = %d, want 1 after approval", rec.calls)
	}
	if counts["tool.confirmed"] != 1 {
		t.Error("missing tool.confirmed event")
	}
	if counts["tool.started"] != 1 {
		t.Error("approved execution must publish tool.started")
	}
}

func TestDeclinedCommandNeverExecutes(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "Okay, I won't touch it.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	h.waitFor(t, "tool.confirmation_required")
	if err := h.engine.Confirm(false); err != nil {
		t.Fatal(err)
	}
	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)

	if rec.calls != 0 {
		t.Fatalf("declined command executed %d times", rec.calls)
	}
	if counts["tool.declined"] != 1 || counts["tool.started"] != 0 {
		t.Errorf("declined=%d started=%d", counts["tool.declined"], counts["tool.started"])
	}
	// The model was told, so it can answer gracefully — and the session
	// still finished with its answer.
	if !strings.Contains(lastToolResult(t, h), "declined") {
		t.Errorf("model saw %q, want a declined-by-user result", lastToolResult(t, h))
	}
}

func TestConfirmationTimeoutDeclines(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "It timed out, nothing was done.")
	// Injected clock: the timeout fires exactly when the test says so.
	fire := make(chan time.Time)
	h.engine.timer = func(time.Duration) (<-chan time.Time, func()) {
		return fire, func() {}
	}

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	h.waitFor(t, "tool.confirmation_required")
	fire <- time.Time{} // the user never answered

	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)
	if rec.calls != 0 {
		t.Fatalf("timed-out command executed %d times", rec.calls)
	}
	if counts["tool.declined"] != 1 {
		t.Error("timeout must publish tool.declined")
	}
	if !strings.Contains(lastToolResult(t, h), "did not confirm") {
		t.Errorf("model saw %q, want a timeout result", lastToolResult(t, h))
	}
}

func TestInterruptDuringConfirmationCancelsCleanly(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "unused")

	first, _ := h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	h.waitFor(t, "tool.confirmation_required")

	// The user starts a new interaction instead of answering.
	second, err := h.engine.StartSession()
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("expected a fresh session")
	}
	ev := h.waitFor(t, "session.cancelled")
	if ev.Data["session_id"] != first {
		t.Errorf("cancelled %v, want %s", ev.Data["session_id"], first)
	}
	if rec.calls != 0 {
		t.Fatal("interrupted confirmation must not execute")
	}

	// The new session works end to end.
	h.provider.ToolCallsByRound = nil
	h.provider.Response = "Fresh answer."
	if err := h.engine.Submit("something else"); err != nil {
		t.Fatal(err)
	}
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
	if rec.calls != 0 {
		t.Error("nothing should have executed across the whole exchange")
	}
}

func TestCancelDuringConfirmationDeclines(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "unused")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	h.waitFor(t, "tool.confirmation_required")
	if err := h.engine.Cancel(); err != nil {
		t.Fatal(err)
	}
	counts := h.countUntil(t, "session.cancelled")
	h.waitIdle(t)
	if rec.calls != 0 {
		t.Fatal("cancelled confirmation must not execute")
	}
	if counts["tool.declined"] != 1 {
		t.Error("abandoning a confirmation must record a decline")
	}
}

func TestSpokenYesApproves(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "Done, the build directory is gone.")
	h.stt.Text = "yes go ahead"

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	h.waitFor(t, "tool.confirmation_required")

	// The user answers by voice: the same push-to-talk flow, flowing into
	// the pending confirmation instead of a new session.
	if err := h.engine.StartVoice(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.StopVoice(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit(""); err != nil {
		t.Fatal(err)
	}
	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)
	if rec.calls != 1 {
		t.Errorf("tool calls = %d, want 1 after spoken yes", rec.calls)
	}
	if counts["tool.confirmed"] != 1 {
		t.Error("missing tool.confirmed")
	}
}

func TestSpokenNoDeclines(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "Understood, leaving it alone.")
	h.stt.Text = "no, don't do that"

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	h.waitFor(t, "tool.confirmation_required")
	_ = h.engine.StartVoice()
	_, _ = h.engine.StopVoice()
	_ = h.engine.Submit("")
	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)
	if rec.calls != 0 {
		t.Fatalf("spoken no executed the command %d times", rec.calls)
	}
	if counts["tool.declined"] != 1 {
		t.Error("missing tool.declined")
	}
}

func TestTypedReplyApproves(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "Done.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	h.waitFor(t, "tool.confirmation_required")
	// A text submission while awaiting is the answer, not a new question.
	if err := h.engine.Submit("go ahead"); err != nil {
		t.Fatal(err)
	}
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
	if rec.calls != 1 {
		t.Errorf("tool calls = %d, want 1", rec.calls)
	}
}

func TestAmbiguousReplyDeclines(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "Nothing was changed.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	h.waitFor(t, "tool.confirmation_required")
	// Anything that is not a clear yes must decline — a misheard word can
	// never run a destructive command.
	_ = h.engine.Submit("what will that delete exactly")
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
	if rec.calls != 0 {
		t.Fatalf("ambiguous reply executed the command %d times", rec.calls)
	}
}

func TestDenyListedCommandNeverAsksNeverRuns(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "unused"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf /", "I'm not allowed to do that.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("wipe the disk")
	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)

	if rec.calls != 0 {
		t.Fatalf("deny-listed command executed %d times", rec.calls)
	}
	if counts["tool.denied"] != 1 || counts["tool.confirmation_required"] != 0 {
		t.Errorf("denied=%d confirmation_required=%d",
			counts["tool.denied"], counts["tool.confirmation_required"])
	}
	if !strings.Contains(lastToolResult(t, h), "not permitted") {
		t.Errorf("model saw %q, want a not-permitted result", lastToolResult(t, h))
	}
}

func TestUnknownToolDefaultsToAsk(t *testing.T) {
	rec := &namedTool{name: "mystery.op", result: "done"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	h.provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "mystery.op", Arguments: `{}`}},
	}
	h.provider.Response = "Mystery handled."

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("do the mystery thing")
	ev := h.waitFor(t, "tool.confirmation_required")
	if ev.Data["tool"] != "mystery.op" {
		t.Errorf("tool = %v", ev.Data["tool"])
	}
	_ = h.engine.Confirm(true)
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
	if rec.calls != 1 {
		t.Errorf("tool calls = %d", rec.calls)
	}
}

func TestRememberForConversation(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{RememberApprovals: true}, rec, tools.PolicyConfig{})
	args, _ := json.Marshal(map[string]string{"command": "rm -rf ./build"})
	call := ai.ToolCall{ID: "c1", Name: "shell.run", Arguments: string(args)}
	// Scripted upfront (the fake's round counter spans sessions): the model
	// runs the same command twice within one turn, answers, and runs it once
	// more in the next conversation.
	h.provider.ToolCallsByRound = [][]ai.ToolCall{{call}, {call}, nil, {call}}
	h.provider.Response = "Removed it."

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir, twice")
	h.waitFor(t, "tool.confirmation_required")
	_ = h.engine.Confirm(true)
	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)

	if rec.calls != 2 {
		t.Errorf("tool calls = %d, want 2", rec.calls)
	}
	// The one confirmation was consumed by waitFor above; the remembered
	// approval means no further one appeared.
	if counts["tool.confirmation_required"] != 0 {
		t.Errorf("asked again %d times; the approval should have been remembered",
			counts["tool.confirmation_required"])
	}

	// A new conversation must ask again: approvals die with the thread.
	h.engine.ResetConversation()
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean it again")
	h.waitFor(t, "tool.confirmation_required")
	_ = h.engine.Confirm(true)
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
	if rec.calls != 3 {
		t.Errorf("tool calls = %d, want 3", rec.calls)
	}
}

func TestApprovalsNotRememberedByDefault(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	args, _ := json.Marshal(map[string]string{"command": "rm -rf ./build"})
	call := ai.ToolCall{ID: "c1", Name: "shell.run", Arguments: string(args)}
	h.provider.ToolCallsByRound = [][]ai.ToolCall{{call}, {call}}
	h.provider.Response = "Removed it twice."

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir, twice")
	h.waitFor(t, "tool.confirmation_required")
	_ = h.engine.Confirm(true)
	// remember_for_conversation = false: the second identical call asks again.
	h.waitFor(t, "tool.confirmation_required")
	_ = h.engine.Confirm(true)
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
	if rec.calls != 2 {
		t.Errorf("tool calls = %d, want 2", rec.calls)
	}
}

func TestConfirmationSummaryIsSpoken(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{SpeakResponses: true}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "Done.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	h.waitFor(t, "tool.confirmation_required")
	// The deadline event is the daemon saying the question has been asked
	// aloud; answering before it now cancels the remaining read-out (issue
	// #119), and this test is about the question being spoken at all.
	h.waitFor(t, "tool.confirmation_deadline")
	_ = h.engine.Confirm(true)
	h.countUntil(t, "session.finished")
	h.waitIdle(t)

	// Two syntheses: the spoken confirmation question and the final answer.
	if h.tts.Speaks() < 2 {
		t.Errorf("speaks = %d, want the summary plus the answer", h.tts.Speaks())
	}
}

func TestConfirmWithNothingPendingErrors(t *testing.T) {
	h := newHarness(t, Options{})
	if err := h.engine.Confirm(true); err == nil {
		t.Error("Confirm with nothing pending must error")
	}
}

func TestIsAffirmative(t *testing.T) {
	yes := []string{
		"yes", "Yes.", "yeah", "yep", "sure", "OK", "okay",
		"go ahead", "Go ahead!", "do it", "go for it", "please do",
		"yes please", "confirmed", "affirmative", "proceed",
	}
	no := []string{
		"", "no", "No!", "nope", "don't", "do not do it", "stop",
		"cancel", "wait", "yes but not now", // negation anywhere declines
		"what will that delete", "tell me more", "the weather is nice",
		"o", "yesterday", // near-misses must not approve
	}
	for _, s := range yes {
		if !isAffirmative(s) {
			t.Errorf("isAffirmative(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if isAffirmative(s) {
			t.Errorf("isAffirmative(%q) = true, want false", s)
		}
	}
}
