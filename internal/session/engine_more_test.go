package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/tts"
)

func TestStartVoiceTwiceIsInvalidTransition(t *testing.T) {
	h := newHarness(t, Options{})
	_, _ = h.engine.StartSession()
	if err := h.engine.StartVoice(); err != nil {
		t.Fatal(err)
	}
	err := h.engine.StartVoice()
	if err == nil {
		t.Fatal("second StartVoice must be rejected")
	}
	if !strings.Contains(err.Error(), "invalid state transition") {
		t.Errorf("err = %v", err)
	}
	// The session survives the rejected call: it can still be cancelled.
	if err := h.engine.Cancel(); err != nil {
		t.Fatal(err)
	}
	h.waitIdle(t)
}

func TestRecorderStartFailureFailsSession(t *testing.T) {
	h := newHarness(t, Options{})
	h.recorder.StartErr = errors.New("no capture device")
	_, _ = h.engine.StartSession()
	if err := h.engine.StartVoice(); err == nil {
		t.Fatal("StartVoice must surface the recorder error")
	}
	seen := h.collectUntil(t, "session.finished")
	if seen["error"].Data["stage"] != "audio" {
		t.Errorf("stage = %v", seen["error"].Data["stage"])
	}
	h.waitIdle(t)
	if s, _ := h.engine.State(); s != StateIdle {
		t.Errorf("state = %s, want idle after audio failure", s)
	}
}

func TestRecorderStopFailureFailsSession(t *testing.T) {
	h := newHarness(t, Options{})
	h.recorder.StopErr = errors.New("wav vanished")
	_, _ = h.engine.StartSession()
	_ = h.engine.StartVoice()
	_, _ = h.engine.StopVoice()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-h.events:
			if ev.Type == "error" {
				if ev.Data["stage"] != "audio" {
					t.Errorf("stage = %v", ev.Data["stage"])
				}
				h.waitIdle(t)
				return
			}
		case <-deadline:
			t.Fatal("no error event for a failed Stop")
		}
	}
}

func TestTTSFailureFailsSession(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	h.tts.Fail = errors.New("synth exploded")
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("hi")
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-h.events:
			if ev.Type == "error" {
				if ev.Data["stage"] != "tts" {
					t.Errorf("stage = %v", ev.Data["stage"])
				}
				h.waitIdle(t)
				return
			}
		case <-deadline:
			t.Fatal("no error event for a failed synthesis")
		}
	}
}

func TestPlayerFailureFailsSession(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	h.player.PlayErr = errors.New("sink gone")
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("hi")
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-h.events:
			if ev.Type == "error" {
				if ev.Data["stage"] != "tts" {
					t.Errorf("stage = %v", ev.Data["stage"])
				}
				h.waitIdle(t)
				return
			}
		case <-deadline:
			t.Fatal("no error event for a failed playback")
		}
	}
}

func TestCancelSpeechOutsideSpeakingIsNoop(t *testing.T) {
	h := newHarness(t, Options{})
	if err := h.engine.CancelSpeech(); err != nil {
		t.Errorf("CancelSpeech on idle engine: %v", err)
	}
	_, _ = h.engine.StartSession()
	if err := h.engine.CancelSpeech(); err != nil {
		t.Errorf("CancelSpeech outside speaking: %v", err)
	}
}

func TestHistoryIsCappedToConfiguredTurns(t *testing.T) {
	h := newHarness(t, Options{HistoryTurns: 1, FollowUpWindow: time.Hour})
	ask := func(text string) {
		_, _ = h.engine.StartSession()
		_ = h.engine.Submit(text)
		h.collectUntil(t, "session.finished")
		h.waitIdle(t)
	}
	ask("first question")
	ask("second question")
	ask("third question")

	// With one remembered turn, the third request may carry the second
	// exchange but must have dropped the first.
	last := h.provider.Requests[len(h.provider.Requests)-1]
	for _, m := range last.Messages {
		if strings.Contains(m.Content, "first question") {
			t.Errorf("history exceeded its cap: %+v", last.Messages)
		}
	}
	// 1 turn of history (user+assistant) + the new user message.
	if len(last.Messages) != 3 {
		t.Errorf("message count = %d, want 3", len(last.Messages))
	}
}

