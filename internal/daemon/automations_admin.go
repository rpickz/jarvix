package daemon

// This file is the Automations tab's daemon half (issue #93): the unified
// listing (`automations.list` — routines and scripts as one collection, each
// entry carrying its phrases, its enabled switch, its markers, its schedule
// with the scheduler's own next-fire arithmetic, and its last observed run),
// the enable/disable verb (`automations.set_enabled`, the surgical config
// write shared with knowledge.set_enabled), and the last-run memory the
// listing serves.
//
// One unified method rather than per-family ones, on purpose (the issue left
// the choice to the implementer): the tab is one collection, both families
// flip the same `enabled` key through the same editor under the same
// discipline, and a `kind` field costs less than two verbs that could drift.
// Running stays on `routines.run` / `scripts.run` untouched — enabling is not
// an execution path (no gate beyond config-write authority); running always
// is (ADR 0013, ADR 0030).

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/automation"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/script"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/tools"
)

// automationRun is one observed ending of a routine or script: the facts the
// bus event carried, worded once, daemon-side. Records live in memory only
// and die with the daemon — an entry that has not run since boot has none,
// and the tab shows nothing rather than fabricating (the activity ring's
// honesty, ADR 0029).
type automationRun struct {
	at         time.Time
	outcome    string
	failed     bool
	durationMS int64
	hasDur     bool
}

// automationRunKey addresses one entry's record: kind plus the
// case-insensitive name every family already uses for uniqueness.
func automationRunKey(kind automation.Kind, name string) string {
	return string(kind) + "\x00" + strings.ToLower(strings.TrimSpace(name))
}

// recordAutomationRun retains the ending of a routine or script run, from
// the same bus subscription that feeds the activity ring. Any other event
// type is ignored.
func (d *Daemon) recordAutomationRun(ev session.Event) {
	var kind automation.Kind
	var name string
	run := automationRun{at: time.Now()}
	switch ev.Type {
	case "routine.finished":
		kind = automation.KindRoutine
		name, _ = ev.Data["routine"].(string)
		run.outcome, run.failed = routineRunOutcome(ev.Data)
	case "script.finished":
		kind = automation.KindScript
		name, _ = ev.Data["script"].(string)
		run.outcome, run.failed = scriptRunOutcome(ev.Data)
	default:
		return
	}
	if name == "" {
		return
	}
	if ms, ok := activityEventInt(ev.Data, "duration_ms"); ok {
		run.durationMS, run.hasDur = ms, true
	}
	d.actMu.Lock()
	if d.lastRuns == nil {
		d.lastRuns = make(map[string]automationRun)
	}
	d.lastRuns[automationRunKey(kind, name)] = run
	d.actMu.Unlock()
}

// lastAutomationRun reads one entry's record; false means it has not run
// under this daemon.
func (d *Daemon) lastAutomationRun(kind automation.Kind, name string) (automationRun, bool) {
	d.actMu.Lock()
	defer d.actMu.Unlock()
	run, ok := d.lastRuns[automationRunKey(kind, name)]
	return run, ok
}

// routineRunOutcome words a routine.finished payload for the last-run line:
// what landed and what failed, in the counts the event carries.
func routineRunOutcome(data map[string]any) (outcome string, failedRun bool) {
	if errText, _ := data["error"].(string); errText != "" {
		return "failed — " + errText, true
	}
	placed, _ := activityEventInt(data, "placed")
	failed, _ := activityEventInt(data, "failed")
	if failed > 0 {
		return fmt.Sprintf("%d placed · %d failed", placed, failed), true
	}
	return fmt.Sprintf("%d placed", placed), false
}

// scriptRunOutcome words a script.finished payload for the last-run line:
// the exit status, always — the same stance as the feed's row, where only
// failures carrying the code would make success mean trusting silence.
func scriptRunOutcome(data map[string]any) (outcome string, failedRun bool) {
	status, _ := data["status"].(string)
	code, _ := activityEventInt(data, "exit_code")
	if status == "failed" {
		if timedOut, _ := data["timed_out"].(bool); timedOut {
			return "stopped at the timeout", true
		}
		return fmt.Sprintf("exit %d", code), true
	}
	return "exit 0", false
}

