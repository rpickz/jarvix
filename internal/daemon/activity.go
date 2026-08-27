package daemon

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/session"
)

// The activity feed (issue #70): what Jarvix is doing, kept instead of
// discarded. The bus already broadcasts nearly everything that happens; this
// file is the one subscriber that assembles those events into rendered rows
// (internal/desktop owns the wording), holds them in a bounded in-memory
// ring, and serves them two ways — `activity.get` for a window that just
// opened, and an `activity.row` push for one that is already watching. The
// window is display-only either way (ADR 0013): every sentence it shows was
// composed here, daemon-side, where it is tested.
//
// Nothing is persisted. Conversations are the durable record; activity is
// operational, and it dies with the daemon on purpose.

// activityEntry is one retained row: the rendered text plus the ordering and
// provenance the feed needs. seq is what lets a client reconcile the
// `activity.get` snapshot with rows that arrived live in the meantime —
// monotonically increasing for the daemon's lifetime, never reused even when
// the ring wraps or is cleared.
type activityEntry struct {
	seq       uint64
	ts        time.Time
	sessionID string
	row       desktop.ActivityRow
}

// watchActivity is the feed's only writer: one bus subscriber, exactly like
// the notification watcher — the engine neither knows nor waits for it, and
// if it falls behind the bus drops events for it rather than wedging a
// session. Dropped events mean missing rows, which `activity.get` cannot
// invent; the feed is honest observation, not a transaction log.
func (d *Daemon) watchActivity(ctx context.Context, events <-chan session.Event, unsubscribe func()) {
	defer unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			// The feed's own echo maps to no rows (the vocabulary guarantees
			// it), so this subscriber cannot feed on its own output.
			if reason, _ := ev.Data["reason"].(string); ev.Type == "conversation.changed" &&
				reason == "reset" && d.activityClearOnNew() {
				d.clearActivity()
			}
			// The same subscription feeds the Automations tab's last-run
			// memory (#93): a routine or script ending is recorded before its
			// rendered row, so a list read racing the row push still agrees.
			d.recordAutomationRun(ev)
			// The replay row (issue #122) is worded beside its verb
			// (replay.go) rather than in the desktop vocabulary — same
			// daemon-side discipline, one special case here.
			if ev.Type == "speech.replayed" {
				d.appendActivity(ev, replayActivityRow(ev.Data))
			}
			for _, row := range desktop.ActivityRowsFor(ev.Type, ev.Data) {
				d.appendActivity(ev, row)
			}
		}
	}
}

// appendActivity retains one rendered row and pushes it to connected clients.
// The ring bound is read per append (ui.activity_rows is a live setting), so
// shrinking it takes effect on the next row, no restart.
func (d *Daemon) appendActivity(ev session.Event, row desktop.ActivityRow) {
	sessionID, _ := ev.Data["session_id"].(string)
	d.actMu.Lock()
	d.activitySeq++
	entry := activityEntry{
		seq:       d.activitySeq,
		ts:        time.Now(),
		sessionID: sessionID,
		row:       row,
	}
	d.activity = append(d.activity, entry)
	if limit := d.activityRowCap(); len(d.activity) > limit {
		// Copy rather than re-slice: a re-sliced ring pins every dropped
		// row's memory to the backing array for the daemon's life.
		kept := make([]activityEntry, limit)
		copy(kept, d.activity[len(d.activity)-limit:])
		d.activity = kept
	}
	d.actMu.Unlock()
	// Published outside the lock: Publish takes the bus's own lock, and a
	// row push must never hold the feed's while it waits there.
	d.bus.Publish(session.Event{Type: "activity.row", Data: activityEntryData(entry)})
}

// clearActivity empties the ring. The sequence counter keeps counting, so a
// client holding pre-clear rows cannot mistake post-clear ones for
// duplicates.
func (d *Daemon) clearActivity() {
	d.actMu.Lock()
	defer d.actMu.Unlock()
	d.activity = nil
}

// activityRowCap reads the live ui.activity_rows bound. Validation keeps the
// configured value positive; the floor here is belt and braces so a bad
// value can never make every append drop everything.
func (d *Daemon) activityRowCap() int {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	if d.cfg.UI.ActivityRows < 1 {
		return 1
	}
	return d.cfg.UI.ActivityRows
}

// activityClearOnNew reads the live ui.activity_clear_on_new switch.
func (d *Daemon) activityClearOnNew() bool {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	return d.cfg.UI.ActivityClearOnNew
}

// activityEntryData renders one entry for the wire — the same shape in the
// `activity.get` snapshot and the `activity.row` push, so a client renders
// both with one code path and reconciles them by seq.
func activityEntryData(e activityEntry) map[string]any {
	return map[string]any{
		"seq":        e.seq,
		"ts":         e.ts.Format(time.RFC3339),
		"session_id": e.sessionID,
		"kind":       e.row.Kind,
		"label":      e.row.Label,
		"detail":     e.row.Detail,
		"failed":     e.row.Failed,
	}
}

// registerActivityMethods adds the feed's IPC surface.
func (d *Daemon) registerActivityMethods() {
	// activity.get returns the ring, oldest first, plus the live bound so a
	// client can trim its own model to match. It is the reconciliation path
	// the slow-client rule requires: pushes may have been dropped, the
	// snapshot is what the daemon actually holds.
	d.server.Handle("activity.get", func(json.RawMessage) (any, error) {
		d.actMu.Lock()
		entries := make([]activityEntry, len(d.activity))
		copy(entries, d.activity)
		d.actMu.Unlock()
		rows := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			rows = append(rows, activityEntryData(e))
		}
		return map[string]any{"rows": rows, "limit": d.activityRowCap()}, nil
	})
}