func TestSystemPromptLeadsMessages(t *testing.T) {
	h := newHarness(t, Options{SystemPrompt: "You are terse."})
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("hello")
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	msgs := h.provider.LastRequest.Messages
	if len(msgs) != 2 || string(msgs[0].Role) != "system" || msgs[0].Content != "You are terse." {
		t.Errorf("messages = %+v", msgs)
	}
}

// The tests below pin behaviours that mutation testing (gremlins) showed
// were unasserted: each one kills a mutant that previously survived.

func TestSessionIDsAreSequential(t *testing.T) {
	h := newHarness(t, Options{})
	first, _ := h.engine.StartSession()
	second, _ := h.engine.StartSession()
	if first != "s1" || second != "s2" {
		t.Errorf("ids = %q, %q; want s1, s2", first, second)
	}
}

func TestStateChangedEventsCarrySessionID(t *testing.T) {
	h := newHarness(t, Options{})
	id, _ := h.engine.StartSession()
	_ = h.engine.Submit("hi")
	seen := h.collectUntil(t, "session.finished")
	ev, okEv := seen["state.changed"]
	if !okEv {
		t.Fatal("no state.changed events")
	}
	if got := ev.Data["session_id"]; got != id {
		t.Errorf("state.changed session_id = %v, want %s", got, id)
	}
}

func TestZeroFollowUpWindowKeepsHistoryForever(t *testing.T) {
	h := newHarness(t, Options{HistoryTurns: 8}) // FollowUpWindow 0: no expiry
	ask := func(text string) {
		_, _ = h.engine.StartSession()
		_ = h.engine.Submit(text)
		h.collectUntil(t, "session.finished")
		h.waitIdle(t)
	}
	ask("first question")
	ask("second question")
	last := h.provider.Requests[len(h.provider.Requests)-1]
	found := false
	for _, m := range last.Messages {
		if strings.Contains(m.Content, "first question") {
			found = true
		}
	}
	if !found {
		t.Error("a zero follow-up window must never expire history")
	}
}

func TestProviderFailureWithSpeechOffStillFailsCleanly(t *testing.T) {
	// The abort path with no speaker attached (speak_responses = false).
	h := newHarness(t, Options{SpeakResponses: false})
	h.provider.Fail = errors.New("model exploded")
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("hi")
	seen := h.collectUntil(t, "session.finished")
	if seen["error"].Data["stage"] != "assistant" {
		t.Errorf("stage = %v", seen["error"].Data["stage"])
	}
	h.waitIdle(t)
}

// erroringSynth fails at Speak itself (unlike tts.Fake, which fails via an
// error chunk after the stream starts) — the nothing-was-ever-spoken path.
type erroringSynth struct{ err error }

func (s *erroringSynth) Name() string { return "erroring" }
func (s *erroringSynth) Speak(_ context.Context, _ tts.Request) (tts.Format, <-chan tts.Chunk, error) {
	return tts.Format{}, nil, s.err
}

func TestSpeakCallErrorFailsSessionBeforeAnyAudio(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	h.engine = NewEngine(h.provider, h.stt, &erroringSynth{err: errors.New("no voice model")},
		h.recorder, h.player, nil, bus, nil, Options{Model: "m", SpeakResponses: true})
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("hi")
	seen := h.collectUntil(t, "session.finished")
	if seen["error"].Data["stage"] != "tts" {
		t.Errorf("stage = %v, data %v", seen["error"].Data["stage"], seen["error"].Data)
	}
	if _, plays := h.player.Played(); plays != 0 {
		t.Error("no audio may start when synthesis never begins")
	}
	h.waitIdle(t)
}

func TestStateValidAndActive(t *testing.T) {
	for _, s := range []State{StateIdle, StateListening, StateTranscribing, StateThinking,
		StateResponding, StateSpeaking, StateCancelling, StateError} {
		if !s.Valid() {
			t.Errorf("%s should be valid", s)
		}
	}
	if State("bogus").Valid() {
		t.Error("unknown state should be invalid")
	}
	if StateIdle.Active() {
		t.Error("idle is not active")
	}
	if !StateSpeaking.Active() {
		t.Error("speaking is active")
	}
}
