package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The `enabled` switch on [[routines]] and [[scripts]] (issue #93): the one
// convention [[knowledge.feeds]] established (#92), adopted by both phrase
// families. Absent means true; disabled entries stay listed and validated;
// their phrases leave the intent grammar and their schedules leave the clock;
// only the cross-entry collision check relaxes, and re-enabling recompiles it.

// enabledScriptStub creates an executable stub so a [[scripts]] table passes
// the path checks.
func enabledScriptStub(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestEnabledDefaultsToTrue: the key is optional and every existing config
// keeps working unchanged — absent means enabled, for both families.
func TestEnabledDefaultsToTrue(t *testing.T) {
	cfg := parseValid(t, `
[[routines]]
name = "morning setup"
phrases = ["morning setup"]
[[routines.steps]]
app = "firefox"
workspace = 1

[[scripts]]
name = "backup notes"
phrases = ["backup my notes"]
path = "`+enabledScriptStub(t)+`"
`)
	if !cfg.Routines[0].IsEnabled() || cfg.Routines[0].Enabled != nil {
		t.Errorf("routine = %+v, want enabled by absence", cfg.Routines[0].Enabled)
	}
	if !cfg.Scripts[0].IsEnabled() || cfg.Scripts[0].Enabled != nil {
		t.Errorf("script = %+v, want enabled by absence", cfg.Scripts[0].Enabled)
	}
}

// TestDisabledPhrasesLeaveTheRouterOptions: IntentOptions carries the switch
// through to the router, where the grammar skip lives — the config half of
// the disabled-phrases-leave-the-grammar property.
func TestDisabledPhrasesLeaveTheRouterOptions(t *testing.T) {
	cfg := parseValid(t, `
[[routines]]
name = "morning setup"
phrases = ["morning setup"]
enabled = false
[[routines.steps]]
app = "firefox"
workspace = 1

[[scripts]]
name = "backup notes"
phrases = ["backup my notes"]
path = "`+enabledScriptStub(t)+`"
enabled = false
`)
	opts := cfg.IntentOptions()
	if len(opts.Routines) != 1 || !opts.Routines[0].Disabled {
		t.Errorf("routine options = %+v, want the entry carried, disabled", opts.Routines)
	}
	if len(opts.Scripts) != 1 || !opts.Scripts[0].Disabled {
		t.Errorf("script options = %+v, want the entry carried, disabled", opts.Scripts)
	}
}

// TestDisabledEntriesLeaveTheSchedules: a parked entry's schedule is off the
// clock — AutomationEntries skips it — while an enabled sibling's stays.
func TestDisabledEntriesLeaveTheSchedules(t *testing.T) {
	cfg := parseValid(t, `
[[routines]]
name = "evening"
phrases = ["evening mode"]
schedule = "18:00"
enabled = false
[[routines.steps]]
app = "mpv"
workspace = 5

[[scripts]]
name = "backup notes"
phrases = ["backup my notes"]
path = "`+enabledScriptStub(t)+`"
schedule = "02:00"
`)
	entries := cfg.AutomationEntries()
	if len(entries) != 1 || entries[0].Name != "backup notes" {
		t.Errorf("entries = %+v, want only the enabled script's schedule", entries)
	}
}

// TestDisabledEntriesAreStillValidated: parking an entry does not park its
// checks — a broken step, a rotten path, or a bad phrase is a load error even
// while disabled, so re-enabling can never surprise with a per-entry problem.
func TestDisabledEntriesAreStillValidated(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"disabled routine, shell-shaped app", `
[[routines]]
name = "bad"
phrases = ["bad routine"]
enabled = false
[[routines.steps]]
app = "firefox; rm -rf ~"
workspace = 1
`, "never through a shell"},
		{"disabled routine, placeholder phrase", `
[[routines]]
name = "bad"
phrases = ["setup {workspace}"]
enabled = false
[[routines.steps]]
app = "firefox"
workspace = 1
`, "contains a placeholder"},
		{"disabled script, missing file", `
[[scripts]]
name = "gone"
phrases = ["run the gone script"]
path = "/nonexistent/gone.sh"
enabled = false
`, "does not exist"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parse([]byte(tt.doc), Default())
			if err != nil {
				t.Fatal(err)
			}
			err = cfg.Validate()
			if err == nil {
				t.Fatal("validated despite the problem")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

// TestReenableCollisionFailsValidation is #93's collision criterion at the
// config level: a disabled entry may coexist with an enabled one holding its
// phrase — that is what a disable leaves behind — and the document with the
// entry re-enabled fails Validate with the load error naming both owners.
// This is exactly the document automations.set_enabled produces and
// validates before writing, so the refused half-enable is pinned here once.
func TestReenableCollisionFailsValidation(t *testing.T) {
	stub := enabledScriptStub(t)
	coexisting := `
[[routines]]
name = "old thing"
phrases = ["do the thing"]
enabled = false
[[routines.steps]]
app = "firefox"
workspace = 1

[[scripts]]
name = "new thing"
phrases = ["do the thing"]
path = "` + stub + `"
`
	parseValid(t, coexisting) // the parked state must load

	reenabled, err := SetEntryField([]byte(coexisting), "routines", "old thing", "enabled", true)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseBytes(reenabled)
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("the re-enabled collision validated")
	}
	if !strings.Contains(err.Error(), `the trigger for routine "old thing"`) {
		t.Errorf("error %q does not name both owners", err)
	}
}
