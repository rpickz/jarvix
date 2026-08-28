package session

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/provenance"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
)

// This file is the engine half of the tool permission gate (ADR 0014): the
// policy classifies a call (internal/tools), and the ask tier becomes a
// session-level exchange here — Jarvix speaks the intent summary, publishes
// the exact command, and waits in AwaitingConfirmation for the user's
// answer. Nothing executes until that answer is an affirmative.

// DefaultConfirmTimeout is how long a pending confirmation waits before
// declining. Long enough to walk to the keyboard, short enough that a missed
// question does not leave a loaded gun on the table.
const DefaultConfirmTimeout = 30 * time.Second

// pendingConfirmation is one tool call waiting for the user. At most one
// exists at a time (tool calls execute sequentially); guarded by Engine.mu.
type pendingConfirmation struct {
	tool    string
	command string
	key     string
	// summary and rule are kept so the conversation snapshot can restate the
	// question to a window opened mid-wait (issue #76): the card must show
	// what is being asked without waiting for an event that already happened.
	summary string
	rule    string
	// granted is the scope the user answered with, set at resolution so the
	// record can say a rule was added and which kind. Zero for the ordinary
	// approve-once, which is what almost every resolution is.
	granted tools.RememberScope
	// remember is the "don't ask again" offer for this question (#162): the
	// exact word-prefix that would be added, or the one-sentence refusal. It
	// is derived daemon-side from the parsed command at the moment the
	// question is asked, kept here so that the card, the snapshot, and the
	// resolution all use one string — the one the user was shown.
	remember tools.RememberOffer
	// timeout is the configured confirmation window, and deadline is when it
	// actually expires. deadline stays zero until the countdown starts — the
	// clock begins when the question has been asked aloud, not when it was
	// published — so a zero deadline means "the question is still being
	// asked" and a client should show the full timeout rather than a tick.
	timeout  time.Duration
	deadline time.Time
	// resume is the state the session returns to once the question is
	// answered: Thinking for a model tool round (the tool loop continues),
	// Acting for a user-defined intent (the router finishes its work). It is
	// a field rather than a constant because both callers share this one
	// confirmation mechanism — there is no second permission path.
	resume State
	// outcome carries the user's answer to the waiting tool loop. Buffered:
	// the resolver must never block on the waiter.
	outcome chan bool
	// engaged is set when a voice reply is being captured; the timeout no
	// longer applies — the user is answering, however long whisper takes.
	engaged bool
	// stopPrompt cancels the spoken question's audio. A resolution can land
	// while the question is still being read out — the overlay's tick, the
	// window card, the CLI, a typed reply — and the user who has already
	// answered must not sit through the rest of a sentence asking what they
	// just decided (issue #119): the conversation's speech resumes the moment
	// the answer lands. It cancels the prompt's context only — a child of the
	// session's — so the turn itself, and any answer audio queued on the same
	// speaker, is untouched. Nil until awaitConfirmation arms it; called under
	// Engine.mu, which is safe because cancelling a context never blocks.
	stopPrompt context.CancelFunc
}

// rememberedPattern is the rule this resolution added, or "" when the answer
// was an ordinary approve-once.
func (p *pendingConfirmation) rememberedPattern() string {
	if p.granted == tools.RememberNone {
		return ""
	}
	return p.remember.Pattern
}

// resumeState is where a resolved confirmation returns the session, defaulting
// to Thinking so a pending confirmation built without one behaves as before.
func (p *pendingConfirmation) resumeState() State {
	if p.resume == "" {
		return StateThinking
	}
	return p.resume
}

// Confirm resolves the pending tool confirmation: approved runs the command,
// !approved declines it. This is the `jarvix confirm` / `session.confirm`
// path; spoken and typed replies resolve through Submit instead.
func (e *Engine) Confirm(approved bool) error {
	return e.ConfirmRemembering(approved, tools.RememberNone)
}

// ConfirmRemembering is Confirm with a standing-grant scope (#162): the third
// answer the confirmation card offers.
//
// It does NOT take a pattern. The pattern is the one this engine derived when
// it asked the question and published it on the card — recomputing it from a
// caller's string would be exactly the channel the feature must not have,
// because a caller's string is one plausible forgery away from being the
// model's. A caller that wants to know what it is agreeing to reads
// PendingConfirmation first, which is where the card got it too.
//
// The permanent scope is not written here. Writing to config.toml is the
// daemon's business — it owns the path, the fingerprint and the reload — and
// it does that BEFORE calling this, so a write that fails leaves the question
// standing rather than approving a command on the strength of a rule that did
// not land. What this does with RememberAlways is record it honestly.
func (e *Engine) ConfirmRemembering(approved bool, scope tools.RememberScope) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pending == nil {
		return fmt.Errorf("no tool confirmation is pending")
	}
	if e.state != StateAwaitingConfirmation {
		return fmt.Errorf("a voice reply is already being captured; finish it or cancel the session")
	}
	if scope != tools.RememberNone {
		if !approved {
			// "Remember that I said no" is not a thing this feature does:
			// shell_allow has no negative form, and a decline that quietly
			// wrote a deny rule would be a permission change the user never
			// asked for.
			return fmt.Errorf("a rule is only added when you approve; decline simply declines")
		}
		if !e.pending.remember.Offered {
			// The refusal matrix, enforced at the resolution rather than only
			// at the offer. A client that never rendered the card — or one
			// built against an older daemon — must not be able to add a rule
			// this daemon refused to offer.
			return fmt.Errorf("that command cannot be remembered: %s", e.pending.remember.Reason)
		}
	}
	if scope == tools.RememberConversation {
		e.grantLocked(e.pending.remember.Pattern)
	}
	e.pending.granted = scope
	e.resolveConfirmationLocked(approved, "cli")
	return nil
}

