package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/tools"
)

// This file is the engine half of the deterministic intent router (ADR 0017).
// internal/intent owns the grammar and the argv table; the engine owns what a
// hit means for a session: a state, an action, an acknowledgement, a history
// entry, and an event — with no provider request anywhere in it.
//
// The seam is maybeThinkLocked. That is the exact point where a final
// transcript would otherwise become a model call, and putting the router
// there is what makes the miss path byte-identical to before: on a miss the
// function continues into Thinking as it always did, one map lookup later.

// errIntentDeclined marks a user-defined intent the user refused at the
// permission gate. It is a "failure" only in the sense that nothing ran; the
// acknowledgement says so plainly rather than apologising for a bug.
var errIntentDeclined = errors.New("declined")

// RoutineRunner executes one named routine (ADR 0026). It is the engine's
// view of internal/routine's Runner, declared as an interface here so session
// tests substitute a fake and never place a window. The contract mirrors the
// runner's: the returned string is the one spoken summary, partial failure is
// *in* the summary rather than an error, and err is reserved for a run that
// could not happen at all (already running, unknown name, no compositor) or
// was cancelled — in which case the engine's cancel path owns the silence.
type RoutineRunner interface {
	Run(ctx context.Context, name string) (string, error)
}

// ScriptRunner executes one named script (ADR 0030). It is the engine's view
// of internal/script's Runner, declared as an interface here so session tests
// substitute a fake and never execute a file. The contract mirrors the
// runner's: the returned string is what success says (empty for a silent
// mode), and err covers everything else — could not run, cancelled, or
// failed — which the engine speaks in every report mode, so a failure can
// never be configured into silence. Path exists for the gate: the
// confirmation must name the exact file about to run, from the same source
// of truth that will run it.
type ScriptRunner interface {
	Run(ctx context.Context, name string) (string, error)
	Path(name string) (string, bool)
}

// routeIntentLocked offers the transcript to the intent table. It reports
// whether the router claimed the utterance; false means the caller proceeds
// to the model exactly as it did before this feature existed.
//
// Must be called with e.mu held and the session already validated.
func (e *Engine) routeIntentLocked(s *sess) bool {
	if e.opts.Intents == nil {
		return false
	}
	m, ok := e.opts.Intents.Match(s.transcript)
	if !ok {
		return false
	}
	// Capture the utterance now: a user-defined intent may pause for a spoken
	// confirmation, and that reply overwrites s.transcript. The conversation
	// must remember "lock the screen", not "yes".
	utterance := s.transcript
	if err := e.setStateLocked(StateActing); err != nil {
		e.failLocked(s, "session", err)
		return true
	}
	// Tracked in e.active like think(): the tail of runIntent is where an
	// intent turn's history and archive writes live, and an untracked goroutine
	// there is invisible to both Shutdown's drain and Reconfigure's — a daemon
	// stop (or an engine rebuild) could then race the archive append, which is
	// exactly the post-session-work loss #29 closed for the think path (#74).
	started := time.Now()
	e.active.Go(func() { e.runIntent(s, m, utterance, started) })
	return true
}

