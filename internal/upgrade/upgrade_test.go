package upgrade

// Hermetic tests for the whole upgrade state machine. Every external command
// — git, make, systemctl, the installed CLI — goes through the exec seam and
// is played back from a script; the filesystem is temp directories. No real
// build, no real daemon, no real restart, ever.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type resp struct {
	out    string
	errOut string
	err    error
	effect func()
}

// fakeRunner plays scripted responses per command line. A key with several
// queued responses pops them in order; the last one repeats, so polling
// loops can call the same command as often as they need.
type fakeRunner struct {
	t       *testing.T
	scripts map[string][]resp
	calls   []string
}

func newFakeRunner(t *testing.T) *fakeRunner {
	return &fakeRunner{t: t, scripts: map[string][]resp{}}
}

func (f *fakeRunner) script(cmd string, r ...resp) {
	f.scripts[cmd] = append(f.scripts[cmd], r...)
}

func (f *fakeRunner) run(_ context.Context, dir, name string, args ...string) (string, string, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, key)
	queue := f.scripts[key]
	if len(queue) == 0 {
		f.t.Fatalf("unexpected command: %s (in %q)", key, dir)
	}
	r := queue[0]
	if len(queue) > 1 {
		f.scripts[key] = queue[1:]
	}
	if r.effect != nil {
		r.effect()
	}
	return r.out, r.errOut, r.err
}

func (f *fakeRunner) callCount(sub string) int {
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

type harness struct {
	t                *testing.T
	repo, bin, slots string
	runner           *fakeRunner
	out              *bytes.Buffer
	u                *Upgrader
}

// newHarness builds a world where v1 is installed as plain files (the shape
// `make install` leaves) and the checkout is ready to go. Tests reshape it.
func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{
		t:      t,
		repo:   filepath.Join(root, "checkout"),
		bin:    filepath.Join(root, "bin"),
		slots:  filepath.Join(root, "releases"),
		runner: newFakeRunner(t),
		out:    &bytes.Buffer{},
	}
	h.u = &Upgrader{
		Repo:       h.repo,
		BinDir:     h.bin,
		SlotsDir:   h.slots,
		LockPath:   filepath.Join(root, "state", "upgrade.lock"),
		Installed:  "v1",
		Run:        h.runner.run,
		Out:        h.out,
		Sleep:      func(time.Duration) {},
		SocketWait: 50 * time.Millisecond,
	}
	h.writeFile(filepath.Join(h.bin, "jarvix"), "cli v1")
	h.writeFile(filepath.Join(h.bin, "jarvixd"), "daemon v1")
	return h
}

