package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
)

// harness bundles an engine wired entirely to fakes, verifying the complete
// session lifecycle without audio hardware or external APIs.
type harness struct {
	engine   *Engine
	provider *ai.Fake
	stt      *stt.Fake
	tts      *tts.Fake
	recorder *audio.FakeRecorder
	player   *audio.FakePlayer
	tools    *tools.Registry
	events   <-chan Event
	cancel   func()
}

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	h := &harness{
		provider: &ai.Fake{Response: "Recursion is a function calling itself."},
		stt:      &stt.Fake{Text: "explain recursion"},
		tts:      &tts.Fake{},
		recorder: &audio.FakeRecorder{Clip: audio.Clip{WAVPath: t.TempDir() + "/rec.wav", SampleRate: 16000, Channels: 1}},
		player:   &audio.FakePlayer{},
	}
	if opts.Model == "" {
		opts.Model = "test-model"
	}
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	t.Cleanup(h.cancel)
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, h.tools, bus, nil, opts)
	return h
}

// waitFor consumes events until one of the wanted type arrives.
func (h *harness) waitFor(t *testing.T, eventType string) Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-h.events:
			if ev.Type == eventType {
				return ev
			}
			if ev.Type == "error" {
				t.Fatalf("waiting for %q, got error event: %v", eventType, ev.Data)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event %q", eventType)
		}
	}
}

// waitIdle blocks until the engine returns to idle.
func (h *harness) waitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s, _ := h.engine.State(); s == StateIdle {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	s, _ := h.engine.State()
	t.Fatalf("engine stuck in state %s", s)
}

func TestVoiceSessionFullLifecycle(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.StartVoice(); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "recording.started")
	if s, _ := h.engine.State(); s != StateListening {
		t.Errorf("state = %s, want listening", s)
	}
	if _, err := h.engine.StopVoice(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit(""); err != nil {
		t.Fatal(err)
	}

	if ev := h.waitFor(t, "transcript.final"); ev.Data["text"] != "explain recursion" {
		t.Errorf("transcript = %v", ev.Data["text"])
	}
	h.waitFor(t, "assistant.started")
	if ev := h.waitFor(t, "assistant.finished"); ev.Data["content"] != "Recursion is a function calling itself." {
		t.Errorf("response = %v", ev.Data["content"])
	}
	h.waitFor(t, "tts.started")
	h.waitFor(t, "tts.finished")
	h.waitFor(t, "session.finished")
	h.waitIdle(t)

	if h.tts.LastRequest.Text != "Recursion is a function calling itself." {
		t.Errorf("tts got %q", h.tts.LastRequest.Text)
	}
	if _, plays := h.player.Played(); plays != 1 {
		t.Errorf("player plays = %d", plays)
	}
	if h.provider.LastRequest.Messages[len(h.provider.LastRequest.Messages)-1].Content != "explain recursion" {
		t.Errorf("provider got %+v", h.provider.LastRequest.Messages)
	}
}

func TestAskPathSkipsRecording(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("hello there"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "assistant.finished")
	h.waitFor(t, "session.finished")
	h.waitIdle(t)
	if started, _, _ := h.recorder.Counts(); started != 0 {
		t.Error("recorder should not run for text submissions")
	}
}

func TestSpeakResponsesOffSkipsTTS(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: false})
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("hi")
	h.waitFor(t, "session.finished")
	h.waitIdle(t)
	if _, plays := h.player.Played(); plays != 0 {
		t.Error("player should not run when speak_responses is off")
	}
}

func TestStateSequenceThroughVoiceSession(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	_, _ = h.engine.StartSession()
	_ = h.engine.StartVoice()
	_, _ = h.engine.StopVoice()
	_ = h.engine.Submit("")

	var states []string
	deadline := time.After(5 * time.Second)
	for len(states) == 0 || states[len(states)-1] != "idle" {
		select {
		case ev := <-h.events:
			if ev.Type == "state.changed" {
				states = append(states, ev.Data["state"].(string))
			}
		case <-deadline:
			t.Fatalf("states so far: %v", states)
		}
	}
	want := []string{"listening", "transcribing", "thinking", "responding", "speaking", "idle"}
	if len(states) != len(want) {
		t.Fatalf("states = %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("states = %v, want %v", states, want)
		}
	}
}

