package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Shell runs non-interactive commands on the user's behalf — the tool behind
// "what's happening in docker?". It is opt-in ([tools] shell = true): the
// assistant gets the same authority as the user's own shell, which is the
// point, and the risk, so every command is logged and execution is bounded
// by a timeout and an output cap.
type Shell struct {
	// Timeout bounds one command. Zero means DefaultShellTimeout.
	Timeout time.Duration
	// MaxOutput caps captured bytes; excess is truncated with a marker.
	// Zero means DefaultShellMaxOutput.
	MaxOutput int
	// Log records every executed command. Nil uses slog.Default().
	Log *slog.Logger
}

// Shell execution bounds.
const (
	DefaultShellTimeout   = 30 * time.Second
	DefaultShellMaxOutput = 16 * 1024
)

// Name implements Tool.
func (s *Shell) Name() string { return "shell.run" }

// Description implements Tool.
func (s *Shell) Description() string {
	return "Run a non-interactive shell command on the user's computer and return its output. " +
		"Use this to answer questions about the system's live state (processes, containers, " +
		"files, git, services) and to perform actions the user asks for. Commands run as the " +
		"user with their normal permissions, with a timeout — do not start long-running or " +
		"interactive programs."
}

// Schema implements Tool.
func (s *Shell) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {
				"type": "string",
				"description": "The command to run, interpreted by bash -c"
			}
		},
		"required": ["command"]
	}`)
}

// Execute implements Tool.
func (s *Shell) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid shell.run arguments: %w", err)
	}
	if strings.TrimSpace(args.Command) == "" {
		return "", fmt.Errorf("shell.run: empty command")
	}

	logger := s.Log
	if logger == nil {
		logger = slog.Default()
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultShellTimeout
	}
	maxOutput := s.MaxOutput
	if maxOutput <= 0 {
		maxOutput = DefaultShellMaxOutput
	}

	logger.Info("running command", "component", "tools", "tool", "shell.run", "command", args.Command)
	start := time.Now()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", args.Command)
	cmd.Dir, _ = os.UserHomeDir()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.Stdin = nil // non-interactive: commands reading stdin get EOF
	err := cmd.Run()

	result := out.String()
	if len(result) > maxOutput {
		result = result[:maxOutput] + "\n[output truncated]"
	}

	switch {
	case ctx.Err() == context.DeadlineExceeded:
		result += fmt.Sprintf("\n[command killed after %s timeout]", timeout)
	case err != nil:
		// The command ran and failed: that is information for the model.
		result += fmt.Sprintf("\n[exit status: %v]", err)
	}
	if strings.TrimSpace(result) == "" {
		result = "[no output, exit status 0]"
	}
	logger.Info("command finished", "component", "tools", "tool", "shell.run",
		"duration_ms", time.Since(start).Milliseconds(), "output_bytes", out.Len())
	return result, nil
}
