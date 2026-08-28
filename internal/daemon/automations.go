package daemon

// This file wires scheduled automations (ADR 0032) into jarvixd: the config →
// entry conversion, the fire path, the load-time warning for schedules that
// cannot run unattended, the automations.schedules IPC method (the future
// Automations tab's read surface, next-fire times computed here, daemon-side),
// and the shutdown drain adapters.
//
// The fire path is deliberately not a private execution path. An allow-tier
// clockfire starts a session and submits the entry's first trigger phrase,
// exactly as routines.run and scripts.run do — so the intent router, the
// permission gate, the already-running refusal, and the events all apply
// identically however an automation is triggered. The two differences are the
// clock's, not the entry's: the session refuses rather than interrupts when
// one is active, and it is quiet unless the entry says announce = true.
//
// The tier check happens *before* any session exists, and that ordering is
// the ADR's central decision: an ask-tier entry's confirmation question has
// nobody to hear it at the scheduled moment, so the firing is refused and
// reported — an activity row plus a notification whose click opens the window
// — and never executed. Only an entry whose effective tier is allow runs
// unattended.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/automation"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/tools"
)

// newAutomationService builds the scheduler. Unlike the knowledge service it
// is always built — zero schedules cost zero goroutines and register nothing
// model-facing — so the first `schedule` key a user writes lands on an idle
// reload, no restart.
func (d *Daemon) newAutomationService(cfg config.Config) *automation.Service {
	entries := cfg.AutomationEntries()
	svc := automation.NewService(d.paths.AutomationsFile(), automation.Options{
		Gate:    d.stateGate,
		Entries: entries,
		Fire:    d.fireAutomation,
		Publish: func(event string, data map[string]any) {
			d.bus.Publish(session.Event{Type: event, Data: data})
		},
	}, d.log)
	if len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name)
		}
		d.log.Info("automation schedules enabled", "component", "automation",
			"schedules", strings.Join(names, ","), "trail", d.paths.AutomationsFile())
	}
	d.warnUnattendableSchedules(cfg, entries)
	return svc
}

// warnUnattendableSchedules says at load — once per entry, at Warn — which
// schedules will be refused at their moment, so the user learns before the
// missed firing, not after (ADR 0032). The scheduled moment still produces
// the refusal notification; this is the earlier, cheaper lesson.
func (d *Daemon) warnUnattendableSchedules(cfg config.Config, entries []automation.Entry) {
	for _, e := range entries {
		verdict, ok := automationVerdict(cfg, d.registry.Policy(), e)
		if !ok || verdict.Decision == tools.PolicyAllow {
			continue
		}
		identity := tools.RoutineToolName
		if e.Kind == automation.KindScript {
			identity = tools.ScriptToolName
		}
		d.log.Warn("scheduled entry cannot run unattended; its firings will be refused with a notification",
			"component", "automation", "kind", string(e.Kind), "name", e.Name,
			"schedule", e.Schedule.String(), "rule", verdict.Rule,
			"fix", fmt.Sprintf(`set [tools.policy.tool].%q = "allow"`, identity))
	}
}

// automationVerdict resolves one entry's effective tier — the same
// DecideRoutine / DecideScript the session gate consults, so the clock and
// the voice can never disagree about what is permitted. false means the entry
// no longer exists in cfg (the tables changed under a firing in flight).
func automationVerdict(cfg config.Config, policy *tools.Policy, e automation.Entry) (tools.Verdict, bool) {
	want := strings.ToLower(strings.TrimSpace(e.Name))
	switch e.Kind {
	case automation.KindRoutine:
		for _, r := range cfg.Routines {
			if strings.ToLower(strings.TrimSpace(r.Name)) == want {
				return policy.DecideRoutine(r.Name), true
			}
		}
	case automation.KindScript:
		for _, s := range cfg.Scripts {
			if strings.ToLower(strings.TrimSpace(s.Name)) == want {
				return policy.DecideScript(s.Name, s.Path), true
			}
		}
	}
	return tools.Verdict{}, false
}

