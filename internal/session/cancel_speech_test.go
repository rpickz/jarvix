package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/tools"
)

// This file covers issue #54: "stop" must stop Jarvix whenever it is actually
// talking. Session state is a statement about what the turn is doing, not
// about whether the audio device is busy, and the two diverge routinely — a
// mid-answer tool round puts the session back in Thinking or Responding while
// the streaming speaker is still draining sentences through its one open
// playback stream. CancelSpeech used to guard on `state == Speaking`, so in
// exactly those moments every stop path (the spoken intent, speech.cancel,
// any binding) did nothing at all.
//
// The tests force the divergence deterministically, in the #52 style: the
// tts fake's hold gate keeps an utterance in flight for as long as a test
// needs, and the provider fake parks until the engine has reached the state
// under test. Nothing sleeps its way into an ordering.

// waitUntil polls a condition to true, bounded, in the waitIdle style: for
// conditions that have no event to wait on because they are internal facts.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting until %s", what)
}

// TestCancelSpeechStopsAudioAfterAToolRound is the headline defect. An
// allow-tier tool call arrives while the first sentence is still playing; the
// tool runs under the audio and the next round's first token moves the session
// Speaking → Responding — the answer is mid-stream, the preamble is still
// audibly draining, and the old `state == Speaking` guard made "stop" a no-op
// at precisely this moment.
func TestCancelSpeechStopsAudioAfterAToolRound(t *testing.T) {
	ss := startSpeakingSession(t, nil, tools.PolicyConfig{}, "docker ps",
		"Three containers are running.")
	// The read-only call asks nothing and runs under the held preamble…
	ss.waitFor(t, "tool.finished")
	// …and the next round's first token returns the session to Responding
	// while the speaker still has the preamble in flight.
	ss.waitForState(t, StateResponding)

	if !ss.engine.CancelSpeech() {
		t.Fatal("CancelSpeech reported nothing playing while the preamble was still draining")
	}
	ev := ss.waitFor(t, "tts.finished")
	if ev.Data["interrupted"] != true {
		t.Errorf("tts.finished = %v, want interrupted: true", ev.Data)
	}
	ss.waitFor(t, "session.finished")
	ss.waitIdle(t)
	if state, id := ss.engine.State(); state != StateIdle || id != "" {
		t.Errorf("state = %s session = %q; want a cleanly finished engine", state, id)
	}
}

// TestCancelSpeechStopsAudioBackInThinking is the Thinking half of the same
// divergence: a confirmation raised mid-sentence is approved and the session
// resumes at Thinking to carry on its tool round — while the preamble and the
// question are both still draining through the one playback stream. This is
// the acceptance criterion's exact shape, asserted with speech held open, not
// timed.
func TestCancelSpeechStopsAudioBackInThinking(t *testing.T) {
	ss := startSpeakingSession(t, nil, tools.PolicyConfig{},
		"rm -rf ./build", "All cleaned up.")
	ss.waitFor(t, "tool.confirmation_required")
	if err := ss.engine.Confirm(true); err != nil {
		t.Fatal(err)
	}
	ss.waitForState(t, StateThinking)

	if !ss.engine.CancelSpeech() {
		t.Fatal("CancelSpeech reported nothing playing while audio was held mid-utterance in Thinking")
	}
	ev := ss.waitFor(t, "tts.finished")
	if ev.Data["interrupted"] != true {
		t.Errorf("tts.finished = %v, want interrupted: true", ev.Data)
	}
	ss.waitFor(t, "session.finished")
	ss.waitIdle(t)
	if state, id := ss.engine.State(); state != StateIdle || id != "" {
		t.Errorf("state = %s session = %q; want a cleanly finished engine", state, id)
	}
}

