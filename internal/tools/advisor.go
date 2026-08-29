package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// This file implements advisor delegation (ADR 0016): Jarvix's local model is
// right for instant conversational turns and wrong for "review this
// architecture". Rather than answer such a request shallowly, it hands the
// question to a stronger assistant CLI the user has already installed and
// authenticated — the way an assistant calls a specialist — and speaks what
// comes back.
//
// The security shape is the point. The model picks *which* advisor and *what
// to ask*; everything else — the binary, the flags, their order, the
// environment, the timeout — comes from configuration it cannot write. No
// shell is involved at any step, so a question is data, never syntax.

// AdvisorSpec is one configured advisor, built from a [advisors.<name>]
// table. Nothing here is model-controlled.
type AdvisorSpec struct {
	// Name is what the model asks for, e.g. "claude".
	Name string
	// Binary is the CLI to run: an absolute path, or a bare name resolved on
	// PATH at call time (so installing the CLI works without a restart).
	Binary string
	// Args is the argv template. Exactly one element may be the literal
	// AdvisorQuestionPlaceholder, replaced by the question as one argument;
	// with none, the question goes to the child's stdin.
	Args []string
	// Timeout bounds one consultation. Zero means DefaultAdvisorTimeout.
	Timeout time.Duration
	// Description tells the model what this advisor is good for.
	Description string
}

// Advisor tool bounds.
const (
	// DefaultAdvisorTimeout bounds one consultation when the spec omits one.
	DefaultAdvisorTimeout = 120 * time.Second
	// DefaultAdvisorMaxOutput caps the captured answer. Generous, because an
	// advisor's answer is the whole point of the call, but finite: the result
	// travels back through the model's context window.
	DefaultAdvisorMaxOutput = 64 * 1024
	// AdvisorQuestionPlaceholder is the argv element replaced by the
	// question. Whole-element only — the question is never interpolated into
	// a larger argument, so it cannot grow a flag.
	AdvisorQuestionPlaceholder = "{question}"
	// advisorToolName is the registry name; the permission gate keys off it.
	advisorToolName = "advisor.ask"
)

// advisorPreamble wraps the user's question before it reaches the advisor.
// The answer is going to be read aloud by a speech engine, so it asks for the
// same shape config.defaultSystemPrompt asks of the local model. Speech
// normalisation strips markdown after the fact; this stops it being written.
const advisorPreamble = "You are being consulted by a voice assistant. Your answer will be read " +
	"aloud, so reply in short plain prose: no markdown, no lists, no code blocks, no preamble, " +
	"and no file paths, URLs, or command lines spelled out verbatim. Be specific and get to the " +
	"point; a few sentences is usually right. The question is:\n\n"

// Advisor is the delegation tool: given an advisor name and a question, it
// runs that advisor's configured command, captures its answer, and hands it
// back for the assistant to speak.
type Advisor struct {
	// Advisors are the configured advisors, in the order they appear in the
	// tool schema.
	Advisors []AdvisorSpec
	// MaxOutput caps captured output; excess is truncated with a marker.
	// Zero means DefaultAdvisorMaxOutput.
	MaxOutput int
	// ScrubEnv names extra environment variables to withhold from advisors,
	// on top of the built-in secret-name patterns — the daemon passes the
	// api_key_env of every configured AI endpoint, so a key Jarvix holds for
	// itself is never handed to another program.
	ScrubEnv []string
	// Log records each consultation: advisor, duration, exit code — never the
	// question or the answer.
	Log *slog.Logger

	// runner overrides process execution in tests that need to observe the
	// exact command; the real path is runCommand.
	runner func(ctx context.Context, spec AdvisorSpec, argv []string, stdin string, env []string, maxOutput int) advisorRun
}

// Name implements Tool.
func (a *Advisor) Name() string { return advisorToolName }