// fireAutomation is the scheduler's Fire callback: one clockfire, blocking
// until the run has finished — that duration is exactly what the scheduler's
// overlap skip measures. It runs on a goroutine the scheduler tracks, so
// shutdown drains it like everything else.
func (d *Daemon) fireAutomation(ctx context.Context, e automation.Entry) {
	cfg := d.runningConfig()
	verdict, known := automationVerdict(cfg, d.registry.Policy(), e)
	if !known {
		// The tables changed under a firing already dispatched; the reload
		// has rebuilt the schedules, so this generation's entry just ends.
		d.log.Warn("scheduled entry vanished before its firing ran",
			"component", "automation", "kind", string(e.Kind), "name", e.Name)
		return
	}
	if verdict.Decision != tools.PolicyAllow {
		d.refuseAutomation(ctx, e, verdict)
		return
	}

	phrase, ok := d.automationPhrase(e)
	if !ok {
		d.log.Warn("scheduled entry has no trigger phrase; nothing to submit",
			"component", "automation", "kind", string(e.Kind), "name", e.Name)
		return
	}
	// Subscribe before starting: the session's finish must not be able to
	// outrun the subscription.
	events, unsubscribe := d.bus.Subscribe()
	defer unsubscribe()
	id, err := d.engine.StartScheduledSession(e.Announce)
	if err != nil {
		// A conversation is active (or the engine is stopping): the clock
		// yields — a clockfire must never interrupt a person — and the yield
		// is reported, never silent.
		d.log.Info("scheduled firing skipped", "component", "automation",
			"kind", string(e.Kind), "name", e.Name, "reason", err.Error())
		d.bus.Publish(session.Event{Type: "automation.skipped", Data: map[string]any{
			"kind": string(e.Kind), "name": e.Name, "schedule": e.Schedule.String(),
			"reason": err.Error(),
		}})
		return
	}
	if err := d.engine.Submit(phrase); err != nil {
		d.log.Warn("scheduled firing could not submit its phrase",
			"component", "automation", "kind", string(e.Kind), "name", e.Name,
			"error", err.Error())
		return
	}
	// Wait for the session to end — finished or cancelled, whichever; "stop"
	// aborting a schedule-fired run is the session path's ordinary cancel.
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

// refuseAutomation reports an ask- or deny-tier firing: an activity row over
// the bus, and a notification riding the existing channel — clicking it opens
// the conversation window per the standing default action, where the user can
// run the entry themself.
func (d *Daemon) refuseAutomation(ctx context.Context, e automation.Entry, verdict tools.Verdict) {
	reason := "it needs your confirmation and a schedule cannot ask"
	if verdict.Decision == tools.PolicyDeny {
		reason = "it is denied by your policy"
	}
	d.log.Info("scheduled firing refused; nothing ran", "component", "automation",
		"kind", string(e.Kind), "name", e.Name, "rule", verdict.Rule)
	d.bus.Publish(session.Event{Type: "automation.refused", Data: map[string]any{
		"kind": string(e.Kind), "name": e.Name, "schedule": e.Schedule.String(),
		"reason": reason, "rule": verdict.Rule,
	}})
	if !d.notificationsEnabled() {
		return
	}
	n := desktop.Notification{
		Summary: "Scheduled run needs you",
		Body:    fmt.Sprintf("%s was scheduled but needs your confirmation — run it now?", e.Name),
		Actions: []desktop.Action{{ID: desktop.DefaultActionID, Label: "Open"}},
	}
	if verdict.Decision == tools.PolicyDeny {
		n.Summary = "Scheduled run refused"
		n.Body = fmt.Sprintf("%s was scheduled but your policy denies it.", e.Name)
	}
	// Dispatched like the session watcher's notifications: Send blocks until
	// clicked or dismissed, and an unclicked notification must not hold this
	// clockfire — or the next firing's overlap arithmetic — hostage.
	d.post.Go(func() { d.deliver(ctx, n) })
}

// automationPhrase resolves an entry to the first trigger phrase of its
// routine or script — the utterance the clockfire replays through the
// ordinary session path, exactly as routines.run / scripts.run do.
func (d *Daemon) automationPhrase(e automation.Entry) (string, bool) {
	if e.Kind == automation.KindScript {
		return d.scriptPhrase(e.Name)
	}
	return d.routinePhrase(e.Name)
}

// automationsDrain and automationsInFlight adapt the service to the shutdown
// stage table.
func (d *Daemon) automationsDrain(ctx context.Context) error {
	return d.automations.Drain(ctx)
}

func (d *Daemon) automationsInFlight() int {
	return d.automations.InFlight()
}

// registerAutomationMethods adds the schedule listing (ADR 0032):
// `automations.schedules`, the read surface the Automations tab (#93) renders.
// Next-fire times are computed daemon-side against the scheduler's own clock,
// and `would_refuse` carries the tier verdict so the tab can show "needs
// allow" before the user learns it from a 2am notification.
func (d *Daemon) registerAutomationMethods() {
	d.server.Handle("automations.schedules", func(json.RawMessage) (any, error) {
		cfg := d.runningConfig()
		statuses := d.automations.Status()
		schedules := make([]map[string]any, 0, len(statuses))
		for _, st := range statuses {
			entry := map[string]any{
				"kind":      string(st.Kind),
				"name":      st.Name,
				"schedule":  st.Schedule,
				"announce":  st.Announce,
				"next_fire": st.NextFire.Format(time.RFC3339),
				"running":   st.Running,
			}
			if !st.LastFired.IsZero() {
				entry["last_fired"] = st.LastFired.Format(time.RFC3339)
			}
			verdict, ok := automationVerdict(cfg, d.registry.Policy(),
				automation.Entry{Kind: st.Kind, Name: st.Name})
			refuse := ok && verdict.Decision != tools.PolicyAllow
			entry["would_refuse"] = refuse
			if refuse {
				entry["rule"] = verdict.Rule
			}
			schedules = append(schedules, entry)
		}
		return map[string]any{"schedules": schedules}, nil
	})
}
