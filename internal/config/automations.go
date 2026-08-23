package config

import (
	"fmt"

	"github.com/rpickz/jarvix/internal/automation"
)

// This file is the config half of scheduled automations (ADR 0032): the
// shared validation for the `schedule` / `announce` keys a [[routines]] or
// [[scripts]] table may carry, and the conversion to the automation package's
// entries. The syntax itself — and the parse errors' worked examples — live
// in internal/automation, so config compiles the real parser as its check and
// there is no second, weaker copy of the grammar.

// scheduleProblems validates one table's schedule keys. The parser's own
// error carries the accepted forms; announce without a schedule is refused
// too, because it configures nothing and almost certainly means a schedule
// was intended.
func scheduleProblems(label, schedule string, announce bool) []string {
	var problems []string
	if schedule != "" {
		if _, err := automation.ParseSpec(schedule); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", label, err))
		}
	} else if announce {
		problems = append(problems, label+": announce is set but there is no schedule; "+
			"announce only affects scheduled firings — add a schedule or remove announce")
	}
	return problems
}

// AutomationEntries converts every scheduled [[routines]] and [[scripts]]
// table into the scheduler's entries, in declaration order (routines first,
// matching the file's own section order). Call on validated configuration:
// an unparseable schedule is skipped here, because Validate already refused
// it. A disabled entry (#93) is skipped too — parked means its schedule is
// off the clock, exactly as its phrases are out of the grammar — so the
// standard reload that flips `enabled` also rebuilds the schedules without
// it.
func (c Config) AutomationEntries() []automation.Entry {
	var entries []automation.Entry
	add := func(kind automation.Kind, name, schedule string, announce, enabled bool) {
		if schedule == "" || !enabled {
			return
		}
		spec, err := automation.ParseSpec(schedule)
		if err != nil {
			return // Validate refuses this first; belt and braces
		}
		entries = append(entries, automation.Entry{
			Kind: kind, Name: name, Schedule: spec, Announce: announce,
		})
	}
	for _, r := range c.Routines {
		add(automation.KindRoutine, r.Name, r.Schedule, r.Announce, r.IsEnabled())
	}
	for _, s := range c.Scripts {
		add(automation.KindScript, s.Name, s.Schedule, s.Announce, s.IsEnabled())
	}
	return entries
}
