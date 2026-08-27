package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/tools"
)

// This file covers issue #52: a tool call needing confirmation while Jarvix is
// already speaking. Streaming speech starts on the first complete sentence, so
// the engine is routinely in Speaking while the model is still asking for
// tools — the combination that killed the session in production.
//
// Every test here forces that ordering rather than hoping for it, with two
// fakes and no clock:
//
//   - tts.Fake.SetHold parks the synthesizer part-way through the preamble, so
//     the sentence really is still in flight when the tool call lands;
//   - ai.Fake.BeforeToolCalls parks the model until the engine has published
//     state.changed → speaking, so the call cannot arrive before it.
//
// Nothing sleeps and nothing races: each side waits on the other's event.

// preamble is the sentence the model narrates before it acts. The trailing
// space is what makes the sentencer emit it as complete, which is what makes
// the engine start speaking mid-generation.
const preamble = "I'll clean that up for you. "

// speakingSession is a gate harness driven to the exact ordering above.
type speakingSession struct {
	*harness
	tool *namedTool
	// hold is closed to let the parked sentence finish synthesizing. Until then
	// the speaker's single playback stream is open with audio outstanding, so
	// anything else that wants to be heard has to queue behind it.
	hold chan struct{}
}

// startSpeakingSession begins a turn and returns once the engine really is
// Speaking with the model's tool call pending behind it.
//
// fire is the injected confirmation clock: the timeout fires exactly when a
// test sends on it and never underneath a test that is asserting something
// else. Pass nil to leave the real clock in place.
func startSpeakingSession(t *testing.T, fire chan time.Time, cfg tools.PolicyConfig,
	command, answer string) *speakingSession {
	t.Helper()
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{SpeakResponses: true}, rec, cfg)
	if fire != nil {
		h.engine.timer = func(time.Duration) (<-chan time.Time, func()) { return fire, func() {} }
	}
	args, _ := json.Marshal(map[string]string{"command": command})
	h.provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "shell.run", Arguments: string(args)}},
	}
	h.provider.Response = answer
	h.provider.Preamble = preamble

	ss := &speakingSession{harness: h, tool: rec, hold: make(chan struct{})}
	h.tts.SetHold(ss.hold)
	speaking := make(chan struct{})
	h.provider.BeforeToolCalls = func(ctx context.Context) {
		select {
		case <-speaking:
		case <-ctx.Done():
		}
	}

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("clean the build dir"); err != nil {
		t.Fatal(err)
	}
	ss.waitForState(t, StateSpeaking)
	close(speaking) // the model may now ask for its tool
	return ss
}

// waitForState consumes events until the engine reports the wanted state.
func (ss *speakingSession) waitForState(t *testing.T, want State) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ss.events:
			if ev.Type == "error" {
				t.Fatalf("waiting for state %s, got error event: %v", want, ev.Data)
			}
			if ev.Type == "state.changed" && ev.Data["state"] == string(want) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for state %s", want)
		}
	}
}

// assertOneVoiceAtATime is the acceptance criterion "the user hears one voice
// at a time", asserted where a user would notice it: the audio device.
//
// One Play call for the whole turn means the confirmation question shared the
// answer's stream, and a stream is drained by a single goroutine in order — so
// "same stream" is exactly "queued behind", not merely "not simultaneous". The
// peak is a high-water mark, so reading it after the fact is deterministic: had
// the question ever opened a second stream, it would have been recorded.
func (ss *speakingSession) assertOneVoiceAtATime(t *testing.T) {
	t.Helper()
	if peak := ss.player.PeakConcurrentPlays(); peak != 1 {
		t.Errorf("peak concurrent playback streams = %d, want 1: the question talked over the answer", peak)
	}
	if _, plays := ss.player.Played(); plays != 1 {
		t.Errorf("playback streams opened = %d, want 1 for the whole turn", plays)
	}
	// Preamble, question, answer — the question was queued, not dropped.
	if speaks := ss.tts.Speaks(); speaks != 3 {
		t.Errorf("sentences synthesized = %d, want 3 (preamble, question, answer)", speaks)
	}
}

