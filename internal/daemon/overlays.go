package daemon

// This file wires the window-overlay feed (#127) into jarvixd: the service
// construction over the shared seams, the bus watcher that keeps it prompt,
// the overlays.get verb, and the shutdown drain adapters.
//
// The feed composes three things the daemon already owns — the focus threads
// (#123), the nickname registry (#126), and the compositor inventory (ADR
// 0022) — into rows the QML overlay surface draws verbatim (ADR 0013). All
// of the deciding lives in internal/overlay, where it is tested against
// fakes; nothing here does more than hand seams across.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/focus"
	"github.com/rpickz/jarvix/internal/overlay"
	"github.com/rpickz/jarvix/internal/session"
)

// overlayWindowsTimeout bounds one inventory read for the feed: a wedged
// compositor must clear the overlays and try again next tick, never hang the
// poll loop (the focusWindowsTimeout stance).
const overlayWindowsTimeout = 3 * time.Second

// newOverlayService builds the feed over the daemon's seams. Everything is
// read per computation — the switch, the threads, the nicknames — so a
// settings change or a hand-edited focus store lands on the next tick or
// poke without a restart.
func (d *Daemon) newOverlayService() *overlay.Service {
	return overlay.NewService(overlay.Options{
		Windows: func(ctx context.Context) ([]desktop.Window, error) {
			ctx, cancel := context.WithTimeout(ctx, overlayWindowsTimeout)
			defer cancel()
			return d.compositor.Windows(ctx)
		},
		Threads: d.overlayThreads,
		Tags: func(windows []desktop.Window) map[string]string {
			// Nil when the window tools are off (tools.desktop): no registry
			// exists, so no window can have a tag — badges still work.
			if d.windows == nil {
				return nil
			}
			return d.windows.NicknamesByAddress(windows)
		},
		NicknamesHeld: func() bool {
			return d.windows != nil && d.windows.NicknameCount() > 0
		},
		Enabled: func() bool { return d.runningConfig().Overlays.Enabled },
		Publish: func(rows []overlay.Row) {
			d.bus.Publish(session.Event{Type: "overlays.changed",
				Data: map[string]any{"rows": overlayRowsPayload(rows)}})
		},
	}, d.log)
}

// overlayThreads adapts the focus snapshot to the feed's input shape: name,
// activity, anchor addresses — and the AI-session state.
//
// The AI state is the seam issue #137 fills. Its classifier will put a
// deterministic working / needs-you / done state on the focus payloads; when
// that field exists, overlayAIState reads it and the dot appears — the feed,
// the wire shape, and the QML already carry it end to end. Until then every
// thread reports "" and no dot renders anywhere, which is the issue's
// absent-means-absent rule, not a stub behaviour.
func (d *Daemon) overlayThreads(ctx context.Context) []overlay.Thread {
	view := d.focus.Snapshot(ctx)
	threads := make([]overlay.Thread, 0, len(view.Threads))
	for _, tv := range view.Threads {
		th := overlay.Thread{
			Name:    tv.Name,
			Active:  tv.Active,
			AIState: overlayAIState(tv),
		}
		for _, a := range tv.Anchors {
			th.Anchors = append(th.Anchors, a.Address)
		}
		threads = append(threads, th)
	}
	return threads
}

// overlayAIState is the one line #137 changes: return the thread view's
// classification once the focus payloads carry one. The vocabulary contract
// is overlay.StateWorking / StateNeedsYou / StateDone — anything else is
// dropped feed-side before the wire (overlay.Compose), so a mismatched
// vocabulary degrades to "no dot", never to a wrong colour.
func overlayAIState(focus.ThreadView) string {
	return ""
}

// registerOverlayMethods adds the overlays surface: one read. Clients attach
// mid-life, so overlays.changed alone would leave a fresh shell blank until
// the next change; overlays.get is the same snapshot-then-events shape as
// status.get and activity.get.
func (d *Daemon) registerOverlayMethods() {
	d.server.Handle("overlays.get", func(json.RawMessage) (any, error) {
		return map[string]any{
			"enabled": d.runningConfig().Overlays.Enabled,
			"rows":    overlayRowsPayload(d.overlays.Current(context.Background())),
		}, nil
	})
}

// overlayRowsPayload normalises for the wire: an empty feed is an empty
// array, never JSON null — "nothing overlaid" is a statement, and clients
// should not need a null guard to hear it.
func overlayRowsPayload(rows []overlay.Row) []overlay.Row {
	if rows == nil {
		return []overlay.Row{}
	}
	return rows
}

// watchOverlays pokes the feed on the bus events that can change what the
// overlays say without the geometry moving: a thread switch or anchor change
// (focus.changed), a nickname assignment (desktop.action), and a settings
// change (config.changed / config.setting_changed — the overlays.enabled
// switch, but also any reload). Poking rather than recomputing here keeps
// one computation path: the service coalesces however many events arrive.
// Everything else — moves, resizes, closed windows — is the poll's job.
func (d *Daemon) watchOverlays(ctx context.Context, events <-chan session.Event, unsubscribe func()) {
	defer unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-events:
			if !open {
				return
			}
			switch ev.Type {
			case "focus.changed", "desktop.action", "config.changed", "config.setting_changed":
				d.overlays.Poke()
			}
		}
	}
}

// overlaysDrain and overlaysInFlight adapt the service to the shutdown stage
// table.
func (d *Daemon) overlaysDrain(ctx context.Context) error {
	return d.overlays.Drain(ctx)
}

func (d *Daemon) overlaysInFlight() int {
	return d.overlays.InFlight()
}
