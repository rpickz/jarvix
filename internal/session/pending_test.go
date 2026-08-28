package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/tools"
)

// The conversation window's pending turn counts a wait from the *daemon's*
// phase start (issue #158). A client counting from when it noticed is right
// only for a client that was already watching; a window opened five seconds
// into a long think would start at zero and tell the user a comfortable lie
// about how long they have been waiting. So the start rides every
// state.changed, and Phase() reports it for the snapshot.

func TestStateChangedCarriesThePhaseStart(t *testing.T) {
	h := newHarness(t, Options{})
	before := time.Now()

	if _, err := h.engine.SubmitText("explain recursion"); err != nil {
		t.Fatal(err)
	}

	// Every state.changed of the session, in publish order — the bus delivers
	// to one subscriber in order, so this needs no goroutine ordering of its
	// own.
	var starts []int64
	deadline := time.After(5 * time.Second)
	for done := false; !done; {
		select {
		case ev := <-h.events:
			switch ev.Type {
			case "state.changed":
				since, ok := ev.Data["since_ms"].(int64)
				if !ok {
					t.Fatalf("state.changed to %v carries since_ms %#v; the window cannot count from it",
						ev.Data["state"], ev.Data["since_ms"])
				}
				starts = append(starts, since)
			case "session.finished":
				done = true
			case "error":
				t.Fatalf("session failed: %v", ev.Data)
			}
		case <-deadline:
			t.Fatal("timed out waiting for the session to finish")
		}
	}
	h.waitIdle(t)
	after := time.Now()

	if len(starts) < 2 {
		t.Fatalf("only %d state transitions seen; the phase clock was never exercised", len(starts))
	}
	for i, since := range starts {
		if since < before.UnixMilli() || since > after.UnixMilli() {
			t.Errorf("transition %d began at %d, outside the test's own window [%d, %d]",
				i, since, before.UnixMilli(), after.UnixMilli())
		}
		// "Elapsed in *this* phase", so the clock restarts on every
		// transition and can never run backwards.
		if i > 0 && since < starts[i-1] {
			t.Errorf("transition %d began at %d, before its predecessor at %d",
				i, since, starts[i-1])
		}
	}
}

// A window opened before anything has ever happened still asks for the phase
// start. A zero time there would be subtracted from now and reported as fifty
// years of idleness.
func TestPhaseStartIsSetBeforeTheFirstSession(t *testing.T) {
	h := newHarness(t, Options{})
	phase := h.engine.Phase()
	if phase.State != StateIdle {
		t.Fatalf("a fresh engine is in %q", phase.State)
	}
	if phase.Since.IsZero() {
		t.Fatal("a fresh engine reports no phase start; a client would compute decades of idleness")
	}
	if time.Since(phase.Since) > time.Minute {
		t.Errorf("idle since %v, which is not when this engine was built", phase.Since)
	}
	if phase.Tool != "" || phase.ToolDetail != "" {
		t.Errorf("a fresh engine reports tool %q / %q", phase.Tool, phase.ToolDetail)
	}
}

// narratedTool blocks inside Execute until the test releases it, and — unlike
// the plain gatedTool next door — describes itself, so the phase carries a
// progress label as well as a name.
//
// The ordering is established explicitly through the two channels rather than
// by sleeping or by hoping the scheduler cooperates: `entered` proves the call
// has begun, and the call provably cannot end until `release` is closed. That
// is what lets these tests look at the engine *while* a tool is genuinely in
// flight, with or without -race.
type narratedTool struct {
	entered chan struct{}
	release chan struct{}
}

func (g *narratedTool) Name() string            { return "advisor.ask" }
func (g *narratedTool) Description() string     { return "fake advisor" }
func (g *narratedTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (g *narratedTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	close(g.entered)
	select {
	case <-g.release:
		return "claude answered: use a queue.", nil
	case <-ctx.Done():
		return "interrupted", nil
	}
}

func (g *narratedTool) Activity(json.RawMessage) (string, string, bool) {
	return "Consulting claude…", "I'm still waiting on claude.", true
}

// A window opened during a tool round has to show what a window that watched
// it start shows: "Consulting claude", not a bare "Thinking". The snapshot
// carries the call in flight because only the daemon knows it.
func TestPhaseNamesTheToolInFlight(t *testing.T) {
	tool := &narratedTool{entered: make(chan struct{}), release: make(chan struct{})}
	h := newGateHarness(t, Options{}, tool,
		tools.PolicyConfig{Tools: map[string]tools.PolicyDecision{"advisor.ask": tools.PolicyAllow}})
	h.provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "advisor.ask", Arguments: `{"advisor":"claude","question":"design my schema"}`}},
	}
	h.provider.Response = "Claude suggests a queue."

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("design my schema"); err != nil {
		t.Fatal(err)
	}

	select {
	case <-tool.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the tool call never started")
	}

	phase := h.engine.Phase()
	if phase.Tool != "advisor.ask" {
		t.Errorf("phase tool = %q while advisor.ask is executing", phase.Tool)
	}
	// The tool's own progress label — the same string tool.started publishes
	// as `detail` and the pending turn renders as "Consulting claude".
	if phase.ToolDetail != "Consulting claude…" {
		t.Errorf("phase tool detail = %q, want the tool's own progress label", phase.ToolDetail)
	}
	if phase.SessionID == "" {
		t.Error("a tool is running but the phase names no session")
	}

	close(tool.release)
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	// Nothing is in flight once the turn is over. A surface still saying
	// "Consulting claude" after the answer arrived would be reporting work
	// that belongs to nothing.
	if after := h.engine.Phase(); after.Tool != "" || after.ToolDetail != "" {
		t.Errorf("after the session, phase reports tool %q / %q", after.Tool, after.ToolDetail)
	}
}

// A tool still blocked inside a cancelled context returns late, or never. The
// session-ending paths clear the record themselves so the pending turn cannot
// outlive the turn that owned it.
func TestPhaseForgetsTheToolWhenTheSessionIsCancelled(t *testing.T) {
	tool := &narratedTool{entered: make(chan struct{}), release: make(chan struct{})}
	h := newGateHarness(t, Options{}, tool,
		tools.PolicyConfig{Tools: map[string]tools.PolicyDecision{"advisor.ask": tools.PolicyAllow}})
	h.provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "advisor.ask", Arguments: `{"advisor":"claude"}`}},
	}

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("ask claude"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tool.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the tool call never started")
	}
	if h.engine.Phase().Tool == "" {
		t.Fatal("the tool is executing but the phase does not name it")
	}

	if err := h.engine.Cancel(); err != nil {
		t.Fatal(err)
	}
	h.waitIdle(t)

	// Asserted after the engine is provably idle: the cancel path clears the
	// record on its way to Idle, so this needs no cooperation from the tool
	// goroutine, which is still parked in Execute.
	if phase := h.engine.Phase(); phase.Tool != "" || phase.ToolDetail != "" {
		t.Errorf("a cancelled session left tool %q / %q on the phase", phase.Tool, phase.ToolDetail)
	}
	close(tool.release)
}
