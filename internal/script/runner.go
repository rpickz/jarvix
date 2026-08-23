package script

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rpickz/jarvix/internal/tools"
)

// Output caps. Stdout is generous because the stdout report mode speaks from
// it; stderr is diagnostics only — logged and quoted one line at a time,
// never dumped. Both are hard ceilings, so a script that prints a gigabyte
// costs a gigabyte of nothing.
const (
	maxStdout = 64 * 1024
	maxStderr = 4 * 1024
	// maxSpokenLine bounds what one stdout/stderr line may put into speech.
	// Past this the line is a document, not an acknowledgement.
	maxSpokenLine = 200
	// waitDelay is how long Wait may block after the process group has been
	// killed — a grandchild holding the output pipe must not wedge a session.
	waitDelay = 2 * time.Second
)

// ErrAlreadyRunning is wrapped into the refusal when a script's phrase
// arrives while a run is still going. Refusing is the point: the phrase is
// not a queue, and two concurrent runs of "backup my notes" would race over
// whatever the script touches.
var ErrAlreadyRunning = errors.New("already running")

// Options configure a Runner.
type Options struct {
	// Definitions are the validated scripts this runner can execute.
	Definitions []Definition
	// Publish emits script.started / script.finished for the activity feed
	// and the window. Nil publishes nothing.
	Publish func(event string, data map[string]any)
	// Log records each run: name, path, exit code, duration — never output.
	// Nil uses slog.Default().
	Log *slog.Logger
}

// Runner executes scripts, one at a time across all of them. One at a time is
// deliberate breadth: the scripts are the user's own and may share state (the
// same notes directory, the same drive), and the runner cannot know which
// pairs are safe to interleave — so none are.
type Runner struct {
	defs    []Definition
	publish func(string, map[string]any)
	log     *slog.Logger

	// now is the run's clock, injectable so duration reporting is
	// deterministic in tests — the same seam the routine runner carries.
	now func() time.Time

	mu      sync.Mutex
	running string // the script currently executing, "" when idle
}