// grantLocked adds a conversation-scoped allow pattern, ignoring a duplicate
// so a user answering the same question twice does not grow the list.
// Callers hold e.mu.
func (e *Engine) grantLocked(pattern string) {
	if pattern == "" {
		return
	}
	for _, g := range e.grants {
		if g == pattern {
			return
		}
	}
	e.grants = append(e.grants, pattern)
	e.log.Info("conversation approval granted", "component", "tools",
		"pattern", pattern, "scope", string(tools.RememberConversation))
}

// grantWords compiles the conversation-scoped grants for the classifier.
// Compiled per check rather than cached because the list is at most a handful
// of patterns and a cache would be one more thing that can disagree with the
// list it caches — the gate is not a hot path, and correctness here is worth
// more than the allocation.
func (e *Engine) grantWords() [][]string {
	e.mu.Lock()
	patterns := append([]string(nil), e.grants...)
	e.mu.Unlock()
	if len(patterns) == 0 {
		return nil
	}
	words, err := tools.CompileGrants(patterns)
	if err != nil {
		// Unreachable: every grant came from a proposal that was already
		// non-empty. A grant that will not compile is dropped rather than
		// allowed to fail the call it was meant to permit.
		e.log.Warn("conversation approvals could not be compiled",
			"component", "tools", "error", err.Error())
		return nil
	}
	return words
}

// RevokeConversationGrant removes a conversation-scoped grant, reporting
// whether it was there. Revocation is the one operation on a grant that is
// not the card's: a user who changes their mind mid-conversation must be able
// to take it back without ending the conversation to do it.
func (e *Engine) RevokeConversationGrant(pattern string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, g := range e.grants {
		if g != pattern {
			continue
		}
		e.grants = append(e.grants[:i:i], e.grants[i+1:]...)
		e.log.Info("conversation approval revoked", "component", "tools", "pattern", pattern)
		return true
	}
	return false
}

// ConversationGrants lists the conversation-scoped allow patterns in grant
// order — what "what have I pre-approved?" reads for the temporary half, and
// what the Approvals view shows above the permanent list.
func (e *Engine) ConversationGrants() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.grants...)
}

// resolveConfirmationLocked delivers the user's answer to the waiting asker
// and returns the session to whichever state asked (Thinking for a tool
// round, Acting for a user-defined intent). Legal from AwaitingConfirmation
// (CLI/text answers) and Transcribing (voice answers). No-op when nothing is
// pending — the confirmation may have timed out a moment earlier.
func (e *Engine) resolveConfirmationLocked(approved bool, source string) {
	p := e.pending
	if p == nil {
		return
	}
	// Silence the question first: the user has answered, so any of it still
	// unsaid is noise between them and the speech that should resume.
	if p.stopPrompt != nil {
		p.stopPrompt()
	}
	e.pending = nil
	e.forceStateLocked(p.resumeState())
	eventType := "tool.confirmed"
	outcome := conversations.ConfirmationApproved
	if !approved {
		eventType = "tool.declined"
		outcome = conversations.ConfirmationDeclined
	}
	// The record before the event (issue #118): a client that has this
	// resolution acknowledged must find it on an immediate history read.
	e.recordConfirmationLocked(p, outcome, source)
	e.publish(Event{Type: eventType, Data: e.confirmationData(p, source)})
	e.log.Info("tool confirmation resolved", "component", "tools", "tool", p.tool,
		"command", p.command, "approved", approved, "source", source)
	p.outcome <- approved
}

// clearPendingLocked abandons a pending confirmation because the session is
// ending (cancel, interruption, stage failure). The waiter is released by the
// session context, not by an outcome; the decline is still recorded so the
// audit trail never shows a question without an answer.
func (e *Engine) clearPendingLocked(source string) {
	p := e.pending
	if p == nil {
		return
	}
	// The session is ending; its context cancellation kills the audio too,
	// but stopping the prompt explicitly keeps the invariant simple: no
	// resolution path leaves the question playing.
	if p.stopPrompt != nil {
		p.stopPrompt()
	}
	e.pending = nil
	// Recorded as declined (issue #118), like any other resolution: the
	// audit promise above — a question never stands without an answer —
	// extends to the durable record, not just the event stream.
	e.recordConfirmationLocked(p, conversations.ConfirmationDeclined, source)
	e.publish(Event{Type: "tool.declined", Data: e.confirmationData(p, source)})
	e.log.Info("tool confirmation abandoned", "component", "tools", "tool", p.tool,
		"command", p.command, "source", source)
}