func (h *harness) writeFile(path, content string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

// managedInstall reshapes the world into slot-managed form: the pair lives
// in slots/<version> and the bin entries are symlinks into it.
func (h *harness) managedInstall(version string) {
	h.t.Helper()
	for _, b := range []string{"jarvix", "jarvixd"} {
		h.writeFile(filepath.Join(h.slots, version, b), b+" "+version)
		link := filepath.Join(h.bin, b)
		_ = os.Remove(link)
		if err := os.Symlink(filepath.Join(h.slots, version, b), link); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) cli() string { return filepath.Join(h.bin, "jarvix") }

// scriptInspect plays the read-only inspection: a clean main checkout at
// sha aaa111, `behind` commits behind an origin/main describing as v2.
func (h *harness) scriptInspect(branch, porcelain string, ahead, behind int) {
	h.runner.script("git fetch --quiet origin", resp{})
	h.runner.script("git rev-parse --abbrev-ref HEAD", resp{out: branch + "\n"})
	h.runner.script("git status --porcelain", resp{out: porcelain})
	h.runner.script("git rev-list --count origin/main..HEAD", resp{out: fmt.Sprintf("%d\n", ahead)})
	h.runner.script("git rev-list --count HEAD..origin/main", resp{out: fmt.Sprintf("%d\n", behind)})
	h.runner.script("git rev-parse HEAD", resp{out: "aaa111\n"})
	h.runner.script("git describe --tags --always origin/main", resp{out: "v2\n"})
}

// scriptHealthyUpgrade plays a clean v1→v2 upgrade end to end: pull, build,
// restart, gate green.
func (h *harness) scriptHealthyUpgrade() {
	h.scriptInspect("main", "", 0, 3)
	h.runner.script("git merge --ff-only origin/main", resp{})
	h.runner.script("git describe --tags --always --dirty", resp{out: "v2\n"})
	h.runner.script("git diff --name-only aaa111 HEAD -- plugin/", resp{})
	h.runner.script("make build", resp{effect: func() {
		h.writeFile(filepath.Join(h.repo, "bin", "jarvix"), "cli v2")
		h.writeFile(filepath.Join(h.repo, "bin", "jarvixd"), "daemon v2")
	}})
	h.runner.script("systemctl --user restart jarvixd", resp{})
	h.runner.script(h.cli()+" status", resp{out: "state:    idle\nversion:  v2 (protocol 1)\n"})
	h.runner.script(h.cli()+" doctor --gate", resp{out: "[OK] everything\n"})
}

func (h *harness) linkTarget(name string) string {
	h.t.Helper()
	target, err := os.Readlink(filepath.Join(h.bin, name))
	if err != nil {
		h.t.Fatalf("%s is not a symlink: %v", name, err)
	}
	return target
}

func (h *harness) fileContent(path string) string {
	h.t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(raw)
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Errorf("missing %q in:\n%s", sub, s)
	}
}

// --- The happy path -------------------------------------------------------

func TestUpgradeInstallsRestartsGatesAndKeepsPrevious(t *testing.T) {
	h := newHarness(t)
	h.scriptHealthyUpgrade()

	if err := h.u.Upgrade(context.Background()); err != nil {
		t.Fatalf("upgrade failed: %v\noutput:\n%s", err, h.out)
	}

	out := h.out.String()
	mustContain(t, out, "installed v1 → available v2")
	mustContain(t, out, "health gate green — v2 is live")
	mustContain(t, out, "daemon-only change")

	// The pair is now symlinks into the v2 slot.
	for _, b := range []string{"jarvix", "jarvixd"} {
		want := filepath.Join(h.slots, "v2", b)
		if got := h.linkTarget(b); got != want {
			t.Errorf("%s links to %s, want %s", b, got, want)
		}
	}
	if got := h.fileContent(filepath.Join(h.slots, "v2", "jarvix")); got != "cli v2" {
		t.Errorf("v2 slot holds %q", got)
	}
	// The previous binaries remain on disk in the rollback slot: the plain
	// v1 files were adopted before the flip.
	if got := h.fileContent(filepath.Join(h.slots, "v1", "jarvix")); got != "cli v1" {
		t.Errorf("v1 rollback slot holds %q", got)
	}
	mustContain(t, out, "previous release v1 kept")

	// The daemon was restarted exactly once and the gate ran on the
	// installed CLI, not on anything in the checkout.
	if n := h.runner.callCount("systemctl --user restart jarvixd"); n != 1 {
		t.Errorf("daemon restarted %d times, want 1", n)
	}
	if n := h.runner.callCount("doctor --gate"); n != 1 {
		t.Errorf("gate ran %d times, want 1", n)
	}
}

// --- Build failure: nothing installed, daemon untouched -------------------

func TestBuildFailureLeavesEverythingUntouched(t *testing.T) {
	h := newHarness(t)
	h.scriptInspect("main", "", 0, 3)
	h.runner.script("git merge --ff-only origin/main", resp{})
	h.runner.script("git describe --tags --always --dirty", resp{out: "v2\n"})
	h.runner.script("make build", resp{
		errOut: "internal/session/engine.go:42:7: undefined: nope",
		err:    fmt.Errorf("exit status 2"),
	})

	err := h.u.Upgrade(context.Background())
	if err == nil {
		t.Fatal("a failed build must fail the upgrade")
	}
	mustContain(t, err.Error(), "nothing was installed")
	mustContain(t, err.Error(), "undefined: nope") // the compiler error, verbatim

	// The install is exactly as it was: plain v1 files, no slots, no restart.
	if fi, statErr := os.Lstat(h.cli()); statErr != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("bin/jarvix changed: %v %v", fi, statErr)
	}
	if got := h.fileContent(h.cli()); got != "cli v1" {
		t.Errorf("bin/jarvix content = %q", got)
	}
	if _, statErr := os.Stat(filepath.Join(h.slots, "v2")); !os.IsNotExist(statErr) {
		t.Errorf("a slot was staged for a build that failed")
	}
	if n := h.runner.callCount("systemctl"); n != 0 {
		t.Errorf("the daemon was touched %d times", n)
	}
}