// activityEventInt reads an integer event field however the bus delivered it
// (int, int64 in-process; float64 after a JSON hop).
func activityEventInt(data map[string]any, key string) (int64, bool) {
	switch v := data[key].(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		return int64(v), true
	}
	return 0, false
}

// automationFamily maps the wire `kind` onto the config table-array it
// names. The closed vocabulary is the same one automations.schedules serves.
func automationFamily(kind string) (automation.Kind, string, error) {
	switch kind {
	case string(automation.KindRoutine):
		return automation.KindRoutine, "routines", nil
	case string(automation.KindScript):
		return automation.KindScript, "scripts", nil
	}
	return "", "", fmt.Errorf("kind %q is not an automation kind; use %q or %q",
		kind, automation.KindRoutine, automation.KindScript)
}

// registerAutomationAdminMethods adds the Automations tab's surface (#93):
// the unified listing and the persisted enable/disable switch.
func (d *Daemon) registerAutomationAdminMethods() {
	d.server.Handle("automations.list", func(json.RawMessage) (any, error) {
		return d.automationsListing()
	})

	// automations.set_enabled: park or resume one routine or script,
	// persisted. The write is the settings discipline end to end (ADR 0015),
	// shared verbatim with knowledge.set_enabled through setEntryEnabled:
	// fingerprint-checked, applied through the surgical entry editor,
	// validated as a whole configuration — which is where re-enabling an
	// entry whose phrase was taken meanwhile fails with the same collision
	// error a config load gives, never a half-enable — written atomically,
	// and picked up by the standard reload, which recompiles the grammar and
	// rebuilds the schedules.
	d.server.Handle("automations.set_enabled", func(params json.RawMessage) (any, error) {
		var p struct {
			Kind    string `json:"kind"`
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
			// Fingerprint is the config fingerprint from the client's
			// automations.list (or config.get). When present, a mismatch with
			// the file on disk fails the set — hand edits are never clobbered.
			Fingerprint string `json:"fingerprint"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "automations.set_enabled params: %v", err)
			}
		}
		_, family, err := automationFamily(p.Kind)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return d.setEntryEnabled(family, p.Name, p.Enabled, p.Fingerprint)
	})
}

// automationsListing composes the unified list. Everything a row shows is
// decided here (ADR 0013): the config tables are the entries, the scheduler
// contributes its own next-fire arithmetic for the entries it holds, the
// tier verdict says which schedules would be refused, the path recheck says
// which script files rotted since load, and the last-run memory contributes
// what this daemon has itself observed — absent otherwise, never fabricated.
func (d *Daemon) automationsListing() (any, error) {
	// The config file's fingerprint travels with the listing so a
	// set_enabled built from these rows can detect an external edit — the
	// config.get/config.set contract, restated for this surface.
	fp, err := config.FingerprintFile(d.paths.ConfigFile())
	if err != nil {
		return nil, ipc.Errorf(ipc.CodeInternalError, "read config: %v", err)
	}
	cfg := d.runningConfig()

	// The scheduler's view, keyed for the join: only entries it holds (that
	// is, enabled ones with a schedule) have next-fire times to contribute.
	held := make(map[string]automation.Status)
	for _, st := range d.automations.Status() {
		held[automationRunKey(st.Kind, st.Name)] = st
	}

	entries := make([]map[string]any, 0, len(cfg.Routines)+len(cfg.Scripts))
	add := func(kind automation.Kind, entry map[string]any, name, schedule string, announce bool) {
		if schedule != "" {
			entry["schedule"] = schedule
			entry["announce"] = announce
		}
		if st, ok := held[automationRunKey(kind, name)]; ok {
			entry["next_fire"] = st.NextFire.Format(time.RFC3339)
			entry["running"] = st.Running
			if !st.LastFired.IsZero() {
				entry["last_fired"] = st.LastFired.Format(time.RFC3339)
			}
			verdict, known := automationVerdict(cfg, d.registry.Policy(),
				automation.Entry{Kind: kind, Name: name})
			refuse := known && verdict.Decision != tools.PolicyAllow
			entry["would_refuse"] = refuse
			if refuse {
				entry["rule"] = verdict.Rule
			}
		}
		if run, ok := d.lastAutomationRun(kind, name); ok {
			last := map[string]any{
				"at":      run.at.Format(time.RFC3339),
				"outcome": run.outcome,
				"failed":  run.failed,
			}
			if run.hasDur {
				last["duration"] = desktop.FormatActivityDuration(int(run.durationMS))
				last["duration_ms"] = run.durationMS
			}
			entry["last_run"] = last
		}
		entries = append(entries, entry)
	}
	for _, r := range cfg.Routines {
		add(automation.KindRoutine, map[string]any{
			"kind":    string(automation.KindRoutine),
			"name":    r.Name,
			"phrases": r.Phrases,
			"enabled": r.IsEnabled(),
			"steps":   len(r.Steps),
			// The capture placeholder marker (#62), same fact routines.list
			// carries, so every surface can say "this one needs a hand".
			"incomplete": r.Incomplete(),
		}, r.Name, r.Schedule, r.Announce)
	}
	for _, s := range cfg.Scripts {
		entry := map[string]any{
			"kind":    string(automation.KindScript),
			"name":    s.Name,
			"phrases": s.Phrases,
			"enabled": s.IsEnabled(),
			// The path is listed on purpose (ADR 0030): it is exactly what
			// the gate's confirmation names.
			"path": s.Path,
		}
		// Load validation refused a bad path, so a problem here means the
		// file changed underneath the running daemon — the runner's own
		// recheck, surfaced before the phrase discovers it.
		if problem := script.PathProblem(s.Path); problem != "" {
			entry["path_problem"] = problem
		}
		add(automation.KindScript, entry, s.Name, s.Schedule, s.Announce)
	}
	return map[string]any{"fingerprint": fp, "automations": entries}, nil
}

// setEntryEnabled flips one [[family]] entry's `enabled` key, persisted —
// the one implementation behind knowledge.set_enabled and
// automations.set_enabled, so the settings discipline (ADR 0015) exists
// once: fingerprint-checked against external edits, applied through the
// surgical entry editor so comments and sibling entries survive
// byte-for-byte, validated as a whole configuration before anything lands,
// written atomically, and picked up by the same reload path a hand edit
// uses.
func (d *Daemon) setEntryEnabled(family, name string, enabled bool, fingerprint string) (map[string]any, error) {
	path := d.paths.ConfigFile()
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, ipc.Errorf(ipc.CodeInternalError, "read config: %v", err)
	}
	fp := config.FingerprintMissing
	if raw != nil {
		fp = config.Fingerprint(raw)
	}
	if fingerprint != "" && fingerprint != fp {
		return nil, &ipc.Error{
			Code: ipc.CodeConfigConflict,
			Message: "config.toml changed on disk since it was read; " +
				"the listing has been refreshed — try the switch again",
			Data: map[string]any{"fingerprint": fp},
		}
	}

	newRaw, err := config.SetEntryField(raw, family, name, "enabled", enabled)
	if err != nil {
		return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
	}
	fileCfg, err := config.ParseBytes(newRaw)
	if err != nil {
		return nil, ipc.Errorf(ipc.CodeInternalError, "rewrite config: %v", err)
	}
	fileCfg.Voices = fileCfg.InstalledVoices(d.paths)
	if err := fileCfg.Validate(); err != nil {
		// For a re-enable this is where a phrase collision surfaces (#93):
		// the validation compiles the real intent router against the entry
		// now enabled, and its error names both owners — the same actionable
		// message a config load gives. Nothing was written.
		return nil, &ipc.Error{
			Code:    ipc.CodeConfigInvalid,
			Message: "the change was rejected by validation; nothing was written",
			Data:    map[string]any{"problems": validationProblems(err)},
		}
	}
	if err := config.WriteFileAtomic(path, newRaw); err != nil {
		return nil, ipc.Errorf(ipc.CodeInternalError, "write config: %v", err)
	}

	applied, reason := d.applyRuntime(fileCfg)
	newFP := config.Fingerprint(newRaw)
	d.publishConfigChanged(newFP)
	result := map[string]any{
		"fingerprint": newFP,
		"applied":     applied,
	}
	if reason != "" {
		result["reason"] = reason
	}
	return result, nil
}
