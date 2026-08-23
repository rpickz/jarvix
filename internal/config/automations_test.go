package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/automation"
)

// The schedule-key config tests (ADR 0032): the `schedule` / `announce` keys
// on [[routines]] and [[scripts]] validate hard through the real parser —
// there is no second copy of the grammar — and convert into the scheduler's
// entries in declaration order.

func scheduledTablesTOML(scriptPath string) string {
	return fmt.Sprintf(`
[[routines]]
name = "morning setup"
phrases = ["morning setup"]
schedule = "08:30 mon-fri"

  [[routines.steps]]
  app = "alacritty"
  workspace = 1

[[scripts]]
name = "backup notes"
phrases = ["backup my notes"]
path = %q
schedule = "02:00"
announce = true
`, scriptPath)
}

// TestScheduleKeysParseAndConvert: the documented shape parses, validates,
// and converts into scheduler entries with the announce default (false)
// applied where the file is silent.
func TestScheduleKeysParseAndConvert(t *testing.T) {
	cfg := parseValid(t, scheduledTablesTOML(stubScriptFile(t)))
	entries := cfg.AutomationEntries()
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	r := entries[0]
	if r.Kind != automation.KindRoutine || r.Name != "morning setup" ||
		r.Schedule.String() != "08:30 mon-fri" || r.Announce {
		t.Errorf("routine entry = %+v; announce must default to false", r)
	}
	s := entries[1]
	if s.Kind != automation.KindScript || s.Name != "backup notes" ||
		s.Schedule.String() != "02:00" || !s.Announce {
		t.Errorf("script entry = %+v", s)
	}
}

// TestUnscheduledTablesYieldNoEntries: tables without a schedule stay purely
// phrase-triggered.
func TestUnscheduledTablesYieldNoEntries(t *testing.T) {
	cfg := parseValid(t, morningSetupTOML)
	if entries := cfg.AutomationEntries(); len(entries) != 0 {
		t.Fatalf("entries = %+v, want none without a schedule key", entries)
	}
}

// TestScheduleValidationSpeaksTheFileLanguage: a bad schedule is refused at
// load, naming the table and teaching the syntax — and announce without a
// schedule is refused as the configuration mistake it is.
func TestScheduleValidationSpeaksTheFileLanguage(t *testing.T) {
	path := stubScriptFile(t)
	for _, tc := range []struct {
		name string
		doc  string
		want string
	}{
		{
			"routine schedule is not a time",
			`
[[routines]]
name = "morning setup"
phrases = ["morning setup"]
schedule = "8am weekdays"

  [[routines.steps]]
  app = "alacritty"
  workspace = 1
`,
			`routines[0] ("morning setup"): schedule "8am weekdays"`,
		},
		{
			"script schedule with cron fields",
			fmt.Sprintf(`
[[scripts]]
name = "backup notes"
phrases = ["backup my notes"]
path = %q
schedule = "0 2 * * *"
`, path),
			`scripts[0] ("backup notes"): schedule "0 2 * * *" has too many parts`,
		},
		{
			"announce without a schedule",
			fmt.Sprintf(`
[[scripts]]
name = "backup notes"
phrases = ["backup my notes"]
path = %q
announce = true
`, path),
			`scripts[0] ("backup notes"): announce is set but there is no schedule`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parse([]byte(tc.doc), Default())
			if err != nil {
				t.Fatal(err)
			}
			err = cfg.Validate()
			if err == nil {
				t.Fatal("the document validated")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("validation = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}