// --- Gate failure: automatic rollback, recovery confirmed -----------------

func TestGateFailureRollsBackAndConfirmsRecovery(t *testing.T) {
	h := newHarness(t)
	h.scriptInspect("main", "", 0, 3)
	h.runner.script("git merge --ff-only origin/main", resp{})
	h.runner.script("git describe --tags --always --dirty", resp{out: "v2\n"})
	h.runner.script("git diff --name-only aaa111 HEAD -- plugin/", resp{})
	h.runner.script("make build", resp{effect: func() {
		h.writeFile(filepath.Join(h.repo, "bin", "jarvix"), "cli v2")
		h.writeFile(filepath.Join(h.repo, "bin", "jarvixd"), "daemon v2")
	}})
	h.runner.script("systemctl --user restart jarvixd", resp{})
	h.runner.script(h.cli()+" status",
		resp{out: "version:  v2 (protocol 1)\n"},
		resp{out: "version:  v1 (protocol 1)\n"})
	h.runner.script(h.cli()+" doctor --gate",
		resp{out: "[FAIL] whisper.cpp transcribes — whisper-cli aborted loading its compute backend\n", err: fmt.Errorf("exit status 1")},
		resp{out: "[OK] everything\n"})

	err := h.u.Upgrade(context.Background())
	if err == nil {
		t.Fatal("a failed gate must fail the upgrade even after a clean rollback")
	}
	mustContain(t, err.Error(), "rolled back to v1")

	out := h.out.String()
	// The failing check, verbatim.
	mustContain(t, out, "[FAIL] whisper.cpp transcribes — whisper-cli aborted loading its compute backend")
	mustContain(t, out, "rolling back to v1")
	mustContain(t, out, "recovery confirmed: v1 is serving again")

	// The binaries are the previous ones again, restarted onto them.
	if got := h.linkTarget("jarvix"); got != filepath.Join(h.slots, "v1", "jarvix") {
		t.Errorf("jarvix links to %s after rollback", got)
	}
	if got := h.fileContent(filepath.Join(h.slots, "v1", "jarvix")); got != "cli v1" {
		t.Errorf("rollback slot holds %q", got)
	}
	if n := h.runner.callCount("systemctl --user restart jarvixd"); n != 2 {
		t.Errorf("daemon restarted %d times, want 2 (onto v2, back onto v1)", n)
	}
	if n := h.runner.callCount("doctor --gate"); n != 2 {
		t.Errorf("gate ran %d times, want 2 (failure, then recovery confirmation)", n)
	}
	// A failed upgrade prunes nothing: both slots remain for inspection.
	for _, slot := range []string{"v1", "v2"} {
		if _, statErr := os.Stat(filepath.Join(h.slots, slot)); statErr != nil {
			t.Errorf("slot %s missing after rollback: %v", slot, statErr)
		}
	}
}

// --- Rollback-of-rollback: no previous → loud stop, delete nothing --------