// Description implements Tool. It is written to be read by a small local
// model deciding whether to use it, so the local-first rule is stated first,
// concretely, and with the cost of getting it wrong spelled out.
func (a *Advisor) Description() string {
	names := make([]string, 0, len(a.Advisors))
	for _, spec := range a.Advisors {
		names = append(names, spec.Name)
	}
	return "Ask a stronger assistant on this computer one question and get its answer. " +
		"Use this ONLY for a request that is genuinely beyond you — deep reasoning, reviewing or " +
		"planning a large amount of material, specialised or current knowledge you do not have — " +
		"or when the user names the advisor (\"ask " + firstOr(names, "claude") + " about…\"). " +
		"Answer everything else yourself: the time, quick facts, conversions, chit-chat, and " +
		"anything about this computer's state. A consultation takes up to two minutes, during " +
		"which the user hears silence, so it must be worth the wait. Ask one complete, " +
		"self-contained question — the advisor sees none of this conversation."
}

// Schema implements Tool. The advisor enum is built from configuration, so
// the model can only name an advisor the user configured.
func (a *Advisor) Schema() json.RawMessage {
	options := make([]string, 0, len(a.Advisors))
	descriptions := make([]string, 0, len(a.Advisors))
	for _, spec := range a.Advisors {
		options = append(options, fmt.Sprintf("%q", spec.Name))
		descriptions = append(descriptions, spec.Name+": "+spec.Description)
	}
	desc, _ := json.Marshal("Which advisor to consult. " + strings.Join(descriptions, "; "))
	return json.RawMessage(fmt.Sprintf(`{
		"type": "object",
		"properties": {
			"advisor": {
				"type": "string",
				"enum": [%s],
				"description": %s
			},
			"question": {
				"type": "string",
				"description": "The complete, self-contained question to ask, including any context the advisor needs. It sees nothing else."
			}
		},
		"required": ["advisor", "question"]
	}`, strings.Join(options, ", "), desc))
}

// advisorArgs is what the model is allowed to say. Anything else it sends is
// ignored by construction: no field here can reach argv or the environment.
type advisorArgs struct {
	Advisor  string `json:"advisor"`
	Question string `json:"question"`
}

// Activity implements Progressive: a consultation is the one tool call that
// can outlast the user's patience, so the overlay gets a label for the whole
// wait and Jarvix gets a sentence to say once it has gone on too long.
func (a *Advisor) Activity(input json.RawMessage) (label, waiting string, ok bool) {
	var args advisorArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return "", "", false
	}
	spec, found := a.spec(args.Advisor)
	if !found {
		return "", "", false
	}
	return "Consulting " + spec.Name + "…",
		"I'm still waiting on " + spec.Name + ". This is taking a moment.", true
}

// Execute implements Tool. Everything an advisor can do wrong — missing,
// slow, noisy, failing — comes back as text for the model, because the
// session should end with one spoken sentence about it, not an error. Only
// malformed tool arguments are an err.
//
// The consultation itself is Consult; this method is the *model-facing*
// wording of its outcome, which is a different thing from the outcome. Model
// tiers (#159) reach the same bridge through Consult and word it for a person
// instead — one process-execution path, two audiences, and no chance of a
// second copy of the kill-the-process-group discipline below.
func (a *Advisor) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args advisorArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid advisor.ask arguments: %w", err)
	}
	answer, err := a.Consult(ctx, args.Advisor, args.Question)
	if err != nil {
		return "", err
	}
	name := answer.Advisor
	switch answer.Outcome {
	case AdvisorMissing:
		return fmt.Sprintf("The %s assistant is not installed on this computer, so nothing was "+
			"asked. Tell the user in one short sentence that you could not reach %s because it "+
			"is not installed, and do not retry.", name, name), nil
	case AdvisorInterrupted:
		return "The consultation was interrupted.", nil
	case AdvisorTimedOut:
		return fmt.Sprintf("The %s assistant did not answer within %s and was stopped. Tell the "+
			"user in one short sentence that %s took too long, and do not retry.",
			name, answer.Timeout, name), nil
	case AdvisorFailed:
		return fmt.Sprintf("The %s assistant failed to answer. Tell the user in one short "+
			"sentence that you could not get an answer from %s. Do not read out any technical "+
			"detail and do not retry.", name, name), nil
	case AdvisorEmpty:
		return fmt.Sprintf("The %s assistant returned nothing. Tell the user in one short "+
			"sentence that %s had no answer, and do not retry.", name, name), nil
	}
	text := answer.Text
	if answer.Truncated {
		text += "\n\n[answer truncated at " + fmt.Sprint(answer.MaxOutput) + " bytes]"
	}
	return fmt.Sprintf("%s answered:\n\n%s\n\nGive the user this answer. Stay faithful to it, "+
		"shorten it if it is long, and say it as speech: no markdown, no lists, and no file "+
		"paths, URLs, or code read out verbatim.", name, text), nil
}