// runIntent carries out a matched intent off the engine lock: act, announce,
// remember, finish. It runs on its own goroutine for the same reason think()
// does — speaking blocks, and mu must never be held across audio.
func (e *Engine) runIntent(s *sess, m intent.Match, utterance string, started time.Time) {
	if m.Control == intent.ControlStopSpeech {
		// "Stop" is answered with silence: an acknowledgement would be the
		// one thing the user just asked for less of. Speech is halted through
		// the one stop mechanism, CancelSpeech, which asks the speaker itself
		// whether audio is live rather than reading the session state — so it
		// works whatever the turn happens to be doing internally (issue #54).
		// On a push-to-talk "stop" the interrupting session.start has usually
		// silenced the old session already and this reports a no-op; a
		// wake-word "stop" (ADR 0024) can arrive with the speaking session
		// still current, and then this is the call that does the stopping.
		e.CancelSpeech()
		e.publishIntent(s, m, "", nil, started)
		e.mu.Lock()
		e.finishLocked(s)
		e.mu.Unlock()
		return
	}

	ack := m.Ack
	var runErr error
	switch {
	case m.Control == intent.ControlNewConversation:
		e.ResetConversation()
	case m.UserDefined:
		var alive bool
		runErr, alive = e.runUserIntent(s, m)
		if !alive {
			return // cancelled or superseded; that path owns the events
		}
	case m.Routine != "":
		var alive bool
		ack, runErr, alive = e.runRoutine(s, m)
		if !alive {
			return // cancelled or superseded; that path owns the events
		}
	case m.Script != "":
		var alive bool
		ack, runErr, alive = e.runScript(s, m)
		if !alive {
			return // cancelled or superseded; that path owns the events
		}
	case m.CaptureName != "":
		var alive bool
		ack, runErr, alive = e.runCapture(s, m)
		if !alive {
			return // cancelled or superseded; that path owns the events
		}
	case m.Desktop != intent.DesktopNone:
		runErr = e.runDesktopIntent(s, m)
	case len(m.Argv) > 0:
		runErr = e.intentRunner().Run(s.ctx, m.Argv)
	}
	if s.ctx.Err() != nil {
		return
	}
	if runErr != nil {
		// A failing command must never leave a stuck session: Jarvix says one
		// line about it and the session completes normally.
		ack = intentFailureAck(runErr)
		e.log.Warn("intent failed", "component", "intent", "session_id", s.id,
			"intent", m.Name, "error", runErr.Error())
	}

	// The router's answer exists now, which is this path's equivalent of a
	// provider's first token. Marking it keeps the latency report coherent
	// across both paths (ADR 0018): the "thinking" stage becomes the intent's
	// own execution time, and jarvix_ms correctly claims the whole budget —
	// there is no model here to blame any of it on.
	s.timings.markFirstDelta()
	e.publishIntent(s, m, ack, runErr, started)
	e.speakAck(s, ack)

	// A matched intent is still a turn of the conversation: recording it means
	// a follow-up that *does* reach the model ("a bit louder than that") knows
	// what just happened. The exception is conversation.new, whose entire
	// purpose is an empty history — re-seeding it with the reset itself would
	// undo the thing the user asked for.
	if m.Control != intent.ControlNewConversation {
		recorded := ack
		if recorded == "" {
			// A silent script success (ADR 0030) speaks nothing, but the
			// record must not carry an empty assistant turn: providers reject
			// empty messages, and the archive would hold a shrug where an
			// action happened. The ear stays silent; the history says "Done."
			recorded = "Done."
		}
		e.commitTurn(utterance, recorded)
	}

	e.mu.Lock()
	e.finishLocked(s)
	e.mu.Unlock()
	e.persistHistory()
	e.persistArchive()
}

// runDesktopIntent carries out an intent that acts on the compositor —
// "workspace four", "open a terminal" — through the seam the window tools use
// (ADR 0022) rather than by running an hyprctl command line of its own.
//
// That indirection is the fix for #47, and it buys two things the old fixed
// argv could not have. The dispatch is written in the dialect this machine's
// compositor was probed for, so it works whether the user configures Hyprland
// in Lua or hyprlang; and a dispatch the compositor *refused* comes back as an
// error, because the seam reads the reply rather than trusting hyprctl's exit
// code. Both halves matter equally: the first made the action fail, the second
// made the failure inaudible.
func (e *Engine) runDesktopIntent(s *sess, m intent.Match) error {
	compositor := e.opts.Compositor
	if compositor == nil {
		// No seam wired: off a graphical session, or a daemon built without
		// one. Saying so is the entire point — the alternative is the
		// acknowledgement without the action that #47 was about.
		return fmt.Errorf("I cannot reach the window manager")
	}
	ctx, cancel := context.WithTimeout(s.ctx, intent.DefaultTimeout)
	defer cancel()
	switch m.Desktop {
	case intent.DesktopWorkspace:
		return compositor.SwitchWorkspace(ctx, m.Slot)
	case intent.DesktopSpawn:
		return compositor.Spawn(ctx, m.Program)
	default:
		// Unreachable for a compiled table; a new action added without a case
		// here must be a spoken failure, never a silent success.
		return fmt.Errorf("I do not know how to do that on this desktop")
	}
}

