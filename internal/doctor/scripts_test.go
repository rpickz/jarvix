package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
)

// The script checks stat stub files in test-owned temp dirs — doctor must
// never execute a script, and these tests never touch a user's files.

func TestScriptChecksReportPerScript(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "backup.sh")
	if err := os.WriteFile(good, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Intents: config.Intents{Enabled: true},
		Scripts: []config.Script{
			{Name: "backup notes", Phrases: []string{"backup my notes"}, Path: good},
			{Name: "gone", Phrases: []string{"run the gone one"}, Path: filepath.Join(dir, "gone.sh")},
			{Name: "relative", Phrases: []string{"run the relative one"}, Path: "bin/x.sh"},
		},
	}
	results := scriptChecks(cfg)
	if len(results) != 3 {
		t.Fatalf("results = %+v", results)
	}

	ok, found := resultNamed(results, `script "backup notes" runnable`)
	if !found || ok.Status != OK {
		t.Errorf("good script = %+v, %v", ok, found)
	}
	if !strings.Contains(ok.Detail, good) || !strings.Contains(ok.Detail, "report summary") {
		t.Errorf("good detail %q should name the path and the effective report mode", ok.Detail)
	}

	missing, found := resultNamed(results, `script "gone" runnable`)
	if !found || missing.Status != Fail {
		t.Errorf("missing script = %+v, %v", missing, found)
	}
	if !strings.Contains(missing.Detail, "does not exist") || missing.Fix == "" {
		t.Errorf("missing script result = %+v", missing)
	}

	rel, found := resultNamed(results, `script "relative" runnable`)
	if !found || rel.Status != Fail || !strings.Contains(rel.Detail, "is not absolute") {
		t.Errorf("relative script = %+v, %v", rel, found)
	}
}

// TestScriptChecksAttributeProblemsToTheRightEntry: the per-entry filter
// must not hand entry two's problem to entry one — the labels are how a user
// finds the table to fix.
func TestScriptChecksAttributeProblemsToTheRightEntry(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "ok.sh")
	if err := os.WriteFile(good, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Intents: config.Intents{Enabled: true},
		Scripts: []config.Script{
			{Name: "fine", Phrases: []string{"run fine"}, Path: good},
			{Name: "broken", Phrases: []string{"run broken"}, Path: filepath.Join(dir, "gone.sh")},
		},
	}
	results := scriptChecks(cfg)
	fine, _ := resultNamed(results, `script "fine" runnable`)
	if fine.Status != OK {
		t.Errorf("the healthy entry inherited a neighbour's problem: %+v", fine)
	}
	broken, _ := resultNamed(results, `script "broken" runnable`)
	if broken.Status != Fail {
		t.Errorf("broken = %+v", broken)
	}
}

func TestScriptChecksAbsentWhenNoneConfigured(t *testing.T) {
	if results := scriptChecks(config.Config{}); len(results) != 0 {
		t.Errorf("results = %+v", results)
	}
}