// AdvisorOutcome classifies one consultation. It exists so the *result* of
// running an advisor can be described once and worded twice — for the model
// (Execute) and for a person (a deep tier answering a turn directly, #159).
type AdvisorOutcome int

// What a consultation did.
const (
	// AdvisorAnswered: the CLI ran and produced text.
	AdvisorAnswered AdvisorOutcome = iota
	// AdvisorMissing: the binary is not on this machine.
	AdvisorMissing
	// AdvisorInterrupted: the session was cancelled or superseded.
	AdvisorInterrupted
	// AdvisorTimedOut: the CLI outlasted its configured budget and was killed.
	AdvisorTimedOut
	// AdvisorFailed: it exited non-zero, or could not be run.
	AdvisorFailed
	// AdvisorEmpty: it exited cleanly and said nothing.
	AdvisorEmpty
)

// AdvisorAnswer is one consultation's result. It carries no stderr and no
// command line: the advisor's own diagnostics stay in the daemon log, because
// anything in here may be read aloud (ADR 0016).
type AdvisorAnswer struct {
	Outcome AdvisorOutcome
	// Advisor is the configured name, for whatever wording the caller uses.
	Advisor string
	// Text is the answer, exactly as the CLI wrote it. Empty unless
	// Outcome is AdvisorAnswered.
	Text string
	// Timeout is the budget that was applied, for the timed-out wording.
	Timeout time.Duration
	// Truncated says the answer hit MaxOutput and was cut.
	Truncated bool
	MaxOutput int
}

