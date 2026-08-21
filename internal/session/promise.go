package session

import (
	"fmt"
	"strings"
)

// This file is the trust half of issue #52.
//
// Streaming speech (ADR 0010) starts on the first complete sentence, so by the
// time a tool call is gated the model's preamble — "I'll clean that up for
// you" — has already left the speaker. If the call is then denied, declined, or
// never answered, Jarvix has announced work it did not do, and the user has no
// way to see that nothing happened. That is worse than the crash it came in
// with: a session that dies is obviously broken, while an assistant that
// narrates actions it never took is quietly untrustworthy, which is the one
// thing a voice interface cannot afford.
//
// The fix is not to stop speaking early — streaming speech is the product. It
// is to make sure the words after the refusal answer for the words before it.

// spokenTurn is the voice of the turn in progress: the speaker that owns the
// audio device, and everything that has already gone out of it.
//
// It travels with the tool call rather than living on the engine because it
// belongs to exactly one turn, is written and read on the think goroutine
// alone, and must not outlive either — the next turn starts owing nothing.
type spokenTurn struct {
	// speaker is the turn's single playback stream, or nil when the turn has no
	// voice (speech disabled, or no synthesizer wired). Nil means both that
	// there is no queue to put a confirmation question on and that no promise
	// was ever made out loud; the same text reached only the overlay, where the
	// user can see for themselves that it was not acted on.
	speaker *streamingSpeaker
	// text is everything streamed to the speaker so far this turn, in order.
	text string
}

// add records a round's text as having been said.
func (t spokenTurn) add(text string) spokenTurn {
	text = strings.TrimSpace(text)
	if text == "" {
		return t
	}
	if t.text == "" {
		t.text = text
		return t
	}
	t.text += " " + text
	return t
}

// promised reports whether words are out there that a refusal has to answer for.
func (t spokenTurn) promised() bool { return t.speaker != nil && t.text != "" }

// refused finishes a tool call that did not run. The model always gets a result
// message — that is what lets it answer gracefully instead of the session dying
// — and when Jarvix has already spoken this turn, that message carries an
// explicit instruction to correct the record first.
//
// The correction is an instruction to the model rather than a sentence Jarvix
// says itself, for the same reason the decline and deny messages are: the
// answer has to be one voice, in the model's own words, continuous with the
// preamble it is taking back. A canned "that did not run" spliced into the
// middle of a streamed answer would be a second speaker talking over the first.
//
// The log line is the other half. A refusal after a preamble is precisely the
// pairing that makes a user distrust the assistant, and it is invisible in a
// journal that records "tool declined" and "assistant finished" as unrelated
// facts, so it gets a line that names both.
func (e *Engine) refused(s *sess, tool, command, reason string, turn spokenTurn, result string) string {
	if !turn.promised() {
		return result
	}
	e.log.Warn("tool refused after Jarvix had already spoken", "component", "tools",
		"session_id", s.id, "tool", tool, "command", command, "reason", reason,
		"spoken", turn.text)
	return fmt.Sprintf("%s IMPORTANT: you have already said this out loud and the user "+
		"heard it: %q. If any of that promised, announced, or implied that this "+
		"command would run, your next words must say plainly and first that it did "+
		"not happen. Do not leave the promise standing.", result, turn.text)
}