func TestCancelWhileListening(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	_, _ = h.engine.StartSession()
	_ = h.engine.StartVoice()
	if err := h.engine.Cancel(); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "session.cancelled")
	h.waitIdle(t)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, _, cancelled := h.recorder.Counts(); cancelled == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recording was not cancelled")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCancelWhileSpeaking(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	// Slow provider so we can reliably catch active states.
	h.provider.Delay = 5 * time.Millisecond
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("hi")
	h.waitFor(t, "tts.started")
	if err := h.engine.Cancel(); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "session.cancelled")
	h.waitIdle(t)
}

func TestCancelWithNoSessionIsNoop(t *testing.T) {
	h := newHarness(t, Options{})
	if err := h.engine.Cancel(); err != nil {
		t.Errorf("Cancel on idle engine: %v", err)
	}
}

func TestInterruptionStartsNewSessionImmediately(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	h.provider.Delay = 5 * time.Millisecond
	first, _ := h.engine.StartSession()
	_ = h.engine.Submit("first question")
	h.waitFor(t, "tts.started")

	// User invokes Jarvix while it is speaking: speech stops, new session begins.
	second, err := h.engine.StartSession()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("expected a new session id")
	}
	ev := h.waitFor(t, "session.cancelled")
	if ev.Data["session_id"] != first {
		t.Errorf("cancelled session = %v, want %s", ev.Data["session_id"], first)
	}
	if err := h.engine.Submit("second question"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "session.finished")
	h.waitIdle(t)
	if h.provider.LastRequest.Messages[len(h.provider.LastRequest.Messages)-1].Content != "second question" {
		t.Errorf("provider last saw %+v", h.provider.LastRequest.Messages)
	}
}

func TestProviderFailureProducesErrorAndRecovers(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	h.provider.Fail = errors.New("model exploded")
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("hi")

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-h.events:
			if ev.Type == "error" {
				if ev.Data["stage"] != "assistant" {
					t.Errorf("stage = %v", ev.Data["stage"])
				}
				h.waitIdle(t)
				// The daemon survives: a fresh session works.
				h.provider.Fail = nil
				_, _ = h.engine.StartSession()
				_ = h.engine.Submit("again")
				h.waitFor(t, "session.finished")
				return
			}
		case <-deadline:
			t.Fatal("no error event")
		}
	}
}

func TestSTTFailureProducesError(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	h.stt.Fail = errors.New("engine crashed")
	_, _ = h.engine.StartSession()
	_ = h.engine.StartVoice()
	_, _ = h.engine.StopVoice()
	_ = h.engine.Submit("")
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-h.events:
			if ev.Type == "error" {
				if ev.Data["stage"] != "stt" {
					t.Errorf("stage = %v", ev.Data["stage"])
				}
				h.waitIdle(t)
				return
			}
		case <-deadline:
			t.Fatal("no error event")
		}
	}
}

func TestEmptyTranscriptIsFriendlyError(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	h.stt.Text = "  "
	_, _ = h.engine.StartSession()
	_ = h.engine.StartVoice()
	_, _ = h.engine.StopVoice()
	_ = h.engine.Submit("")
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-h.events:
			if ev.Type == "error" {
				h.waitIdle(t)
				return
			}
		case <-deadline:
			t.Fatal("no error event for empty transcript")
		}
	}
}

func TestCancelSpeechStopsOnlySpeech(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	h.provider.Delay = 5 * time.Millisecond
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("hi")
	h.waitFor(t, "tts.started")
	if err := h.engine.CancelSpeech(); err != nil {
		t.Fatal(err)
	}
	ev := h.waitFor(t, "tts.finished")
	if ev.Data["interrupted"] != true {
		t.Errorf("tts.finished data = %v", ev.Data)
	}
	h.waitFor(t, "session.finished")
	h.waitIdle(t)
}