// runUserIntent executes a configured intent's command through the tool
// permission gate. User-written or not, it is arbitrary shell execution
// triggered by speech, so it faces the same allow/ask/deny classifier and the
// same spoken confirmation as anything the model asks for (ADR 0014) — the
// gate is reused wholesale rather than reimplemented with softer rules.
func (e *Engine) runUserIntent(s *sess, m intent.Match) (runErr error, alive bool) {
	verdict := e.intentVerdict(m.Command)
	switch verdict.Decision {
	case tools.PolicyDeny:
		e.log.Info("intent denied", "component", "tools", "tool", tools.IntentToolName,
			"command", verdict.Command, "rule", verdict.Rule, "source", "policy")
		e.publish(Event{Type: "tool.denied", Data: map[string]any{
			"session_id": s.id, "tool": tools.IntentToolName,
			"command": verdict.Command, "rule": verdict.Rule}})
		return fmt.Errorf("that command is not permitted (%s)", verdict.Rule), true
	case tools.PolicyAsk:
		outcome, ok := e.awaitConfirmation(s, confirmRequest{
			tool:    tools.IntentToolName,
			command: verdict.Command,
			summary: verdict.Summary,
			rule:    verdict.Rule,
			key:     tools.IntentToolName + "\x00" + verdict.Command,
			// A user-defined intent is a command the user wrote themselves, so
			// remembering its approval is exactly what the setting is for.
			rememberable: tools.RememberableApproval(tools.IntentToolName),
			resume:       StateActing,
		})
		if !ok {
			return nil, false
		}
		if outcome == confirmUnavailable {
			// The gate could not be entered — a defect in the state machine,
			// never the user's answer. "Cancelled." would put a decision in
			// their mouth they never made, so this says what happened instead.
			return errors.New("I could not ask you to confirm that, so I have not run it"), true
		}
		if outcome != confirmApproved {
			return errIntentDeclined, true
		}
	default:
		e.log.Debug("intent allowed", "component", "tools", "tool", tools.IntentToolName,
			"command", verdict.Command, "rule", verdict.Rule, "source", "policy")
	}
	return e.intentRunner().RunShell(s.ctx, m.Command), true
}

// runRoutine carries out a matched routine phrase (ADR 0026) through the
// routine runner, behind its own gate identity. The routine's summary is the
// acknowledgement — the whole run speaks exactly once, at the end — which is
// why this returns ack where the other intent paths return only an error.
//
// The gate mirrors runUserIntent's shape deliberately: same confirmation
// mechanism, same events, same audit trail. Only the identity (routine.run,
// default allow — the user authored every step) and the absence of a shell
// classifier differ.
func (e *Engine) runRoutine(s *sess, m intent.Match) (ack string, runErr error, alive bool) {
	if e.opts.Routines == nil {
		return "", fmt.Errorf("routines are not available on this daemon"), true
	}
	verdict := e.routineVerdict(m.Routine)
	switch verdict.Decision {
	case tools.PolicyDeny:
		e.log.Info("routine denied", "component", "tools", "tool", tools.RoutineToolName,
			"routine", m.Routine, "rule", verdict.Rule, "source", "policy")
		e.publish(Event{Type: "tool.denied", Data: map[string]any{
			"session_id": s.id, "tool": tools.RoutineToolName,
			"command": verdict.Command, "rule": verdict.Rule}})
		return "", fmt.Errorf("that routine is not permitted (%s)", verdict.Rule), true
	case tools.PolicyAsk:
		outcome, ok := e.awaitConfirmation(s, confirmRequest{
			tool:    tools.RoutineToolName,
			command: verdict.Command,
			summary: verdict.Summary,
			rule:    verdict.Rule,
			key:     tools.RoutineToolName + "\x00" + verdict.Command,
			// A routine is entirely the user's own configuration, so a
			// remembered approval reproduces exactly what was approved.
			rememberable: tools.RememberableApproval(tools.RoutineToolName),
			resume:       StateActing,
		})
		if !ok {
			return "", nil, false
		}
		if outcome == confirmUnavailable {
			return "", errors.New("I could not ask you to confirm that, so I have not run it"), true
		}
		if outcome != confirmApproved {
			return "", errIntentDeclined, true
		}
	default:
		e.log.Debug("routine allowed", "component", "tools", "tool", tools.RoutineToolName,
			"routine", m.Routine, "rule", verdict.Rule, "source", "policy")
	}
	summary, err := e.opts.Routines.Run(s.ctx, m.Routine)
	return summary, err, true
}

