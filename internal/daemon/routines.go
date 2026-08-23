package daemon

import (
	"encoding/json"
	"strings"

	"github.com/rpickz/jarvix/internal/ipc"
)

// registerRoutineMethods adds the routine surface (ADR 0026): `routines.list`
// for the CLI and the window's Automations tab, and `routines.run` to
// trigger one by name from either.
//
// routines.run is deliberately not a private execution path. It starts a
// session and submits the routine's first phrase, exactly as if the user had
// spoken it — so the intent router, the routine.run permission gate, the
// already-running refusal, the spoken summary, and the bus events all apply
// identically however a routine is triggered. A second way to run a routine
// that skipped the gate would be a hole in it.
func (d *Daemon) registerRoutineMethods() {
	d.server.Handle("routines.list", func(json.RawMessage) (any, error) {
		d.cfgMu.Lock()
		routines := d.cfg.Routines
		d.cfgMu.Unlock()
		out := make([]map[string]any, 0, len(routines))
		for _, r := range routines {
			out = append(out, map[string]any{
				"name":    r.Name,
				"phrases": r.Phrases,
				"steps":   len(r.Steps),
				// A capture that could not derive every launch command (#62)
				// leaves a placeholder; the listing carries the mark so every
				// surface can say "this one still needs a hand".
				"incomplete": r.Incomplete(),
				// The enabled switch (#93): a parked routine still lists —
				// disabled means switched off, never hidden.
				"enabled": r.IsEnabled(),
			})
		}
		return map[string]any{"routines": out}, nil
	})

	d.server.Handle("routines.run", func(params json.RawMessage) (any, error) {
		var p struct {
			Name string `json:"name"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "routines.run params: %v", err)
			}
		}
		phrase, ok := d.routinePhrase(p.Name)
		if !ok {
			// A disabled routine (#93) is refused by name: its phrases are out
			// of the intent grammar, so submitting one would fall through to
			// the model — the opposite of running the routine — and a run
			// surface must say "switched off", not "unknown".
			if d.routineDisabled(p.Name) {
				return nil, ipc.Errorf(ipc.CodeInvalidParams,
					"routine %q is disabled; enable it in the Automations tab or set enabled = true in config.toml", p.Name)
			}
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"no routine is called %q; `jarvix routines` lists them", p.Name)
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

// routinePhrase resolves a routine name (case-insensitively) to its first
// trigger phrase — the utterance routines.run replays through the ordinary
// session path. A disabled routine resolves to nothing: its phrases are out
// of the grammar, so there is no utterance that runs it (#93).
func (d *Daemon) routinePhrase(name string) (string, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	for _, r := range d.cfg.Routines {
		if strings.ToLower(strings.TrimSpace(r.Name)) == want && len(r.Phrases) > 0 && r.IsEnabled() {
			return r.Phrases[0], true
		}
	}
	return "", false
}

// routineDisabled reports whether name exists but is switched off — the fact
// that turns routines.run's "unknown" into the actionable "disabled".
func (d *Daemon) routineDisabled(name string) bool {
	want := strings.ToLower(strings.TrimSpace(name))
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	for _, r := range d.cfg.Routines {
		if strings.ToLower(strings.TrimSpace(r.Name)) == want {
			return !r.IsEnabled()
		}
	}
	return false
}
