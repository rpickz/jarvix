package session

import (
	"context"
	"fmt"

	"github.com/rpickz/jarvix/internal/intent"
)

// This file is the engine half of one-shot reminders (#141, ADR 0046): the
// router decides an utterance is a reminder action, and the engine hands the
// whole match to the reminder runner, which owns the store and composes the
// one sentence spoken back. The engine's part is deliberately tiny — a
// state, an acknowledgement, the standard events — because a reminder action
// touches nothing but Jarvix's own store: no argv, no shell, no compositor
// dispatch, and therefore no permission gate and NO confirmation card — the
// focus dispatch's stance (ADR 0041), which is itself the memory book's for
// reversible writes (ADR 0025). "Remind me at three" spoken out loud is the
// authorisation; a card would ask the user to confirm their own sentence.
//
// The runner arrives through the one seam the engine already has for
// executing intents, Options.IntentRunner: the daemon injects a runner that
// also implements ReminderRunner (internal/reminders.IntentRunner), wrapping
// the focus runner and delegating the argv and shell halves down the chain.

// ReminderRunner is the reminder service as the engine sees it: one call,
// one spoken sentence. Implementations must compose that sentence entirely
// from the reminder store's own record (ADR 0013) — the engine speaks it
// verbatim as the intent acknowledgement.
type ReminderRunner interface {
	RunReminder(ctx context.Context, m intent.Match) (spoken string, err error)
}

// runReminder carries out a matched reminder phrase — the runFocus shape:
// act, return the one spoken line, no gate. alive follows the same contract
// as the other intent paths: false means the session ended underneath us and
// the cancel path owns the events.
func (e *Engine) runReminder(s *sess, m intent.Match) (ack string, runErr error, alive bool) {
	runner, ok := e.intentRunner().(ReminderRunner)
	if !ok {
		// No reminder-capable runner wired — a daemon built without the
		// service, or a test with a bare fake. Saying so plainly beats a
		// silent shrug the user would read as a mishearing.
		return "", fmt.Errorf("reminders are not available on this daemon"), true
	}
	line, err := runner.RunReminder(s.ctx, m)
	if s.ctx.Err() != nil {
		return "", nil, false
	}
	return line, err, true
}
