package main

// The CLI face of `jarvix upgrade` (#139): flag handling and finding the
// user's checkout. The state machine itself is tested hermetically in
// internal/upgrade; nothing here builds, installs, or restarts anything.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunUpgradeAndDoctorFlagErrors(t *testing.T) {
	hermeticEnv(t)
	cases := map[string][]string{
		"upgrade unknown flag":  {"upgrade", "--checkk"},
		"upgrade extra args":    {"upgrade", "--check", "now"},
		"doctor unknown flag":   {"doctor", "--gatee"},
		"doctor gate plus more": {"doctor", "--gate", "verbose"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var code int
			_, stderr := capture(t, func() { code = run(args) })
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if !strings.Contains(stderr, "usage:") {
				t.Errorf("stderr = %q, want usage guidance", stderr)
			}
		})
	}
}

// Without a plugin symlink and outside any checkout, upgrade must say how to
// give it a checkout rather than guessing one.
func TestRunUpgradeOutsideACheckoutExplainsItself(t *testing.T) {
	hermeticEnv(t)
	t.Chdir(t.TempDir())
	var code int
	_, stderr := capture(t, func() { code = run([]string{"upgrade", "--check"}) })
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "cannot find the jarvix checkout") {
		t.Errorf("stderr = %q", stderr)
	}
}

// fakeCheckout builds a directory that passes isJarvixCheckout.
func fakeCheckout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module github.com/rpickz/jarvix\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "plugin", "omarchy"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The installed plugin symlink points at <checkout>/plugin/omarchy, so the
// checkout is two levels up from its target — no configuration needed.
func TestResolveRepoFollowsThePluginSymlink(t *testing.T) {
	hermeticEnv(t)
	t.Chdir(t.TempDir())
	checkout := fakeCheckout(t)
	link := filepath.Join(os.Getenv("HOME"), ".config", "omarchy", "plugins", "jarvix")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(checkout, "plugin", "omarchy"), link); err != nil {
		t.Fatal(err)
	}

	repo, err := resolveRepo()
	if err != nil {
		t.Fatal(err)
	}
	if repo != checkout {
		t.Errorf("resolveRepo = %q, want %q", repo, checkout)
	}
}

// A daemon-only install has no plugin; running from inside the checkout
// (any depth) still finds it.
func TestResolveRepoFallsBackToTheWorkingDirectory(t *testing.T) {
	hermeticEnv(t)
	checkout := fakeCheckout(t)
	inside := filepath.Join(checkout, "internal", "upgrade")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(inside)

	repo, err := resolveRepo()
	if err != nil {
		t.Fatal(err)
	}
	// The checkout may be reported through a resolved symlink (t.TempDir on
	// macOS); compare the identity, not the spelling.
	want, got := mustStat(t, checkout), mustStat(t, repo)
	if !os.SameFile(want, got) {
		t.Errorf("resolveRepo = %q, want %q", repo, checkout)
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// Only the real checkout qualifies: a copied source tree without .git, or
// some other module, must not be pulled into.
func TestIsJarvixCheckoutRejectsLookalikes(t *testing.T) {
	noGit := t.TempDir()
	if err := os.WriteFile(filepath.Join(noGit, "go.mod"),
		[]byte("module github.com/rpickz/jarvix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	otherModule := t.TempDir()
	if err := os.WriteFile(filepath.Join(otherModule, "go.mod"),
		[]byte("module github.com/somebody/else\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(otherModule, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"no .git":      noGit,
		"other module": otherModule,
		"empty":        t.TempDir(),
	} {
		if isJarvixCheckout(dir) {
			t.Errorf("%s accepted as a checkout", name)
		}
	}
}
