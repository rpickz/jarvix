package session

import (
	"context"

	"github.com/rpickz/jarvix/internal/desktop"
)

// This file is the engine half of desktop context (ADR 0019).
// internal/desktop owns the gathering — which sources, which subprocesses,
// what redaction — while the engine owns three decisions that are only
// answerable here:
//
//  1. *When* context is gathered. Not at session start, but inside think(),
//     on the one path that actually opens a provider request. The
//     deterministic intent router (ADR 0017) runs first and answers "mute"
//     without a model; making that path wait on wl-paste would hand back the
//     milliseconds the router exists to save. A matched intent must never pay
//     for context it will never use.
//  2. *Where* it lands in the conversation. A system message immediately
//     before the user's question — see conversationMessages.
//  3. *That it is disclosed.* The last capture is retained for the
//     context.last IPC method and `jarvix status --last`, and announced with
//     a context.captured event carrying sizes only. What Jarvix saw must
//     always be answerable after the fact.

// ContextCollector gathers opt-in desktop context. Nil in Options disables
// the feature outright: no gathering, no message, nothing published.
type ContextCollector interface {
	Collect(ctx context.Context) desktop.Snapshot
}

// gatherContext captures desktop context for a session that is about to reach
// the provider. It never fails: with no collector, a hung source, or nothing
// on screen it returns an empty snapshot and the turn proceeds exactly as it
// would with context switched off.
func (e *Engine) gatherContext(s *sess) desktop.Snapshot {
	collector := e.opts.Context
	if collector == nil {
		return desktop.Snapshot{}
	}
	snap := collector.Collect(s.ctx)
	// Charged to Jarvix, not to the model: gathering happens inside the window
	// that would otherwise be reported as thinking time (ADR 0018's timings).
	s.timings.markContext()
	if s.ctx.Err() != nil {
		// Cancelled mid-capture: the session is over, and recording what a
		// dead turn saw would only confuse the audit.
		return desktop.Snapshot{}
	}

	// Retained even when empty: "nothing was captured" is an audit answer,
	// and the alternative is `jarvix status --last` reporting a stale capture
	// from three questions ago as though it were this one's.
	e.mu.Lock()
	e.lastContext = snap
	e.lastContextSession = s.id
	e.lastContextTaken = true
	e.mu.Unlock()

	// Which capture sources contributed (issue #168) — the source word only,
	// never the captured text, on the same rule as the event below.
	s.noteSources(contextSources(snap)...)

	sources := make([]map[string]any, 0, len(snap.Items))
	for _, item := range snap.Items {
		// Sizes and flags, never content: events fan out to every connected
		// client and anything in them may be displayed or logged by one.
		sources = append(sources, map[string]any{
			"source":    string(item.Source),
			"chars":     item.Chars,
			"truncated": item.Truncated,
			"redacted":  item.Redacted,
		})
	}
	e.publish(Event{Type: "context.captured", Data: map[string]any{
		"session_id":  s.id,
		"sources":     sources,
		"duration_ms": snap.Elapsed.Milliseconds(),
	}})
	return snap
}

// LastContext reports the most recent capture and the session it was taken
// for, so a client can show the user exactly what Jarvix saw. ok is false
// until the first capture of the daemon's life.
func (e *Engine) LastContext() (snap desktop.Snapshot, sessionID string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastContext, e.lastContextSession, e.lastContextTaken
}