// New builds a Runner.
func New(opts Options) *Runner {
	r := &Runner{
		defs:    append([]Definition(nil), opts.Definitions...),
		publish: opts.Publish,
		log:     opts.Log,
		now:     time.Now,
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	return r
}

// Definitions returns the scripts this runner knows, in configured order.
func (r *Runner) Definitions() []Definition {
	return append([]Definition(nil), r.defs...)
}

// Path resolves a script name (case-insensitively) to its configured path.
// It exists for the permission gate: the confirmation must name the exact
// file about to run, and the engine asks the runner rather than carrying a
// second copy of the configuration that could drift from the one executed.
func (r *Runner) Path(name string) (string, bool) {
	def, ok := r.definition(name)
	if !ok {
		return "", false
	}
	return def.Path, true
}

// Run executes the named script under ctx and returns what the engine should
// speak — possibly nothing, for a silent success. err is for a run that could
// not happen (unknown name, already running, a file that vanished), was
// cancelled, or failed; the engine speaks errors in every report mode, which
// is what makes failures impossible to configure away. Error strings are
// composed to read aloud after "Sorry, ".
func (r *Runner) Run(ctx context.Context, name string) (string, error) {
	def, ok := r.definition(name)
	if !ok {
		// Unreachable from the router, which only matches configured names —
		// but the IPC surface takes names too, and a bug must be a sentence.
		return "", fmt.Errorf("no script is called %q", name)
	}
	if !r.begin(def.Name) {
		return "", fmt.Errorf("%s is %w", def.Name, ErrAlreadyRunning)
	}
	defer r.end()

	// Re-check the file now, not just at config load: the gate's confirmation
	// named this exact path, and a file deleted or de-executabled since load
	// must be a spoken sentence, never a raw exec error.
	if problem := pathProblem(def.Path); problem != "" {
		return "", fmt.Errorf("the %s script cannot run: %s", def.Name, problem)
	}

	r.emit("script.started", map[string]any{"script": def.Name, "path": def.Path})
	r.log.Info("script started", "component", "script", "script", def.Name,
		"path", def.Path, "timeout_sec", int(def.Timeout.Seconds()))
	started := r.now()

	runCtx, cancel := context.WithTimeout(ctx, def.Timeout)
	defer cancel()
	res := run(runCtx, def.Path)
	duration := r.now().Sub(started)

	if ctx.Err() != nil {
		// Cancelled ("stop", an interrupting session, shutdown): the process
		// group is already dead and the cancel path owns what, if anything,
		// is said. No script.finished either — session.cancelled owns that
		// ending, exactly as it does for a routine.
		r.log.Info("script cancelled", "component", "script", "script", def.Name,
			"duration_ms", duration.Milliseconds())
		return "", ctx.Err()
	}

	status := "ok"
	if res.timedOut || res.err != nil || res.exitCode != 0 {
		status = "failed"
	}
	r.emit("script.finished", map[string]any{
		"script": def.Name, "path": def.Path, "status": status,
		"exit_code": res.exitCode, "timed_out": res.timedOut,
		"duration_ms": duration.Milliseconds(),
	})
	r.log.Info("script finished", "component", "script", "script", def.Name,
		"status", status, "exit_code", res.exitCode, "timed_out", res.timedOut,
		"duration_ms", duration.Milliseconds(), "stdout_bytes", len(res.stdout))

	switch {
	case res.timedOut:
		return "", fmt.Errorf("%s did not finish within %s and was stopped",
			def.Name, spokenDuration(def.Timeout))
	case res.err != nil:
		// The exec itself failed. The OS error stays in the log — it is the
		// operator's material, and anything returned here will be read aloud.
		r.log.Warn("script could not start", "component", "script",
			"script", def.Name, "path", def.Path, "error", res.err.Error())
		return "", fmt.Errorf("%s could not start", def.Name)
	case res.exitCode != 0:
		// Failure always names the exit code, and carries stderr's first line
		// when there is one — that line is what the script's author wrote for
		// exactly this moment. Speech normalisation happens in the speaker.
		if line := firstLine(res.stderr); line != "" {
			return "", fmt.Errorf("%s failed — exit %d: %s", def.Name, res.exitCode, line)
		}
		return "", fmt.Errorf("%s failed — exit %d", def.Name, res.exitCode)
	}

	return successLine(def, res.stdout), nil
}

// successLine applies the report mode to a successful run. Only success: by
// the time this is called every failure has already become an error the
// engine speaks unconditionally.
func successLine(def Definition, stdout string) string {
	switch def.Report {
	case ReportSilent:
		return ""
	case ReportStdout:
		if line := firstLine(stdout); line != "" {
			return line
		}
		// A script that promised stdout and printed none still finished;
		// saying so beats silence the user did not configure.
		return capitalise(def.Name) + " finished."
	default:
		return capitalise(def.Name) + " finished."
	}
}

// definition finds a script by name, case-insensitively — the IPC surface
// should not have to reproduce exact casing.
func (r *Runner) definition(name string) (Definition, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	for _, def := range r.defs {
		if strings.ToLower(strings.TrimSpace(def.Name)) == want {
			return def, true
		}
	}
	return Definition{}, false
}

// begin claims the runner for one script; false means a run is in flight.
func (r *Runner) begin(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running != "" {
		return false
	}
	r.running = name
	return true
}

func (r *Runner) end() {
	r.mu.Lock()
	r.running = ""
	r.mu.Unlock()
}

// emit publishes one bus event, if anyone is listening. Events carry names,
// paths, exit codes, and durations — never a byte of output, which can hold
// anything the script read.
func (r *Runner) emit(event string, data map[string]any) {
	if r.publish != nil {
		r.publish(event, data)
	}
}

// result is the outcome of one child process.
type result struct {
	stdout   string
	stderr   string
	exitCode int
	timedOut bool
	err      error
}

// run executes one script. The discipline is the advisor path's (ADR 0016),
// restated because every property is load-bearing here too:
//
//   - Zero arguments and no shell. The exec call names the path and nothing
//     else — no argv slice exists for anything spoken (or anything at all)
//     to travel in, which is how the v1 no-arguments rule is enforced by
//     construction rather than by a filter someone could weaken.
//   - A scrubbed environment. The daemon holds API keys; the user's script
//     is another program and gets none of them (tools.ScrubbedEnv, the one
//     shared copy of the secret-name rules).
//   - Its own process group, killed as a group on timeout or cancellation.
//     Scripts spawn children; killing only the parent after "stop" would
//     leave the rsync running.
//   - Capped output, empty stdin.
func run(ctx context.Context, path string) result {
	cmd := exec.CommandContext(ctx, path) //nolint:gosec // the path comes from validated config; nothing else is passed
	cmd.Env = tools.ScrubbedEnv(os.Environ(), nil)
	cmd.Dir, _ = os.UserHomeDir()
	cmd.Stdin = strings.NewReader("") // never the daemon's stdin
	out := &cappedBuffer{max: maxStdout}
	errBuf := &cappedBuffer{max: maxStderr}
	cmd.Stdout = out
	cmd.Stderr = errBuf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid: the whole group, so the script's children die too.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = waitDelay

	err := cmd.Run()
	res := result{
		stdout:   out.String(),
		stderr:   errBuf.String(),
		timedOut: errors.Is(ctx.Err(), context.DeadlineExceeded),
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.exitCode = exitErr.ExitCode()
	default:
		res.err = err
	}
	return res
}

// cappedBuffer collects at most max bytes and discards the rest. Each stream
// has its own buffer, written by one goroutine. (The advisor path carries an
// identical unexported one; the eleven lines are cheaper than the export.)
type cappedBuffer struct {
	max int
	buf bytes.Buffer
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		c.buf.Write(p)
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }

// firstLine reduces captured output to the one line speech gets, bounded so a
// script that prints a novel on one line costs a sentence.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	runes := []rune(s)
	if len(runes) > maxSpokenLine {
		s = string(runes[:maxSpokenLine]) + "…"
	}
	return s
}

// spokenDuration renders a timeout the way it should be heard: "45 seconds",
// "5 minutes" — never "1m30s", which a speech engine reads as a part number.
func spokenDuration(d time.Duration) string {
	sec := int(d.Seconds())
	switch {
	case sec < 120:
		return fmt.Sprintf("%d seconds", sec)
	case sec%60 == 0:
		return fmt.Sprintf("%d minutes", sec/60)
	default:
		return fmt.Sprintf("%d minutes %d seconds", sec/60, sec%60)
	}
}

// capitalise upper-cases the first letter so a script named in lower case
// opens its sentence properly.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
