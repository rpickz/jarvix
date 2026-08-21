package session

import (
	"errors"
	"strings"
)

// This file is the typed-input entry point (issue #35). Speaking is the wrong
// input for a URL, a file path, a flag, or an unusual proper noun — exactly
// the things whisper transcribes worst — so the conversation window can type
// instead. What it types must join the same conversation: same history, same
// tools, same spoken answer.
//
// Nothing new happens downstream of here. SubmitText is a *composition* of the
// two calls `jarvix ask` already makes (StartSession then Submit), with the
// one decision those two calls imply — is this a new turn, or the answer to a
// question Jarvix is waiting on? — made once, in Go, under a single lock.
//
// It lives in the daemon rather than in the client for two reasons. The
// window is QML and QML is display-only (ADR 0013): a decision worth testing
// does not belong there. And a client that read the state, then started a
// session, would be holding a fact that could go stale between the two calls —
// a confirmation can time out in that gap, and starting a session then
// silently abandons the tool call the user was about to approve.

// ErrEmptyText rejects a submission with nothing in it. Pressing Enter on an
// empty field must start no session at all: an empty turn costs a provider
// request, interrupts whatever Jarvix was saying, and asks the model nothing.
var ErrEmptyText = errors.New("submitted text is empty")

// TextResult reports what a typed submission did, so the caller can render the
// outcome without guessing from events. Confirmation distinguishes the two
// very different things Enter can mean.
type TextResult struct {
	// SessionID is the session the text went to: a newly started one for a
	// question, or the session waiting on a confirmation.
	SessionID string
	// Confirmation is true when the text answered a pending tool confirmation
	// (ADR 0014) instead of starting a new turn.
	Confirmation bool
	// Approved is the reading of that answer — meaningful only alongside
	// Confirmation. Anything that is not a clear affirmative declines.
	Approved bool
}

// SubmitText submits typed text as a conversational turn.
//
// With a tool confirmation pending, the text answers it (the same reading a
// spoken "yes" or "no" gets, from the same parser) and the waiting session is
// left alone. Otherwise a new session begins — cancelling any session in
// flight, which is what makes typing over a spoken answer interrupt it — and
// the text is submitted as the turn's question, skipping audio exactly as
// `jarvix ask` does.
//
// Text that is empty or whitespace is rejected with ErrEmptyText before
// anything is started or interrupted.
func (e *Engine) SubmitText(text string) (TextResult, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return TextResult{}, ErrEmptyText
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// The confirmation reading is computed here only to report it; the actual
	// resolution happens inside submitLocked, through the one parser in
	// confirm.go. isAffirmative is pure, so the two cannot disagree.
	if e.state == StateAwaitingConfirmation && e.pending != nil {
		id := ""
		if e.current != nil {
			id = e.current.id
		}
		if err := e.submitLocked(trimmed); err != nil {
			return TextResult{}, err
		}
		return TextResult{SessionID: id, Confirmation: true, Approved: isAffirmative(trimmed)}, nil
	}

	id, err := e.startSessionLocked()
	if err != nil {
		return TextResult{}, err
	}
	if err := e.submitLocked(trimmed); err != nil {
		return TextResult{}, err
	}
	return TextResult{SessionID: id}, nil
}