func (e *Engine) confirmationData(p *pendingConfirmation, source string) map[string]any {
	id := ""
	if e.current != nil {
		id = e.current.id
	}
	d := map[string]any{"session_id": id, "tool": p.tool, "command": p.command, "source": source}
	// A resolution that added a rule says so on the wire (#162), so the
	// activity feed's approval row and the window card both report the whole
	// answer rather than the half of it that ran a command.
	if pattern := p.rememberedPattern(); pattern != "" {
		d["remembered"] = pattern
		d["remember_scope"] = string(p.granted)
	}
	return d
}

// gateAndExecute applies the permission gate to one tool call and executes it
// only if the policy — and, for the ask tier, the user — allows. The returned
// text always goes back to the model; ok is false only when the session ended
// (cancelled, superseded, failed) and the tool loop must stop.
//
// turn carries what Jarvix has already said aloud this turn, so a call that
// does not run can be reported to the model together with the promise it has
// to take back — see refused.
func (e *Engine) gateAndExecute(s *sess, call ai.ToolCall, turn spokenTurn) (result string, ok bool) {
	verdict := e.tools.CheckWithGrants(call, e.grantWords())
	switch verdict.Decision {
	case tools.PolicyDeny:
		e.log.Info("tool call denied", "component", "tools", "tool", call.Name,
			"command", verdict.Command, "rule", verdict.Rule, "source", "policy")
		e.publish(Event{Type: "tool.denied", Data: map[string]any{
			"session_id": s.id, "tool": call.Name, "command": verdict.Command, "rule": verdict.Rule}})
		return e.refused(s, call.Name, verdict.Command, "policy", turn,
			fmt.Sprintf("This tool call is not permitted (%s) and will never run, "+
				"with or without confirmation. Do not retry it; tell the user what you "+
				"wanted to do and that policy forbids it.", verdict.Rule)), true
	case tools.PolicyAsk:
		return e.confirmAndExecute(s, call, verdict, turn)
	default:
		e.auditPreApproved(s, verdict)
		e.log.Debug("tool call allowed", "component", "tools", "tool", call.Name,
			"command", verdict.Command, "rule", verdict.Rule, "source", "policy")
		return e.executeTool(s, call, turn.speaker)
	}
}

// auditPreApproved puts a pre-approved run on the bus (#162, ADR 0053).
//
// This is the price of the feature and the reason it is acceptable: a command
// that runs because of a rule the user once agreed to is not asked about, so
// it must be *said* — in the activity feed, naming the rule that let it
// through, at the moment it happens. Nothing Jarvix does behind a standing
// grant is ever silent.
//
// It fires for user-granted patterns only. A shipped read-only allow pattern
// (`ls`, `git status`) is not a grant anybody made and has never produced a
// row; adding one now would bury the rows that matter under the ones that
// never did.
func (e *Engine) auditPreApproved(s *sess, verdict tools.Verdict) {
	if !verdict.PreApproved {
		return
	}
	e.log.Info("tool call pre-approved", "component", "tools", "tool", verdict.Tool,
		"command", verdict.Command, "rule", verdict.Rule, "pattern", verdict.Pattern,
		"source", "remembered rule")
	e.publish(Event{Type: "tool.pre_approved", Data: map[string]any{
		"session_id": s.id, "tool": verdict.Tool, "command": verdict.Command,
		"rule": verdict.Rule, "pattern": verdict.Pattern,
		"scope": string(e.grantScope(verdict.Pattern)),
	}})
}

// grantScope reports whether pattern is a conversation-scoped grant or a
// configured one, so the audit row can say which — "for this conversation"
// and "until you revoke it" are different promises and the row must not blur
// them.
func (e *Engine) grantScope(pattern string) tools.RememberScope {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, g := range e.grants {
		if g == pattern {
			return tools.RememberConversation
		}
	}
	return tools.RememberAlways
}

// rememberOffer derives the standing-grant offer for a verdict, or a refusal
// when there is no gate to derive it from (tests wire an engine without a
// registry). A missing gate means no offer: the one thing this must never do
// is invent a rule for a policy it cannot see.
func (e *Engine) rememberOffer(verdict tools.Verdict) tools.RememberOffer {
	if e.tools == nil {
		return tools.RememberOffer{Reason: "there is no permission policy to add a rule to."}
	}
	policy := e.tools.Policy()
	if policy == nil {
		return tools.RememberOffer{Reason: "there is no permission policy to add a rule to."}
	}
	return policy.RememberOfferFor(verdict)
}

