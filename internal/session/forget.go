package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/tools"
)

// This file is the engine half of the window's per-fact Forget (issue #92).
// The window's button is not a private deletion path: the memory.forget IPC
// method already deletes directly (the CLI's line to the store, ADR 0025),
// but a *surface* wants what the model gets — the permission gate. So this
// runs the exact tool identity through the exact machinery: the policy
// classifies memory.forget as it always does, the ask tier goes through the
// one shared confirmation exchange (ADR 0014) with the question naming the
// exact fact — resolved daemon-side by the tool's own Confirmable — and an
// approval executes through the registry, so tool.started/tool.finished and
// the audit trail read identically however a forget was triggered. A second
// way to forget that skipped the gate would be a hole in it (the
// routines.run argument, restated).

// ForgetFact starts a session that forgets one remembered fact by id, gated.
// content is the fact's text, already resolved by the caller against the
// store; it names the action in the conversation record. Mirrors how
// routines.run starts a session for a button press: an active session is
// interrupted, exactly as any new interaction interrupts.
func (e *Engine) ForgetFact(id, content string) (string, error) {
	return e.startGatedForget(tools.MemoryForgetToolName, id,
		"Forget the remembered fact: "+content)
}

// ForgetVocabularyEntry starts a session that forgets one taught phrase by
// id, gated (issue #129). The same machinery on the same argument: the
// window's Delete button in the Vocabulary section deserves the second look
// the model gets, because deleting an entry destroys its taught history —
// the reversibility split of ADR 0025, applied to vocabulary as the ADR
// records. description names the entry as a person would ("quid means
// pounds"), resolved by the caller against the store.
func (e *Engine) ForgetVocabularyEntry(id, description string) (string, error) {
	return e.startGatedForget(tools.VocabularyForgetToolName, id,
		"Forget the taught word: "+description)
}

// startGatedForget starts the session both gated forgets share: one tool
// identity, one id argument, and the sentence the conversation record keeps.
func (e *Engine) startGatedForget(tool, id, recorded string) (string, error) {
	args, err := json.Marshal(struct {
		ID string `json:"id"`
	}{ID: id})
	if err != nil {
		return "", err
	}
	call := ai.ToolCall{ID: "forget-" + id, Name: tool, Arguments: string(args)}

	e.mu.Lock()
	defer e.mu.Unlock()
	sid, err := e.startSessionLocked()
	if err != nil {
		return "", err
	}
	s := e.current
	if err := e.setStateLocked(StateActing); err != nil {
		e.failLocked(s, "session", err)
		return "", err
	}
	// Tracked like every acting turn (#74): the tail writes history and the
	// archive, and an untracked goroutine there is invisible to the drains.
	e.active.Go(func() { e.runForget(s, call, recorded) })
	return sid, nil
}

// runForget carries the forget out off the engine lock: gate, act, one spoken
// line, one recorded turn — the runIntent shape without a router in front.
func (e *Engine) runForget(s *sess, call ai.ToolCall, recorded string) {
	ack, runErr, alive := e.gatedForget(s, call)
	if !alive {
		return // cancelled or superseded; that path owns the events
	}
	if s.ctx.Err() != nil {
		return
	}
	if runErr != nil {
		ack = intentFailureAck(runErr)
		e.log.Warn("forget failed", "component", "session", "session_id", s.id,
			"tool", call.Name, "error", runErr.Error())
	}
	s.timings.markFirstDelta()
	e.speakAck(s, ack)
	// Recorded like an intent turn: a follow-up that reaches the model must
	// know the fact is gone — injection alone would only fall silent about
	// it. The content is already conversation-record material (it was
	// injected every turn), so naming it here discloses nothing new.
	e.commitTurn(s, recorded, ack)
	e.mu.Lock()
	e.finishLocked(s)
	e.mu.Unlock()
	e.persistHistory()
	e.persistArchive()
}

// gatedForget applies the permission gate to the forget call and executes it
// only if the policy — and, for the ask tier, the user — allows. The shape is
// runUserIntent's: same events, same audit trail, same outcome wording.
func (e *Engine) gatedForget(s *sess, call ai.ToolCall) (ack string, runErr error, alive bool) {
	if e.tools == nil {
		return "", errors.New("memory is not available on this daemon"), true
	}
	verdict := e.tools.Check(call)
	switch verdict.Decision {
	case tools.PolicyDeny:
		e.log.Info("forget denied", "component", "tools", "tool", call.Name,
			"command", verdict.Command, "rule", verdict.Rule, "source", "policy")
		e.publish(Event{Type: "tool.denied", Data: map[string]any{
			"session_id": s.id, "tool": call.Name,
			"command": verdict.Command, "rule": verdict.Rule}})
		return "", fmt.Errorf("forgetting is not permitted (%s)", verdict.Rule), true
	case tools.PolicyAsk:
		outcome, ok := e.awaitConfirmation(s, confirmRequest{
			tool:    call.Name,
			command: verdict.Command,
			summary: verdict.Summary,
			rule:    verdict.Rule,
			key:     call.Name + "\x00" + verdict.Command,
			// The key names the exact fact, so a remembered approval can only
			// ever re-approve deleting the same content — but deletion is
			// still deletion: rememberable follows the tool's own setting.
			rememberable: tools.RememberableApproval(call.Name),
			resume:       StateActing,
		})
		if !ok {
			return "", nil, false
		}
		if outcome == confirmUnavailable {
			return "", errors.New("I could not ask you to confirm that, so nothing was forgotten"), true
		}
		if outcome != confirmApproved {
			return "", errIntentDeclined, true
		}
	default:
		e.log.Info("forget allowed", "component", "tools", "tool", call.Name,
			"command", verdict.Command, "rule", verdict.Rule, "source", "policy")
	}
	result, ok := e.executeTool(s, call, nil)
	if !ok {
		// The session ended before execution could begin (cancelled or
		// superseded between the approval and here); that path owns the
		// events, exactly as an abandoned confirmation does.
		return "", nil, false
	}
	if strings.HasPrefix(result, "error: ") {
		// The tool's own refusal (fact vanished between the click and the
		// approval, a store write failure) — the detail lives in the events
		// and the log; the ear gets one honest line.
		return "", errors.New("that fact could not be forgotten"), true
	}
	return "Forgotten.", nil, true
}
