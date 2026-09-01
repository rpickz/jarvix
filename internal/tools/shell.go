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

	"github.com/rpickz/jarvix/internal/confine"
	"github.com/rpickz/jarvix/internal/undo"
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

// How a command's ending is written down. Both paths — a session's command and
// a job's confined one — use these two, and CommandSucceeded reads them back,
// so the writer and the reader of the sentence are the same package.
const (
	exitNotice    = "\n[exit status: "
	stoppedNotice = "\n[command stopped "
)

// CommandSucceeded reports whether a shell.run result describes a command that
// ran and exited zero.
//
// It exists for the job ledger (#222), and the distinction it draws is one #71
// makes expensive to get wrong. shell.run returns a command's failure as part
// of its RESULT rather than as an error, because a command that ran and exited
// 3 is information for the model rather than a fault in the tool — but a job's
// ledger reads a tool's result to decide whether the step it names happened,
// and a step recorded as done is a step the report will say was done, under the
// model's own label for it. So "the tool worked" and "the command worked" have
// to be told apart somewhere, and this is that somewhere: one predicate, next
// to the two sentences it reads, rather than a substring match in the daemon
// that would drift the first time the wording improved.
func CommandSucceeded(said string) bool {
	return !strings.Contains(said, exitNotice) && !strings.Contains(said, stoppedNotice)
}

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

	// The boundary, and the fail-CLOSED half of it (#222, ADR 0068).
	//
	// A command dispatched by a job must run inside the kernel-held boundary
	// its scope describes. The job runner installs that boundary on the
	// context; this reads it back. What makes the arrangement safe rather than
	// a hole waiting for somebody to forget is the second question: the call
	// also carries the job's id (undo.JobFrom, installed by the same runner for
	// the account), so a command that belongs to a job and arrives with NO
	// boundary is refused outright rather than run the old way.
	//
	// So the failure mode of a future caller forgetting to install a spec is a
	// command that does not run — never one that runs unconfined. That
	// direction is the whole point: today a job parks visibly when it proposes
	// a command, and a silently-unconfined command would be strictly worse than
	// the refusal it replaced.
	//
	// A session's own command is untouched. The user is present, they are being
	// asked at the gate, and the authority they are lending is their own —
	// which is what shell.run has always been and what its opt-in switch is
	// about.
	spec, bounded := confine.From(ctx)
	if job := undo.JobFrom(ctx); job != "" && !bounded {
		return "", fmt.Errorf("I won't run a command for a job without a boundary around it: " +
			"nothing told me which directories this one may touch, and a command I cannot " +
			"hold inside a scope is a command that has no scope")
	}
	if bounded {
		return s.confined(ctx, logger, spec, args.Command, timeout, maxOutput, start)
	}

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
		result += stoppedNotice + fmt.Sprintf("after %s timeout]", timeout)
	case err != nil:
		// The command ran and failed: that is information for the model.
		result += exitNotice + fmt.Sprintf("%v]", err)
	}
	if strings.TrimSpace(result) == "" {
		result = "[no output, exit status 0]"
	}
	logger.Info("command finished", "component", "tools", "tool", "shell.run",
		"duration_ms", time.Since(start).Milliseconds(), "output_bytes", out.Len())
	// The account records that this ran, verbatim, and promises nothing about
	// putting it back (#201, ADR 0064). That distinction is the ticket's
	// spine: a shell command the user approved is described honestly, never
	// falsely offered as undoable, because Jarvix has no idea what it did and
	// an offer it could not keep would be worse than no offer at all. The
	// command is the summary because a summary of a command is a paraphrase,
	// and this is the one row where a paraphrase would be a lie.
	//
	// Recorded whatever the exit status: a command that failed still ran, and
	// half of it may well have landed.
	undo.Note(ctx, undo.Action{
		Tool:    ShellToolName,
		Summary: "ran " + args.Command,
		Restore: undo.OneWay(ShellToolName),
	})
	return result, nil
}

// confined runs one command inside the kernel-held boundary a job's scope
// describes, and reports what was observed and nothing more.
//
// The account record is the part worth reading twice. It is written when — and
// only when — internal/confine observed the command actually start inside the
// boundary, which it reports as an observation of its helper's status pipe
// rather than as the absence of an error. A record saying "ran ..." for a
// command that never started would be the #71 failure written straight into the
// ledger the report is built from, which is precisely the thing a job's account
// exists to make impossible.
//
// A command that ran and FAILED still gets its record, exactly as the
// unconfined path does: a command that failed still ran, and half of it may
// well have landed.
func (s *Shell) confined(ctx context.Context, logger *slog.Logger, spec confine.Spec,
	command string, timeout time.Duration, maxOutput int, start time.Time) (string, error) {
	outcome, err := confine.Runner{}.Run(ctx, confine.Request{
		Command: command, Spec: spec, Timeout: timeout, MaxOutput: maxOutput,
	})
	if err != nil {
		// Refusals reach the model and the ledger as the sentence the
		// confinement layer wrote, because that layer is the only code that
		// knows what was measured. Nothing ran.
		logger.Info("command not run: the boundary could not be held", "component", "tools",
			"tool", ShellToolName, "reason", err.Error())
		return "", err
	}
	result := outcome.Output
	if outcome.Truncated {
		result += "\n[output truncated]"
	}
	switch {
	case outcome.TimedOut:
		result += stoppedNotice + fmt.Sprintf("after %s timeout]", timeout)
	case outcome.Killed:
		// Stopped by whoever owned the context — the job was stopped, or the
		// daemon is shutting down. Deliberately not worded as a timeout: how
		// long it had been going is not the reason it ended, and a sentence
		// that named the timeout would be a small false claim about why.
		result += stoppedNotice + "before it finished]"
	case outcome.Exit != 0:
		result += exitNotice + fmt.Sprintf("%d]", outcome.Exit)
	}
	if strings.TrimSpace(result) == "" {
		result = "[no output, exit status 0]"
	}
	logger.Info("command finished", "component", "tools", "tool", ShellToolName,
		"duration_ms", time.Since(start).Milliseconds(), "output_bytes", len(outcome.Output),
		"confined", outcome.Confined, "exit", outcome.Exit)
	undo.Note(ctx, undo.Action{
		Tool:    ShellToolName,
		Summary: "ran " + command,
		Restore: undo.OneWay(ShellToolName),
	})
	return result, nil
}
