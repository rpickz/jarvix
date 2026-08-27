package daemon

// This file wires focus threads (#123, ADR 0041) into jarvixd: the service
// construction and its late binds, the firing path for check-in reminders
// and timebox moments, the focus.* IPC methods (the Focus tab's surface and
// the integration contract for the bar/overlay siblings), and the shutdown
// drain adapters.
//
// The firing path is deliberately not a private speech channel. A check-in
// or a timebox moment starts a scheduled session and submits an ordinary
// focus phrase — "where am i on <thread>", "focus session update" — exactly
// as ADR 0032's clockfires replay a routine's trigger, so the intent router,
// the events, the activity feed, and the conversation record all apply
// identically however the sentence was asked for. The do-not-nag rule falls
// out of the same reuse: StartScheduledSession refuses while any session is
// live or speech is playing, and a refused firing is dropped with a report —
// never queued, never retried into a backlog.

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/focus"
	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/session"
)

// focusWindowsTimeout bounds one inventory read for anchors: a wedged
// compositor must degrade an anchor, never hang a recap.
const focusWindowsTimeout = 3 * time.Second

// newFocusService builds the thread store over the shared compositor seam.
// Built before the daemon exists because the engine's intent runner carries
// it; the firing path and the midpoint switch bind after (bindFocus), the
// capture service's pattern.
func newFocusService(paths config.Paths, compositor desktop.Compositor, bus *session.Bus,
	logger *slog.Logger) *focus.Service {
	return focus.NewService(paths.FocusFile(), focus.Options{
		Windows: func(ctx context.Context) ([]desktop.Window, error) {
			ctx, cancel := context.WithTimeout(ctx, focusWindowsTimeout)
			defer cancel()
			return compositor.Windows(ctx)
		},
		Publish: func(event string, data map[string]any) {
			bus.Publish(session.Event{Type: event, Data: data})
		},
	}, logger)
}

// bindFocus completes the service once the daemon exists: the firing path,
// and the midpoint switch read from the running config at fire time so a
// reload lands without a restart.
func (d *Daemon) bindFocus() {
	d.focus.Bind(d.fireFocus, func() bool {
		return d.runningConfig().Focus.MidpointCheckin
	})
}

// fireFocus speaks one scheduled focus moment through the ordinary session
// path. It blocks until the spoken turn has finished — it runs on a
// goroutine the focus service tracks, so shutdown drains it like everything
// else — and a refusal is a skipped announcement with a report, never a
// backlog: the state the firing announces was already recorded by the
// service before it dispatched.
func (d *Daemon) fireFocus(ctx context.Context, f focus.Firing) {
	phrase, ok := focusPhrase(f)
	if !ok {
		// Only reachable for a hand-edited thread name too long for the
		// grammar: the router could not claim the phrase, and an unattended
		// firing must never fall through to the model.
		d.log.Warn("focus check-in skipped; the thread's name is too long for the phrase table — shorten it",
			"component", "focus", "thread", f.Thread.ID)
		return
	}
	// Subscribe before starting: the session's finish must not be able to
	// outrun the subscription.
	events, unsubscribe := d.bus.Subscribe()
	defer unsubscribe()
	id, err := d.engine.StartScheduledSession(true)
	if err != nil {
		// A conversation is active or speech is playing: the clock yields —
		// this is the do-not-nag rule doing its job — and the yield is
		// reported, never silent.
		d.log.Info("focus firing skipped", "component", "focus",
			"kind", string(f.Kind), "thread", f.Thread.ID, "reason", err.Error())
		d.bus.Publish(session.Event{Type: "focus.skipped", Data: map[string]any{
			"kind": string(f.Kind), "thread": f.Thread.ID, "reason": err.Error(),
		}})
		return
	}
	if err := d.engine.Submit(phrase); err != nil {
		d.log.Warn("focus firing could not submit its phrase",
			"component", "focus", "kind", string(f.Kind), "thread", f.Thread.ID,
			"error", err.Error())
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-events:
			if !open {
				return
			}
			if ev.Type != "session.finished" && ev.Type != "session.cancelled" {
				continue
			}
			if sid, _ := ev.Data["session_id"].(string); sid == id {
				return
			}
		}
	}
}

// focusPhrase renders one firing as the utterance the session replays — a
// sentence the user could equally have spoken, so the record reads the same
// whether the clock or the voice asked. false means the thread's name cannot
// ride the grammar (hand-edited past the name-word bound) and the firing
// must be skipped rather than reach the model.
func focusPhrase(f focus.Firing) (string, bool) {
	switch f.Kind {
	case focus.FiringReminder:
		name := strings.ToLower(strings.TrimSpace(f.Thread.Name))
		if n := len(strings.Fields(name)); n == 0 || n > intent.MaxFocusNameWords {
			return "", false
		}
		return "where am i on " + name, true
	default:
		// Midpoint and close both land on the tick phrase: the service's
		// session state decides which sentence it earns, so the clock and a
		// spoken "focus session update" can never disagree.
		return "focus session update", true
	}
}

// focusDrain and focusInFlight adapt the service to the shutdown stage table.
func (d *Daemon) focusDrain(ctx context.Context) error {
	return d.focus.Drain(ctx)
}

func (d *Daemon) focusInFlight() int {
	return d.focus.InFlight()
}

