package session

import (
	"time"

	"github.com/rpickz/jarvix/internal/ai"
)

// This file is the engine half of long-tool progress (ADR 0016). A voice
// interface has no spinner: while a tool runs, the user hears nothing and has
// no way to tell "thinking hard" from "crashed". Tools that can take minutes
// (advisor consultations) therefore describe themselves — the overlay shows
// the label for the whole call, and Jarvix says once, out loud, that it is
// still working.

// DefaultToolProgressAfter is how long a tool call may run in silence before
// Jarvix speaks up. Ten seconds is past the point where a person starts to
// wonder, and short enough that the reassurance still lands before they give
// up and interrupt.
const DefaultToolProgressAfter = 10 * time.Second

// startToolProgress arms the progress announcement for one tool call and
// returns the function that disarms it. The returned stop waits for any
// in-flight announcement to finish speaking, so the tool result never
// overtakes it into the speaker and no goroutine outlives the call.
//
// It fires at most once: the point is reassurance, not a countdown.
func (e *Engine) startToolProgress(s *sess, call ai.ToolCall, waiting string,
	speaker *streamingSpeaker) (stop func()) {
	after := e.progressAfter
	if after <= 0 {
		after = DefaultToolProgressAfter
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTimer(after)
		defer t.Stop()
		select {
		case <-done:
		case <-s.ctx.Done():
		case <-t.C:
			e.publish(Event{Type: "tool.progress", Data: map[string]any{
				"session_id": s.id, "tool": call.Name, "message": waiting,
				"elapsed_sec": int(after.Seconds()),
			}})
			e.log.Info("tool still running", "component", "tools",
				"tool", call.Name, "session_id", s.id, "elapsed_sec", int(after.Seconds()))
			// An aside, like a confirmation question: the session is
			// Thinking, not Speaking, and this sentence must not become part
			// of the answer being assembled. It still goes through the turn's
			// speaker when there is one, because a slow tool can be running
			// while an earlier sentence is still playing and reassurance that
			// talks over the answer is worse than no reassurance.
			// The session's own context as the prompt context: nothing
			// resolves a reassurance early, so only the session ending can
			// (and should) cut it off. keep is false: "still working" is only
			// true while the tool runs, and a newer turn's sentence can only
			// have been committed after the tool returned — a reassurance
			// still queued by then would announce work already finished, so
			// cross-turn supersession drops it (issue #120; utterance.keep).
			e.speakPrompt(s, s.ctx, waiting, speaker, false)
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}