// rememberEventData renders an offer for the wire. Present keys are the fact:
// remember_pattern is set exactly when the control should appear, and
// remember_reason exactly when it should not.
func rememberEventData(offer tools.RememberOffer) map[string]any {
	if offer.Offered {
		return map[string]any{"remember_pattern": offer.Pattern, "remember_segment": offer.Segment}
	}
	if offer.Reason == "" {
		return nil
	}
	return map[string]any{"remember_reason": offer.Reason}
}

// executeTool runs an approved call through the registry, bracketed by the
// tool.started/tool.finished events — which therefore mark real executions
// only, never denied or declined attempts. A tool that describes itself as
// slow (tools.Progressive) also carries a label on tool.started for the
// overlay to show for the duration, and gets a spoken "still working" if it
// outlives the progress threshold.
//
// speaker is the turn's voice, passed through so that reassurance queues
// behind whatever is playing rather than over it — a slow tool can perfectly
// well be running while the sentence that introduced it is still being said.
//
// ok is false only when the session ended before execution could begin, in
// which case nothing ran and the tool loop must stop without reporting
// anything — the path that ended the session owns the events.
func (e *Engine) executeTool(s *sess, call ai.ToolCall, speaker *streamingSpeaker) (result string, ok bool) {
	// A tool call must never *begin* on a session that has already ended. The
	// tool loop's own guard reads s.ctx between calls without the lock, and
	// every session-ending path (failure, cancel, interruption, speech cancel)
	// runs on some other goroutine — so a session could fail after the loop's
	// check and before execution started, and the tool would then run for a
	// turn that had already published its failure. That is exactly what the
	// #111 incident log shows: "session failed", and then the tool executing
	// into the void. Checking e.current and the context *under the lock*
	// orders this decision against those paths, which all clear e.current and
	// cancel the context while holding mu: once a session's end is published,
	// no tool call of its can start. Work already inside Execute when the
	// session ends is a different matter, and handled the way the drain
	// discipline always has (#29/#54): it runs under s.ctx, which those paths
	// cancel, and unwinds at its next check.
	//
	// The same critical section records the call as the one in flight, so a
	// window opening mid-round can be told what is running (issue #158). It is
	// set here rather than after the publish because the record and the
	// decision to run must not be separable: a conversation.get between them
	// would see a live session with nothing running and word the pending turn
	// as a bare "Thinking".
	label, waiting, slow := e.tools.Activity(call)
	e.mu.Lock()
	live := e.current == s && s.ctx.Err() == nil
	if live {
		e.runningTool = call.Name
		e.runningToolDetail = ""
		if slow {
			e.runningToolDetail = label
		}
	}
	e.mu.Unlock()
	if !live {
		return "", false
	}
	started := map[string]any{"session_id": s.id, "tool": call.Name, "arguments": call.Arguments}
	if slow {
		started["detail"] = label
	}
	e.publish(Event{Type: "tool.started", Data: started})

	stopProgress := func() {}
	if slow {
		stopProgress = e.startToolProgress(s, call, waiting, speaker)
	}
	// Execution time is the user's command running, not Jarvix latency: an
	// excluded span, reported as tool_ms and kept out of jarvix_ms (#72).
	// start clocks the same span for this one call: tool_ms totals the
	// session's tool time, tool.finished's duration_ms belongs to this call.
	// A sink for what this one call has to say about itself (issue #168).
	// Most tools report nothing and are named from their own call; the two
	// that know something the arguments cannot — which file artifact.create
	// actually wrote, which conversations a search matched — note it here.
	var sink provenance.Sink
	s.timings.beginExcluded(StageToolRuns)
	start := time.Now()
	result = e.tools.Execute(provenance.WithSink(s.ctx, &sink), call)
	s.timings.endExcluded()
	stopProgress()
	// Derived from the call and its outcome, never from the model's words:
	// this ran, and this is what it returned.
	s.noteSources(toolSource(call, result, sink.Drain())...)

	// The finish event carries how long the call took and whether the
	// registry could run it at all, for the activity feed (issue #70).
	// "error" means the registry's own failure encoding — an unknown tool or
	// an infrastructure err — the one failure shape this layer can attest to.
	// A tool that ran and *refused* reports that in its result text to the
	// model, and on the bus through its own audit events (typing.audit,
	// desktop.refusal), where the reason travels with it.
	// Nothing is in flight any more. Guarded on identity so a call that
	// unwound late cannot clear a *later* call's record — the session-end
	// paths clear it too (setStateLocked), and either may get here first.
	e.mu.Lock()
	if e.runningTool == call.Name {
		e.runningTool, e.runningToolDetail = "", ""
	}
	e.mu.Unlock()

	outcome := "ok"
	if strings.HasPrefix(result, "error: ") {
		outcome = "error"
	}
	e.publish(Event{Type: "tool.finished", Data: map[string]any{
		"session_id": s.id, "tool": call.Name,
		"duration_ms": time.Since(start).Milliseconds(), "outcome": outcome}})
	return result, true
}

