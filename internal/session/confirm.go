package session

import (
	"fmt"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
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
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pending == nil {
		return fmt.Errorf("no tool confirmation is pending")
	}
	if e.state != StateAwaitingConfirmation {
		return fmt.Errorf("a voice reply is already being captured; finish it or cancel the session")
	}
	e.resolveConfirmationLocked(approved, "cli")
	return nil
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
	e.pending = nil
	e.forceStateLocked(p.resumeState())
	eventType := "tool.confirmed"
	if !approved {
		eventType = "tool.declined"
	}
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
	e.pending = nil
	e.publish(Event{Type: "tool.declined", Data: e.confirmationData(p, source)})
	e.log.Info("tool confirmation abandoned", "component", "tools", "tool", p.tool,
		"command", p.command, "source", source)
}

func (e *Engine) confirmationData(p *pendingConfirmation, source string) map[string]any {
	id := ""
	if e.current != nil {
		id = e.current.id
	}
	return map[string]any{"session_id": id, "tool": p.tool, "command": p.command, "source": source}
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
	verdict := e.tools.Check(call)
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
		e.log.Debug("tool call allowed", "component", "tools", "tool", call.Name,
			"command", verdict.Command, "rule", verdict.Rule, "source", "policy")
		return e.executeTool(s, call, turn.speaker), true
	}
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
func (e *Engine) executeTool(s *sess, call ai.ToolCall, speaker *streamingSpeaker) string {
	started := map[string]any{"session_id": s.id, "tool": call.Name, "arguments": call.Arguments}
	label, waiting, slow := e.tools.Activity(call)
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
	s.timings.beginExcluded(StageToolRuns)
	result := e.tools.Execute(s.ctx, call)
	s.timings.endExcluded()
	stopProgress()

	e.publish(Event{Type: "tool.finished", Data: map[string]any{
		"session_id": s.id, "tool": call.Name}})
	return result
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
	return e.executeTool(s, call, turn.speaker), true
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
	p := &pendingConfirmation{
		tool:    req.tool,
		command: req.command,
		key:     req.key,
		resume:  req.resume,
		outcome: make(chan bool, 1),
	}
	e.pending = p
	// The exact command goes on the bus before anything is spoken: the
	// overlay must show what the user is confirming, verbatim.
	e.publish(Event{Type: "tool.confirmation_required", Data: map[string]any{
		"session_id": s.id, "tool": req.tool, "command": req.command,
		"summary": req.summary, "rule": req.rule,
		"timeout_sec": int(timeout.Seconds()),
	}})
	e.mu.Unlock()
	e.log.Info("tool confirmation required", "component", "tools", "tool", req.tool,
		"command", req.command, "rule", req.rule)

	// Speak first, then start the clock: the user's 30 seconds begin when
	// the question has been asked, not while it is still being said.
	e.speakPrompt(s, req.summary, req.speaker)

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
// Failures degrade to silence either way: the overlay still shows the question
// and the timeout still declines, so a broken voice never blocks the safety
// decision.
func (e *Engine) speakPrompt(s *sess, text string, speaker *streamingSpeaker) {
	if !e.opts.SpeakResponses || e.tts == nil || e.player == nil {
		return
	}
	if speaker != nil {
		speaker.interject(text)
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
	format, chunks, err := e.tts.Speak(s.ctx, tts.Request{Text: spoken})
	if err != nil {
		e.log.Warn("confirmation prompt could not be spoken", "component", "tts", "error", err.Error())
		return
	}
	pcm := make(chan []byte, 8)
	playDone := make(chan error, 1)
	go func() {
		playDone <- e.player.Play(s.ctx, format.SampleRate, format.Channels, pcm)
	}()
	var synthErr error
	for c := range chunks {
		if synthErr != nil || s.ctx.Err() != nil {
			continue // drain so the synthesizer can exit
		}
		if c.Err != nil {
			synthErr = c.Err
			continue
		}
		select {
		case pcm <- c.PCM:
		case <-s.ctx.Done():
		}
	}
	close(pcm)
	playErr := <-playDone
	if s.ctx.Err() != nil {
		return
	}
	if synthErr == nil {
		synthErr = playErr
	}
	if synthErr != nil {
		e.log.Warn("confirmation prompt could not be spoken", "component", "tts", "error", synthErr.Error())
	}
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
