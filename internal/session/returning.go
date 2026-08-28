package session

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// This file is the engine's half of the return briefing (#150, ADR 0050).
//
// The engine is the only component that knows the two moments the feature
// needs, and it knows them for free. The first is *the user is here*: a
// user-started exchange reaching the engine is the daemon's one unambiguous
// sighting of a person, and measuring an absence from anything else — a
// window that stayed open, a process that kept running — would be the
// machine-activity tracking this feature exists without. The second is *the
// answer is finished but not yet said*: the one point at which an extra
// sentence can still ride the same breath as the answer the user came back
// and asked for.
//
// Everything else — what counts as an absence, whether there is anything to
// report, what the briefing says — lives in internal/briefing. The engine
// holds no policy about any of it.

// Returning is the return briefing's seam into the session engine.
type Returning interface {
	// Arrive records a user-started exchange at now. It must be cheap enough
	// to sit on every exchange: no I/O, nothing that can block.
	Arrive(now time.Time)
	// OfferLine reports the one sentence to append to this answer, or "" —
	// which is the answer on every exchange that did not follow an absence.
	// transient marks a sentence that must be spoken but not recorded: the
	// whole briefing, when briefing.speak_on_return is set.
	OfferLine(ctx context.Context) (line string, transient bool)
	// Briefing composes the spoken briefing for "what did I miss?".
	Briefing(ctx context.Context) (string, error)
}

// arrive tells the briefing service the user is demonstrably back. Called
// from the two goroutines that carry a user's turn — think and runIntent —
// rather than from maybeThinkLocked, deliberately: maybeThinkLocked holds
// e.mu, and nothing on the briefing's side of this seam is ever going to be
// allowed to make an ordinary exchange wait on a lock it did not need.
//
// A scheduled or replayed session is not a sighting. A quiet one may be —
// quiet is about the speaker, not about who asked.
func (e *Engine) arrive(s *sess) {
	if e.opts.Returning == nil || s.scheduled || s.replay {
		return
	}
	e.opts.Returning.Arrive(e.now())
}

// offerLine asks whether this answer should carry the briefing offer. The
// call happens once per absence — the service answers "" immediately
// otherwise — so an ordinary exchange pays one interface call and a mutex.
func (e *Engine) offerLine(s *sess) (string, bool) {
	if e.opts.Returning == nil || s.scheduled || s.replay || s.quiet {
		// A quiet turn is one nobody is listening to: appending an offer to
		// it would spend the absence's one offer on a sentence with no ear.
		return "", false
	}
	if s.ctx.Err() != nil {
		return "", false
	}
	line, transient := e.opts.Returning.OfferLine(s.ctx)
	return strings.TrimSpace(line), transient
}

// briefingRecord stands in for a spoken briefing in the conversation record.
// The exchange happened and the record says so; what was said does not enter
// the history head, the archive, or anything a later turn is sent — the
// transience rule (#150, ADR 0050) applied where it would otherwise leak.
const briefingRecord = "I gave the return briefing."

// runBriefing answers the deterministic briefing phrases. It runs on
// runIntent's goroutine, off the engine lock, which is what lets it spend a
// bounded model call the way a focus recap does.
func (e *Engine) runBriefing(s *sess) (string, error) {
	if e.opts.Returning == nil {
		return "", fmt.Errorf("the return briefing is not available on this daemon")
	}
	return e.opts.Returning.Briefing(s.ctx)
}
