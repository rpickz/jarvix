package session

import (
	"fmt"

	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/vocabulary"
)

// This file is the engine half of the taught vocabulary (issue #129), in two
// seams shaped after their precedents:
//
//   - The injector half follows memory.go (ADR 0025) exactly: consulted
//     inside think() so only a turn that reaches the provider pays (one
//     stat(2)), the block lands beside the remembered facts as standing
//     knowledge (see conversationMessages), the last injection is retained
//     for the vocabulary.last audit surface, and the bus hears counts only.
//   - The teach half follows nickname.go (#126): the router decides an
//     utterance is "when i say X i mean Y", and the engine turns that into
//     one seam call and one spoken sentence. Everything about storage —
//     supersede, caps, the bias flag — lives behind VocabularyTeacher, so
//     session tests substitute a fake and never touch a store.

// VocabularyInjector supplies the taught-vocabulary block for turns that
// reach the provider. Nil in Options disables the feature outright: no
// consultation, no message, nothing published.
type VocabularyInjector interface {
	Inject() vocabulary.Injection
}

// VocabularyTeacher is the engine's view of the vocabulary seam for the
// deterministic voice phrases. Every method returns the sentence to speak;
// err is a spoken-ready refusal ("the vocabulary store is full …") that
// intentFailureAck frames as "Sorry, …".
type VocabularyTeacher interface {
	// TeachEntry stores phrase → meaning (superseding an existing phrase)
	// and returns the soft spoken confirmation. source references the
	// teaching turn.
	TeachEntry(phrase, meaning, source string) (spoken string, err error)
	// ListenFor flags an already-taught phrase as hard to hear, within the
	// bias cap, and returns the spoken confirmation — or a refusal naming
	// what to do (teach the word first, or unflag another).
	ListenFor(phrase string) (spoken string, err error)
	// SpokenListing returns the one short spoken listing of taught words.
	SpokenListing() (spoken string, err error)
}

// gatherVocabulary consults the taught vocabulary for a session that is
// about to reach the provider. It never fails: with no injector or an empty
// (or unreadable) store it returns an empty injection and the turn proceeds
// exactly as it would with the feature switched off — byte-identical, which
// is the pinned zero-entry contract of #129.
func (e *Engine) gatherVocabulary(s *sess) vocabulary.Injection {
	injector := e.opts.Vocabulary
	if injector == nil {
		return vocabulary.Injection{}
	}
	inj := injector.Inject()
	if s.ctx.Err() != nil {
		// Cancelled while consulting: the session is over, and recording what
		// a dead turn was given would only confuse the audit.
		return vocabulary.Injection{}
	}

	// Retained even when empty: "no vocabulary was injected" is an audit
	// answer, the memory.last arrangement.
	e.mu.Lock()
	e.lastVocabulary = inj
	e.lastVocabularySession = s.id
	e.lastVocabularyTaken = true
	e.mu.Unlock()

	// The taught words this turn was given (issue #168), by id.
	s.noteSources(vocabularySources(inj)...)

	// Counts and estimates, never content: events fan out to every connected
	// client and anything in them may be displayed or logged by one.
	e.publish(Event{Type: "vocabulary.injected", Data: map[string]any{
		"session_id": s.id,
		"entries":    len(inj.Entries),
		"trimmed":    inj.Trimmed,
		"total":      inj.Total,
		"est_tokens": inj.EstTokens,
	}})
	return inj
}

// LastVocabulary reports the most recent injection and the session it was
// made for, so a client can show exactly which taught words the model was
// given. ok is false until the first vocabulary-enabled turn of the daemon's
// life.
func (e *Engine) LastVocabulary() (inj vocabulary.Injection, sessionID string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastVocabulary, e.lastVocabularySession, e.lastVocabularyTaken
}

// runVocabTeach carries out a matched "when i say X i mean Y".
func (e *Engine) runVocabTeach(s *sess, m intent.Match) (ack string, runErr error) {
	if e.opts.VocabularyTeacher == nil {
		return "", fmt.Errorf("teaching words is not available on this daemon")
	}
	return e.opts.VocabularyTeacher.TeachEntry(m.VocabPhrase, m.VocabMeaning, s.id)
}

// runVocabListen carries out a matched "listen for the word X".
func (e *Engine) runVocabListen(s *sess, m intent.Match) (ack string, runErr error) {
	if e.opts.VocabularyTeacher == nil {
		return "", fmt.Errorf("teaching words is not available on this daemon")
	}
	return e.opts.VocabularyTeacher.ListenFor(m.VocabListen)
}

// runVocabList carries out a matched "what words have i taught you".
func (e *Engine) runVocabList(s *sess) (ack string, runErr error) {
	if e.opts.VocabularyTeacher == nil {
		return "", fmt.Errorf("teaching words is not available on this daemon")
	}
	return e.opts.VocabularyTeacher.SpokenListing()
}
