package session

import (
	"github.com/rpickz/jarvix/internal/knowledge"
)

// This file is the engine half of the feed cache (ADR 0030), shaped exactly
// like the engine half of the memory book (memory.go, ADR 0025).
// internal/knowledge owns the values — the scheduler, the ttl, the
// persistence — while the engine owns the same decisions it owns for memory:
// *when* feeds are consulted (inside think(), so only a turn that reaches the
// provider pays, and what it pays is a map read — never a fetch), *where* the
// block lands (with the desktop capture, adjacent to the question, because a
// reading describes "right now"; see conversationMessages), and *that it is
// disclosed* (a knowledge.injected event carrying counts only — values may be
// sensitive and events fan out to every connected client).

// KnowledgeInjector supplies the feed values block for turns that reach the
// provider. Nil in Options disables the feature outright: no consultation,
// no message, nothing published.
type KnowledgeInjector interface {
	Inject() knowledge.Injection
}

// gatherKnowledge consults the feed cache for a session that is about to
// reach the provider. It never fails and never fetches: with no injector, no
// opted-in feeds, or an empty cache it returns an empty injection and the
// turn proceeds exactly as it would with feeds switched off.
func (e *Engine) gatherKnowledge(s *sess) knowledge.Injection {
	injector := e.opts.Knowledge
	if injector == nil {
		return knowledge.Injection{}
	}
	inj := injector.Inject()
	if s.ctx.Err() != nil {
		// Cancelled while consulting: the session is over, and announcing
		// what a dead turn was given would only confuse the audit.
		return knowledge.Injection{}
	}
	if inj.Message == "" {
		return inj
	}
	e.publish(Event{Type: "knowledge.injected", Data: map[string]any{
		"session_id": s.id,
		"feeds":      inj.Feeds,
		"trimmed":    inj.Trimmed,
		"est_tokens": inj.EstTokens,
	}})
	return inj
}
