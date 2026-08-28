package daemon

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/monitors"
	"github.com/rpickz/jarvix/internal/placement"
)

// registerMonitorMethods adds the screen-name surface (#180, ADR 0057):
// `monitors.list` for the window's picker and the CLI — the outputs that are
// plugged in, with their sizes and any name the user gave them, plus every
// stored name including those whose screen is currently absent — and
// `monitors.name` / `monitors.repoint` / `monitors.forget` to edit them.
//
// All four are ungated, like `windows.name`: naming a screen changes nothing
// on screen, the opposite act undoes it, and the collision matrix lives in
// the one assignment seam, so these verbs cannot be a way around it.
//
// A name whose screen is unplugged is served as `present: false` rather than
// omitted. That is the whole difference between a UI a user can fix and one
// that appears to have lost their nickname: a dock in a bag must look like a
// dock in a bag.
func (d *Daemon) registerMonitorMethods() {
	d.server.Handle("monitors.list", func(json.RawMessage) (any, error) {
		if d.windows == nil {
			return nil, ipc.Errorf(ipc.CodeSessionError,
				"the window tools are switched off on this daemon (tools.desktop)")
		}
		screens, names, err := d.windows.MonitorListings(context.Background())
		if err != nil {
			d.log.Warn("monitors.list failed", "component", "daemon", "error", err.Error())
			return nil, ipc.Errorf(ipc.CodeSessionError,
				"the window manager is not available, so the screens cannot be listed")
		}
		count, max := d.screens.Count()
		return map[string]any{
			"monitors":  screens,
			"nicknames": names,
			"path":      d.windows.MonitorNicknamePath(),
			"count":     count,
			"max":       max,
			// The words a nickname may not take, served rather than restated
			// in the window: the vocabulary owns them (ADR 0056) and a second
			// copy in QML would be a copy that drifts.
			"reserved": placement.ReservedMonitorWords(),
			// The reference that means "wherever I am", so the picker can
			// offer it without inventing the spelling.
			"current": string(placement.MonitorCurrent),
		}, nil
	})

	d.server.Handle("monitors.name", d.monitorWrite("monitors.name",
		func(name, connector string) (string, error) {
			return d.windows.AssignMonitorNickname(context.Background(), name, connector)
		}))
	d.server.Handle("monitors.repoint", d.monitorWrite("monitors.repoint",
		func(name, connector string) (string, error) {
			return d.windows.RepointMonitorNickname(context.Background(), name, connector)
		}))
	d.server.Handle("monitors.forget", d.monitorWrite("monitors.forget",
		func(name, _ string) (string, error) {
			return d.windows.ForgetMonitorNickname(context.Background(), name)
		}))
}

// monitorWrite is the shared shape of the three write verbs: the same params,
// the same emptiness check, and the same translation of a refusal into a
// field-keyed problem the window's form can pin to a control.
//
// One function rather than three copies because the refusal mapping is the
// part that must not drift: a form that shows "top is already the name of …"
// under the connector field instead of the name field is a form the user
// cannot act on.
func (d *Daemon) monitorWrite(verb string, act func(name, connector string) (string, error)) func(json.RawMessage) (any, error) {
	return func(params json.RawMessage) (any, error) {
		if d.windows == nil {
			return nil, ipc.Errorf(ipc.CodeSessionError,
				"the window tools are switched off on this daemon (tools.desktop)")
		}
		var p struct {
			// Name is the screen name; Connector is which output it points
			// at, "" meaning the screen holding focus.
			Name      string `json:"name"`
			Connector string `json:"connector"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "%s params: %v", verb, err)
			}
		}
		if p.Name == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%s needs a name", verb)
		}
		spoken, err := act(p.Name, p.Connector)
		if err != nil {
			return nil, monitorWriteError(err)
		}
		return map[string]any{"spoken": spoken}, nil
	}
}

// monitorWriteError renders a refusal for the wire. A field-keyed Refusal
// becomes the same `-32001` + `problems` shape every other form on this
// surface uses (entryProblem), so the window pins it to the right control
// without a second wording; anything else is a session error carrying the
// seam's own sentence.
func monitorWriteError(err error) error {
	var refusal *monitors.Refusal
	if errors.As(err, &refusal) {
		return &ipc.Error{Code: ipc.CodeConfigInvalid, Message: refusal.Error(),
			Data: map[string]any{"problems": []entryProblem{
				{Field: refusal.Problem.Field, Message: refusal.Problem.Message},
			}}}
	}
	return ipc.Errorf(ipc.CodeSessionError, "%v", err)
}
