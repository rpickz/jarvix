package session

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/tools"
)

// slowTool stands in for an advisor consultation: it takes long enough that
// the user would wonder, and it describes itself so Jarvix can say so.
type slowTool struct {
	duration time.Duration
	silent   bool // no Activity: a tool that does not describe itself

	mu    sync.Mutex
	calls int
}

func (s *slowTool) Name() string            { return "advisor.ask" }
func (s *slowTool) Description() string     { return "fake advisor" }
func (s *slowTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (s *slowTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	select {
	case <-time.After(s.duration):
		return "claude answered: use a queue.", nil
	case <-ctx.Done():
		return "interrupted", nil
	}
}

func (s *slowTool) Activity(json.RawMessage) (string, string, bool) {
	if s.silent {
		return "", "", false
	}
	return "Consulting claude…", "I'm still waiting on claude.", true
}

// newProgressHarness wires a registry holding one slow tool, with the
// progress threshold shortened so the test does not wait ten seconds.
func newProgressHarness(t *testing.T, tool tools.Tool, after time.Duration) *harness {
	t.Helper()
	h := newGateHarness(t, Options{SpeakResponses: true},
		tool, tools.PolicyConfig{Tools: map[string]tools.PolicyDecision{"advisor.ask": tools.PolicyAllow}})
	h.engine.progressAfter = after
	h.provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "advisor.ask", Arguments: `{"advisor":"claude","question":"design my schema"}`}},
	}
	h.provider.Response = "Claude suggests a queue."
	return h
}

func TestSlowToolAnnouncesItselfOnceAndIsLabelled(t *testing.T) {
	tool := &slowTool{duration: 150 * time.Millisecond}
	h := newProgressHarness(t, tool, 10*time.Millisecond)

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("design my schema")

	var progress []Event
	var started Event
	deadline := time.After(5 * time.Second)
	for done := false; !done; {
		select {
		case ev := <-h.events:
			switch ev.Type {
			case "tool.progress":
				progress = append(progress, ev)
			case "tool.started":
				started = ev
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

	// Once, not a countdown: reassurance is the point.
	if len(progress) != 1 {
		t.Fatalf("tool.progress published %d times, want 1", len(progress))
	}
	if msg, _ := progress[0].Data["message"].(string); !strings.Contains(msg, "claude") {
		t.Errorf("progress message = %q", msg)
	}
	if progress[0].Data["tool"] != "advisor.ask" {
		t.Errorf("progress tool = %v", progress[0].Data["tool"])
	}
	// The overlay's label covers the whole call, so it rides tool.started.
	if detail, _ := started.Data["detail"].(string); !strings.HasPrefix(detail, "Consulting claude") {
		t.Errorf("tool.started detail = %q", detail)
	}
	// And it was said out loud: one synthesis for the progress line, one for
	// the answer.
	if speaks := h.tts.Speaks(); speaks < 2 {
		t.Errorf("tts speaks = %d, want the progress line spoken as well as the answer", speaks)
	}
}

func TestFastToolStaysSilent(t *testing.T) {
	// The tool finishes long before the threshold: nothing is announced.
	tool := &slowTool{duration: 0}
	h := newProgressHarness(t, tool, time.Hour)

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("design my schema")
	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)

	if counts["tool.progress"] != 0 {
		t.Errorf("a fast tool must not announce progress (%d)", counts["tool.progress"])
	}
	if counts["tool.started"] != 1 || counts["tool.finished"] != 1 {
		t.Errorf("started/finished = %d/%d", counts["tool.started"], counts["tool.finished"])
	}
}

func TestToolWithoutActivityIsNeverLabelled(t *testing.T) {
	tool := &slowTool{duration: 30 * time.Millisecond, silent: true}
	h := newProgressHarness(t, tool, time.Millisecond)

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("design my schema")
	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)

	if counts["tool.progress"] != 0 {
		t.Error("a tool that does not describe itself must not be announced")
	}
}

func TestInterruptionStopsTheProgressWatcher(t *testing.T) {
	// A cancelled session must not speak a reassurance about work nobody is
	// waiting for any more, and must leave no goroutine behind.
	tool := &slowTool{duration: 2 * time.Second}
	h := newProgressHarness(t, tool, 50*time.Millisecond)

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("design my schema")
	h.waitFor(t, "tool.started")
	_ = h.engine.Cancel()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-h.events:
			switch ev.Type {
			case "tool.progress":
				t.Fatal("cancelled session announced progress")
			case "session.cancelled":
				h.waitIdle(t)
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the session to cancel")
		}
	}
}
