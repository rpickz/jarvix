package config

import (
	"fmt"
	"time"

	"github.com/rpickz/jarvix/internal/script"
)

// Script is one [[scripts]] table (ADR 0030): a named executable behind
// trigger phrases, run with zero arguments through the permission gate's
// script.run identity. The schema is deliberately flat and deliberately
// small: there is no args field and no env field, so v1's "nothing from
// speech — or anywhere else — reaches the script's argv or environment" is a
// property of the shape, not a promise of the code.
//
// Scripts are hand-edited TOML like [[routines]] and [[intents.custom]]:
// structured tables outside the config.set surface, landing on the next
// idle-class reload or restart. The daemon lists them read-only through
// `scripts.list` (docs/ipc.md).
type Script struct {
	// Name is what the gate's confirmation names, what `jarvix scripts run`
	// takes, and what every log and event carries. Unique across scripts,
	// case-insensitively.
	Name string `toml:"name"`
	// Phrases are the literal trigger phrases the intent router matches, so
	// they follow intent grammar (plain spoken words, no placeholders) and
	// must not collide with built-ins, custom intents, routines, or other
	// scripts — validated at load.
	Phrases []string `toml:"phrases"`
	// Path is the executable to run: absolute, present, executable —
	// validated at load and re-checked at run time. It is the entire command:
	// no arguments, no shell.
	Path string `toml:"path"`
	// TimeoutSec bounds one run in seconds; expiry kills the script's whole
	// process group. Zero means the default (script.DefaultTimeout).
	TimeoutSec int `toml:"timeout_sec"`
	// Report is what a successful run says: "summary" (the default),
	// "stdout" (the first line the script printed), or "silent". Failures
	// are spoken in every mode.
	Report string `toml:"report"`
	// Schedule optionally fires the script on a clock (ADR 0032): a time of
	// day with optional days — "02:00", "08:30 mon-fri". Empty means
	// phrase-triggered only. Because script.run defaults to ask and a
	// schedule cannot answer a question, a scheduled script only executes
	// when the identity is explicitly allowed; anything else is refused with
	// a notification at the scheduled moment, and warned about at load.
	Schedule string `toml:"schedule"`
	// Announce opts a scheduled firing's report line into speech. Off by
	// default on purpose: an unattended run reports through the activity
	// feed and a notification, never a voice at whatever hour the schedule
	// names.
	Announce bool `toml:"announce"`
}

// ScriptDefinitions converts the TOML tables into the script package's
// definitions, applying the defaults (60-second timeout, summary report).
// Conversion is shape- and order-preserving, so the labels script.Problems
// produces line up with the file's own indices.
func (c Config) ScriptDefinitions() []script.Definition {
	defs := make([]script.Definition, 0, len(c.Scripts))
	for _, s := range c.Scripts {
		def := script.Definition{
			Name:    s.Name,
			Phrases: append([]string(nil), s.Phrases...),
			Path:    s.Path,
			Timeout: time.Duration(s.TimeoutSec) * time.Second,
			Report:  script.Report(s.Report),
		}
		if s.TimeoutSec == 0 {
			def.Timeout = script.DefaultTimeout
		}
		if s.Report == "" {
			def.Report = script.ReportSummary
		}
		defs = append(defs, def)
	}
	return defs
}

// scriptProblems validates the [[scripts]] tables: the structural rules (and
// the file checks — a missing or non-executable script is refused at load,
// per the acceptance criterion "before any phrase is ever spoken") live in
// script.Problems, and the phrase grammar and collisions in intentProblems,
// which compiles the real router. There is no second, weaker copy of any
// rule.
func (c Config) scriptProblems() []string {
	if len(c.Scripts) == 0 {
		return nil
	}
	problems := script.Problems(c.ScriptDefinitions())
	for i, s := range c.Scripts {
		problems = append(problems,
			scheduleProblems(fmt.Sprintf("scripts[%d] (%q)", i, s.Name), s.Schedule, s.Announce)...)
	}
	if !c.Intents.Enabled {
		// The router is the only trigger there is: with it disabled a phrase
		// would fall through to the model, which must never be how a script
		// "runs". Saying so at load beats a phrase that silently stops working.
		problems = append(problems,
			"scripts are configured but intents.enabled is false; the intent router is what "+
				"triggers scripts, so re-enable it or remove the [[scripts]] tables")
	}
	return problems
}
