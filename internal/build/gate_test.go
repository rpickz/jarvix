package build

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This file guards the gate itself. It lives beside TestVersionStamping for
// the same reason that test does: the Makefile and the CI workflow are part
// of the build contract, and nothing else in the tree is in a position to
// notice when they drift apart.

// repoRoot returns the checkout root (the directory holding go.mod), found by
// walking up from the test's working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}

// stubBin writes an executable of the given name that exits with the given
// status, and returns the directory holding it — to be put first on PATH.
func stubBin(t *testing.T, name string, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// `make lint` must fail when the linter finds something. The old recipe was
//
//	command -v golangci-lint && golangci-lint run || echo "not installed"
//
// where the `||` catches BOTH branches: a nonzero exit from the linter — real
// findings — was reported as "not installed" and `make ci` went green on code
// the CI gate rejects (raised in review of #15).
func TestMakeLintPropagatesLinterFailure(t *testing.T) {
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not on PATH")
	}
	root := repoRoot(t)

	run := func(exitCode int) error {
		stub := stubBin(t, "golangci-lint", exitCode)
		// GO=true turns the `vet` prerequisite into a no-op, so this test
		// measures the lint recipe and nothing else.
		cmd := exec.Command(makeBin, "GO=true", "lint")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "PATH="+stub+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		t.Logf("make lint (stub exit %d): %v\n%s", exitCode, err, out)
		return err
	}

	if err := run(1); err == nil {
		t.Error("a linter that reports findings must fail `make lint`, not be mistaken for a missing tool")
	}
	if err := run(0); err != nil {
		t.Errorf("a clean linter run must succeed: %v", err)
	}
}

// `make ci` is documented as mirroring .github/workflows/ci.yml. It omitted
// the build and the gofmt check, so it could pass locally while the gate went
// red on formatting (raised in review of #15).
func TestMakeCIMirrorsTheWorkflow(t *testing.T) {
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not on PATH")
	}
	// -n prints the recipe without running any of it.
	cmd := exec.Command(makeBin, "-n", "ci")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n ci: %v\n%s", err, out)
	}
	for _, want := range []string{"go build ./...", "gofmt -l .", "go vet ./...", "go test -race -count=2 ./..."} {
		if !strings.Contains(string(out), want) {
			t.Errorf("`make ci` does not run %q; the workflow does\n%s", want, out)
		}
	}
}

// The workflow's own promise, in its first line, is that every push gets a
// verdict — and a pinned linter is what makes that verdict mean the same
// thing tomorrow (raised in review of #15).
func TestCIWorkflowGradesEveryBranchWithAPinnedLinter(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	if strings.Contains(workflow, "branches: [main]") {
		t.Error("the push trigger must not be restricted to main: a feature branch with no open PR would get no verdict")
	}
	if strings.Contains(workflow, "version: latest") {
		t.Error("golangci-lint must be pinned; `latest` lets a tool release redden an unchanged commit")
	}
	if !strings.Contains(workflow, "concurrency:") {
		t.Error("with push covering every branch, a concurrency group is what stops a branch with an open PR being graded twice")
	}
	// The push trigger already grades a same-repo branch, so the duplicate
	// pull_request run is skipped rather than cancelled: a skipped job is
	// neutral, a cancelled one shows up red next to the green result. Fork
	// PRs still need the pull_request trigger, hence a guard rather than
	// dropping it.
	//
	// Counted against the number of jobs rather than a literal, because the
	// count changed the first time a job was added (the coverage ratchet,
	// #171) and a hard-coded 2 turns "a new job forgot the guard" into "the
	// number moved", which is a different and much less useful failure.
	jobs := jobNames(workflow)
	if len(jobs) < 2 {
		t.Fatalf("found %v jobs in ci.yml; this guard is no longer watching anything", jobs)
	}
	if guards := strings.Count(workflow, "github.event.pull_request.head.repo.full_name != github.repository"); guards != len(jobs) {
		t.Errorf("all %d jobs (%v) must skip the duplicate same-repo pull_request run, found %d guards",
			len(jobs), jobs, guards)
	}
	// Cancelling is the usual concurrency setting and the wrong one here: a
	// cancelled run is a lost verdict. Observed for real — the skipped
	// pull_request run cancelled the push run that had done the grading.
	if strings.Contains(workflow, "cancel-in-progress: true") {
		t.Error("cancel-in-progress must stay false: a cancelled run is a push without a verdict")
	}
	// The gate's other promise is speed, so every push can afford it. The
	// soak's high-count runs belong on a schedule; a `-count=50` that crept in
	// here would put tens of minutes on every push and the soak would be
	// deleted for being slow rather than for being wrong (issue #171).
	for _, banned := range []string{"-count=50", "-count=25", "GOMAXPROCS=2"} {
		if strings.Contains(workflow, banned) {
			t.Errorf("the PR gate must not run %s; that is soak.yml's job", banned)
		}
	}
	if !strings.Contains(workflow, "scripts/coverage-ratchet.sh") {
		t.Error("the gate does not run the coverage ratchet, so coverage can slide unnoticed")
	}
}

