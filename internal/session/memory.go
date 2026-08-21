package session

import (
	"github.com/rpickz/jarvix/internal/memory"
)

// This file is the engine half of the knowledge base (ADR 0025), shaped
// exactly like the engine half of desktop context (context.go, ADR 0019).
// internal/memory owns the store — the file, the caps, the hand-edit pickup
// — while the engine owns the same three decisions it owns for context:
//
//  1. *When* memory is consulted. Inside think(), after the deterministic
//     intent router (ADR 0017): only the one path that opens a provider
//     request pays for it, and what it pays is one stat(2) of a file already
//     in memory — a matched intent pays nothing at all.
//  2. *Where* it lands in the conversation. A system message immediately
//     after the system prompt, before the carried-over history — standing
//     knowledge precedes the thread, while a desktop capture (which
//     describes "right now") stays adjacent to the question. See
//     conversationMessages.
//  3. *That it is disclosed.* The last injection is retained for the
//     memory.last IPC method and `jarvix status --last`, and announced with
//     a memory.injected event carrying counts only. What Jarvix consulted
//     must always be answerable after the fact.

// MemoryInjector supplies the remembered-facts block for turns that reach
// the provider. Nil in Options disables the feature outright: no
// consultation, no message, nothing published.
type MemoryInjector interface {
	Inject() memory.Injection
}

// gatherMemory consults the knowledge base for a session that is about to
// reach the provider. It never fails: with no injector or an empty (or
// unreadable) store it returns an empty injection and the turn proceeds
// exactly as it would with memory switched off.
func (e *Engine) gatherMemory(s *sess) memory.Injection {
	injector := e.opts.Memory
	if injector == nil {
		return memory.Injection{}
	}
	inj := injector.Inject()
	if s.ctx.Err() != nil {
		// Cancelled while consulting: the session is over, and recording what
		// a dead turn was given would only confuse the audit.
		return memory.Injection{}
	}

	// Retained even when empty: "no facts were injected" is an audit answer,
	// and the alternative is `jarvix status --last` reporting a stale
	// injection from three questions ago as though it were this one's.
	e.mu.Lock()
	e.lastMemory = inj
	e.lastMemorySession = s.id
	e.lastMemoryTaken = true
	e.mu.Unlock()

	// Counts and estimates, never content: events fan out to every connected
	// client and anything in them may be displayed or logged by one.
	e.publish(Event{Type: "memory.injected", Data: map[string]any{
		"session_id": s.id,
		"facts":      len(inj.Facts),
		"trimmed":    inj.Trimmed,
		"total":      inj.Total,
		"est_tokens": inj.EstTokens,
	}})
	return inj
}

// LastMemory reports the most recent injection and the session it was made
// for, so a client can show the user exactly which facts the model was
// given. ok is false until the first memory-enabled turn of the daemon's
// life.
func (e *Engine) LastMemory() (inj memory.Injection, sessionID string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastMemory, e.lastMemorySession, e.lastMemoryTaken
}
