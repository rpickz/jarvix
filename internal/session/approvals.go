package session

import "fmt"

// This file is the engine's half of "what have I pre-approved?" (issue #162,
// ADR 0053).
//
// The seam is a lister and nothing more. There is no Grant method here and no
// Forget: the engine can *read* what standing grants exist so it can say them
// out loud, and it cannot change them. That asymmetry is deliberate and it is
// the same one #109 drew around [tools.policy] — the surface that answers a
// question about the gate must not also be a surface that moves it, because
// the conversation is exactly where untrusted text arrives.

// ApprovalsLister is the engine's view of the standing-grant list for the
// deterministic voice phrase. It returns the sentence to speak, already
// composed: the engine speaks the string and words nothing itself, the same
// contract VocabularyTeacher.SpokenListing has, so the CLI, the window and
// the voice cannot end up describing one grant three ways.
//
// conversation carries the grants that live only in this conversation, which
// the engine knows and the daemon does not — they are handed in rather than
// looked up so the seam stays a pure composer with no reach into session
// state.
type ApprovalsLister interface {
	SpokenApprovals(conversation []string) (spoken string, err error)
}

// runApprovalsList carries out a matched "what have i pre-approved".
func (e *Engine) runApprovalsList() (ack string, runErr error) {
	if e.opts.Approvals == nil {
		return "", fmt.Errorf("pre-approved commands are not available on this daemon")
	}
	return e.opts.Approvals.SpokenApprovals(e.ConversationGrants())
}
