package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
)

func runShell(t *testing.T, s *Shell, command string) string {
	t.Helper()
	input, _ := json.Marshal(map[string]string{"command": command})
	out, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute(%q): %v", command, err)
	}
	return out
}

func TestShellRunsCommand(t *testing.T) {
	out := runShell(t, &Shell{}, "echo hello world")
	if !strings.Contains(out, "hello world") {
		t.Errorf("out = %q", out)
	}
}

func TestShellCapturesStderrAndExitStatus(t *testing.T) {
	out := runShell(t, &Shell{}, "echo oops >&2; exit 3")
	if !strings.Contains(out, "oops") || !strings.Contains(out, "exit status") {
		t.Errorf("out = %q", out)
	}
}

func TestShellTimeout(t *testing.T) {
	s := &Shell{Timeout: 200 * time.Millisecond}
	start := time.Now()
	out := runShell(t, s, "sleep 10")
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout not enforced")
	}
	if !strings.Contains(out, "timeout") {
		t.Errorf("out = %q", out)
	}
}

func TestShellOutputCap(t *testing.T) {
	s := &Shell{MaxOutput: 100}
	out := runShell(t, s, "yes x | head -1000")
	if len(out) > 200 || !strings.Contains(out, "truncated") {
		t.Errorf("len=%d out tail=%q", len(out), out[max(0, len(out)-40):])
	}
}

func TestShellEmptyOutput(t *testing.T) {
	if out := runShell(t, &Shell{}, "true"); !strings.Contains(out, "no output") {
		t.Errorf("out = %q", out)
	}
}

func TestShellCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	input, _ := json.Marshal(map[string]string{"command": "sleep 10"})
	start := time.Now()
	_, _ = (&Shell{}).Execute(ctx, input)
	if time.Since(start) > 3*time.Second {
		t.Fatal("session cancellation did not kill the command")
	}
}

func TestShellRejectsBadInput(t *testing.T) {
	if _, err := (&Shell{}).Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Error("empty command must error")
	}
	if _, err := (&Shell{}).Execute(context.Background(), json.RawMessage(`not json`)); err == nil {
		t.Error("malformed input must error")
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry(nil)
	if !r.Empty() {
		t.Error("new registry should be empty")
	}
	r.Register(&Shell{})
	if r.Empty() || len(r.Defs()) != 1 || r.Defs()[0].Name != "shell.run" {
		t.Errorf("defs = %+v", r.Defs())
	}
	out := r.Execute(context.Background(), ai.ToolCall{Name: "shell.run", Arguments: `{"command":"echo hi"}`})
	if !strings.Contains(out, "hi") {
		t.Errorf("out = %q", out)
	}
	out = r.Execute(context.Background(), ai.ToolCall{Name: "nope", Arguments: `{}`})
	if !strings.Contains(out, "unknown tool") {
		t.Errorf("out = %q", out)
	}
	// Infrastructure failure surfaces as readable text, not a dead session.
	out = r.Execute(context.Background(), ai.ToolCall{Name: "shell.run", Arguments: `bad json`})
	if !strings.Contains(out, "error") {
		t.Errorf("out = %q", out)
	}
}

// The verbatim detail a confirmation card shows is asked of the tool itself,
// without a tier (#221).
//
// This is the difference the method exists for. CheckWithGrants consults
// Confirmable only at the ask tier, because only the ask tier has a question to
// word — so re-deriving a parked job's detail through Check would blank it the
// moment somebody re-tiered the tool, leaving the window showing a question
// with nothing underneath it. What the job parked on has not changed; only what
// the gate would decide about it next time has.
func TestTheConfirmationDetailIsTheToolsAnswerAndNotTheTiers(t *testing.T) {
	r := NewRegistry(nil)
	r.Register(&JobsStop{svc: &fakeWorking{}})
	call := ai.ToolCall{Name: JobsStopToolName, Arguments: `{"name":"deploy"}`}

	asking, err := NewPolicy(PolicyConfig{Default: PolicyAsk})
	if err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(asking)
	card := r.Check(call)
	if card.Decision != PolicyAsk || card.Command == "" {
		t.Fatalf("the fixture no longer tests what it claims to: %#v", card)
	}
	if got := r.ConfirmationDetail(call); got != card.Command {
		t.Errorf("ConfirmationDetail = %q, want the card's own %q", got, card.Command)
	}

	// Re-tiered to allow: the card's Command goes, because there is no card.
	// The detail must not, because a job parked under the old tier is still
	// parked and the user still has to see what they are approving.
	allowing, err := NewPolicy(PolicyConfig{Default: PolicyAsk,
		Tools: map[string]PolicyDecision{JobsStopToolName: PolicyAllow}})
	if err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(allowing)
	if r.Check(call).Command != "" {
		t.Fatal("an allow-tier verdict carries a command; the fixture is wrong")
	}
	if got := r.ConfirmationDetail(call); got != card.Command {
		t.Errorf("ConfirmationDetail after a re-tier = %q, want the unchanged %q", got, card.Command)
	}

	// A tool with nothing to say, and one nobody registered, answer with
	// silence rather than with a guess — which a card renders as no detail.
	if got := r.ConfirmationDetail(ai.ToolCall{Name: "shell.run", Arguments: `{"command":"ls"}`}); got != "" {
		t.Errorf("an unregistered tool answered %q", got)
	}
}
