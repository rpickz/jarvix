package intent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner executes a matched intent's action. It is an interface for the same
// reason the speech engines are (ADR 0002): the daemon shells out to real
// binaries, and tests must be able to prove exactly what would run without
// changing the machine's volume or spawning a terminal.
//
// The two methods are deliberately different shapes, and that difference is
// the security boundary:
//
//   - Run takes an argv the intent table built. No shell, no word splitting,
//     no interpolation — the transcript contributed at most one integer,
//     already rendered into an argument.
//   - RunShell takes a command the *user* wrote in their own configuration.
//     It goes through bash, which is why the engine only calls it after the
//     tool permission gate (ADR 0014) has classified the command.
type Runner interface {
	Run(ctx context.Context, argv []string) error
	RunShell(ctx context.Context, command string) error
}

// DefaultTimeout bounds one intent action. Intents are meant to be
// instantaneous; anything still running after this is wedged, and a wedged
// wpctl must not hold a session open.
const DefaultTimeout = 5 * time.Second

// ExecRunner is the real Runner: it runs commands as the user.
type ExecRunner struct {
	// Timeout bounds one action. Zero means DefaultTimeout.
	Timeout time.Duration
	// Log records every execution. Nil uses slog.Default().
	Log *slog.Logger
}

// Run implements Runner. argv[0] is looked up on PATH first so a missing
// binary produces "wpctl is not installed" — something worth speaking aloud —
// rather than exec's "no such file or directory".
func (r *ExecRunner) Run(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("intent has no command to run")
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		return fmt.Errorf("%s is not installed", argv[0])
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, _ = os.UserHomeDir()
	cmd.Stdin = nil
	return r.finish(ctx, argv[0], cmd)
}

// RunShell implements Runner for user-defined intents.
func (r *ExecRunner) RunShell(ctx context.Context, command string) error {
	if strings.TrimSpace(command) == "" {
		return fmt.Errorf("intent has no command to run")
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir, _ = os.UserHomeDir()
	cmd.Stdin = nil
	return r.finish(ctx, firstWord(command), cmd)
}

func (r *ExecRunner) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return DefaultTimeout
}

// finish runs the command and turns its failure into one short sentence,
// because the caller speaks it.
func (r *ExecRunner) finish(ctx context.Context, name string, cmd *exec.Cmd) error {
	logger := r.Log
	if logger == nil {
		logger = slog.Default()
	}
	start := time.Now()
	out, err := cmd.CombinedOutput()
	logger.Debug("intent command finished", "component", "intent", "command", name,
		"duration_ms", time.Since(start).Milliseconds())
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%s did not finish in time", name)
	}
	if err != nil {
		if detail := firstLine(string(out)); detail != "" {
			return fmt.Errorf("%s failed: %s", name, detail)
		}
		return fmt.Errorf("%s failed", name)
	}
	return nil
}

func firstWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return "the command"
}

// firstLine keeps a spoken failure to one sentence: command output can be
// pages long, and none of it belongs in a text-to-speech queue.
func firstLine(s string) string {
	const maxSpoken = 120
	line := strings.TrimSpace(s)
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if runes := []rune(line); len(runes) > maxSpoken {
		return string(runes[:maxSpoken]) + "…"
	}
	return line
}