// TestCancelSpeechStopsAudioWhateverTheSessionState is the enumeration the
// acceptance criteria ask for: a table over every state the streaming speaker
// can be draining in, so the next feature that adds a state cannot silently
// reintroduce the gap. The rows are toolRequestStates — the states a tool
// round can move the session through while its audio drains, kept covering
// the whole table by TestToolRequestStatesCoverEveryState — plus
// AwaitingConfirmation, which a queued question can hold audio through.
//
// Each row starts a real turn whose speech is held mid-utterance and then sets
// the state directly. Reality reaches these states through legal transitions
// (the two tests above walk two of the paths); the direct write is what makes
// this a table rather than four bespoke choreographies, and it is honest about
// the property under test: CancelSpeech must not read the state at all.
func TestCancelSpeechStopsAudioWhateverTheSessionState(t *testing.T) {
	states := append(append([]State{}, toolRequestStates...), StateAwaitingConfirmation)
	for _, forced := range states {
		t.Run(string(forced), func(t *testing.T) {
			h := newHarness(t, Options{SpeakResponses: true})
			hold := make(chan struct{})
			h.tts.SetHold(hold)
			_, _ = h.engine.StartSession()
			_ = h.engine.Submit("hi")
			h.waitFor(t, "tts.started") // the answer's audio is committed and held

			h.engine.mu.Lock()
			h.engine.state = forced
			h.engine.mu.Unlock()

			if !h.engine.CancelSpeech() {
				t.Fatalf("CancelSpeech in %s reported nothing playing while speech was held", forced)
			}
			ev := h.waitFor(t, "tts.finished")
			if ev.Data["interrupted"] != true {
				t.Errorf("tts.finished = %v, want interrupted: true", ev.Data)
			}
			h.waitFor(t, "session.finished")
			h.waitIdle(t)
			if state, id := h.engine.State(); state != StateIdle || id != "" {
				t.Errorf("state = %s session = %q; want a cleanly finished engine", state, id)
			}
		})
	}
}

// TestCancelSpeechWithNothingPlayingIsAReportedNoop pins the other half of the
// contract: a cancel that finds no audio must say so — false and a debug line
// — and must leave the turn completely untouched. The turn here is parked
// mid-Thinking with a speaker that exists but has never been handed a
// sentence, which is exactly the shape that must not be confused with "there
// is something to stop".
func TestCancelSpeechWithNothingPlayingIsAReportedNoop(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "3 containers"}
	h := newGateHarness(t, Options{SpeakResponses: true}, rec, tools.PolicyConfig{})
	logs := captureLog(h)
	scriptShellCall(h, "docker ps", "Three containers are running.")
	parked := make(chan struct{})
	release := make(chan struct{})
	h.provider.BeforeToolCalls = func(ctx context.Context) {
		close(parked)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("what's in docker")
	<-parked // the turn is in Thinking; no sentence has reached the speaker

	if h.engine.CancelSpeech() {
		t.Fatal("CancelSpeech claimed to stop something while nothing was playing")
	}
	if !strings.Contains(logs.String(), "speech cancel found nothing playing") {
		t.Error("the no-op left no debug line; silence is what made issue #54 invisible")
	}

	// The turn was untouched: released, it completes normally, speech and all.
	close(release)
	seen := h.collectUntil(t, "session.finished")
	if ev, ok := seen["tts.finished"]; !ok {
		t.Error("the turn's speech did not complete after the no-op cancel")
	} else if ev.Data["interrupted"] == true {
		t.Errorf("tts.finished = %v; the no-op interrupted after all", ev.Data)
	}
	h.waitIdle(t)
	if rec.calls != 1 {
		t.Errorf("tool ran %d times, want 1", rec.calls)
	}
}

// TestCancelSpeechAbandonsAQueuedConfirmationQuestion documents the decided
// interaction with the permission gate (#53's asides): cancelling speech while
// a confirmation question is queued or playing silences the question with
// everything else and abandons the confirmation as declined. "Stop" while
// being asked "should I run this?" is the emphatic no — the word is literally
// in the decline vocabulary for spoken replies — and the audit trail still
// records an answer for the question (tool.declined, source "interrupted"),
// so the pending confirmation is left coherent: resolved, never dangling.
func TestCancelSpeechAbandonsAQueuedConfirmationQuestion(t *testing.T) {
	ss := startSpeakingSession(t, nil, tools.PolicyConfig{}, "rm -rf ./build",
		"Okay, that is dealt with.")
	ss.waitFor(t, "tool.confirmation_required")
	if state, _ := ss.engine.State(); state != StateAwaitingConfirmation {
		t.Fatalf("state = %s, want awaiting_confirmation", state)
	}

	if !ss.engine.CancelSpeech() {
		t.Fatal("CancelSpeech reported nothing playing while the preamble and question were queued")
	}
	declined := ss.waitFor(t, "tool.declined")
	if declined.Data["source"] != "interrupted" {
		t.Errorf("tool.declined source = %v, want interrupted", declined.Data["source"])
	}
	ev := ss.waitFor(t, "tts.finished")
	if ev.Data["interrupted"] != true {
		t.Errorf("tts.finished = %v, want interrupted: true", ev.Data)
	}
	ss.waitFor(t, "session.finished")
	ss.waitIdle(t)

	if ss.tool.calls != 0 {
		t.Errorf("a cancelled confirmation ran its tool %d times", ss.tool.calls)
	}
	if err := ss.engine.Confirm(true); err == nil {
		t.Error("a confirmation survived the cancel; it must be abandoned, not left pending")
	}
}