// The soak workflow (issue #171) is the one job in this repo whose value is
// entirely in the shape of its commands: the wrong flags produce a green run
// that proves nothing, on a schedule, where nobody is looking. So the shape is
// asserted rather than trusted.
func TestSoakWorkflowRunsTheCommandsThatFoundTheDefects(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "soak.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)

	// Scheduled, never on the PR path. Both halves matter: without the
	// schedule it never runs, and on the PR path it would be turned off.
	if !strings.Contains(workflow, "schedule:") || !strings.Contains(workflow, "cron:") {
		t.Error("the soak has no schedule, so nothing runs it")
	}
	for _, trigger := range []string{"\n  push:", "\n  pull_request:"} {
		if strings.Contains(workflow, trigger) {
			t.Errorf("the soak must not run on %s: the PR gate's promise is a verdict in minutes",
				strings.TrimSpace(trigger))
		}
	}

	// The three modes, each earned by a defect that only it catches. See
	// docs/soak.md and scripts/soak.sh for which is which.
	for _, mode := range []string{"repeat", "constrained", "unraced"} {
		if !strings.Contains(workflow, "scripts/soak.sh "+mode+" ") {
			t.Errorf("the soak does not run the %q mode", mode)
		}
	}

	// Every step bounded, and the job bounded behind them. A soak that hangs
	// has stopped being a soak and started being a bill.
	if got := strings.Count(workflow, "timeout-minutes:"); got < 4 {
		t.Errorf("timeout-minutes appears %d times; want the job plus each soak step", got)
	}

	// The artefact is the whole point of capturing output at all. #170's first
	// sighting was piped through `tail` and the evidence was lost.
	if !strings.Contains(workflow, "upload-artifact") || !strings.Contains(workflow, "retention-days:") {
		t.Error("the soak does not retain its logs as an artefact")
	}
	for _, truncator := range []string{"| tail", "|tail", "| head", "|head"} {
		if strings.Contains(workflow, truncator) {
			t.Errorf("the soak pipes output through %q: that is how #170's first failure lost its evidence", truncator)
		}
	}
	if strings.Contains(workflow, "fail-fast: true") {
		t.Error("fail-fast would hide five packages behind the first failure; #155 and #166 were the same defect in two")
	}

	// The workflow's matrix and the script's default list are two copies of
	// one fact, and a package that falls out of either stops being soaked
	// silently.
	script, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "soak.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{"session", "daemon", "focus", "reminders", "conversations", "automation"} {
		if !strings.Contains(workflow, "\n          - "+pkg+"\n") {
			t.Errorf("internal/%s is not in the soak matrix", pkg)
		}
		if !strings.Contains(string(script), "./internal/"+pkg+"\n") {
			t.Errorf("internal/%s is not in scripts/soak.sh's default package list", pkg)
		}
	}
}

// The coverage floor is a committed number with an argument attached (issue
// #171). This test is the argument's guard: the file must still hold a number
// a script can read, and the script must still fail on a real drop and forgive
// noise — a ratchet nobody has watched fail is a ratchet nobody trusts.
func TestCoverageRatchetFailsOnADropAndForgivesNoise(t *testing.T) {
	root := repoRoot(t)
	floor, err := os.ReadFile(filepath.Join(root, "coverage.floor"))
	if err != nil {
		t.Fatal(err)
	}
	var number string
	for _, line := range strings.Split(string(floor), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		number = line
	}
	value, err := strconv.ParseFloat(number, 64)
	if err != nil {
		t.Fatalf("coverage.floor holds %q, which is not a percentage: %v", number, err)
	}
	if value <= 0 || value > 100 {
		t.Fatalf("coverage.floor holds %v, which is not a percentage", value)
	}

	// COVERAGE_TOTAL short-circuits the measurement, so the comparison can be
	// exercised in milliseconds instead of behind a full `go test ./...`.
	for _, tc := range []struct {
		total    float64
		wantPass bool
		why      string
	}{
		{value + 1, true, "above the floor"},
		{value, true, "exactly the floor"},
		{value - 0.4, true, "within the 0.5pp tolerance: unrelated changes move the total by a tenth or two"},
		{value - 0.6, false, "past the tolerance: this is the slide the ratchet exists to stop"},
		{value - 20, false, "a collapse"},
	} {
		cmd := exec.Command(filepath.Join(root, "scripts", "coverage-ratchet.sh"))
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"COVERAGE_TOTAL="+strconv.FormatFloat(tc.total, 'f', 2, 64))
		out, err := cmd.CombinedOutput()
		if gotPass := err == nil; gotPass != tc.wantPass {
			t.Errorf("coverage %.2f%% (%s): pass=%v, want %v\n%s",
				tc.total, tc.why, gotPass, tc.wantPass, out)
		}
	}
}

// jobNames returns the top-level job keys of a workflow. A two-space-indented
// `name:` under `jobs:` is a job and nothing else is, which is enough
// structure to count without pulling a YAML parser into the module for one
// test (the module has exactly one dependency and it is a TOML parser).
func jobNames(workflow string) []string {
	_, after, found := strings.Cut(workflow, "\njobs:\n")
	if !found {
		return nil
	}
	var names []string
	for _, line := range strings.Split(after, "\n") {
		trimmed := strings.TrimSuffix(line, ":")
		if trimmed == line || !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			continue
		}
		names = append(names, strings.TrimSpace(trimmed))
	}
	return names
}
