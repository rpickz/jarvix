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
	"github.com/rpickz/jarvix/internal/history"
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
	return newHarnessWithStore(t, opts, nil)
}

// newHarnessWithStore wires the engine to a history store. The persistence
// tests build two harnesses over the same store: the store survives between
// them the way the disk survives a daemon restart.
func newHarnessWithStore(t *testing.T, opts Options, store history.Store) *harness {
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
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, h.tools, store, bus, nil, opts)
	return h
}

// ask drives one complete text exchange through the engine.
func (h *harness) ask(t *testing.T, text string) {
	t.Helper()
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit(text); err != nil {
		t.Fatal(err)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)
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

// collectUntil drains events until one of terminalType arrives, returning
// every event seen (keyed by type, last value wins). Order-independent, which
// matters once speech streams: tts.started can precede assistant.finished.
func (h *harness) collectUntil(t *testing.T, terminalType string) map[string]Event {
	t.Helper()
	seen := map[string]Event{}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-h.events:
			seen[ev.Type] = ev
			if ev.Type == terminalType {
				return seen
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q; saw %v", terminalType, keysOf(seen))
		}
	}
}

func keysOf(m map[string]Event) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
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

	seen := h.collectUntil(t, "session.finished")
	if seen["transcript.final"].Data["text"] != "explain recursion" {
		t.Errorf("transcript = %v", seen["transcript.final"].Data["text"])
	}
	if seen["assistant.finished"].Data["content"] != "Recursion is a function calling itself." {
		t.Errorf("response = %v", seen["assistant.finished"].Data["content"])
	}
	for _, want := range []string{"assistant.started", "tts.started", "tts.finished"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("missing event %q", want)
		}
	}
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
	// Hold speech in progress deterministically: no chunk is delivered until
	// the gate opens, so the cancel always lands mid-speech.
	hold := make(chan struct{})
	h.tts.SetHold(hold)
	defer close(hold)
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
	// Keep the first session speaking so the interrupt lands mid-utterance:
	// the gate is never opened for it, only cancellation releases it.
	hold := make(chan struct{})
	h.tts.SetHold(hold)
	defer close(hold)
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
	// The second session's speech must run to completion: remove the gate.
	h.tts.SetHold(nil)
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
	// Deterministic: speech cannot complete before the cancel — no chunk is
	// delivered until the gate opens, and only cancellation opens it here.
	hold := make(chan struct{})
	h.tts.SetHold(hold)
	defer close(hold)
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
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, h.tools, nil, bus, nil,
		Options{Model: "m", SpeakResponses: true})

	// Round 0: model asks to run the tool. Round 1: final spoken answer.
	h.provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "run", Arguments: `{"command":"docker ps"}`}},
	}
	h.provider.Response = "You have three containers running: web, db, and cache."

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("what's happening in docker")

	seen := h.collectUntil(t, "session.finished")
	if seen["tool.started"].Data["tool"] != "run" {
		t.Errorf("tool = %v", seen["tool.started"].Data["tool"])
	}
	if _, ok := seen["tool.finished"]; !ok {
		t.Error("missing tool.finished")
	}
	if seen["assistant.finished"].Data["content"] != "You have three containers running: web, db, and cache." {
		t.Errorf("answer = %v", seen["assistant.finished"].Data["content"])
	}
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
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, h.tools, nil, bus, nil,
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
				// The final round fails before running its tools (they would
				// have no round left to be used in), so tools run one fewer.
				if rec.calls != maxToolRounds-1 {
					t.Errorf("tool ran %d times, want %d", rec.calls, maxToolRounds-1)
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

func (r *recordingTool) Name() string            { return "run" }
func (r *recordingTool) Description() string     { return "run something" }
func (r *recordingTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (r *recordingTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	r.calls++
	return r.result, nil
}

func TestConversationMemoryCarriesContext(t *testing.T) {
	h := newHarness(t, Options{HistoryTurns: 8, FollowUpWindow: time.Hour})

	ask := func(text string) {
		_, _ = h.engine.StartSession()
		_ = h.engine.Submit(text)
		h.collectUntil(t, "session.finished")
		h.waitIdle(t)
	}
	ask("why is my build failing?")
	ask("what should I change?")

	// The second turn's request must contain the first exchange as context.
	last := h.provider.Requests[len(h.provider.Requests)-1]
	var roles []string
	for _, m := range last.Messages {
		roles = append(roles, string(m.Role)+":"+m.Content)
	}
	joined := strings.Join(roles, " | ")
	if !strings.Contains(joined, "why is my build failing?") {
		t.Errorf("second turn lost the first question: %s", joined)
	}
	if !strings.Contains(joined, "what should I change?") {
		t.Errorf("second turn missing its own question: %s", joined)
	}
}

func TestConversationResetForgetsContext(t *testing.T) {
	h := newHarness(t, Options{HistoryTurns: 8, FollowUpWindow: time.Hour})
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("first")
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	h.engine.ResetConversation()

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("second")
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	last := h.provider.Requests[len(h.provider.Requests)-1]
	for _, m := range last.Messages {
		if strings.Contains(m.Content, "first") {
			t.Error("reset did not clear prior context")
		}
	}
}

func TestConversationFollowUpWindowExpires(t *testing.T) {
	// A zero window with a non-zero lastTurn would keep history; a tiny window
	// expires immediately, so the second turn starts fresh.
	h := newHarness(t, Options{HistoryTurns: 8, FollowUpWindow: time.Nanosecond})
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("first")
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	time.Sleep(2 * time.Millisecond) // exceed the window

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("second")
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	last := h.provider.Requests[len(h.provider.Requests)-1]
	for _, m := range last.Messages {
		if strings.Contains(m.Content, "first") {
			t.Error("stale conversation was not reset after the follow-up window")
		}
	}
}

func TestHistoryDisabledByDefault(t *testing.T) {
	h := newHarness(t, Options{}) // HistoryTurns 0
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("first")
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("second")
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	last := h.provider.Requests[len(h.provider.Requests)-1]
	if len(last.Messages) != 1 {
		t.Errorf("history should be off: %d messages", len(last.Messages))
	}
}

func TestStreamingSpeechSpeaksMultipleSentences(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	h.provider.Response = "First sentence here. Second sentence follows. Third one ends it."
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("tell me three things")
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	// Each sentence is synthesized separately as it streams — proof that
	// speech does not wait for the whole message.
	if h.tts.Speaks() < 3 {
		t.Errorf("expected >=3 sentence syntheses, got %d", h.tts.Speaks())
	}
	// But playback is one continuous stream, not one Play per sentence.
	if _, plays := h.player.Played(); plays != 1 {
		t.Errorf("player Play calls = %d, want 1 continuous stream", plays)
	}
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
