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
			case "error":
				errStage, _ = ev.Data["stage"].(string)
				errMessage, _ = ev.Data["message"].(string)
			case "session.timings":
				// Retained past the session so `jarvix status --last` can
				// print the budget of the interaction that just happened.
				d.setLastTimings(ev.Data)
			case "session.finished":
				// Retain the failure past the session so a window opened by
				// clicking the error notification can still render it.
				d.setLastError(errStage, errMessage)
				// ui.notifications is a live setting: checked per session so
				// the switch acts immediately, no restart (settings.go).
				if d.notificationsEnabled() {
					n := d.buildNotification(answer, errStage, errMessage)
					// Send blocks until the notification is clicked, dismissed,
					// or expires; dispatch from its own goroutine so back-to-back
					// sessions never queue behind an unclicked notification.
					go d.deliver(ctx, n)
				}
				answer, errStage, errMessage = "", "", ""
			case "session.cancelled":
				answer, errStage, errMessage = "", "", ""
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
