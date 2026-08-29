package session

import (
	"context"
	"fmt"
)

// This file is the engine's half of the situation report (#196, ADR 0061).
//
// It is deliberately thinner than the return briefing's half beside it, and the
// difference is the whole distinction between the two features. A briefing is
// about a stretch of time the user was not here, so the engine — the only thing
// that knows when a person is present — has to record arrivals, arm an offer,
// and ride an answer. A situation report is about *now*. Nothing has to be
// witnessed, nothing has to be armed, and there is nothing to volunteer: the
// user asks, and the answer is composed from the machine as it stands.
//
// So the engine holds no policy about it at all. It routes a matched phrase and
// keeps the composed answer out of conversation memory, and that is the list.

// Operating is the situation report's seam into the session engine. One verb,
// declared here so the session package depends on nothing the daemon owns.
type Operating interface {
	// Situation composes the spoken situation report for "what's going on?".
	Situation(ctx context.Context) (string, error)
}

// situationRecord stands in for a spoken report in the conversation record.
// The exchange happened and the record says so; what was said does not enter
// the history head, the archive, or anything a later turn is sent.
//
// The transience rule is ADR 0050's, and it applies here for a reason of its
// own on top of that one: a report is a description of a moment, and a moment
// that has passed is the single most misleading thing a later turn could be
// handed. "Claude is waiting on you in the deploy thread" committed at nine
// o'clock is a false statement by half past, and a model reading it back as
// context would state it with the confidence of something it was told.
const situationRecord = "I gave the situation report."

// runSituation answers the deterministic situation phrases. It runs on
// runIntent's goroutine, off the engine lock, which is what lets it spend a
// bounded model call the way a focus recap does.
func (e *Engine) runSituation(s *sess) (string, error) {
	if e.opts.Operating == nil {
		return "", fmt.Errorf("the situation report is not available on this daemon")
	}
	return e.opts.Operating.Situation(s.ctx)
}
