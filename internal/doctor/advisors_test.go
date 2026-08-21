package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
)

// stubBinary writes an executable file and puts its directory on PATH, so
// exec.LookPath finds it without the test depending on any installed CLI.
func stubBinary(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return path
}

func resultNamed(results []Result, name string) (Result, bool) {
	for _, r := range results {
		if r.Name == name {
			return r, true
		}
	}
	return Result{}, false
}

func TestAdvisorChecksReportPresence(t *testing.T) {
	present := stubBinary(t, "oracle")
	cfg := config.Config{Advisors: map[string]config.Advisor{
		"oracle":  {Binary: present, ReadOnly: true},
		"missing": {Binary: "jarvix-not-installed-anywhere"},
	}}

	results := advisorChecks(cfg)
	if len(results) != 2 {
		t.Fatalf("want one result per advisor, got %d", len(results))
	}

	ok, found := resultNamed(results, `advisor "oracle" available`)
	if !found || ok.Status != OK || ok.Detail != present {
		t.Errorf("present advisor: %+v", ok)
	}

	missing, found := resultNamed(results, `advisor "missing" available`)
	if !found || missing.Status != Warn {
		t.Errorf("missing advisor should warn, not fail: %+v", missing)
	}
	if !strings.Contains(missing.Fix, "[advisors.missing]") {
		t.Errorf("fix should name the table to edit: %q", missing.Fix)
	}
	// A missing advisor must not make the whole installation unhealthy:
	// Jarvix answers fine without one.
	if !Healthy(results) {
		t.Error("a missing advisor must not fail the doctor run")
	}
}

func TestAdvisorChecksSayWhenConsultationsAreConfirmed(t *testing.T) {
	present := stubBinary(t, "agent")
	cfg := config.Config{Advisors: map[string]config.Advisor{
		"agent": {Binary: present}, // no read-only claim → confirmed each use
	}}
	r := advisorChecks(cfg)[0]
	if !strings.Contains(r.Detail, "confirmed before each use") {
		t.Errorf("detail should explain the permission tier: %q", r.Detail)
	}
}

func TestAdvisorChecksWithNoneConfigured(t *testing.T) {
	results := advisorChecks(config.Config{})
	if len(results) != 1 || results[0].Status != OK {
		t.Fatalf("results = %+v", results)
	}
	if !strings.Contains(results[0].Detail, "jarvix setup") {
		t.Errorf("detail should point at the wizard: %q", results[0].Detail)
	}
}
