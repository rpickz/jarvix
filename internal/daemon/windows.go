package daemon

import (
	"context"
	"encoding/json"
	"sync/atomic"

	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/ipc"
)

// routerHolder tracks the intent router currently in force, for the window
// nickname collision check (#126). The window tools live for the daemon's
// whole life — their nickname registry is session-scoped by design — while
// the router is rebuilt on every config reload, so the tools hold this
// indirection rather than a router that could go stale. Atomic because the
// assignment path reads it from a session goroutine while a reload stores it.
type routerHolder struct {
	p atomic.Pointer[intent.Router]
}

// set stores the router now in force; nil (intents disabled) is fine —
// Owner answers "unowned" on a nil router.
func (h *routerHolder) set(r *intent.Router) { h.p.Store(r) }

// owner is the tools.DesktopOptions.PhraseOwner seam.
func (h *routerHolder) owner(phrase string) (string, bool) {
	return h.p.Load().Owner(phrase)
}

// registerWindowMethods adds the window surface (#126): `windows.list` for
// the CLI (and any window-list UI) — the live inventory with nicknames —
// and `windows.name` to assign one from a listing, resolving the reference
// through the same seam every spoken window command uses. Addresses never
// travel (ADR 0022): a row is app, title, workspace, focus, nickname.
//
// windows.name is deliberately ungated, like the model's desktop.name_window
// (allow tier): naming changes nothing on screen and the opposite
// assignment undoes it. The collision, reserved-word, and single-word rules
// all live in the one assignment seam, so this verb cannot be a way around
// them.
func (d *Daemon) registerWindowMethods() {
	d.server.Handle("windows.list", func(json.RawMessage) (any, error) {
		if d.windows == nil {
			return nil, ipc.Errorf(ipc.CodeSessionError,
				"the window tools are switched off on this daemon (tools.desktop)")
		}
		listings, err := d.windows.WindowListings(context.Background())
		if err != nil {
			d.log.Warn("windows.list failed", "component", "daemon", "error", err.Error())
			return nil, ipc.Errorf(ipc.CodeSessionError,
				"the window manager is not available, so the windows cannot be listed")
		}
		return map[string]any{"windows": listings}, nil
	})

	d.server.Handle("windows.name", func(params json.RawMessage) (any, error) {
		if d.windows == nil {
			return nil, ipc.Errorf(ipc.CodeSessionError,
				"the window tools are switched off on this daemon (tools.desktop)")
		}
		var p struct {
			// Name is the nickname to assign; Window describes which window,
			// as a person would ("" or "this" means the focused one).
			Name   string `json:"name"`
			Window string `json:"window"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "windows.name params: %v", err)
			}
		}
		if p.Name == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "windows.name needs a name")
		}
		spoken, err := d.windows.AssignNickname(context.Background(), p.Window, p.Name)
		if err != nil {
			// The seam's refusals are spoken-ready and safe on the wire —
			// collision owners and reserved-word descriptions, never
			// addresses or compositor diagnostics.
			return nil, ipc.Errorf(ipc.CodeSessionError, "%v", err)
		}
		return map[string]any{"spoken": spoken}, nil
	})
}
