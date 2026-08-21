package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/rpickz/jarvix/internal/intent"
)

// This file is the engine half of layout capture (#62): the router decides
// an utterance is "save this as <name>", and the engine turns that into a
// plan, at most one confirmation question, and one spoken confirmation. The
// capture service itself — reading the inventory, deriving steps, writing
// configuration — lives behind RoutineCapturer, so session tests substitute
// a fake and never read a desktop or touch a config file.

// captureToolName labels the replace confirmation in events and logs. It is
// an event identity, not a policy identity: capture writes the user's own
// configuration and moves nothing, so it is not gated — only the destructive
// half, replacing an existing entry, asks, and it asks always.
const captureToolName = "routine.capture"

// RoutineCapturer plans a layout capture. Plan is read-only against both the
// desktop and the configuration — nothing is written until the plan's Commit
// — which is what lets the engine put the replace question between the two.
type RoutineCapturer interface {
	Plan(ctx context.Context, name string) (CapturePlan, error)
}

// CapturePlan is one prepared capture, waiting for Commit.
type CapturePlan interface {
	// ReplaceQuestion returns the spoken question to ask when an entry with
	// this name already exists; replaces false means nothing is overwritten
	// and Commit needs no confirmation.
	ReplaceQuestion() (question string, replaces bool)
	// Commit writes the entry and returns the spoken confirmation ("Seven
	// windows across three workspaces, saved as morning setup."). A failed
	// commit leaves the configuration untouched; err is the sentence
	// fragment the engine speaks about it.
	Commit(ctx context.Context) (spoken string, err error)
}

// runCapture carries out a matched capture phrase. The shape mirrors
// runRoutine — plan, optional confirmation through the one shared exchange
// (ADR 0014), act, one spoken line — and alive follows the same contract:
// false means the session ended underneath us and the cancel path owns the
// events.
func (e *Engine) runCapture(s *sess, m intent.Match) (ack string, runErr error, alive bool) {
	if e.opts.Capture == nil {
		return "", fmt.Errorf("saving layouts is not available on this daemon"), true
	}
	plan, err := e.opts.Capture.Plan(s.ctx, m.CaptureName)
	if err != nil {
		if s.ctx.Err() != nil {
			return "", nil, false
		}
		return "", err, true
	}
	if question, replaces := plan.ReplaceQuestion(); replaces {
		outcome, ok := e.awaitConfirmation(s, confirmRequest{
			tool:    captureToolName,
			command: fmt.Sprintf("replace routine %q", m.CaptureName),
			summary: question,
			rule:    "a routine with this name already exists",
			key:     captureToolName + "\x00" + m.CaptureName,
			// Never remembered, whatever remember_for_conversation says: a
			// remembered "yes" would let a later misheard phrase overwrite a
			// curated routine silently — the exact clobbering this exchange
			// exists to prevent.
			rememberable: false,
			resume:       StateActing,
		})
		if !ok {
			return "", nil, false
		}
		if outcome == confirmUnavailable {
			return "", errors.New("I could not ask you to confirm that, so nothing was saved"), true
		}
		if outcome != confirmApproved {
			// "Cancelled." — and the file untouched, which is the whole point.
			return "", errIntentDeclined, true
		}
	}
	spoken, err := plan.Commit(s.ctx)
	if err != nil {
		if s.ctx.Err() != nil {
			return "", nil, false
		}
		return "", err, true
	}
	return spoken, nil, true
}
