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
