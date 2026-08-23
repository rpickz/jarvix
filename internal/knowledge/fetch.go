package knowledge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// This file executes one feed fetch, with the advisor path's subprocess
// discipline applied verbatim (internal/tools/advisor.go, ADR 0016) — kept as
// its own copy rather than shared because tools must be able to import this
// package for the knowledge.get tool, and the discipline is short enough
// that a dependency cycle would cost more than the duplication:
//
//   - No shell. The program and every argument go straight to execve; the
//     argv comes from configuration the model cannot write, and nothing the
//     model says can reach it at all.
//   - Its own process group, killed as a group on timeout or shutdown, so a
//     fetch script's helpers (curl, jq) die with it.
//   - A scrubbed environment: anything named like a credential is withheld —
//     a feed command carries its own secrets in its own file if it needs
//     them, and the daemon's keys are never handed to it.
//   - Capped output. A value is a number or a sentence; a command that
//     prints a gigabyte costs a gigabyte of nothing.

// maxFeedOutput caps a fetched value. Feeds carry a price, a headline, a
// short report — 16 KB is far beyond any of those, and small enough that a
// runaway command cannot bloat the values file or a model turn.
const maxFeedOutput = 16 * 1024

// FetchResult is the outcome of one feed command.
type FetchResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	TimedOut  bool
	Truncated bool
	Err       error
}

// runFeed executes one fetch. The caller has already bounded ctx with the
// feed's timeout.
func runFeed(ctx context.Context, feed Feed, env []string) FetchResult {
	if len(feed.Argv) == 0 {
		return FetchResult{Err: errors.New("feed has no command")}
	}
	binary, err := resolveFeedBinary(feed.Argv[0])
	if err != nil {
		return FetchResult{Err: err}
	}
	cmd := exec.CommandContext(ctx, binary, feed.Argv[1:]...) //nolint:gosec // argv comes from config; the model only ever names a feed
	cmd.Env = env
	cmd.Dir, _ = os.UserHomeDir()
	cmd.Stdin = strings.NewReader("") // the child must never inherit the daemon's stdin
	out := &cappedBuffer{max: maxFeedOutput}
	errBuf := &cappedBuffer{max: 4 * 1024} // diagnostics only; journal material, never a value
	cmd.Stdout = out
	cmd.Stderr = errBuf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid: the whole group, so a script's curl dies too.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// A grandchild holding the output pipe must not keep Wait blocked after
	// the group has been killed.
	cmd.WaitDelay = 2 * time.Second

	runErr := cmd.Run()
	res := FetchResult{
		Stdout:    out.String(),
		Stderr:    errBuf.String(),
		Truncated: out.truncated(),
		TimedOut:  errors.Is(ctx.Err(), context.DeadlineExceeded),
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		res.Err = runErr
	}
	return res
}

// resolveFeedBinary finds the feed's program: an absolute path must exist and
// be executable, a bare name is looked up on PATH at fetch time — so
// installing the script works without a restart, and one removed since fails
// as "not found" rather than as an exec error nobody can act on.
func resolveFeedBinary(binary string) (string, error) {
	if strings.TrimSpace(binary) == "" {
		return "", errors.New("feed has no command")
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

// secretEnvFragments name environment variables Jarvix must not hand to a
// feed command. Matching by name fragment rather than an allow list, because
// a missed variable leaks a secret while a wrongly-dropped one at worst costs
// the script a setting it can read from its own file instead.
//
// This is a deliberate copy of tools.ScrubbedEnv's rules (advisor.go): tools
// imports this package for the knowledge.get tool, so sharing the one
// exported copy would cycle. A change to the secret-name rules there must be
// mirrored here, and vice versa — both packages test the same cases.
var secretEnvFragments = []string{
	"API_KEY", "APIKEY", "SECRET", "TOKEN", "PASSWORD", "PASSWD", "CREDENTIAL", "SESSION_KEY",
}

// scrubbedFeedEnv builds the child environment: the daemon's, minus anything
// named like a credential, minus the explicitly named extras (the api_key_env
// of every configured AI endpoint).
func scrubbedFeedEnv(extra []string) []string {
	drop := make(map[string]bool, len(extra))
	for _, name := range extra {
		if name != "" {
			drop[strings.ToUpper(name)] = true
		}
	}
	environ := os.Environ()
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		upper := strings.ToUpper(name)
		if isSecretEnvName(upper) || drop[upper] {
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

// cappedBuffer collects at most max bytes and discards the rest. Each stream
// has its own buffer, written by one goroutine.
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

// firstStderrLine bounds what a failing feed can write into the journal.
func firstStderrLine(s string) string {
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
