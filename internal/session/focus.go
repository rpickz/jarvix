package session

import (
	"context"
	"fmt"

	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/provenance"
)

// This file is the engine half of focus threads (#123, ADR 0041): the router
// decides an utterance is a focus action, and the engine hands the whole
// match to the focus runner, which owns the thread store and composes the one
// sentence spoken back. The engine's part is deliberately tiny — a state, an
// acknowledgement, the standard events — because a focus action touches
// nothing but Jarvix's own store: no argv, no shell, no compositor dispatch,
// and therefore no permission gate, the same stance the memory book takes for
// its reversible writes (ADR 0025).
//
// The runner arrives through the one seam the engine already has for
// executing intents, Options.IntentRunner: the daemon injects a runner that
// also implements FocusRunner (internal/focus.IntentRunner), delegating the
// argv and shell halves to the real ExecRunner untouched. That keeps this
// feature out of engine.go entirely — no new Options field, no new engine
// state — which matters while sibling work is landing in this package.

// FocusRunner is the focus service as the engine sees it: one call, one
// spoken sentence. Implementations must compose that sentence entirely from
// the thread store's own record (ADR 0013) — the engine speaks it verbatim as
// the intent acknowledgement.
type FocusRunner interface {
	RunFocus(ctx context.Context, m intent.Match) (spoken string, err error)
}

// runFocus carries out a matched focus phrase. The shape follows runRoutine
// — act, return the one spoken line — minus the gate: every focus action is
// a reversible edit of Jarvix's own state file, so there is no command to
// classify and nothing to confirm. alive follows the same contract as the
// other intent paths: false means the session ended underneath us and the
// cancel path owns the events.
func (e *Engine) runFocus(s *sess, m intent.Match) (ack string, runErr error, alive bool) {
	runner, ok := e.intentRunner().(FocusRunner)
	if !ok {
		// No focus-capable runner wired — a daemon built without the focus
		// service, or a test with a bare fake. Saying so plainly beats a
		// silent shrug the user would read as a mishearing.
		return "", fmt.Errorf("focus threads are not available on this daemon"), true
	}
	// A sink for what the action read (issue #168): the focus service names
	// the thread its sentence was composed from, and nothing else — the
	// recap's captured text and summary stay transient (ADR 0043/0047).
	var sink provenance.Sink
	line, err := runner.RunFocus(provenance.WithSink(s.ctx, &sink), m)
	if s.ctx.Err() != nil {
		return "", nil, false
	}
	for _, ref := range sink.Drain() {
		ref.Strength = provenance.Returned
		s.noteSources(ref)
	}
	return line, err, true
}