func TestGateFailureWithoutPreviousRefusesLoudly(t *testing.T) {
	h := newHarness(t)
	// The install links into a slot that no longer exists: there is no
	// working previous copy anywhere.
	for _, b := range []string{"jarvix", "jarvixd"} {
		link := filepath.Join(h.bin, b)
		_ = os.Remove(link)
		if err := os.Symlink(filepath.Join(h.slots, "ghost", b), link); err != nil {
			t.Fatal(err)
		}
	}
	h.scriptInspect("main", "", 0, 3)
	h.runner.script("git merge --ff-only origin/main", resp{})
	h.runner.script("git describe --tags --always --dirty", resp{out: "v2\n"})
	h.runner.script("git diff --name-only aaa111 HEAD -- plugin/", resp{})
	h.runner.script("make build", resp{effect: func() {
		h.writeFile(filepath.Join(h.repo, "bin", "jarvix"), "cli v2")
		h.writeFile(filepath.Join(h.repo, "bin", "jarvixd"), "daemon v2")
	}})
	h.runner.script("systemctl --user restart jarvixd", resp{})
	h.runner.script(h.cli()+" status", resp{out: "version:  v2 (protocol 1)\n"})
	h.runner.script(h.cli()+" doctor --gate",
		resp{out: "[FAIL] piper synthesizes — no audio\n", err: fmt.Errorf("exit status 1")})

	err := h.u.Upgrade(context.Background())
	if err == nil {
		t.Fatal("a failed gate with no rollback target must fail loudly")
	}
	mustContain(t, err.Error(), "no previous release to roll back to")
	mustContain(t, err.Error(), "refusing to delete the only installed copy")
	mustContain(t, h.out.String(), "[FAIL] piper synthesizes — no audio")

	// The new (possibly broken, but only existing) install is untouched.
	if got := h.linkTarget("jarvix"); got != filepath.Join(h.slots, "v2", "jarvix") {
		t.Errorf("jarvix links to %s, want the v2 slot left in place", got)
	}
	if got := h.fileContent(filepath.Join(h.slots, "v2", "jarvix")); got != "cli v2" {
		t.Errorf("v2 slot holds %q", got)
	}
	if n := h.runner.callCount("systemctl --user restart jarvixd"); n != 1 {
		t.Errorf("daemon restarted %d times, want 1 (no rollback restart)", n)
	}
}

// --- Refusals: the checkout is the user's ---------------------------------

func TestDirtyCheckoutRefusedWithExactState(t *testing.T) {
	h := newHarness(t)
	h.scriptInspect("main", " M internal/session/engine.go\n?? notes.txt\n", 0, 3)

	err := h.u.Upgrade(context.Background())
	if err == nil {
		t.Fatal("a dirty checkout must refuse")
	}
	mustContain(t, err.Error(), "uncommitted changes")
	mustContain(t, err.Error(), "M internal/session/engine.go")
	mustContain(t, err.Error(), "?? notes.txt")
	mustContain(t, err.Error(), "never stashed or reset")

	// No side effects of any kind.
	if n := h.runner.callCount("merge"); n != 0 {
		t.Errorf("merge ran on a dirty checkout")
	}
	if n := h.runner.callCount("make"); n != 0 {
		t.Errorf("build ran on a dirty checkout")
	}
	if n := h.runner.callCount("systemctl"); n != 0 {
		t.Errorf("the daemon was touched on a dirty checkout")
	}
	if got := h.fileContent(h.cli()); got != "cli v1" {
		t.Errorf("the install changed: %q", got)
	}
}

func TestDivergedBranchRefused(t *testing.T) {
	h := newHarness(t)
	h.scriptInspect("main", "", 2, 3)

	err := h.u.Upgrade(context.Background())
	if err == nil {
		t.Fatal("a diverged branch must refuse")
	}
	mustContain(t, err.Error(), "2 commit(s) ahead of origin/main")
	if n := h.runner.callCount("merge"); n != 0 {
		t.Errorf("merge ran on a diverged branch")
	}
}

func TestOffMainBranchRefused(t *testing.T) {
	h := newHarness(t)
	h.scriptInspect("feature/foo", "", 0, 3)

	err := h.u.Upgrade(context.Background())
	if err == nil {
		t.Fatal("an off-main checkout must refuse")
	}
	mustContain(t, err.Error(), `on branch "feature/foo", not main`)
}

// --- --check: report, change nothing --------------------------------------

func TestCheckReportsAvailableVersusInstalledWithoutChanges(t *testing.T) {
	h := newHarness(t)
	h.scriptInspect("main", "", 0, 3)

	if err := h.u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	mustContain(t, out, "installed: v1")
	mustContain(t, out, "available: v2")
	mustContain(t, out, "3 commit(s) behind")

	for _, forbidden := range []string{"merge", "make", "systemctl", "doctor"} {
		if n := h.runner.callCount(forbidden); n != 0 {
			t.Errorf("--check ran %s", forbidden)
		}
	}
	if got := h.fileContent(h.cli()); got != "cli v1" {
		t.Errorf("--check changed the install: %q", got)
	}
	if _, err := os.Stat(h.slots); !os.IsNotExist(err) {
		t.Errorf("--check created the slots directory")
	}
}

