package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The backup/restore commands through run() — flag parsing, report wording,
// and the exit codes a cron line depends on. The machinery itself is tested
// in internal/backup; what is pinned here is the CLI contract.

// seedCLIState writes a store into the hermetic XDG roots hermeticEnv set.
func seedCLIState(t *testing.T) {
	t.Helper()
	state := filepath.Join(os.Getenv("XDG_STATE_HOME"), "jarvix")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := "version = 1\nnext_id = 2\n\n[[fact]]\nid = \"m1\"\ncontent = \"the staging server is called atlas\"\nstored = 2026-08-01T10:00:00Z\nupdated = 2026-08-01T10:00:00Z\n"
	if err := os.WriteFile(filepath.Join(state, "memory.toml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBackupCommandWritesAnArchiveAndReports(t *testing.T) {
	hermeticEnv(t)
	seedCLIState(t)
	archive := filepath.Join(t.TempDir(), "cli.tar.gz")

	var code int
	stdout, _ := capture(t, func() { code = run([]string{"backup", archive}) })
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("no archive written: %v", err)
	}
	if !strings.Contains(stdout, archive) || !strings.Contains(stdout, "direct capture") {
		t.Errorf("report %q does not name the archive and capture mode", stdout)
	}
}

// --quiet is the cron contract: success says nothing and exits 0.
func TestBackupQuietSaysNothingOnSuccess(t *testing.T) {
	hermeticEnv(t)
	seedCLIState(t)
	archive := filepath.Join(t.TempDir(), "cli.tar.gz")
	var code int
	stdout, stderr := capture(t, func() { code = run([]string{"backup", archive, "--quiet"}) })
	if code != 0 || stdout != "" || stderr != "" {
		t.Errorf("quiet backup: code=%d stdout=%q stderr=%q, want silent success", code, stdout, stderr)
	}
}

func TestBackupRejectsUnknownFlags(t *testing.T) {
	hermeticEnv(t)
	var code int
	_, stderr := capture(t, func() { code = run([]string{"backup", "--no-such-flag"}) })
	if code != 1 || !strings.Contains(stderr, "usage: jarvix backup") {
		t.Errorf("code=%d stderr=%q, want usage failure", code, stderr)
	}
}

func TestRestoreCommandRoundTripsAndNamesSafetyCopies(t *testing.T) {
	hermeticEnv(t)
	seedCLIState(t)
	archive := filepath.Join(t.TempDir(), "cli.tar.gz")
	if code := run([]string{"backup", archive, "--quiet"}); code != 0 {
		t.Fatalf("backup exit = %d", code)
	}

	var code int
	stdout, _ := capture(t, func() { code = run([]string{"restore", archive}) })
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "moved aside to") || !strings.Contains(stdout, ".pre-restore-") {
		t.Errorf("report %q does not name the safety copy", stdout)
	}
	if !strings.Contains(stdout, "jarvix doctor") {
		t.Errorf("report %q does not point at the doctor", stdout)
	}
}

// A refusal reaches the user with the specific reason and exit 1 — the
// stable failure code the docs promise cron.
func TestRestoreRefusalExitsOneWithTheReason(t *testing.T) {
	hermeticEnv(t)
	garbage := filepath.Join(t.TempDir(), "not-an-archive.tar.gz")
	if err := os.WriteFile(garbage, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	var code int
	_, stderr := capture(t, func() { code = run([]string{"restore", garbage}) })
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "restore refused") || !strings.Contains(stderr, "not a gzip archive") {
		t.Errorf("stderr %q does not carry the refusal reason", stderr)
	}
}

func TestRestoreRequiresAnArchiveArgument(t *testing.T) {
	hermeticEnv(t)
	var code int
	_, stderr := capture(t, func() { code = run([]string{"restore"}) })
	if code != 1 || !strings.Contains(stderr, "usage: jarvix restore") {
		t.Errorf("code=%d stderr=%q, want usage failure", code, stderr)
	}
}