// Consult runs one advisor and returns what happened. It is the whole of the
// process discipline of ADR 0016 — resolved at call time, no shell, own
// process group killed as a group, scrubbed environment, capped output — and
// the only way anything in this codebase runs an advisor.
//
// An err is returned only for a request that could never be served: an empty
// question, or a name that is not a configured advisor. Everything the CLI
// itself can do wrong is an Outcome, because it is a thing to *say*, not a
// thing to fail on.
func (a *Advisor) Consult(ctx context.Context, name, rawQuestion string) (AdvisorAnswer, error) {
	question := strings.TrimSpace(rawQuestion)
	if question == "" {
		return AdvisorAnswer{}, fmt.Errorf("advisor.ask: empty question")
	}
	spec, found := a.spec(name)
	if !found {
		return AdvisorAnswer{}, fmt.Errorf("advisor.ask: unknown advisor %q; configured advisors: %s",
			name, strings.Join(a.names(), ", "))
	}

	logger := a.Log
	if logger == nil {
		logger = slog.Default()
	}
	maxOutput := a.MaxOutput
	if maxOutput <= 0 {
		maxOutput = DefaultAdvisorMaxOutput
	}

	// Resolved per call, not at registration: installing the CLI mid-session
	// works, and a CLI removed since startup fails as "not installed" rather
	// than as an exec error nobody can act on.
	binary, err := resolveAdvisorBinary(spec.Binary)
	if err != nil {
		logger.Warn("advisor unavailable", "component", "tools", "tool", a.Name(),
			"advisor", spec.Name, "binary", spec.Binary, "error", err.Error())
		return AdvisorAnswer{Outcome: AdvisorMissing, Advisor: spec.Name}, nil
	}

	argv, stdin := buildAdvisorInput(spec, advisorPreamble+question)
	run := a.runner
	if run == nil {
		run = runCommand
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultAdvisorTimeout
	}
	logger.Info("consulting advisor", "component", "tools", "tool", a.Name(),
		"advisor", spec.Name, "binary", binary, "timeout_sec", int(timeout.Seconds()))
	start := time.Now()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resolved := spec
	resolved.Binary = binary
	result := run(runCtx, resolved, argv, stdin, scrubbedEnv(os.Environ(), a.ScrubEnv), maxOutput)

	logger.Info("advisor finished", "component", "tools", "tool", a.Name(),
		"advisor", spec.Name, "duration_ms", time.Since(start).Milliseconds(),
		"exit_code", result.exitCode, "output_bytes", len(result.stdout),
		"timed_out", result.timedOut)

	out := AdvisorAnswer{Advisor: spec.Name, Timeout: timeout, MaxOutput: maxOutput}
	switch {
	case ctx.Err() != nil:
		// The session was cancelled or superseded: the child is already dead
		// and nobody is listening for this result.
		out.Outcome = AdvisorInterrupted
		return out, nil
	case result.timedOut:
		out.Outcome = AdvisorTimedOut
		return out, nil
	case result.err != nil || result.exitCode != 0:
		// The advisor's own diagnostics stay daemon-side: they are the
		// operator's material, and anything returned to a caller may be read
		// aloud.
		logger.Warn("advisor failed", "component", "tools", "tool", a.Name(),
			"advisor", spec.Name, "exit_code", result.exitCode,
			"error", errText(result.err), "stderr", firstLine(result.stderr))
		out.Outcome = AdvisorFailed
		return out, nil
	}

	answer := strings.TrimSpace(result.stdout)
	if answer == "" {
		out.Outcome = AdvisorEmpty
		return out, nil
	}
	out.Outcome, out.Text, out.Truncated = AdvisorAnswered, answer, result.truncated
	return out, nil
}

// spec resolves a model-named advisor.
func (a *Advisor) spec(name string) (AdvisorSpec, bool) {
	for _, spec := range a.Advisors {
		if spec.Name == name {
			return spec, true
		}
	}
	return AdvisorSpec{}, false
}

func (a *Advisor) names() []string {
	names := make([]string, 0, len(a.Advisors))
	for _, spec := range a.Advisors {
		names = append(names, spec.Name)
	}
	return names
}

// buildAdvisorInput places the question: as the single argv element written
// as the placeholder, or on stdin when there is none. There is no third
// option, and in neither case does the question meet a shell.
func buildAdvisorInput(spec AdvisorSpec, question string) (argv []string, stdin string) {
	argv = make([]string, 0, len(spec.Args))
	placed := false
	for _, arg := range spec.Args {
		if arg == AdvisorQuestionPlaceholder && !placed {
			argv = append(argv, question)
			placed = true
			continue
		}
		argv = append(argv, arg)
	}
	if placed {
		return argv, ""
	}
	return argv, question
}

// resolveAdvisorBinary finds the CLI: an absolute path must exist and be
// executable, a bare name is looked up on PATH. exec.LookPath only — no
// network, no invocation (the same rule `jarvix setup` and `jarvix doctor`
// follow).
func resolveAdvisorBinary(binary string) (string, error) {
	if binary == "" {
		return "", errors.New("no binary configured")
	}
	if filepath.IsAbs(binary) {
		info, err := os.Stat(binary)
		if err != nil {
			return "", err
		}
		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("%s is not executable", binary)
		}
		return binary, nil
	}
	return exec.LookPath(binary)
}

// advisorRun is the outcome of one child process.
type advisorRun struct {
	stdout    string
	stderr    string
	exitCode  int
	timedOut  bool
	truncated bool
	err       error
}