// TestToolConfirmationWhileSpeaking is the regression test for #52 itself: the
// session must survive a tool call gated at the ask tier while a sentence of
// the answer is playing, and must finish normally whichever way the question is
// answered. Before the fix all three rows died with "invalid state transition
// speaking → awaiting_confirmation" and the user was left mid-conversation with
// a promise Jarvix had already spoken.
func TestToolConfirmationWhileSpeaking(t *testing.T) {
	tests := []struct {
		name string
		// answer resolves the pending confirmation. fire is the injected
		// confirmation clock, non-nil only for the timeout row.
		answer   func(t *testing.T, ss *speakingSession, fire chan time.Time)
		wantRuns int
	}{
		{
			name: "approved",
			answer: func(t *testing.T, ss *speakingSession, _ chan time.Time) {
				if err := ss.engine.Confirm(true); err != nil {
					t.Fatal(err)
				}
			},
			wantRuns: 1,
		},
		{
			name: "declined",
			answer: func(t *testing.T, ss *speakingSession, _ chan time.Time) {
				if err := ss.engine.Confirm(false); err != nil {
					t.Fatal(err)
				}
			},
			wantRuns: 0,
		},
		{
			name: "timed out",
			answer: func(_ *testing.T, _ *speakingSession, fire chan time.Time) {
				// The send blocks until awaitConfirmation is actually watching
				// the clock, which it only does once the question has been
				// asked. No sleep, and no way to fire early.
				fire <- time.Time{}
			},
			wantRuns: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fire := make(chan time.Time)
			ss := startSpeakingSession(t, fire, tools.PolicyConfig{},
				"rm -rf ./build", "Okay, that is dealt with.")
			ss.waitFor(t, "tool.confirmation_required")
			if state, _ := ss.engine.State(); state != StateAwaitingConfirmation {
				t.Fatalf("state = %s, want awaiting_confirmation", state)
			}
			if ss.tool.calls != 0 {
				t.Fatal("the tool ran before the user was asked")
			}
			// The question is queued behind the sentence still being
			// synthesized; releasing it lets both be heard, in that order.
			close(ss.hold)
			// Answer only once the question has actually been asked — the
			// deadline event is the daemon saying so. An answer landing
			// earlier now cancels the rest of the question (issue #119),
			// which is its own behaviour with its own tests; this test is
			// about the question being queued and heard, never dropped.
			ss.waitFor(t, "tool.confirmation_deadline")
			tc.answer(t, ss, fire)

			counts := ss.countUntil(t, "session.finished")
			ss.waitIdle(t)
			if counts["error"] != 0 {
				t.Errorf("the session failed instead of finishing: %d error events", counts["error"])
			}
			if ss.tool.calls != tc.wantRuns {
				t.Errorf("tool ran %d times, want %d", ss.tool.calls, tc.wantRuns)
			}
			ss.assertOneVoiceAtATime(t)
		})
	}
}

// TestAllowedToolWhileSpeakingFinishesTheAnswer covers the same gap on the tier
// that asks nothing. A read-only call needs no confirmation, so it never
// touched the gate — and the session simply stopped: Speaking had no way back
// to Responding, so the next round's first token was refused, think() unwound
// without reporting anything, and there was no error, no session.finished, and
// no answer. Silence is the worst possible failure for a voice assistant,
// because nothing tells the user to try again.
func TestAllowedToolWhileSpeakingFinishesTheAnswer(t *testing.T) {
	ss := startSpeakingSession(t, nil, tools.PolicyConfig{}, "docker ps",
		"Three containers are running.")
	// The ordering under test — the call arriving while Speaking — is already
	// guaranteed by the time this returns, and nothing here asks a question,
	// so the held sentence can be let go: the session stays in Speaking either
	// way, and the answer needs it finished to be spoken at all.
	close(ss.hold)

	counts := ss.countUntil(t, "session.finished")
	ss.waitIdle(t)
	if counts["error"] != 0 {
		t.Errorf("the session failed instead of finishing: %d error events", counts["error"])
	}
	if counts["tool.confirmation_required"] != 0 {
		t.Error("a read-only command must not ask")
	}
	if ss.tool.calls != 1 {
		t.Errorf("tool ran %d times, want 1", ss.tool.calls)
	}
	if state, _ := ss.engine.State(); state != StateIdle {
		t.Errorf("state = %s, want idle", state)
	}
	// The answer continued on the stream the preamble opened, gaplessly.
	if _, plays := ss.player.Played(); plays != 1 {
		t.Errorf("playback streams opened = %d, want 1 for the whole turn", plays)
	}
}