func TestAccidentalTapIsDiscardedQuietly(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true, MinRecording: time.Hour})
	_, _ = h.engine.StartSession()
	_ = h.engine.StartVoice()
	h.waitFor(t, "recording.started")
	// Released (almost) immediately: far below the minimum.
	if _, err := h.engine.StopVoice(); err != nil {
		t.Fatal(err)
	}

	ev := h.waitFor(t, "session.cancelled")
	reason, _ := ev.Data["reason"].(string)
	if !strings.Contains(reason, "too short") {
		t.Errorf("reason = %q", reason)
	}
	h.waitIdle(t)

	// No transcription was attempted, no error event was published, and the
	// recording itself was discarded.
	if h.stt.LastInput.WAVPath != "" {
		t.Error("transcriber must not run for a too-short recording")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, stopped, cancelled := h.recorder.Counts(); cancelled == 1 && stopped == 0 {
			break
		}
		if time.Now().After(deadline) {
			_, stopped, cancelled := h.recorder.Counts()
			t.Fatalf("recording stopped=%d cancelled=%d; want discarded", stopped, cancelled)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Submit from the already-dead session (the PTT release/toggle still
	// sends it) must not resurrect anything.
	if err := h.engine.Submit(""); err == nil {
		t.Error("Submit after discard should report no active session")
	}
}

func TestMinRecordingAllowsNormalHold(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true, MinRecording: 30 * time.Millisecond})
	_, _ = h.engine.StartSession()
	_ = h.engine.StartVoice()
	h.waitFor(t, "recording.started")
	time.Sleep(60 * time.Millisecond) // hold past the minimum
	_, _ = h.engine.StopVoice()
	_ = h.engine.Submit("")
	h.waitFor(t, "transcript.final")
	h.waitFor(t, "session.finished")
	h.waitIdle(t)
}

func TestToolCallLoop(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	// A registry with one tool that records its calls.
	rec := &recordingTool{result: "3 containers running: web, db, cache"}
	h.tools = tools.NewRegistry(nil)
	h.tools.Register(rec)
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, h.tools, bus, nil,
		Options{Model: "m", SpeakResponses: true})

	// Round 0: model asks to run the tool. Round 1: final spoken answer.
	h.provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "run", Arguments: `{"command":"docker ps"}`}},
	}
	h.provider.Response = "You have three containers running: web, db, and cache."

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("what's happening in docker")

	started := h.waitFor(t, "tool.started")
	if started.Data["tool"] != "run" {
		t.Errorf("tool = %v", started.Data["tool"])
	}
	h.waitFor(t, "tool.finished")
	fin := h.waitFor(t, "assistant.finished")
	if fin.Data["content"] != "You have three containers running: web, db, and cache." {
		t.Errorf("answer = %v", fin.Data["content"])
	}
	h.waitFor(t, "session.finished")
	h.waitIdle(t)

	// The tool actually ran, and its result was fed back to the model.
	if rec.calls != 1 {
		t.Errorf("tool called %d times", rec.calls)
	}
	last := h.provider.Requests[len(h.provider.Requests)-1]
	foundResult := false
	for _, m := range last.Messages {
		if m.Role == ai.RoleTool && strings.Contains(m.Content, "web, db, cache") {
			foundResult = true
		}
	}
	if !foundResult {
		t.Error("tool result was not sent back to the provider")
	}
}

func TestToolLoopRunawayIsBounded(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	rec := &recordingTool{result: "ok"}
	h.tools = tools.NewRegistry(nil)
	h.tools.Register(rec)
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, h.tools, bus, nil,
		Options{Model: "m", SpeakResponses: true})

	// Model always asks for a tool, never answers.
	rounds := make([][]ai.ToolCall, maxToolRounds+2)
	for i := range rounds {
		rounds[i] = []ai.ToolCall{{ID: "c", Name: "run", Arguments: `{"command":"x"}`}}
	}
	h.provider.ToolCallsByRound = rounds

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("loop forever")

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-h.events:
			if ev.Type == "error" {
				if !strings.Contains(ev.Data["message"].(string), "tool rounds") {
					t.Errorf("message = %v", ev.Data["message"])
				}
				h.waitIdle(t)
				if rec.calls != maxToolRounds {
					t.Errorf("tool ran %d times, want %d", rec.calls, maxToolRounds)
				}
				return
			}
		case <-deadline:
			t.Fatal("runaway tool loop was not bounded")
		}
	}
}

// recordingTool is a Tool that counts invocations and returns a fixed result.
type recordingTool struct {
	result string
	calls  int
}

func (r *recordingTool) Name() string                { return "run" }
func (r *recordingTool) Description() string          { return "run something" }
func (r *recordingTool) Schema() json.RawMessage      { return json.RawMessage(`{"type":"object"}`) }
func (r *recordingTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	r.calls++
	return r.result, nil
}

func TestVoiceWithoutSessionFails(t *testing.T) {
	h := newHarness(t, Options{})
	if err := h.engine.StartVoice(); err == nil {
		t.Error("StartVoice without a session should fail")
	}
	if err := h.engine.Submit("x"); err == nil {
		t.Error("Submit without a session should fail")
	}
	if _, err := h.engine.StopVoice(); err == nil {
		t.Error("StopVoice without recording should fail")
	}
}