// confirmAndExecute runs the ask tier for a model tool call: ask, then
// execute if and only if the answer was yes. It runs on the think goroutine,
// which is exactly the point — the model's turn pauses on the user's word.
func (e *Engine) confirmAndExecute(s *sess, call ai.ToolCall, verdict tools.Verdict,
	turn spokenTurn) (string, bool) {
	outcome, alive := e.awaitConfirmation(s, confirmRequest{
		tool:         call.Name,
		command:      verdict.Command,
		summary:      verdict.Summary,
		rule:         verdict.Rule,
		key:          approvalKey(call, verdict),
		rememberable: tools.RememberableApproval(call.Name),
		remember:     e.rememberOffer(verdict),
		resume:       StateThinking,
		speaker:      turn.speaker,
	})
	if !alive {
		return "", false
	}
	switch outcome {
	case confirmDeclined:
		return e.refused(s, call.Name, verdict.Command, "declined", turn,
			"The user declined to run this command. It was not executed. "+
				"Do not retry it; acknowledge the decline and continue helping."), true
	case confirmTimedOut:
		return e.refused(s, call.Name, verdict.Command, "timeout", turn,
			"The user did not confirm in time, so the command was not executed. "+
				"Do not retry it unless asked again."), true
	case confirmUnavailable:
		return e.refused(s, call.Name, verdict.Command, "unavailable", turn,
			"The user could not be asked to confirm this command, so it was not "+
				"executed. Do not retry it; tell the user it did not run and ask "+
				"them again if they still want it."), true
	}
	return e.executeTool(s, call, turn.speaker)
}

// confirmRequest is one thing that needs the user's go-ahead. Model tool
// calls and user-defined voice intents both build one of these, so there is
// exactly one confirmation mechanism in the daemon — one state, one timeout,
// one set of events, one audit trail.
type confirmRequest struct {
	// tool is the identity shown and logged: a tool name for a model call,
	// tools.IntentToolName for a user-defined intent.
	tool string
	// command is the exact command being judged, published verbatim.
	command string
	// summary is the spoken question, generated daemon-side from command.
	summary string
	// rule names the policy rule that decided to ask.
	rule string
	// key identifies the request for remember_for_conversation.
	key string
	// rememberable is false for a tool whose approval must never be reused,
	// however remember_for_conversation is configured (tools.RememberableApproval).
	rememberable bool
	// remember is the standing-grant offer for this question (#162), derived
	// from the verdict by whichever caller has one. The zero value is a
	// refusal with no reason, which renders as "no remember control" — the
	// safe default for any future caller that forgets to fill it in.
	remember tools.RememberOffer
	// resume is the state to return to once answered.
	resume State
	// speaker is the turn's streaming speaker when it has one. The question is
	// queued on it rather than played on a stream of its own, so it cannot talk
	// over a sentence still playing (issue #52). Nil for a user-defined intent,
	// which asks before it has said anything and so has no stream to queue
	// behind.
	speaker *streamingSpeaker
}

// confirmOutcome is how a confirmation ended. Each is distinct because the
// model is told something different about each: "the user said no", "the user
// did not answer", and "the user was never asked" are three different things
// to have to explain, and collapsing them would put words in the user's mouth.
type confirmOutcome int

const (
	confirmApproved confirmOutcome = iota
	confirmDeclined
	confirmTimedOut
	// confirmUnavailable means the gate could not be entered at all — the
	// session was in a state with no legal path to AwaitingConfirmation. It is
	// the safe reading of a bug: nothing runs, the model is told the truth, and
	// the conversation continues.
	confirmUnavailable
)

