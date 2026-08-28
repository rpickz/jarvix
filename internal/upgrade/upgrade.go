// Package upgrade implements `jarvix upgrade` (issue #139, ADR 0044): fetch
// and fast-forward the user's checkout, build through the Makefile, install
// into a versioned release slot, restart the daemon, and hold the result to
// the doctor's health gate — rolling back onto the previous slot the moment
// the gate says the new build cannot serve.
//
// Two rules shape everything here. The user's checkout is theirs: it is
// inspected read-only, refused loudly when dirty or diverged, and the one
// mutation ever made is the fast-forward the invocation itself asked for.
// And the previous working version is sacred: it is always kept on disk in
// its slot, and no failure path ever deletes the only copy that is known to
// work.
package upgrade

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RunFunc is the exec seam: every external command — git, make, systemctl,
// and the freshly installed CLI itself — goes through it, so tests can play
// the whole world without building or restarting anything real.
type RunFunc func(ctx context.Context, dir, name string, args ...string) (stdout, stderr string, err error)

// ExecRun is the production seam: the command really runs, in dir, with its
// output captured for the report.
func ExecRun(ctx context.Context, dir, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// Timeouts. The socket wait bounds "daemon won't start" — systemd respawns
// fast, so a daemon that has not answered in 30s is not coming. The gate
// budget bounds one `jarvix doctor --gate` run: its two engine probes are
// each allowed 30s cold starts (internal/doctor.probeTimeout), so the budget
// covers both plus slack, and only ever bites a hung gate.
const (
	DefaultSocketWait = 30 * time.Second
	DefaultGateBudget = 2 * time.Minute

	socketPollInterval = 500 * time.Millisecond
)

// Upgrader is the state machine. Everything it touches goes through a seam:
// commands through Run, time through Sleep, liveness through Alive — the
// filesystem is touched directly but only under BinDir, SlotsDir, and
// LockPath, which tests point at temp directories.
type Upgrader struct {
	Repo      string // the user's checkout (never mutated beyond ff-only)
	BinDir    string // where the jarvix/jarvixd symlinks live (~/.local/bin)
	SlotsDir  string // versioned release slots (~/.local/share/jarvix/releases)
	LockPath  string // concurrency lock (~/.local/state/jarvix/upgrade.lock)
	Installed string // the running CLI's build.Version

	Run RunFunc
	Out io.Writer

	// Sleep and Alive default to the real thing; tests inject.
	Sleep func(time.Duration)
	Alive func(pid int) bool

	SocketWait time.Duration // 0 = DefaultSocketWait
	GateBudget time.Duration // 0 = DefaultGateBudget
}

func (u *Upgrader) sleep(d time.Duration) {
	if u.Sleep != nil {
		u.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (u *Upgrader) socketWait() time.Duration {
	if u.SocketWait > 0 {
		return u.SocketWait
	}
	return DefaultSocketWait
}

func (u *Upgrader) gateBudget() time.Duration {
	if u.GateBudget > 0 {
		return u.GateBudget
	}
	return DefaultGateBudget
}

// Check answers "is there anything to upgrade to?" and changes nothing: one
// fetch (remote refs only — the working tree, branches, and installed
// binaries are untouched), then a report.
func (u *Upgrader) Check(ctx context.Context) error {
	st, err := u.inspect(ctx)
	if err != nil {
		return err
	}
	fprintf(u.Out, "installed: %s\n", u.Installed)
	fprintf(u.Out, "available: %s (origin/main)\n", st.available)
	switch {
	case st.behind > 0:
		fprintf(u.Out, "%d commit(s) behind — update with: jarvix upgrade\n", st.behind)
	case st.available != u.Installed:
		fprintln(u.Out, "nothing to pull, but the installed version differs — a previous upgrade"+
			" may have been rolled back; rebuild with: jarvix upgrade")
	default:
		fprintln(u.Out, "up to date")
	}
	if msg := refusal(u.Repo, st); msg != "" {
		fprintf(u.Out, "note: %s\n", msg)
	}
	return nil
}

// Upgrade runs the whole state machine:
//
//	lock → inspect → refuse or ff-only pull → make build → stage slot
//	     → flip symlinks → restart → health gate
//	     → green: prune, report │ fail: roll back, restart, gate again
//
// Every stop before the flip leaves the running install untouched; every
// stop after it either restores the previous slot or says loudly why not.
func (u *Upgrader) Upgrade(ctx context.Context) error {
	unlock, err := u.acquireLock()
	if err != nil {
		return err
	}
	defer unlock()

	st, err := u.inspect(ctx)
	if err != nil {
		return err
	}
	if msg := refusal(u.Repo, st); msg != "" {
		return errors.New("refusing to upgrade: " + msg)
	}
	fprintf(u.Out, "installed %s → available %s\n", u.Installed, st.available)
	if st.behind == 0 && st.available == u.Installed {
		fprintln(u.Out, "already up to date; nothing to do")
		return nil
	}

	// The one mutation of the user's checkout, and the invocation asked for
	// exactly it. Nothing to pull happens after a rollback: the checkout is
	// already at origin/main and only the binaries are old.
	if st.behind > 0 {
		if _, err := u.git(ctx, "merge", "--ff-only", "origin/main"); err != nil {
			return fmt.Errorf("fast-forward to origin/main failed — your checkout was not modified further: %w", err)
		}
	}
	version, err := u.git(ctx, "describe", "--tags", "--always", "--dirty")
	if err != nil {
		return err
	}

	fprintf(u.Out, "building %s (make build)…\n", version)
	if _, errOut, err := u.Run(ctx, u.Repo, "make", "build"); err != nil {
		return fmt.Errorf("build failed — nothing was installed and the running daemon is untouched:\n%s",
			strings.TrimSpace(errOut))
	}

	// Whether the shell's half changed is a question about the two commits,
	// answered read-only before anything is installed.
	pluginDiff, err := u.git(ctx, "diff", "--name-only", st.head, "HEAD", "--", "plugin/")
	if err != nil {
		return err
	}

	prev, err := u.previousSlot()
	if err != nil {
		return err
	}
	if prev.name == version {
		// Rebuilding the very version we would roll back to: staging would
		// overwrite it, so there is no distinct previous. Say so now rather
		// than during a failed rollback.
		fprintf(u.Out, "rebuilding the installed version %s in place; no distinct previous release to roll back to\n", version)
		prev = slot{}
	}
	slotDir, err := u.stage(version)
	if err != nil {
		return err
	}
	fprintf(u.Out, "installing %s → %s\n", version, slotDir)
	if err := u.flip(slotDir); err != nil {
		return err
	}

	fprintln(u.Out, "restarting jarvixd…")
	var failed []string
	if err := u.restart(ctx); err != nil {
		failed = []string{err.Error()}
	} else {
		failed = u.runGate(ctx, version)
	}
	if failed == nil {
		u.prune(version, prev.name)
		fprintf(u.Out, "health gate green — %s is live\n", version)
		if prev.name != "" {
			fprintf(u.Out, "previous release %s kept at %s\n", prev.name, prev.dir)
		}
		u.reportShell(pluginDiff)
		return nil
	}
	return u.rollback(ctx, prev, version, failed)
}

// rollback restores the previous slot after a failed gate, restarts onto it,
// and re-runs the gate to confirm recovery. When there is no previous slot,
// it stops loudly and deletes nothing: a possibly-broken install is
// recoverable by hand, a deleted one is not.
func (u *Upgrader) rollback(ctx context.Context, prev slot, version string, failed []string) error {
	fprintf(u.Out, "health gate FAILED for %s:\n", version)
	for _, f := range failed {
		fprintln(u.Out, "  "+f)
	}
	if prev.name == "" {
		return fmt.Errorf("the health gate failed and there is no previous release to roll back to — "+
			"refusing to delete the only installed copy. The %s binaries stay in place; "+
			"inspect the daemon with: systemctl --user status jarvixd", version)
	}
	fprintf(u.Out, "rolling back to %s…\n", prev.name)
	if err := u.flip(prev.dir); err != nil {
		return fmt.Errorf("rollback failed while restoring %s: %w", prev.name, err)
	}
	if err := u.restart(ctx); err != nil {
		return fmt.Errorf("rolled the binaries back to %s but the daemon would not restart: %w", prev.name, err)
	}
	if refailed := u.runGate(ctx, prev.name); refailed != nil {
		fprintf(u.Out, "the gate STILL fails on %s:\n", prev.name)
		for _, f := range refailed {
			fprintln(u.Out, "  "+f)
		}
		return fmt.Errorf("rolled back to %s but the health gate still fails — the problem is not the new build; "+
			"start with: jarvix doctor", prev.name)
	}
	fprintf(u.Out, "recovery confirmed: %s is serving again\n", prev.name)
	return fmt.Errorf("upgrade to %s failed its health gate and was rolled back to %s; the gate report above names the failing check", version, prev.name)
}

// restart bounces the daemon under its supervisor. The unit name is fixed:
// systemd/jarvixd.service, user scope.
func (u *Upgrader) restart(ctx context.Context) error {
	if _, errOut, err := u.Run(ctx, "", "systemctl", "--user", "restart", "jarvixd"); err != nil {
		return fmt.Errorf("systemctl --user restart jarvixd: %v: %s", err, strings.TrimSpace(errOut))
	}
	return nil
}

// runGate holds the current install to the health gate and returns nil on
// green, else the failure lines for the report — each verbatim from the
// check that failed.
//
// Two phases. First wait for the daemon socket by polling the installed
// CLI's `jarvix status` until it answers or the startup budget lapses; its
// answer must carry the expected version, or the restart did not take.
// Then run `jarvix doctor --gate` once: the freshly installed binary runs
// the doctor's own critical subset (internal/doctor.GateChecks) in its own
// build generation, so the probes and the protocol comparison are exactly
// the shipped ones — zero duplicated logic, and a deliberate protocol bump
// is judged by the new binary, which is the pair that must match.
func (u *Upgrader) runGate(ctx context.Context, expectVersion string) []string {
	cli := filepath.Join(u.BinDir, "jarvix")
	deadline := time.Now().Add(u.socketWait())
	for {
		out, errOut, err := u.Run(ctx, "", cli, "status")
		if err == nil {
			if v := statusVersion(out); v != "" && v != expectVersion {
				return []string{fmt.Sprintf("daemon answered as version %s, expected %s — the restart did not take", v, expectVersion)}
			}
			break
		}
		if time.Now().After(deadline) {
			return []string{fmt.Sprintf("daemon socket dead within the %s startup budget: %s",
				u.socketWait(), strings.TrimSpace(errOut))}
		}
		u.sleep(socketPollInterval)
	}

	gctx, cancel := context.WithTimeout(ctx, u.gateBudget())
	defer cancel()
	out, errOut, err := u.Run(gctx, "", cli, "doctor", "--gate")
	if err == nil {
		return nil
	}
	var fails []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[FAIL]") {
			fails = append(fails, strings.TrimSpace(line))
		}
	}
	if len(fails) == 0 {
		fails = []string{"the health gate did not run: " + strings.TrimSpace(err.Error()+" "+errOut)}
	}
	return fails
}

// statusVersion extracts the version from `jarvix status` output
// ("version:  v1.2.3 (protocol 1)"). Empty when the line is absent — the
// gate then skips the comparison rather than failing on a format change.
func statusVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(line, "version:")
		if !ok {
			continue
		}
		if fields := strings.Fields(rest); len(fields) > 0 {
			return fields[0]
		}
	}
	return ""
}

// reportShell tells the user whether the shell's half of Jarvix changed.
// The plugin directory is symlinked from the checkout, so the QML on disk is
// already new — but the running shell keeps executing what it loaded, and
// only a full restart replaces it. The command is offered, never run: a
// shell restart flickers every bar and panel, and when that happens is the
// user's call. omarchy-restart-shell, never the refresh variant — refresh
// reloads configuration without tearing down the QML engine, so a changed
// plugin would keep running its old code.
func (u *Upgrader) reportShell(pluginDiff string) {
	if strings.TrimSpace(pluginDiff) == "" {
		fprintln(u.Out, "daemon-only change: the shell plugin did not change, no shell restart needed")
		return
	}
	fprintln(u.Out, "the shell plugin changed in this update — a shell restart is pending to load it:")
	fprintln(u.Out, "  omarchy-restart-shell")
}