func TestCheckMentionsARefusalThatWouldBite(t *testing.T) {
	h := newHarness(t)
	h.scriptInspect("main", " M go.mod\n", 0, 3)

	if err := h.u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustContain(t, h.out.String(), "note: ")
	mustContain(t, h.out.String(), "uncommitted changes")
}

func TestCheckUpToDate(t *testing.T) {
	h := newHarness(t)
	h.scriptInspect("main", "", 0, 0)
	h.runner.scripts["git describe --tags --always origin/main"] = []resp{{out: "v1\n"}}

	if err := h.u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustContain(t, h.out.String(), "up to date")
}

// --- Nothing to do / post-rollback rebuild --------------------------------

func TestAlreadyUpToDateDoesNothing(t *testing.T) {
	h := newHarness(t)
	h.scriptInspect("main", "", 0, 0)
	h.runner.scripts["git describe --tags --always origin/main"] = []resp{{out: "v1\n"}}

	if err := h.u.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustContain(t, h.out.String(), "already up to date")
	if n := h.runner.callCount("make"); n != 0 {
		t.Errorf("build ran with nothing to do")
	}
}

// After a rollback the checkout is already at origin/main while the binaries
// are old: nothing to pull, everything to rebuild. The version comparison,
// not the commit count, is what decides.
func TestRebuildAfterRollbackSkipsThePullAndStillUpgrades(t *testing.T) {
	h := newHarness(t)
	h.scriptInspect("main", "", 0, 0)
	h.runner.script("git describe --tags --always --dirty", resp{out: "v2\n"})
	h.runner.script("git diff --name-only aaa111 HEAD -- plugin/", resp{})
	h.runner.script("make build", resp{effect: func() {
		h.writeFile(filepath.Join(h.repo, "bin", "jarvix"), "cli v2")
		h.writeFile(filepath.Join(h.repo, "bin", "jarvixd"), "daemon v2")
	}})
	h.runner.script("systemctl --user restart jarvixd", resp{})
	h.runner.script(h.cli()+" status", resp{out: "version:  v2 (protocol 1)\n"})
	h.runner.script(h.cli()+" doctor --gate", resp{out: "[OK] everything\n"})

	if err := h.u.Upgrade(context.Background()); err != nil {
		t.Fatalf("rebuild failed: %v\noutput:\n%s", err, h.out)
	}
	if n := h.runner.callCount("merge"); n != 0 {
		t.Errorf("merge ran with nothing to pull")
	}
	if got := h.linkTarget("jarvix"); got != filepath.Join(h.slots, "v2", "jarvix") {
		t.Errorf("jarvix links to %s", got)
	}
}

// --- The shell's half -----------------------------------------------------

func TestQmlChangeReportsAPendingShellRestart(t *testing.T) {
	h := newHarness(t)
	h.scriptHealthyUpgrade()
	h.runner.scripts["git diff --name-only aaa111 HEAD -- plugin/"] =
		[]resp{{out: "plugin/omarchy/JarvixBar.qml\n"}}

	if err := h.u.Upgrade(context.Background()); err != nil {
		t.Fatalf("upgrade failed: %v\noutput:\n%s", err, h.out)
	}
	out := h.out.String()
	mustContain(t, out, "shell restart is pending")
	mustContain(t, out, "omarchy-restart-shell")
	if strings.Contains(out, "daemon-only") {
		t.Errorf("a QML change reported as daemon-only:\n%s", out)
	}
}

// --- Concurrency lock -----------------------------------------------------