// registerFocusMethods adds the focus.* verbs: the Focus tab's whole surface,
// and the contract the bar/overlay siblings (#124, #127) integrate against.
// Reads are focus.list; every mutation returns the same spoken-style sentence
// the voice path earns, so a client that wants to show it can, and publishes
// focus.changed for the ones that don't.
func (d *Daemon) registerFocusMethods() {
	d.server.Handle("focus.list", func(json.RawMessage) (any, error) {
		return focusViewReport(d.focus.Snapshot(context.Background())), nil
	})

	d.server.Handle("focus.create", func(params json.RawMessage) (any, error) {
		p := struct {
			Name    string `json:"name"`
			Windows int    `json:"windows"`
		}{}
		if err := unmarshalFocusParams("focus.create", params, &p); err != nil {
			return nil, err
		}
		th, spoken, err := d.focus.Create(context.Background(), p.Name, p.Windows)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"thread": focusThreadID(th), "spoken": spoken}, nil
	})

	d.server.Handle("focus.switch", func(params json.RawMessage) (any, error) {
		p := struct {
			Thread string `json:"thread"`
		}{}
		if err := unmarshalFocusParams("focus.switch", params, &p); err != nil {
			return nil, err
		}
		th, recap, err := d.focus.Switch(context.Background(), p.Thread)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"thread": focusThreadID(th), "recap": recap}, nil
	})

	d.server.Handle("focus.park", func(params json.RawMessage) (any, error) {
		p := struct {
			Text string `json:"text"`
		}{}
		if err := unmarshalFocusParams("focus.park", params, &p); err != nil {
			return nil, err
		}
		spoken, err := d.focus.Park(p.Text)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"spoken": spoken}, nil
	})

	d.server.Handle("focus.end", func(params json.RawMessage) (any, error) {
		p := struct {
			Thread string `json:"thread"`
		}{}
		if err := unmarshalFocusParams("focus.end", params, &p); err != nil {
			return nil, err
		}
		spoken, err := d.focus.End(p.Thread)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"spoken": spoken}, nil
	})

	d.server.Handle("focus.session.start", func(params json.RawMessage) (any, error) {
		p := struct {
			Thread  string `json:"thread"`
			Minutes int    `json:"minutes"`
		}{}
		if err := unmarshalFocusParams("focus.session.start", params, &p); err != nil {
			return nil, err
		}
		spoken, err := d.focus.StartSession(context.Background(), p.Thread, p.Minutes)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"spoken": spoken}, nil
	})

	d.server.Handle("focus.session.end", func(json.RawMessage) (any, error) {
		spoken, err := d.focus.EndSession()
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"spoken": spoken}, nil
	})

	d.server.Handle("focus.remind", func(params json.RawMessage) (any, error) {
		p := struct {
			Thread  string `json:"thread"`
			Minutes int    `json:"minutes"`
		}{}
		if err := unmarshalFocusParams("focus.remind", params, &p); err != nil {
			return nil, err
		}
		// The verb reaches past the active thread when one is named — the
		// tab edits any row — by switching resolution, not by a second code
		// path: Remind acts on the active thread, so a named thread is made
		// active first only in the resolve sense, never by side effect.
		if strings.TrimSpace(p.Thread) != "" {
			spoken, err := d.focus.RemindThread(p.Thread, p.Minutes)
			if err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
			}
			return map[string]any{"spoken": spoken}, nil
		}
		spoken, err := d.focus.Remind(p.Minutes)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"spoken": spoken}, nil
	})
}

// unmarshalFocusParams reads one verb's params with the standard refusal.
func unmarshalFocusParams(verb string, params json.RawMessage, into any) error {
	if len(params) == 0 {
		return nil
	}
	if err := json.Unmarshal(params, into); err != nil {
		return ipc.Errorf(ipc.CodeInvalidParams, "%s params: %v", verb, err)
	}
	return nil
}

// focusViewReport renders the snapshot for the wire. Times are RFC 3339;
// ages arrive pre-worded (the shared spoken scale), so no client invents its
// own arithmetic (ADR 0013).
func focusViewReport(v focus.View) map[string]any {
	threads := make([]map[string]any, 0, len(v.Threads))
	for _, tv := range v.Threads {
		entry := map[string]any{
			"id":                   tv.ID,
			"name":                 tv.Name,
			"active":               tv.Active,
			"created":              tv.Created.Format(time.RFC3339),
			"last_activity":        tv.LastActivity.Format(time.RFC3339),
			"last_activity_spoken": tv.LastActivitySpoken,
			"parked_count":         len(tv.Parked),
		}
		if !tv.LastSwitched.IsZero() {
			entry["last_switched"] = tv.LastSwitched.Format(time.RFC3339)
		}
		if tv.RemindEveryMin > 0 {
			entry["remind_every_min"] = tv.RemindEveryMin
		}
		if len(tv.Anchors) > 0 {
			anchors := make([]map[string]any, 0, len(tv.Anchors))
			for i, a := range tv.Anchors {
				anchors = append(anchors, map[string]any{
					"app": a.App, "title": a.Title, "gone": tv.AnchorsGone[i],
				})
			}
			entry["anchors"] = anchors
		}
		if len(tv.Parked) > 0 {
			parked := make([]map[string]any, 0, len(tv.Parked))
			for _, pk := range tv.Parked {
				parked = append(parked, map[string]any{
					"id": pk.ID, "text": pk.Text, "at": pk.At.Format(time.RFC3339),
				})
			}
			entry["parked"] = parked
		}
		threads = append(threads, entry)
	}
	out := map[string]any{"threads": threads, "active": v.Active}
	if v.Session != nil {
		out["session"] = map[string]any{
			"thread":        v.Session.ThreadID,
			"thread_name":   v.Session.ThreadName,
			"started":       v.Session.Started.Format(time.RFC3339),
			"minutes":       v.Session.Minutes,
			"phase":         v.Session.Phase,
			"remaining_sec": v.Session.RemainingSec,
		}
	}
	return out
}

// focusThreadID reports one thread for a mutation reply.
func focusThreadID(th focus.Thread) map[string]any {
	return map[string]any{"id": th.ID, "name": th.Name}
}