// runCommand executes the advisor. Three properties matter and are all
// deliberate:
//
//   - No shell. The binary and every argument are passed straight to execve,
//     so a question containing `;`, backticks, or `$(…)` is inert text.
//   - Its own process group, killed as a group. Assistant CLIs spawn helpers;
//     killing only the parent would leave a language model running against
//     the user's account after they said "stop".
//   - A scrubbed environment, passed in by the caller.
func runCommand(ctx context.Context, spec AdvisorSpec, argv []string, stdin string, env []string, maxOutput int) advisorRun {
	cmd := exec.CommandContext(ctx, spec.Binary, argv...) //nolint:gosec // binary and argv come from config; the question is a value, never syntax
	cmd.Env = env
	cmd.Dir, _ = os.UserHomeDir()
	cmd.Stdin = strings.NewReader(stdin) // always set: the child must never inherit the daemon's stdin
	out := &cappedBuffer{max: maxOutput}
	errBuf := &cappedBuffer{max: 4 * 1024} // diagnostics only; logged, never spoken
	cmd.Stdout = out
	cmd.Stderr = errBuf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid: the whole group, so helper processes die too.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// A grandchild holding the output pipe must not keep Wait blocked after
	// the group has been killed.
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	run := advisorRun{
		stdout:    out.String(),
		stderr:    errBuf.String(),
		truncated: out.truncated(),
		timedOut:  errors.Is(ctx.Err(), context.DeadlineExceeded),
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		run.exitCode = exitErr.ExitCode()
	default:
		run.err = err
	}
	return run
}

// secretEnvFragments name environment variables Jarvix must not hand to
// another program. Advisors carry their own authentication (that is the
// premise of delegating to them), so anything that looks like a credential
// is withheld — matching by name fragment rather than an allow list, because
// a missed variable leaks a secret while a wrongly-dropped one at worst
// costs the advisor a setting.
var secretEnvFragments = []string{
	"API_KEY", "APIKEY", "SECRET", "TOKEN", "PASSWORD", "PASSWD", "CREDENTIAL", "SESSION_KEY",
}

// ScrubbedEnv builds a child-process environment: environ minus anything
// named like a credential, minus the explicitly named extras. Exported for
// the script runner (internal/script, ADR 0030), which owes its children the
// same discipline advisors get, from this one copy of the secret-name rules —
// a second list would be the one that misses the next variable.
//
// One package cannot share it: internal/knowledge (ADR 0031) carries its own
// copy in fetch.go, because this package imports knowledge for the
// knowledge.get tool and the import would cycle. A change to the secret-name
// rules here must be mirrored there — its test suite covers the same cases.
func ScrubbedEnv(environ []string, extra []string) []string {
	return scrubbedEnv(environ, extra)
}

// scrubbedEnv builds the child environment: the daemon's, minus anything
// named like a credential, minus the explicitly named extras.
func scrubbedEnv(environ []string, extra []string) []string {
	drop := make(map[string]bool, len(extra))
	for _, name := range extra {
		if name != "" {
			drop[strings.ToUpper(name)] = true
		}
	}
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		if isSecretEnvName(strings.ToUpper(name)) || drop[strings.ToUpper(name)] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func isSecretEnvName(upper string) bool {
	if strings.HasSuffix(upper, "_KEY") {
		return true
	}
	for _, fragment := range secretEnvFragments {
		if strings.Contains(upper, fragment) {
			return true
		}
	}
	return false
}

// cappedBuffer collects at most max bytes and discards the rest, so an
// advisor that prints a gigabyte costs a gigabyte of nothing. Each stream has
// its own buffer, written by one goroutine.
type cappedBuffer struct {
	max     int
	buf     bytes.Buffer
	dropped int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.max - c.buf.Len()
	switch {
	case room <= 0:
		c.dropped += len(p)
	case len(p) <= room:
		c.buf.Write(p)
	default:
		c.buf.Write(p[:room])
		c.dropped += len(p) - room
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string  { return c.buf.String() }
func (c *cappedBuffer) truncated() bool { return c.dropped > 0 }

// firstLine bounds what a failing advisor can write into the log.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const maxLogged = 200
	if len(s) > maxLogged {
		s = s[:maxLogged] + "…"
	}
	return s
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func firstOr(values []string, fallback string) string {
	if len(values) > 0 {
		return values[0]
	}
	return fallback
}
