package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/session"
)

// notificationPreviewLimit caps how much of an answer the notification body
// shows. Roughly one line of a desktop notification; the full text lives in
// the conversation window the click opens.
const notificationPreviewLimit = 80

// watchSessions turns completed sessions into desktop notifications. It is
// just another bus subscriber — the same feed the overlay and CLI follow —
// so the engine neither knows nor waits for notification delivery, and a
// stalled notifier can at worst drop events like any slow client.
//
// It accumulates the outcome of the session in flight (final answer, or
// failing stage + message) and dispatches on session.finished, which the bus
// guarantees is a session's last event. Cancellations notify nothing: the
// user cancelled it themself.
func (d *Daemon) watchSessions(ctx context.Context, events <-chan session.Event, unsubscribe func()) {
	defer unsubscribe()
	var answer, errStage, errMessage string
	// nothingHeard marks a session whose capture produced no words at all
	// (issue #191). It suppresses the finish notification, for the reason
	// cancellations are already silent: the user is standing at the keyboard
	// having just pressed a key, the overlay has already said "I didn't catch
	// that", and a desktop notification per accidental tap or quiet room
	// would be the noisiest thing Jarvix does.
	var nothingHeard bool
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			switch ev.Type {
			case "state.changed":
				// A session becoming active makes any previous failure
				// history — the same rule the window applies to its banner.
				if s, _ := ev.Data["state"].(string); s != "" && s != "idle" {
					d.setLastError("", "")
				}
			case "assistant.finished":
				answer, _ = ev.Data["content"].(string)
			case "intent.executed":
				// An intent turn has no assistant.finished; its acknowledgement
				// is the outcome. Retaining it gives the finish notification a
				// body — which for a quiet schedule-fired run (ADR 0032) is the
				// one place the summary lands besides the activity feed.
				if ack, _ := ev.Data["acknowledgement"].(string); ack != "" {
					answer = ack
				}
			case "error":
				errStage, _ = ev.Data["stage"].(string)
				errMessage, _ = ev.Data["message"].(string)
			case "session.nothing_heard":
				nothingHeard = true
			case "session.timings":
				// Retained past the session so `jarvix status --last` can
				// print the budget of the interaction that just happened.
				d.setLastTimings(ev.Data)
			case "typing.audit":
				// The typing audit trail (ADR 0023). Retained for the same
				// reason and answered by the same flag: "what did it just do
				// with my keyboard?" is asked after it happened, by which time
				// the event has already gone out on the bus. What is kept is
				// the window, the length and the outcome — the payload is not
				// in the event, so it cannot be in the trail.
				d.setLastTyping(ev.Data)
			case "session.finished":
				// Retain the failure past the session so a window opened by
				// clicking the error notification can still render it.
				d.setLastError(errStage, errMessage)
				// ui.notifications is a live setting: checked per session so
				// the switch acts immediately, no restart (settings.go).
				// "Jarvix answered" is a claim, and a turn that heard
				// nothing has nothing to claim. Silence here is the honest
				// report; the activity feed carries the reason.
				if d.notificationsEnabled() && !nothingHeard {
					n := d.buildNotification(answer, errStage, errMessage)
					// Send blocks until the notification is clicked, dismissed,
					// or expires; dispatch from its own goroutine so back-to-back
					// sessions never queue behind an unclicked notification.
					// Tracked so shutdown can wait for it: delivery outlives the
					// session that produced it, and it is holding a child process
					// (notify-send --wait) that a bare exit would orphan.
					d.post.Go(func() { d.deliver(ctx, n) })
				}
				// A layout capture (#62) may have written routines the
				// engine's router does not know; now that the session is
				// finished, the reload it could not do mid-session can run.
				if d.consumeCaptureReload() {
					d.post.Go(d.applyCapturedRoutines)
				}
				answer, errStage, errMessage, nothingHeard = "", "", "", false
			case "session.cancelled":
				answer, errStage, errMessage, nothingHeard = "", "", "", false
			}
		}
	}
}

// setLastError records (or clears, with empty arguments) the failure the
// conversation window should show until the next session starts.
func (d *Daemon) setLastError(stage, message string) {
	d.errMu.Lock()
	defer d.errMu.Unlock()
	d.lastErrStage, d.lastErrMessage = stage, message
}

// lastError reports the retained failure for conversation.get.
func (d *Daemon) lastError() (stage, message string) {
	d.errMu.Lock()
	defer d.errMu.Unlock()
	return d.lastErrStage, d.lastErrMessage
}

// setLastTimings records the latency report of the session that just ended.
// The map is copied because the event's own map is shared with every other
// bus subscriber.
func (d *Daemon) setLastTimings(data map[string]any) {
	copied := make(map[string]any, len(data))
	for k, v := range data {
		copied[k] = v
	}
	d.errMu.Lock()
	defer d.errMu.Unlock()
	d.lastTimings = copied
}

// lastTimingsReport returns the retained latency report for status.get, or nil
// when no session has finished since the daemon started.
func (d *Daemon) lastTimingsReport() map[string]any {
	d.errMu.Lock()
	defer d.errMu.Unlock()
	return d.lastTimings
}

// setLastTyping records the most recent typing decision. The map is copied
// because the event's own map is shared with every other bus subscriber.
func (d *Daemon) setLastTyping(data map[string]any) {
	copied := make(map[string]any, len(data))
	for k, v := range data {
		copied[k] = v
	}
	d.errMu.Lock()
	defer d.errMu.Unlock()
	d.lastTyping = copied
}

// lastTypingReport returns the retained typing audit for status.get, or nil
// when nothing has been typed since the daemon started.
func (d *Daemon) lastTypingReport() map[string]any {
	d.errMu.Lock()
	defer d.errMu.Unlock()
	return d.lastTyping
}

// buildNotification decides what a finished session's notification says,
// honouring the privacy switch: with ui.notification_preview = false, answer
// content never reaches the notification daemon — only the generic outcome
// (and, for failures, the failing stage, which is operational rather than
// conversational).
func (d *Daemon) buildNotification(answer, errStage, errMessage string) desktop.Notification {
	n := desktop.Notification{
		// The reserved "default" action makes the notification body itself
		// the click target: click anywhere → open the window.
		Actions: []desktop.Action{{ID: desktop.DefaultActionID, Label: "Open"}},
	}
	preview := d.previewEnabled()
	if errStage != "" || errMessage != "" {
		n.Summary = "Jarvix hit a problem"
		if preview {
			n.Body = fmt.Sprintf("Failed at %s: %s", errStage, errMessage)
		} else {
			n.Body = "Failed at " + errStage
		}
		return n
	}
	n.Summary = "Jarvix answered"
	if preview {
		n.Body = previewText(answer, notificationPreviewLimit)
	}
	return n
}

// deliver sends one notification and opens the conversation window when it
// is clicked. Delivery failure (no notification daemon, notify-send absent)
// degrades to a log line — and never one that contains the body, because the
// body is the assistant's answer.
func (d *Daemon) deliver(ctx context.Context, n desktop.Notification) {
	invoked, err := d.notifier.Send(ctx, n)
	if err != nil {
		d.log.Debug("notification not delivered", "component", "notify", "error", err.Error())
		return
	}
	if invoked != desktop.DefaultActionID {
		return
	}
	if err := d.openWindow(ctx); err != nil {
		d.log.Warn("notification clicked but the window did not open",
			"component", "notify", "error", err.Error())
	}
}

// previewText returns the first limit runes of s, ellipsised. Runes, not
// bytes: an answer must never be cut mid-character.
func previewText(s string, limit int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return strings.TrimSpace(string(r[:limit])) + "…"
}
