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
	if guards := strings.Count(workflow, "github.event.pull_request.head.repo.full_name != github.repository"); guards != 2 {
		t.Errorf("both jobs must skip the duplicate same-repo pull_request run, found %d guards", guards)
	}
	// Cancelling is the usual concurrency setting and the wrong one here: a
	// cancelled run is a lost verdict. Observed for real — the skipped
	// pull_request run cancelled the push run that had done the grading.
	if strings.Contains(workflow, "cancel-in-progress: true") {
		t.Error("cancel-in-progress must stay false: a cancelled run is a push without a verdict")
	}
}