// TestRefusalAfterASpokenPreambleCorrectsTheRecord is the trust half of #52.
// The preamble is out of the speaker before the tool is even gated, so a call
// that then does not run leaves Jarvix having announced work it never did —
// which is what the user reported as it "pretending to run CLI commands".
//
// The correction is carried in the tool result, so the model takes the promise
// back in its own voice and continuous with the sentence that made it. Every
// way a call can fail to run has to carry it, which is why this is a table:
// declining is the obvious one, and policy denial and the timeout are the two
// that would have been forgotten.
func TestRefusalAfterASpokenPreambleCorrectsTheRecord(t *testing.T) {
	tests := []struct {
		name    string
		command string
		// refuse resolves the pending confirmation, or is nil for a denial,
		// which never reaches the user at all.
		refuse func(t *testing.T, ss *speakingSession, fire chan time.Time)
		want   string // the phrase proving the model was told what happened
	}{
		{
			name:    "declined",
			command: "rm -rf ./build",
			refuse: func(t *testing.T, ss *speakingSession, _ chan time.Time) {
				ss.waitFor(t, "tool.confirmation_required")
				close(ss.hold)
				if err := ss.engine.Confirm(false); err != nil {
					t.Fatal(err)
				}
			},
			want: "declined",
		},
		{
			name:    "timed out",
			command: "rm -rf ./build",
			refuse: func(t *testing.T, ss *speakingSession, fire chan time.Time) {
				ss.waitFor(t, "tool.confirmation_required")
				close(ss.hold)
				fire <- time.Time{}
			},
			want: "did not confirm in time",
		},
		{
			name:    "denied by policy",
			command: "rm -rf /",
			refuse: func(t *testing.T, ss *speakingSession, _ chan time.Time) {
				ss.waitFor(t, "tool.denied")
				close(ss.hold)
			},
			want: "not permitted",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fire := make(chan time.Time)
			ss := startSpeakingSession(t, fire, tools.PolicyConfig{}, tc.command,
				"Sorry, I said I would and then did not.")
			tc.refuse(t, ss, fire)
			ss.countUntil(t, "session.finished")
			ss.waitIdle(t)

			if ss.tool.calls != 0 {
				t.Fatalf("a refused command ran %d times", ss.tool.calls)
			}
			result := lastToolResult(t, ss.harness)
			if !strings.Contains(result, tc.want) {
				t.Errorf("model saw %q, want it to contain %q", result, tc.want)
			}
			// It must know what was already said, verbatim, or it cannot
			// take back the specific promise it made.
			if !strings.Contains(result, strings.TrimSpace(preamble)) {
				t.Errorf("model saw %q; it does not quote the preamble it already spoke", result)
			}
			if !strings.Contains(result, "did not happen") {
				t.Errorf("model saw %q; it is not told to correct the record", result)
			}
		})
	}
}

// TestNothingToCorrectWhenNothingWasSpoken is the other side of that: with
// speech off, the preamble only ever reached the overlay, where the user can
// see for themselves that the call was refused. Telling the model to retract a
// promise it never made out loud would make it apologise for nothing.
func TestNothingToCorrectWhenNothingWasSpoken(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{}) // SpeakResponses off
	scriptShellCall(h, "rm -rf ./build", "Nothing was changed.")
	h.provider.Preamble = preamble

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	h.waitFor(t, "tool.confirmation_required")
	if err := h.engine.Confirm(false); err != nil {
		t.Fatal(err)
	}
	h.countUntil(t, "session.finished")
	h.waitIdle(t)

	result := lastToolResult(t, h)
	if !strings.Contains(result, "declined") {
		t.Errorf("model saw %q, want a declined-by-user result", result)
	}
	if strings.Contains(result, "out loud") {
		t.Errorf("model saw %q; nothing was spoken, so there is nothing to take back", result)
	}
}

// TestUnreachableGateDoesNotKillTheSession pins the reliability requirement. A
// state with no legal path to AwaitingConfirmation is a defect in this package,
// and the user must not pay for it with their conversation: nothing runs, the
// model is told the truth, and the turn carries on. This used to be e.fail,
// which ended the session mid-sentence — the whole of #52's first symptom.
//
// Idle stands in for the next such state: it is deliberately not in
// toolRequestStates, so it is exactly what a state nobody enumerated looks like
// from inside the gate.
func TestUnreachableGateDoesNotKillTheSession(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	id, err := h.engine.StartSession()
	if err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	s := h.engine.current
	h.engine.mu.Unlock()

	outcome, alive := h.engine.awaitConfirmation(s, confirmRequest{
		tool: "shell.run", command: "rm -rf ./build",
		summary: "Should I go ahead?", rule: "risky", resume: StateThinking,
	})
	if outcome != confirmUnavailable {
		t.Errorf("outcome = %v, want confirmUnavailable", outcome)
	}
	if !alive {
		t.Error("the session must survive a gate it could not enter")
	}
	if state, gotID := h.engine.State(); state != StateIdle || gotID != id {
		t.Errorf("state = %s session = %q; want the session still alive and idle", state, gotID)
	}
	if s.ctx.Err() != nil {
		t.Error("the session context was cancelled; the turn was killed after all")
	}
	// The refusal is still recorded: an audit trail must never show a command
	// that was neither run nor accounted for.
	ev := h.waitFor(t, "tool.declined")
	if ev.Data["source"] != "unavailable" {
		t.Errorf("tool.declined source = %v, want unavailable", ev.Data["source"])
	}
	_ = h.engine.Cancel()
	h.waitIdle(t)
}
