package daemon

import (
	"encoding/json"
	"strings"

	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/script"
)

// registerScriptMethods adds the script surface (ADR 0030): `scripts.list`
// for the CLI and the window's Automations tab, and `scripts.run` to
// trigger one by name from either.
//
// scripts.run is deliberately not a private execution path. It starts a
// session and submits the script's first phrase, exactly as if the user had
// spoken it — so the intent router, the script.run permission gate and its
// ask-by-default confirmation, the already-running refusal, and the spoken
// outcome all apply identically however a script is triggered. A second way
// to run a script that skipped the gate would be a hole in it, and for
// scripts the gate is the design's first control.
func (d *Daemon) registerScriptMethods() {
	d.server.Handle("scripts.list", func(json.RawMessage) (any, error) {
		d.cfgMu.Lock()
		scripts := d.cfg.Scripts
		d.cfgMu.Unlock()
		out := make([]map[string]any, 0, len(scripts))
		for _, s := range scripts {
			report := s.Report
			if report == "" {
				report = string(script.ReportSummary)
			}
			timeout := s.TimeoutSec
			if timeout == 0 {
				timeout = int(script.DefaultTimeout.Seconds())
			}
			out = append(out, map[string]any{
				"name":    s.Name,
				"phrases": s.Phrases,
				// The path is listed on purpose: it is exactly what the gate's
				// confirmation names, and a listing that hid it would make a
				// substituted file harder to notice, not easier.
				"path":        s.Path,
				"report":      report,
				"timeout_sec": timeout,
				// The enabled switch (#93): a parked script still lists —
				// disabled means switched off, never hidden.
				"enabled": s.IsEnabled(),
			})
		}
		return map[string]any{"scripts": out}, nil
	})

	d.server.Handle("scripts.run", func(params json.RawMessage) (any, error) {
		var p struct {
			Name string `json:"name"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "scripts.run params: %v", err)
			}
		}
		phrase, ok := d.scriptPhrase(p.Name)
		if !ok {
			// A disabled script (#93) is refused by name: its phrases are out
			// of the intent grammar, so submitting one would fall through to
			// the model — the opposite of running the script — and a run
			// surface must say "switched off", not "unknown".
			if d.scriptDisabled(p.Name) {
				return nil, ipc.Errorf(ipc.CodeInvalidParams,
					"script %q is disabled; enable it in the Automations tab or set enabled = true in config.toml", p.Name)
			}
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"no script is called %q; `jarvix scripts` lists them", p.Name)
		}
		id, err := d.engine.StartSession()
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeSessionError, "%v", err)
		}
		if err := d.engine.Submit(phrase); err != nil {
			return nil, ipc.Errorf(ipc.CodeSessionError, "%v", err)
		}
		return map[string]string{"session_id": id}, nil
	})
}

// scriptPhrase resolves a script name (case-insensitively) to its first
// trigger phrase — the utterance scripts.run replays through the ordinary
// session path. A disabled script resolves to nothing: its phrases are out
// of the grammar, so there is no utterance that runs it (#93).
func (d *Daemon) scriptPhrase(name string) (string, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	for _, s := range d.cfg.Scripts {
		if strings.ToLower(strings.TrimSpace(s.Name)) == want && len(s.Phrases) > 0 && s.IsEnabled() {
			return s.Phrases[0], true
		}
	}
	return "", false
}

// scriptDisabled reports whether name exists but is switched off — the fact
// that turns scripts.run's "unknown" into the actionable "disabled".
func (d *Daemon) scriptDisabled(name string) bool {
	want := strings.ToLower(strings.TrimSpace(name))
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	for _, s := range d.cfg.Scripts {
		if strings.ToLower(strings.TrimSpace(s.Name)) == want {
			return !s.IsEnabled()
		}
	}
	return false
}
