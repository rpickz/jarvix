package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A tarball install puts the two binaries on PATH and leaves the helper
// scripts wherever the user unpacked them. findScript used to look only in
// /usr/share, next to ../scripts of the executable, and in the working
// directory, so `jarvix setup` on such a machine silently dropped the Kokoro
// and Hyprland steps — the prompts never appeared and nothing said why. The
// user-local data dir is the answer, and it is what INSTALL.md now tells
// people to use (raised in review of #20).
func TestFindScriptSearchesUserLocalDataDir(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	// Keep the working-directory candidate from matching by accident.
	t.Chdir(t.TempDir())

	if got := findScript("setup-kokoro.sh"); got != "" {
		t.Fatalf("nothing is installed yet, got %q", got)
	}

	scripts := filepath.Join(data, "jarvix", "scripts")
	if err := os.MkdirAll(scripts, 0o700); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(scripts, "setup-kokoro.sh")
	if err := os.WriteFile(want, []byte("#!/bin/bash\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := findScript("setup-kokoro.sh"); got != want {
		t.Errorf("findScript = %q, want %q", got, want)
	}
}

// A directory is not a script: a stray directory of that name must not be
// handed to bash.
func TestFindScriptIgnoresDirectories(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(filepath.Join(data, "jarvix", "scripts", "setup-kokoro.sh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := findScript("setup-kokoro.sh"); got != "" {
		t.Errorf("findScript = %q, want \"\" for a directory", got)
	}
}

// The install notes generated into every release tarball have to name a
// directory findScript actually searches, or the delegated setup steps are
// unreachable after a manual install. This pins the two halves together.
func TestReleaseInstallNotesPointAtASearchedDirectory(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "scripts", "package-release.sh"))
	if err != nil {
		t.Fatalf("read the packaging script: %v", err)
	}
	if !strings.Contains(string(data), "~/.local/share/jarvix/scripts") {
		t.Error("INSTALL.md must tell users to install the helper scripts into " +
			"~/.local/share/jarvix/scripts, which findScript searches")
	}
}