// runScript carries out a matched script phrase (ADR 0030) through the
// script runner, behind its own gate identity. The shape is runRoutine's —
// same confirmation mechanism, same events, same audit trail — but the stance
// is inverted where it matters: script.run defaults to ask, because a script
// is an arbitrary executable behind a possibly-misheard phrase, and the
// confirmation names both the script and the exact file about to run so a
// substituted path is visible in the question itself.
func (e *Engine) runScript(s *sess, m intent.Match) (ack string, runErr error, alive bool) {
	if e.opts.Scripts == nil {
		return "", fmt.Errorf("scripts are not available on this daemon"), true
	}
	path, known := e.opts.Scripts.Path(m.Script)
	if !known {
		return "", fmt.Errorf("no script is called %q", m.Script), true
	}
	verdict := e.scriptVerdict(m.Script, path)
	switch verdict.Decision {
	case tools.PolicyDeny:
		e.log.Info("script denied", "component", "tools", "tool", tools.ScriptToolName,
			"script", m.Script, "rule", verdict.Rule, "source", "policy")
		e.publish(Event{Type: "tool.denied", Data: map[string]any{
			"session_id": s.id, "tool": tools.ScriptToolName,
			"command": verdict.Command, "rule": verdict.Rule}})
		return "", fmt.Errorf("that script is not permitted (%s)", verdict.Rule), true
	case tools.PolicyAsk:
		outcome, ok := e.awaitConfirmation(s, confirmRequest{
			tool:    tools.ScriptToolName,
			command: verdict.Command,
			summary: verdict.Summary,
			rule:    verdict.Rule,
			// The key carries name AND path (verdict.Command holds both), so
			// a remembered approval reproduces exactly what was approved: the
			// same entry pointing at the same file. Repoint the config and
			// the remembered approval no longer applies.
			key:          tools.ScriptToolName + "\x00" + verdict.Command,
			rememberable: tools.RememberableApproval(tools.ScriptToolName),
			resume:       StateActing,
		})
		if !ok {
			return "", nil, false
		}
		if outcome == confirmUnavailable {
			return "", errors.New("I could not ask you to confirm that, so I have not run it"), true
		}
		if outcome != confirmApproved {
			return "", errIntentDeclined, true
		}
	default:
		e.log.Info("script allowed", "component", "tools", "tool", tools.ScriptToolName,
			"script", m.Script, "rule", verdict.Rule, "source", "policy")
	}
	line, err := e.opts.Scripts.Run(s.ctx, m.Script)
	return line, err, true
}

// scriptVerdict classifies a script by name and path. With no registry wired
// (tests) there is no gate to consult, and an ungated arbitrary executable
// must not run silently — so the safe reading is "ask", the same stance
// intentVerdict takes for a bare command and the opposite of routineVerdict's.
func (e *Engine) scriptVerdict(name, path string) tools.Verdict {
	if e.tools == nil {
		return tools.Verdict{
			Decision: tools.PolicyAsk, Tool: tools.ScriptToolName,
			Command: name + " (" + path + ")",
			Rule:    "no permission gate installed",
			Summary: fmt.Sprintf("I'm about to run your %s script, at %s. Should I go ahead?", name, path),
		}
	}
	return e.tools.CheckScript(name, path)
}

