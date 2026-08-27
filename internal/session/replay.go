package session

// This file is the speak-again path (issue #122): any turn of the live
// conversation can be spoken again on demand. Speech is ephemeral — miss a
// sentence while multi-tasking and the only recourse used to be re-reading
// the transcript — so the window's replay control and `jarvix say-again` ask
// the daemon to say a recorded turn once more.
//
// Replay is honest speech, which pins three decisions:
//
//   - The daemon speaks what the record holds, never what a client sends. The
//     verb carries a turn *address* (a position in the conversation.get
//     snapshot); the text is resolved here, from the same Conversation() view
//     the window rendered. A client that could submit text for the voice to
//     read as "what Jarvix said" could misquote the record.
//   - Replay is a speech turn, not a conversation turn. It runs as a session
//     so that every existing stop path finds it — CancelSpeech, Cancel, the
//     spoken "stop", a new utterance's interruption, shutdown's drain — and
//     so the bar/overlay see the same Speaking state and tts.* bookends every
//     answer produces. But it commits nothing: no history, no archive, no
//     model call. The record is what is being read; reading it must not grow
//     it.
//   - Live conversation speech wins (the issue's precedence question, pinned
//     here). A replay is refused while a real session is active — speaking,
//     thinking, listening, or waiting on a confirmation — because the live
//     exchange is the newer intent and a replay talking over it would be the
//     stale-narration problem issue #120 exists to prevent. The other
//     direction composes with the session machinery that already exists:
//     new live speech supersedes a replay in progress through the same
//     instant interruption any session gets (startSessionLocked cancels it),
//     and a second replay supersedes the first the same way. The speaker's
//     queue-level floor (#120/#133) still governs *within* the replay's own
//     turn; between turns, supersession has always been session cancellation,
//     and a replay is its own turn.

import (
	"errors"
	"fmt"

	"github.com/rpickz/jarvix/internal/ai"
)

// ErrReplayBusy marks a replay refused because a live session holds the
// floor. Callers surface it as "busy, try again", distinct from a bad turn
// address, which is a client-side staleness problem.
var ErrReplayBusy = errors.New("the conversation is busy")

// ReplaySpeech speaks one turn of the live conversation again, addressed as a
// 1-based position in the conversation.get snapshot (the view the window
// holds; positions only ever append while a thread lives, and a rebuilt or
// reopened window re-reads the same snapshot, so the address survives both).
// turn 0 means the newest assistant turn — the "say that again" case.
//
// role, when non-empty, must match the addressed turn's role. It is a
// staleness guard, not part of the address: the window's model can trail the
// record (an intent turn's acknowledgement, a confirmation reply), and a
// mismatch means the click landed on a view the record has moved past — the
// caller should refresh and try again rather than have the wrong turn read
// out.
//
// Every turn the snapshot shows is replayable on the same terms, and exactly
// as shown: an interrupted turn's text carries its bracketed annotation into
// speech, and a confirmation record reads its summary — the spoken question —
// never the verbatim command, which the record shows on screen but the voice
// never uttered.
//
// On success the daemon speaks the turn through the standard pipeline — the
// same speaker, normalization, lexicon, and voice settings as live speech —
// as its own session (Acting → Speaking → Idle, the deterministic local-
// action path; no provider request, nothing committed). A replay already in
// progress is cancelled first: the newest click wins, exactly as a new
// utterance wins over either.
func (e *Engine) ReplaySpeech(turn int, role string) (resolvedTurn int, resolvedRole string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.opts.SpeakResponses || e.tts == nil {
		return 0, "", fmt.Errorf("speech is turned off (conversation.speak_responses)")
	}
	if e.current != nil && !e.current.replay {
		// Live conversation speech wins — see the file comment for the pinned
		// policy. The state is named so the refusal reads as "wait", not as a
		// fault.
		return 0, "", fmt.Errorf("%w (%s) — a replay never talks over the conversation", ErrReplayBusy, e.state)
	}

	turns := e.conversationLocked()
	if turn == 0 {
		for i := len(turns) - 1; i >= 0; i-- {
			if turns[i].Role == string(ai.RoleAssistant) {
				turn = i + 1
				break
			}
		}
		if turn == 0 {
			return 0, "", fmt.Errorf("nothing to say again — the conversation has no assistant turn yet")
		}
	}
	if turn < 1 || turn > len(turns) {
		return 0, "", fmt.Errorf("no turn %d — the conversation has %d", turn, len(turns))
	}
	t := turns[turn-1]
	if role != "" && role != t.Role {
		return 0, "", fmt.Errorf("turn %d is a %s turn, not %s — the conversation may have moved; refresh and try again", turn, t.Role, role)
	}
	if e.spokenForm(t.Text) == "" {
		// Nothing would be said: refuse now rather than run a session that
		// announces nothing and "succeeds" silently.
		return 0, "", fmt.Errorf("turn %d has nothing speakable", turn)
	}

	if e.current != nil {
		// A replay is already playing: the newest click supersedes it, through
		// the same instant interruption a new session performs (the pinned
		// between-turns policy above — the queue-level floor never spans
		// speakers).
		e.cancelLocked("superseded by another replay")
	}
	if _, err := e.startSessionLocked(); err != nil {
		return 0, "", err
	}
	s := e.current
	s.replay = true
	// Claimed committed from birth: every teardown path (cancel, stop,
	// interruption, failure) consults s.committed before writing an exchange,
	// so this is what makes "the conversation record is untouched" hold by
	// construction rather than by each path's goodwill. There is no exchange
	// aboard anyway — a replay has no transcript — but the claim closes the
	// question permanently.
	s.committed = true
	// Acting is the deterministic local-action state (ADR 0017): no provider
	// request is open, and the speaker's announce takes the same legal
	// Acting → Speaking edge an intent acknowledgement takes. No new table
	// edge for replay.
	if err := e.setStateLocked(StateActing); err != nil {
		e.failLocked(s, "session", err)
		return 0, "", err
	}
	text := t.Text
	resolvedRole = t.Role
	e.active.Go(func() { e.runReplay(s, text, turn, resolvedRole) })
	e.log.Info("replaying turn", "component", "session", "session_id", s.id, "turn", turn, "role", resolvedRole)
	return turn, resolvedRole, nil
}

// runReplay says one recorded turn through the standard streaming speaker and
// finishes the session. It mirrors runIntent's tail — same speaker, same
// events — minus everything a replay must not do: no commit, no history
// write, no archive stage.
//
// The text is split into sentences by the same sentencer the streaming path
// uses, so a long turn synthesizes and plays sentence by sentence — cancel
// and supersession land between sentences exactly as they do live.
func (e *Engine) runReplay(s *sess, text string, turn int, role string) {
	speaker := newStreamingSpeaker(e, s)
	var sc sentencer
	sentences := append(sc.push(text), sc.flush()...)
	for _, sentence := range sentences {
		speaker.speak(sentence)
	}
	err := speaker.close()
	if s.ctx.Err() != nil {
		return // cancelled or superseded; that path owns the events
	}
	if err != nil {
		e.fail(s, "tts", err)
		return
	}
	// The replay's own record: which turn was spoken again. Published before
	// session.finished, which the bus guarantees is the session's last engine
	// event; the daemon's activity watcher renders it as the feed row.
	e.publish(Event{Type: "speech.replayed", Data: map[string]any{
		"session_id": s.id, "turn": turn, "role": role}})
	e.mu.Lock()
	e.finishLocked(s)
	e.mu.Unlock()
}
