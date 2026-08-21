package daemon

// This file is the disclosure surface for desktop context (ADR 0019).
//
// Gathering what the user is looking at is only acceptable if they can always
// find out what was taken, so auditability is wired as a first-class IPC
// method rather than left to a log line: `context.last` answers "what did
// Jarvix just see?" with the exact text that reached the model — already
// truncated, already redacted — and `jarvix status --last` is a thin client
// of it.

import (
	"encoding/json"
	"time"
)

func (d *Daemon) registerContextMethods() {
	// The full capture, contents included. It is the user's own screen,
	// asked for over their own 0600 socket, and anything less than the real
	// text would leave "what did it send?" unanswerable.
	d.server.Handle("context.last", func(json.RawMessage) (any, error) {
		snap, sessionID, ok := d.engine.LastContext()
		if !ok {
			return map[string]any{"captured": false}, nil
		}
		sources := make([]map[string]any, 0, len(snap.Items))
		for _, item := range snap.Items {
			sources = append(sources, map[string]any{
				"source":    string(item.Source),
				"text":      item.Text,
				"chars":     item.Chars,
				"truncated": item.Truncated,
				"redacted":  item.Redacted,
			})
		}
		return map[string]any{
			"captured":    true,
			"session_id":  sessionID,
			"at":          snap.At.Format(time.RFC3339),
			"age_sec":     int(time.Since(snap.At).Seconds()),
			"duration_ms": snap.Elapsed.Milliseconds(),
			"sources":     sources,
		}, nil
	})
}