// routineVerdict classifies a routine by name. With no registry wired (tests)
// the shipped default applies — allow — because unlike a bare shell command,
// a routine's steps were validated at load and authored by the user.
func (e *Engine) routineVerdict(name string) tools.Verdict {
	if e.tools == nil {
		return tools.Verdict{Decision: tools.PolicyAllow, Tool: tools.RoutineToolName,
			Command: name, Rule: "no permission gate installed"}
	}
	return e.tools.CheckRoutine(name)
}

// intentVerdict classifies a user-defined command. With no registry wired
// (tests) there is no gate to consult, and an ungated shell command must not
// run silently — so the safe reading is "ask".
func (e *Engine) intentVerdict(command string) tools.Verdict {
	if e.tools == nil {
		return tools.Verdict{
			Decision: tools.PolicyAsk, Tool: tools.IntentToolName, Command: command,
			Rule:    "no permission gate installed",
			Summary: fmt.Sprintf("I want to run %q. Should I go ahead?", command),
		}
	}
	return e.tools.CheckCommand(tools.IntentToolName, command)
}

// intentRunner returns the configured runner. NewEngine installs a real one
// alongside a router, so this is nil only when a caller passed a router and
// then cleared the runner.
func (e *Engine) intentRunner() intent.Runner {
	if e.opts.IntentRunner != nil {
		return e.opts.IntentRunner
	}
	return &intent.ExecRunner{Log: e.log}
}

// speakAck says the acknowledgement through the ordinary streaming speaker,
// so the overlay sees the same Speaking state and tts.* events it sees for an
// AI answer — a deterministic intent should be indistinguishable from the
// outside except for how fast it happens. Speech failure is logged, never
// fatal: the action already happened.
func (e *Engine) speakAck(s *sess, ack string) {
	if ack == "" || s.quiet || !e.opts.SpeakResponses || e.tts == nil {
		return
	}
	speaker := newStreamingSpeaker(e, s)
	speaker.speak(ack)
	if err := speaker.close(); err != nil && s.ctx.Err() == nil {
		e.log.Warn("intent acknowledgement could not be spoken",
			"component", "tts", "session_id", s.id, "error", err.Error())
	}
}

// intentFailureAck turns a command failure into one spoken sentence.
func intentFailureAck(err error) string {
	if errors.Is(err, errIntentDeclined) {
		return "Cancelled."
	}
	return "Sorry, " + err.Error() + "."
}

// publishIntent records the hit for the overlay and the logs. duration_ms is
// measured from the final transcript, which is the number the ≤300ms budget
// is about.
func (e *Engine) publishIntent(s *sess, m intent.Match, ack string, runErr error, started time.Time) {
	source := "builtin"
	switch {
	case m.UserDefined:
		source = "user"
	case m.Routine != "":
		source = "routine"
	case m.Script != "":
		source = "script"
	}
	elapsed := time.Since(started)
	data := map[string]any{
		"session_id":      s.id,
		"intent":          m.Name,
		"source":          source,
		"status":          "ok",
		"acknowledgement": ack,
		"duration_ms":     elapsed.Milliseconds(),
	}
	attrs := []any{"component", "intent", "session_id", s.id, "intent", m.Name,
		"source", source, "duration_ms", elapsed.Milliseconds()}
	if m.HasSlot {
		data["slot"] = m.Slot
		attrs = append(attrs, "slot", m.Slot)
	}
	if m.Routine != "" {
		data["routine"] = m.Routine
		attrs = append(attrs, "routine", m.Routine)
	}
	if m.CaptureName != "" {
		data["routine"] = m.CaptureName
		attrs = append(attrs, "routine", m.CaptureName)
	}
	if m.Script != "" {
		data["script"] = m.Script
		attrs = append(attrs, "script", m.Script)
	}
	if runErr != nil {
		data["status"] = "failed"
		data["error"] = runErr.Error()
		attrs = append(attrs, "error", runErr.Error())
	}
	e.publish(Event{Type: "intent.executed", Data: data})
	e.log.Info("intent executed", attrs...)
}