// awaitConfirmation runs the ask tier: enter AwaitingConfirmation, speak the
// summary, and block the caller's goroutine until the user answers, the
// timeout fires, or the session dies. alive is false only when the session
// ended underneath it, in which case the caller must stop without reporting
// anything — the cancel path owns those events.
func (e *Engine) awaitConfirmation(s *sess, req confirmRequest) (outcome confirmOutcome, alive bool) {
	timeout := e.opts.ConfirmTimeout
	if timeout <= 0 {
		timeout = DefaultConfirmTimeout
	}

	e.mu.Lock()
	if e.current != s || s.ctx.Err() != nil {
		e.mu.Unlock()
		return confirmDeclined, false
	}
	if e.opts.RememberApprovals && req.rememberable && e.approvals[req.key] {
		e.mu.Unlock()
		e.log.Info("tool call allowed", "component", "tools", "tool", req.tool,
			"command", req.command, "rule", req.rule, "source", "remembered approval")
		// remember_for_conversation's fast path was the one place Jarvix
		// re-ran an approved command with nothing on the bus to say so
		// (#162's audit criterion applies to it too — it is the same promise,
		// and it was already being broken before a single rule existed).
		// Published outside the lock, like every other event on this path.
		e.publish(Event{Type: "tool.pre_approved", Data: map[string]any{
			"session_id": s.id, "tool": req.tool, "command": req.command,
			"rule":  fmt.Sprintf("you approved %q earlier in this conversation", req.command),
			"scope": string(tools.RememberConversation),
		}})
		return confirmApproved, true
	}
	if err := e.setStateLocked(StateAwaitingConfirmation); err != nil {
		// Never fatal. This used to be e.fail, which killed the conversation
		// mid-turn the first time a legal-but-unenumerated state reached the
		// gate — Speaking, in issue #52 — and the user was left with a promise
		// Jarvix had already made out loud and a session that had silently
		// stopped existing. A transition the table does not allow is a defect
		// in this package, and the user should never pay for it with their
		// turn: the safe reading is that the command was not confirmed, so it
		// does not run, the model is told so, and the answer continues.
		//
		// It is logged at error level, with the transition, because it means
		// exactly one thing: a state reached the tool path that toolRequestStates
		// does not know about.
		e.mu.Unlock()
		e.log.Error("tool confirmation could not be requested", "component", "session",
			"session_id", s.id, "tool", req.tool, "command", req.command, "error", err.Error())
		e.publish(Event{Type: "tool.declined", Data: map[string]any{
			"session_id": s.id, "tool": req.tool, "command": req.command, "source": "unavailable"}})
		return confirmUnavailable, true
	}
	// The prompt's own context, a child of the session's: resolving the
	// confirmation cancels it so a mid-read-out answer silences the rest of
	// the question (issue #119), while a session-ending cancel still kills it
	// like everything else. Created before the pending is visible so no
	// resolution can ever find stopPrompt unarmed.
	promptCtx, stopPrompt := context.WithCancel(s.ctx)
	defer stopPrompt()
	p := &pendingConfirmation{
		tool:       req.tool,
		command:    req.command,
		key:        req.key,
		summary:    req.summary,
		rule:       req.rule,
		remember:   req.remember,
		timeout:    timeout,
		resume:     req.resume,
		outcome:    make(chan bool, 1),
		stopPrompt: stopPrompt,
	}
	e.pending = p
	// The exact command goes on the bus before anything is spoken: the
	// overlay must show what the user is confirming, verbatim.
	confirmData := map[string]any{
		"session_id": s.id, "tool": req.tool, "command": req.command,
		"summary": req.summary, "rule": req.rule,
		"timeout_sec": int(timeout.Seconds()),
	}
	// The remember offer rides the same event as the question (#162), so the
	// card can show the exact rule on the button before the user commits and
	// never has to ask a second round trip what it is about to write. The
	// refusal rides it too: a card that simply omitted the control would
	// leave the user wondering, and "one short sentence saying why" is an
	// acceptance criterion, not a nicety.
	for k, v := range rememberEventData(req.remember) {
		confirmData[k] = v
	}
	e.publish(Event{Type: "tool.confirmation_required", Data: confirmData})
	e.mu.Unlock()
	e.log.Info("tool confirmation required", "component", "tools", "tool", req.tool,
		"command", req.command, "rule", req.rule)

	// Speak first, then start the clock: the user's 30 seconds begin when
	// the question has been asked, not while it is still being said. keep is
	// true: a confirmation question gates progress and must survive
	// cross-turn supersession (issue #120) — see utterance.keep.
	e.speakPrompt(s, promptCtx, e.spokenConfirmationPrompt(req), req.speaker, true)

	// The clock is starting *now*, which is later than the event above went
	// out — so the deadline gets an announcement of its own rather than a
	// guess in confirmation_required. The window's countdown derives from
	// this timestamp (issue #76): a number the daemon computed from the
	// configured timeout, never a client-side hardcoded 30. Guarded because
	// an answer can land while the question is still being spoken — a
	// deadline for a confirmation that no longer exists must not go out.
	e.mu.Lock()
	if e.pending == p {
		p.deadline = time.Now().Add(timeout)
		e.publish(Event{Type: "tool.confirmation_deadline", Data: map[string]any{
			"session_id": s.id, "tool": p.tool, "command": p.command,
			"deadline_ms": p.deadline.UnixMilli(),
			"timeout_sec": int(timeout.Seconds()),
		}})
	}
	e.mu.Unlock()

	// From here the turn is waiting on the user's decision — time that
	// belongs to neither Jarvix nor the model. It accrues as the excluded
	// confirm_wait_ms span (#72), opened exactly where the timeout clock
	// starts and closed however the wait ends.
	s.timings.beginExcluded(StageConfirmWait)
	defer s.timings.endExcluded()

	timerC, stopTimer := e.timer(timeout)
	defer stopTimer()
	for {
		select {
		case approved := <-p.outcome:
			if !approved {
				return confirmDeclined, true
			}
			if e.opts.RememberApprovals && req.rememberable {
				e.mu.Lock()
				e.approvals[req.key] = true
				e.mu.Unlock()
			}
			return confirmApproved, true
		case <-timerC:
			e.mu.Lock()
			if e.pending != p || p.engaged {
				// Resolved a moment ago (the outcome is on its way), or the
				// user is mid voice-reply — either way the timeout no longer
				// applies. A nil channel never fires again.
				e.mu.Unlock()
				timerC = nil
				continue
			}
			e.pending = nil
			e.forceStateLocked(p.resumeState())
			// Recorded as its own outcome, never conflated with a spoken no
			// (issue #118): "the user did not answer" and "the user said no"
			// are different facts, on disk as much as to the model.
			e.recordConfirmationLocked(p, conversations.ConfirmationTimedOut, "timeout")
			e.publish(Event{Type: "tool.declined", Data: e.confirmationData(p, "timeout")})
			e.mu.Unlock()
			e.log.Info("tool confirmation timed out", "component", "tools",
				"tool", req.tool, "command", req.command)
			return confirmTimedOut, true
		case <-s.ctx.Done():
			// Cancelled or superseded; the cancel path published the events.
			return confirmDeclined, false
		}
	}
}