// TestCancelSpeechSilencesADirectConfirmationPrompt covers the one stretch of
// audio no streaming speaker owns: a user-defined intent's confirmation
// question, spoken on speakPrompt's direct path before the turn has a voice of
// its own. Without the session's promptAudio mark, the single moment Jarvix
// speaks on an intent turn would be the single moment it could not be stopped.
func TestCancelSpeechSilencesADirectConfirmationPrompt(t *testing.T) {
	h := newIntentHarness(t, Options{SpeakResponses: true},
		intent.Custom{Match: "tidy the downloads", Run: "rm -rf ~/Downloads/tmp", Say: "Tidied."})
	hold := make(chan struct{})
	h.tts.SetHold(hold)
	defer close(hold)

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("tidy the downloads")
	h.waitFor(t, "tool.confirmation_required")
	// promptAudio is marked before the synthesizer is called, so a synthesis
	// in flight proves the mark is visible to CancelSpeech.
	waitUntil(t, "the question reaches the synthesizer", func() bool { return h.tts.Speaks() > 0 })

	if !h.engine.CancelSpeech() {
		t.Fatal("CancelSpeech reported nothing playing while the question was being spoken")
	}
	declined := h.waitFor(t, "tool.declined")
	if declined.Data["source"] != "interrupted" {
		t.Errorf("tool.declined source = %v, want interrupted", declined.Data["source"])
	}
	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)
	// The question was an aside: tts.started never fired, so no bookend is
	// owed — publishing one would be an unmatched tts.finished.
	if counts["tts.finished"] != 0 {
		t.Errorf("tts.finished published %d times for an unannounced question, want 0", counts["tts.finished"])
	}
	if counts["error"] != 0 {
		t.Errorf("the session failed instead of finishing: %d error events", counts["error"])
	}
	if h.runner.Shell() != nil {
		t.Errorf("the cancelled command ran: %v", h.runner.Shell())
	}
}

// TestSpeakerAnswersIsAudioPlaying pins where the answer to "is audio playing"
// now lives: on the speaker that owns the playback stream, not in the session
// state — and that the two can disagree without breaking interruption, which
// is the acceptance criterion issue #54 states outright.
func TestSpeakerAnswersIsAudioPlaying(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	hold := make(chan struct{})
	h.tts.SetHold(hold)
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("hi")
	h.waitFor(t, "tts.started")

	h.engine.mu.Lock()
	sp := h.engine.current.speaker
	h.engine.mu.Unlock()
	if sp == nil {
		t.Fatal("no speaker registered on the session")
	}
	if live, announced := sp.speaking(); !live || !announced {
		t.Fatalf("speaking() = (%v, %v) with audio held mid-utterance, want (true, true)", live, announced)
	}

	// Make the state disagree with the device — the divergence a tool round
	// produces naturally — and interruption must not care.
	h.engine.mu.Lock()
	h.engine.state = StateThinking
	h.engine.mu.Unlock()
	if live, _ := sp.speaking(); !live {
		t.Fatal("the speaker's answer changed with the session state; it must come from playback alone")
	}
	if !h.engine.CancelSpeech() {
		t.Fatal("interruption broke the moment state and speaker disagreed")
	}
	h.waitFor(t, "session.finished")
	h.waitIdle(t)
	// Once the turn has fully unwound the speaker reports drained — false
	// forever, so a late "stop" is a no-op rather than a stale kill.
	waitUntil(t, "the speaker reports drained", func() bool {
		live, _ := sp.speaking()
		return !live
	})
}
