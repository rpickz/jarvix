package testdiscipline_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/testdiscipline"
)

// These two tests are the guards themselves, pointed at the whole checkout.
// The fixture tests in scan_test.go prove the scanners can tell a violation
// from a legitimate use; these prove the tree is clean today and stays clean.

func TestNoDerivedStateReadAfterOnlyItsCause(t *testing.T) {
	findings, err := testdiscipline.ScanDerivedState(goFiles(t, func(path string) bool {
		return strings.HasSuffix(path, "_test.go")
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("%s", f)
	}
}

func TestNoExportedMutableStateOnTestFakes(t *testing.T) {
	findings, err := testdiscipline.ScanFakeFields(goFiles(t, func(string) bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("%s", f)
	}
}

// The exemption list is a ratchet, and a ratchet that can slip is a list. A
// stale entry — a field that has since been unexported, or renamed away — must
// be deleted rather than left to make the list look longer than the debt is.
func TestFakeFieldExemptionsAreAllStillNeeded(t *testing.T) {
	for key, reason := range testdiscipline.FakeFieldExemptions {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("exemption %q carries no reason", key)
		}
	}
	// Re-run the scan with the exemptions disabled: every key must appear.
	// There is no API to disable them, so the check is the other way round —
	// each exempted field must still exist, written by its type's own method.
	files := goFiles(t, func(string) bool { return true })
	live := map[string]bool{}
	for key := range testdiscipline.FakeFieldExemptions {
		live[key] = false
	}
	findings, err := testdiscipline.ScanFakeFieldsIncludingExempt(files)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if _, known := live[f.Key]; known {
			live[f.Key] = true
		}
	}
	for key, seen := range live {
		if !seen {
			t.Errorf("exemption %q no longer matches anything: delete it, the debt is paid", key)
		}
	}
}

// goFiles lists every Go source file in the checkout that the guards should
// read. testdata is excluded because it holds the fixtures that must trip the
// guards on purpose — the go tool ignores those directories for exactly this
// reason, and so must the scan.
func goFiles(t *testing.T, keep func(path string) bool) []string {
	t.Helper()
	root := repoRoot(t)
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			// testdata holds the fixtures that must trip the guards on
			// purpose. .claude holds agent worktrees — whole second copies of
			// this checkout, which would be scanned twice and reported twice
			// under paths nobody can act on. Both are gitignored; so is the
			// rest of this list.
			case "testdata", ".git", ".claude", "bin", "dist", "soak-logs":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || !keep(path) {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found; this guard is no longer watching anything")
	}
	return files
}

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
