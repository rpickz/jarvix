package daemon

// This file wires one-shot reminders (#141, ADR 0046) into jarvixd: the
// service construction and its late bind, the delivery path, the session
// boundary watcher that releases deferred deliveries, the reminders.* IPC
// methods (the Automations tab's one-shot section), and the shutdown drain
// adapters.
//
// The delivery path is deliberately not a private speech channel. A
// reminder's moment starts a scheduled session and submits the ordinary
// check phrase — "reminder check" — exactly as ADR 0032's clockfires replay
// a routine's trigger and the focus clockwork replays "where am i on …", so
// the intent router, the events, the activity feed, and the conversation
// record all apply identically however the sentence was asked for. The
// difference from those siblings is the owed contract (ADR 0046): a refused
// start does not drop the announcement — the service parks the reminder and
// this file's boundary watcher releases it when the session that held the
// floor ends.

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/reminders"
	"github.com/rpickz/jarvix/internal/session"
)

// newRemindersService builds the reminder store. Built before the engine
// because the engine's intent runner carries it (the focus service's
// construction rule); the delivery path binds after (bindReminders).
func newRemindersService(paths config.Paths, bus *session.Bus, logger *slog.Logger) *reminders.Service {
	return reminders.NewService(paths.RemindersFile(), reminders.Options{
		Publish: func(event string, data map[string]any) {
			bus.Publish(session.Event{Type: event, Data: data})
		},
	}, logger)
}

// bindReminders completes the service once the daemon exists: the delivery
// path — the capture service's late-bind pattern, wired once before Start
// ever runs.
func (d *Daemon) bindReminders() {
	d.reminders.Bind(d.fireReminders)
}

// fireReminders is one delivery attempt: start a scheduled session, replay
// the check phrase, and block until the spoken turn has finished — it runs
// on a goroutine the reminder service tracks, so shutdown drains it like
// everything else. false reports a refused floor (a session is live or
// speech is playing); the service holds the owed reminders for the next
// boundary, which is the do-not-nag rule with the owed variant this feature
// promises — deferred, never lost, and the claim inside the session is what
// keeps it never doubled.
func (d *Daemon) fireReminders(ctx context.Context) bool {
	// Subscribe before starting: the session's finish must not be able to
	// outrun the subscription.
	events, unsubscribe := d.bus.Subscribe()
	defer unsubscribe()
	id, err := d.engine.StartScheduledSession(true)
	if err != nil {
		d.log.Info("reminder delivery deferred to the session boundary",
			"component", "reminders", "reason", err.Error())
		return false
	}
	if err := d.engine.Submit(intent.ReminderCheckPhrase); err != nil {
		d.log.Warn("reminder delivery could not submit its phrase",
			"component", "reminders", "error", err.Error())
		return false
	}
	for {
		select {
		case <-ctx.Done():
			return true
		case ev, open := <-events:
			if !open {
				return true
			}
			if ev.Type != "session.finished" && ev.Type != "session.cancelled" {
				continue
			}
			if sid, _ := ev.Data["session_id"].(string); sid == id {
				return true
			}
		}
	}
}

// watchReminderBoundaries releases deferred reminder deliveries at the one
// moment they become speakable: a session's end. One bus subscriber on the
// notification watcher's terms — the engine neither knows nor waits for it —
// and FlushOwed is a cheap no-op whenever nothing is parked.
func (d *Daemon) watchReminderBoundaries(ctx context.Context, events <-chan session.Event, unsubscribe func()) {
	defer unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Type == "session.finished" || ev.Type == "session.cancelled" {
				d.reminders.FlushOwed()
			}
		}
	}
}

// remindersDrain and remindersInFlight adapt the service to the shutdown
// stage table.
func (d *Daemon) remindersDrain(ctx context.Context) error {
	return d.reminders.Drain(ctx)
}

func (d *Daemon) remindersInFlight() int {
	return d.reminders.InFlight()
}

// registerReminderMethods adds the reminders.* verbs: the Automations tab's
// one-shot section. Reads are reminders.list; the one mutation a tab needs
// is cancel-by-id, and it returns the same spoken-style sentence the voice
// path earns, publishing reminders.changed for the surfaces that watch.
func (d *Daemon) registerReminderMethods() {
	d.server.Handle("reminders.list", func(json.RawMessage) (any, error) {
		return remindersViewReport(d.reminders.Snapshot()), nil
	})

	d.server.Handle("reminders.cancel", func(params json.RawMessage) (any, error) {
		p := struct {
			ID string `json:"id"`
		}{}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "reminders.cancel params: %v", err)
			}
		}
		spoken, err := d.reminders.Cancel(p.ID)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"spoken": spoken}, nil
	})
}

// remindersViewReport renders the snapshot for the wire. Times are RFC 3339;
// due moments arrive pre-worded (the shared spoken scale), so no client
// invents its own arithmetic (ADR 0013).
func remindersViewReport(v reminders.View) map[string]any {
	pending := make([]map[string]any, 0, len(v.Pending))
	for _, p := range v.Pending {
		pending = append(pending, map[string]any{
			"id":         p.ID,
			"text":       p.Text,
			"due":        p.Due.Format(time.RFC3339),
			"due_spoken": p.DueSpoken,
			"created":    p.Created.Format(time.RFC3339),
		})
	}
	history := make([]map[string]any, 0, len(v.History))
	for _, f := range v.History {
		entry := map[string]any{
			"id":      f.ID,
			"text":    f.Text,
			"due":     f.Due.Format(time.RFC3339),
			"at":      f.At.Format(time.RFC3339),
			"outcome": f.Outcome,
		}
		if f.Late {
			entry["late"] = true
		}
		history = append(history, entry)
	}
	return map[string]any{"reminders": pending, "history": history}
}