func TestConcurrentUpgradeRefusedOnTheLock(t *testing.T) {
	h := newHarness(t)
	h.u.Alive = func(int) bool { return true }
	if err := os.MkdirAll(filepath.Dir(h.u.LockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.u.LockPath, []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := h.u.Upgrade(context.Background())
	if err == nil {
		t.Fatal("a held lock must refuse the second invocation")
	}
	mustContain(t, err.Error(), "another upgrade is already running")
	mustContain(t, err.Error(), "4242")
	if len(h.runner.calls) != 0 {
		t.Errorf("commands ran despite the lock: %v", h.runner.calls)
	}
	// The refusal must not free the first invocation's lock.
	if _, statErr := os.Stat(h.u.LockPath); statErr != nil {
		t.Errorf("the holder's lock was removed: %v", statErr)
	}
}

func TestStaleLockFromADeadProcessIsTakenOver(t *testing.T) {
	h := newHarness(t)
	h.u.Alive = func(int) bool { return false }
	if err := os.MkdirAll(filepath.Dir(h.u.LockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.u.LockPath, []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.scriptInspect("main", "", 0, 0)
	h.runner.scripts["git describe --tags --always origin/main"] = []resp{{out: "v1\n"}}

	if err := h.u.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustContain(t, h.out.String(), "stale upgrade lock")
	// The lock is released again after the run.
	if _, err := os.Stat(h.u.LockPath); !os.IsNotExist(err) {
		t.Errorf("the lock was not released: %v", err)
	}
}

// --- Gate plumbing: daemon won't start, wrong version, restart fails ------

func TestDaemonNeverAnsweringFailsTheGateAndRollbackThatAlsoFailsIsNamed(t *testing.T) {
	h := newHarness(t)
	h.u.SocketWait = time.Millisecond
	h.scriptInspect("main", "", 0, 3)
	h.runner.script("git merge --ff-only origin/main", resp{})
	h.runner.script("git describe --tags --always --dirty", resp{out: "v2\n"})
	h.runner.script("git diff --name-only aaa111 HEAD -- plugin/", resp{})
	h.runner.script("make build", resp{effect: func() {
		h.writeFile(filepath.Join(h.repo, "bin", "jarvix"), "cli v2")
		h.writeFile(filepath.Join(h.repo, "bin", "jarvixd"), "daemon v2")
	}})
	h.runner.script("systemctl --user restart jarvixd", resp{})
	h.runner.script(h.cli()+" status",
		resp{errOut: "jarvixd is not reachable", err: fmt.Errorf("exit status 1")})

	err := h.u.Upgrade(context.Background())
	if err == nil {
		t.Fatal("a daemon that never answers must fail the upgrade")
	}
	// Both gate runs saw the dead socket: the first triggered the rollback,
	// the second means recovery could not be confirmed — and says so.
	mustContain(t, h.out.String(), "socket dead within the 1ms startup budget")
	mustContain(t, err.Error(), "health gate still fails")
	// Rolled back all the same: broken-for-another-reason must not strand
	// the new binaries in place.
	if got := h.linkTarget("jarvix"); got != filepath.Join(h.slots, "v1", "jarvix") {
		t.Errorf("jarvix links to %s after failed recovery", got)
	}
}

func TestWrongVersionAfterRestartFailsTheGate(t *testing.T) {
	h := newHarness(t)
	h.scriptInspect("main", "", 0, 3)
	h.runner.script("git merge --ff-only origin/main", resp{})
	h.runner.script("git describe --tags --always --dirty", resp{out: "v2\n"})
	h.runner.script("git diff --name-only aaa111 HEAD -- plugin/", resp{})
	h.runner.script("make build", resp{effect: func() {
		h.writeFile(filepath.Join(h.repo, "bin", "jarvix"), "cli v2")
		h.writeFile(filepath.Join(h.repo, "bin", "jarvixd"), "daemon v2")
	}})
	h.runner.script("systemctl --user restart jarvixd", resp{})
	// The daemon answers, but as the old version: the restart did not take.
	h.runner.script(h.cli()+" status", resp{out: "version:  v1 (protocol 1)\n"})
	h.runner.script(h.cli()+" doctor --gate", resp{out: "[OK] everything\n"})

	err := h.u.Upgrade(context.Background())
	if err == nil {
		t.Fatal("a daemon still on the old version must fail the gate")
	}
	mustContain(t, h.out.String(), "the restart did not take")
	// Rollback ran and its gate — expecting v1 — passed: recovery confirmed.
	mustContain(t, h.out.String(), "recovery confirmed: v1 is serving again")
}

func TestRestartFailureTriggersRollback(t *testing.T) {
	h := newHarness(t)
	h.scriptInspect("main", "", 0, 3)
	h.runner.script("git merge --ff-only origin/main", resp{})
	h.runner.script("git describe --tags --always --dirty", resp{out: "v2\n"})
	h.runner.script("git diff --name-only aaa111 HEAD -- plugin/", resp{})
	h.runner.script("make build", resp{effect: func() {
		h.writeFile(filepath.Join(h.repo, "bin", "jarvix"), "cli v2")
		h.writeFile(filepath.Join(h.repo, "bin", "jarvixd"), "daemon v2")
	}})
	h.runner.script("systemctl --user restart jarvixd",
		resp{errOut: "Failed to restart jarvixd.service", err: fmt.Errorf("exit status 1")},
		resp{})
	h.runner.script(h.cli()+" status", resp{out: "version:  v1 (protocol 1)\n"})
	h.runner.script(h.cli()+" doctor --gate", resp{out: "[OK] everything\n"})

	err := h.u.Upgrade(context.Background())
	if err == nil {
		t.Fatal("a failed restart must fail the upgrade")
	}
	mustContain(t, h.out.String(), "Failed to restart jarvixd.service")
	mustContain(t, h.out.String(), "recovery confirmed: v1 is serving again")
	if got := h.linkTarget("jarvix"); got != filepath.Join(h.slots, "v1", "jarvix") {
		t.Errorf("jarvix links to %s after rollback", got)
	}
}

// --- Retention ------------------------------------------------------------

func TestPruneKeepsOnlyCurrentAndPrevious(t *testing.T) {
	h := newHarness(t)
	h.managedInstall("v1")
	h.writeFile(filepath.Join(h.slots, "v0", "jarvix"), "cli v0")
	h.writeFile(filepath.Join(h.slots, "v0", "jarvixd"), "daemon v0")
	h.scriptHealthyUpgrade()

	if err := h.u.Upgrade(context.Background()); err != nil {
		t.Fatalf("upgrade failed: %v\noutput:\n%s", err, h.out)
	}
	if _, err := os.Stat(filepath.Join(h.slots, "v0")); !os.IsNotExist(err) {
		t.Errorf("the v0 slot survived pruning")
	}
	for _, keep := range []string{"v1", "v2"} {
		if _, err := os.Stat(filepath.Join(h.slots, keep, "jarvix")); err != nil {
			t.Errorf("slot %s was pruned: %v", keep, err)
		}
	}
	mustContain(t, h.out.String(), "pruned old release v0")
}

// Re-staging the very slot that is also the rollback target would destroy
// the only working copy the moment the gate failed; the machine must treat
// that as "no distinct previous" instead.
func TestRestagingThePreviousSlotForfeitsRollbackLoudly(t *testing.T) {
	h := newHarness(t)
	h.managedInstall("v2") // already on a slot named v2...
	h.scriptInspect("main", "", 0, 0)
	h.runner.script("git describe --tags --always --dirty", resp{out: "v2\n"}) // ...and rebuilding v2
	h.runner.script("git diff --name-only aaa111 HEAD -- plugin/", resp{})
	h.runner.script("make build", resp{effect: func() {
		h.writeFile(filepath.Join(h.repo, "bin", "jarvix"), "cli v2 rebuilt")
		h.writeFile(filepath.Join(h.repo, "bin", "jarvixd"), "daemon v2 rebuilt")
	}})
	h.runner.script("systemctl --user restart jarvixd", resp{})
	h.runner.script(h.cli()+" status", resp{out: "version:  v2 (protocol 1)\n"})
	h.runner.script(h.cli()+" doctor --gate", resp{out: "[OK] everything\n"})

	if err := h.u.Upgrade(context.Background()); err != nil {
		t.Fatalf("rebuild failed: %v\noutput:\n%s", err, h.out)
	}
	mustContain(t, h.out.String(), "no distinct previous release to roll back to")
}

// --- Small parsers --------------------------------------------------------

func TestStatusVersionParsing(t *testing.T) {
	cases := map[string]string{
		"state:    idle\nversion:  v1.2.3 (protocol 1)\n": "v1.2.3",
		"version: abc123\n": "abc123",
		"state: idle\n":     "",
		"":                  "",
	}
	for in, want := range cases {
		if got := statusVersion(in); got != want {
			t.Errorf("statusVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
