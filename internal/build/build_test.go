package build

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestVersionStamping proves the ldflags wiring the Makefile, the release
// workflow, and the PKGBUILD all rely on: -X on build.Version must surface
// through `jarvix --version`.
func TestVersionStamping(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}

	bin := filepath.Join(t.TempDir(), "jarvix")
	build := exec.Command(goBin, "build",
		"-ldflags", "-X github.com/rpickz/jarvix/internal/build.Version=v9.9.9-test",
		"-o", bin, "github.com/rpickz/jarvix/cmd/jarvix")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("--version failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "jarvix v9.9.9-test" {
		t.Fatalf("got %q, want %q", got, "jarvix v9.9.9-test")
	}
}

func TestDefaultVersion(t *testing.T) {
	if Version != "dev" {
		t.Fatalf("unstamped Version must be %q, got %q", "dev", Version)
	}
}