// spokenConfirmationPrompt is the question actually said aloud for one
// confirmation request (issue #119). Two modes, chosen by configuration:
//
//   - default: a short prompt naming the kind of action and pointing at the
//     screen. Honest by construction — the card and the overlay really do
//     show the details, verbatim, because the events and the snapshot carry
//     req.summary and the exact command whatever is spoken (ADR 0014's
//     display doctrine is untouched; only the audio is abbreviated).
//   - confirmations.speak_details = true: the full generated summary, exactly
//     as it has always been read out — the wording is composed in
//     internal/tools from the parsed command and is not re-derived here.
//
// The mode never changes what is displayed, recorded, or allowed: the record
// keeps the full summary as its text, and the safety decision waits on the
// same answer either way.
func (e *Engine) spokenConfirmationPrompt(req confirmRequest) string {
	if e.opts.SpeakConfirmationDetails {
		return req.summary
	}
	return shortConfirmationPrompt(req.tool)
}

// shortConfirmationPrompt words the default spoken ask for a tool identity:
// the action class in plain words, then "the details are on screen" — which
// is where the verbatim command actually is. Keyed on the gate's tool
// identities, not on anything the model said; an identity the table does not
// know names itself, so a future tool is never announced as something
// friendlier than its own name.
//
// The class comes from desktop.ToolActionAsk rather than from a switch here
// (issue #158) because the conversation window's pending turn needs the same
// fact in the present tense — "Running a shell command" while it runs. One
// table, two grammatical forms: the gate and the transcript cannot end up
// describing one capability two ways.
func shortConfirmationPrompt(tool string) string {
	return fmt.Sprintf("May I %s? The details are on screen.", desktop.ToolActionAsk(tool))
}

// speakPrompt asks the confirmation question out loud.
//
// When the turn has a streaming speaker it goes on that speaker's queue, and
// that is the whole answer to "do not talk over the audio already playing": one
// queue, one audio.Player.Play, one voice at a time. Playing the question on a
// stream of its own — which is what this function used to do unconditionally —
// meant that the instant streaming speech and an ask-tier tool call overlapped,
// the user heard the question mixed into the sentence before it (issue #52).
// The speaker knows not to claim the Speaking state or emit tts.* events for a
// question, so the session still sits visibly in AwaitingConfirmation while it
// plays and the overlay's cue remains the confirmation_required event.
//
// Without a speaker — a user-defined intent, which asks before it has said
// anything — there is nothing to overlap with, and the question synthesizes and
// plays directly here, with no state change and no tts.* events.
//
// ctx is the prompt's own context (a child of the session's): a resolution
// arriving mid-read-out cancels it, which stops the remaining audio on either
// path without touching the turn (issue #119).
//
// keep is the aside's supersession exemption (issue #120), passed through to
// the speaker's queue: true for a confirmation question (it gates progress
// and must play however far the answer has moved on), false for a progress
// reassurance (stale comfort drops). Meaningless on the direct path, which
// has no queue for a newer turn to supersede. See utterance.keep for the
// pinned policy.
//
// Failures degrade to silence either way: the overlay still shows the question
// and the timeout still declines, so a broken voice never blocks the safety
// decision.
func (e *Engine) speakPrompt(s *sess, ctx context.Context, text string, speaker *streamingSpeaker, keep bool) {
	// A quiet session (ADR 0032) never reaches a confirmation on the intended
	// path — the daemon refuses an ask-tier clockfire before a session exists
	// — but belt and braces: if one ever does, the overlay still shows the
	// question and the timeout still declines; the one thing that must not
	// happen is a voice asking an empty room at 3am.
	if s.quiet || !e.opts.SpeakResponses || e.tts == nil || e.player == nil {
		return
	}
	if speaker != nil {
		speaker.interject(ctx, text, keep)
		return
	}
	spoken := e.spokenForm(text)
	if spoken == "" {
		return
	}
	// This is the one stretch of audio no streaming speaker accounts for, so
	// it registers itself: CancelSpeech asks the session's voice whether
	// anything is playing (issue #54), and a question asked on the direct path
	// must be stoppable exactly like one queued on a speaker. Marked before
	// synthesis, not before playback — killing the question must not wait on
	// the synthesizer.
	e.mu.Lock()
	s.promptAudio = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		s.promptAudio = false
		e.mu.Unlock()
	}()
	// Everything below runs under the prompt's context, not the session's: a
	// resolution mid-read-out cancels ctx and the synthesis and playback both
	// unwind here, while the session — and whatever it says next — carries on.
	format, chunks, err := e.tts.Speak(ctx, tts.Request{Text: spoken})
	if err != nil {
		e.log.Warn("confirmation prompt could not be spoken", "component", "tts", "error", err.Error())
		return
	}
	pcm := make(chan []byte, 8)
	playDone := make(chan error, 1)
	go func() {
		playDone <- e.player.Play(ctx, format.SampleRate, format.Channels, pcm)
	}()
	var synthErr error
	for c := range chunks {
		if synthErr != nil || ctx.Err() != nil {
			continue // drain so the synthesizer can exit
		}
		if c.Err != nil {
			synthErr = c.Err
			continue
		}
		select {
		case pcm <- c.PCM:
		case <-ctx.Done():
		}
	}
	close(pcm)
	playErr := <-playDone
	if ctx.Err() != nil {
		// Cancelled: the session ended, or the answer arrived while the
		// question was still being said — a stopped prompt, not a broken voice.
		return
	}
	if synthErr == nil {
		synthErr = playErr
	}
	if synthErr != nil {
		e.log.Warn("confirmation prompt could not be spoken", "component", "tts", "error", synthErr.Error())
	}
}

