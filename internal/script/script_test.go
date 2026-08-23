package script

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The validation tests run against real files in test-owned temp dirs —
// never a user's — because the checks under test are exactly the filesystem
// facts (present, executable, a file) the acceptance criterion wants caught
// before any phrase is spoken.

// stubScript writes an executable shell script into dir and returns its path.
func stubScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// valid returns a definition that passes every check, for tests to break one
// field at a time.
func valid(t *testing.T, dir string) Definition {
	t.Helper()
	return Definition{
		Name:    "backup notes",
		Phrases: []string{"backup my notes"},
		Path:    stubScript(t, dir, "backup.sh", "exit 0"),
		Timeout: DefaultTimeout,
		Report:  ReportSummary,
	}
}

func TestProblemsAcceptsAValidDefinition(t *testing.T) {
	if problems := Problems([]Definition{valid(t, t.TempDir())}); len(problems) != 0 {
		t.Errorf("valid definition rejected: %v", problems)
	}
}

func TestProblemsNamesEveryBrokenShape(t *testing.T) {
	dir := t.TempDir()
	notExec := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(notExec, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Definition)
		want   string
	}{
		{"empty name", func(d *Definition) { d.Name = "" }, "scripts[0]: name is empty"},
		{"no phrases", func(d *Definition) { d.Phrases = nil }, "it has no phrases"},
		{"empty path", func(d *Definition) { d.Path = "" }, "path is empty"},
		{"relative path", func(d *Definition) { d.Path = "bin/backup.sh" }, "is not absolute"},
		{"missing file", func(d *Definition) { d.Path = filepath.Join(dir, "gone.sh") }, "does not exist"},
		{"directory", func(d *Definition) { d.Path = dir }, "is a directory"},
		{"not executable", func(d *Definition) { d.Path = notExec }, "is not executable"},
		{"bad report", func(d *Definition) { d.Report = "shout" }, `report "shout" is not a mode`},
		{"zero timeout", func(d *Definition) { d.Timeout = 0 }, "timeout_sec must be between"},
		{"negative timeout", func(d *Definition) { d.Timeout = -time.Second }, "timeout_sec must be between"},
		{"huge timeout", func(d *Definition) { d.Timeout = MaxTimeout + time.Second }, "timeout_sec must be between"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := valid(t, t.TempDir())
			tt.mutate(&def)
			problems := Problems([]Definition{def})
			if len(problems) == 0 {
				t.Fatal("accepted despite the problem")
			}
			if !strings.Contains(strings.Join(problems, "; "), tt.want) {
				t.Errorf("problems %v do not contain %q", problems, tt.want)
			}
		})
	}
}

// TestProblemsRefusesDuplicateNames: names are the unit of triggering,
// logging, and gating, so two scripts sharing one (in any casing) is a
// config error naming both entries.
func TestProblemsRefusesDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	a, b := valid(t, dir), valid(t, dir)
	b.Name = "Backup Notes" // case must not disguise the duplicate
	problems := Problems([]Definition{a, b})
	if len(problems) != 1 {
		t.Fatalf("problems = %v", problems)
	}
	if !strings.Contains(problems[0], "scripts[1]") || !strings.Contains(problems[0], "scripts[0]") {
		t.Errorf("duplicate-name problem %q does not name both entries", problems[0])
	}
}

// TestProblemsLabelsCarryTheIndex: a broken third entry must be reported as
// the third entry — the labels are what the user greps their config with.
func TestProblemsLabelsCarryTheIndex(t *testing.T) {
	dir := t.TempDir()
	a, b := valid(t, dir), valid(t, dir)
	b.Name = "photos"
	b.Phrases = []string{"backup my photos"}
	b.Path = filepath.Join(dir, "missing.sh")
	problems := Problems([]Definition{a, b})
	if len(problems) != 1 || !strings.Contains(problems[0], `scripts[1] ("photos")`) {
		t.Errorf("problems = %v; want one naming scripts[1]", problems)
	}
}