// PendingConfirmationInfo describes the tool call currently waiting on the
// user, for the conversation snapshot (issue #76): a window opened during the
// wait must be able to render the question, the exact command, and the
// countdown without having seen the events that announced them.
type PendingConfirmationInfo struct {
	Tool    string
	Command string
	Summary string
	Rule    string
	// Timeout is the configured confirmation window; Deadline is when it
	// expires, zero while the question is still being spoken (the clock has
	// not started yet).
	Timeout  time.Duration
	Deadline time.Time
	// Remember is the standing-grant offer (#162) — the same one the
	// confirmation_required event carried, so a window that opened mid-wait
	// renders the identical third button rather than a differently-worded
	// one derived from what it can see.
	Remember tools.RememberOffer
}

// PendingConfirmation reports the confirmation the session is waiting on, if
// any. It answers whenever one is pending — including while a voice reply to
// it is being captured — because the question remains open until resolved,
// whatever intermediate state the answer passes through.
func (e *Engine) PendingConfirmation() (PendingConfirmationInfo, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.pending
	if p == nil {
		return PendingConfirmationInfo{}, false
	}
	return PendingConfirmationInfo{
		Tool:     p.tool,
		Command:  p.command,
		Summary:  p.summary,
		Rule:     p.rule,
		Timeout:  p.timeout,
		Deadline: p.deadline,
		Remember: p.remember,
	}, true
}

// approvalKey identifies a call for remember_for_conversation: the tool plus
// the exact command (or raw arguments), so an approval never bleeds onto a
// different command that merely shares a tool.
func approvalKey(call ai.ToolCall, verdict tools.Verdict) string {
	if verdict.Command != "" {
		return call.Name + "\x00" + strings.TrimSpace(verdict.Command)
	}
	return call.Name + "\x00" + strings.TrimSpace(call.Arguments)
}

// Affirmative vocabulary for spoken/typed confirmation replies. Matching is
// deliberately strict: any negation anywhere declines, only a clear leading
// yes-word or a known whole phrase approves, and everything else — silence,
// a question, a new topic — declines. Executing on a misheard "yes" is the
// failure mode this gate exists to prevent.
var (
	affirmativeWords = map[string]bool{
		"yes": true, "yeah": true, "yep": true, "yup": true, "aye": true,
		"sure": true, "ok": true, "okay": true, "confirm": true,
		"confirmed": true, "affirmative": true, "proceed": true,
	}
	affirmativePhrases = map[string]bool{
		"go ahead": true, "do it": true, "go for it": true,
		"please do": true, "sounds good": true, "of course": true,
	}
	negationWords = map[string]bool{
		"no": true, "not": true, "don't": true, "dont": true, "nope": true,
		"stop": true, "cancel": true, "never": true, "wait": true,
	}
)

// isAffirmative interprets a confirmation reply.
func isAffirmative(text string) bool {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == ' ':
			b.WriteRune(r)
		case r == '\'' || r == '’':
			b.WriteRune('\'')
		default:
			b.WriteRune(' ')
		}
	}
	fields := strings.Fields(b.String())
	if len(fields) == 0 {
		return false
	}
	for _, f := range fields {
		if negationWords[f] {
			return false
		}
	}
	if affirmativePhrases[strings.Join(fields, " ")] {
		return true
	}
	return affirmativeWords[fields[0]]
}
