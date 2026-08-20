package session

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
)

// Options tunes engine behaviour. Values come from configuration.
type Options struct {
	Model          string
	SystemPrompt   string
	MaxTokens      int
	Temperature    float64
	SpeakResponses bool
	// MinRecording discards captures shorter than this as accidental
	// activations (a stray key tap) instead of transcribing them.
	MinRecording time.Duration
	// HistoryTurns is how many prior user+assistant exchanges to keep as
	// conversation context. Zero disables memory (each turn is standalone).
	HistoryTurns int
	// FollowUpWindow resets the conversation when this long has passed since
	// the last exchange, so an old thread does not bleed into a new one. Zero
	// keeps history until an explicit reset.
	FollowUpWindow time.Duration
}

// Engine owns the session lifecycle: one active session at a time, one
// authoritative state, cancellation from every active state, and interruption
// (a new session while speaking) as a first-class operation.
type Engine struct {
	provider ai.Provider
	stt      stt.Transcriber
	tts      tts.Synthesizer
	recorder audio.Recorder
	player   audio.Player
	tools    *tools.Registry
	bus      *Bus
	log      *slog.Logger
	opts     Options

	mu      sync.Mutex
	state   State
	current *sess
	counter int

	// Conversation memory: prior exchanges carried across sessions so
	// follow-up questions have context. Guarded by mu.
	history  []ai.Message
	lastTurn time.Time
}

// sess is one interaction from start to finish.
type sess struct {
	id           string
	ctx          context.Context
	cancel       context.CancelFunc
	recording    audio.Recording
	started      time.Time
	voiceStarted time.Time

	// A session proceeds to Thinking only once both are true: the transcript
	// is ready (from STT or provided directly) and the client has submitted.
	transcript      string
	transcriptReady bool
	submitted       bool
}

// NewEngine wires the engine. logger and registry may be nil (no tools).
func NewEngine(provider ai.Provider, transcriber stt.Transcriber, synthesizer tts.Synthesizer,
	recorder audio.Recorder, player audio.Player, registry *tools.Registry,
	bus *Bus, logger *slog.Logger, opts Options) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{
		provider: provider,
		stt:      transcriber,
		tts:      synthesizer,
		recorder: recorder,
		player:   player,
		tools:    registry,
		bus:      bus,
		log:      logger,
		opts:     opts,
		state:    StateIdle,
	}
}

// State returns the current state and active session id ("" when idle).
func (e *Engine) State() (State, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := ""
	if e.current != nil {
		id = e.current.id
	}
	return e.state, id
}

// StartSession begins a new session. If a session is already active — for
// example Jarvix is mid-sentence — it is cancelled immediately so the new
// interaction can begin: interruption must feel instant.
func (e *Engine) StartSession() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current != nil {
		e.cancelLocked("interrupted by new session")
	}
	e.counter++
	ctx, cancel := context.WithCancel(context.Background())
	e.current = &sess{
		id:      fmt.Sprintf("s%d", e.counter),
		ctx:     ctx,
		cancel:  cancel,
		started: time.Now(),
	}
	e.log.Info("session started", "component", "session", "session_id", e.current.id)
	return e.current.id, nil
}

// StartVoice begins microphone capture for the active session.
func (e *Engine) StartVoice() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.current
	if s == nil {
		return fmt.Errorf("no active session; call session.start first")
	}
	if err := e.setStateLocked(StateListening); err != nil {
		return err
	}
	rec, err := e.recorder.Start(s.ctx)
	if err != nil {
		e.failLocked(s, "audio", err)
		return err
	}
	s.recording = rec
	s.voiceStarted = time.Now()
	e.publish(Event{Type: "recording.started", Data: map[string]any{"session_id": s.id}})
	return nil
}

// StopVoice ends capture and starts transcription in the background. A
// capture shorter than Options.MinRecording is discarded as an accidental
// activation: no transcription, no error — the session just ends quietly,
// and discarded=true tells the caller to skip its follow-up submit.
func (e *Engine) StopVoice() (discarded bool, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.current
	if s == nil || s.recording == nil {
		return false, fmt.Errorf("not recording")
	}
	if held := time.Since(s.voiceStarted); held < e.opts.MinRecording {
		e.log.Info("recording discarded as accidental", "component", "session",
			"session_id", s.id, "held_ms", held.Milliseconds(),
			"min_ms", e.opts.MinRecording.Milliseconds())
		e.cancelLocked(fmt.Sprintf("recording too short (%dms, minimum %dms)",
			held.Milliseconds(), e.opts.MinRecording.Milliseconds()))
		return true, nil
	}
	if err := e.setStateLocked(StateTranscribing); err != nil {
		return false, err
	}
	rec := s.recording
	s.recording = nil
	e.publish(Event{Type: "recording.stopped", Data: map[string]any{"session_id": s.id}})
	go e.transcribe(s, rec)
	return false, nil
}

// Submit marks the session ready to proceed to the assistant. With text, the
// transcript step is skipped entirely (the `jarvix ask` path); without text
// the session proceeds once transcription finishes.
func (e *Engine) Submit(text string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.current
	if s == nil {
		return fmt.Errorf("no active session; call session.start first")
	}
	if text != "" {
		s.transcript = text
		s.transcriptReady = true
		e.publish(Event{Type: "transcript.final", Data: map[string]any{"session_id": s.id, "text": text}})
	}
	s.submitted = true
	e.maybeThinkLocked(s)
	return nil
}

// Cancel stops everything associated with the current interaction.
func (e *Engine) Cancel() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return nil // nothing to cancel; not an error
	}
	e.cancelLocked("cancelled")
	return nil
}

// CancelSpeech stops spoken output. In V1 speech is the final stage, so this
// also completes the session; text output is untouched.
func (e *Engine) CancelSpeech() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.current
	if s == nil || e.state != StateSpeaking {
		return nil
	}
	s.cancel()
	_ = e.setStateLocked(StateIdle)
	e.publish(Event{Type: "tts.finished", Data: map[string]any{"session_id": s.id, "interrupted": true}})
	e.finishLocked(s)
	return nil
}

// ---------------------------------------------------------------- internals

// setStateLocked performs a validated transition and publishes state.changed.
func (e *Engine) setStateLocked(to State) error {
	if !CanTransition(e.state, to) {
		return invalidTransition(e.state, to)
	}
	e.state = to
	id := ""
	if e.current != nil {
		id = e.current.id
	}
	e.log.Debug("state changed", "component", "session", "state", string(to), "session_id", id)
	e.publish(Event{Type: "state.changed", Data: map[string]any{"state": string(to), "session_id": id}})
	return nil
}

// advance transitions on behalf of a background stage, refusing if the
// session was superseded or cancelled in the meantime.
func (e *Engine) advance(s *sess, to State) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current != s || s.ctx.Err() != nil {
		return false
	}
	return e.setStateLocked(to) == nil
}

func (e *Engine) publish(ev Event) {
	if e.bus != nil {
		e.bus.Publish(ev)
	}
}

// cancelLocked tears the current session down through Cancelling → Idle.
func (e *Engine) cancelLocked(reason string) {
	s := e.current
	if s == nil {
		return
	}
	s.cancel()
	if s.recording != nil {
		rec := s.recording
		s.recording = nil
		// Recording teardown can block briefly; do not hold the lock for it.
		go rec.Cancel()
	}
	if e.state.Active() {
		_ = e.setStateLocked(StateCancelling)
	}
	_ = e.setStateLocked(StateIdle)
	e.publish(Event{Type: "session.cancelled", Data: map[string]any{"session_id": s.id, "reason": reason}})
	e.log.Info("session cancelled", "component", "session", "session_id", s.id,
		"reason", reason, "duration_ms", time.Since(s.started).Milliseconds())
	e.current = nil
}

// finishLocked completes the session normally.
func (e *Engine) finishLocked(s *sess) {
	if e.current != s {
		return
	}
	if e.state.Active() {
		_ = e.setStateLocked(StateIdle)
	}
	e.publish(Event{Type: "session.finished", Data: map[string]any{"session_id": s.id}})
	e.log.Info("session finished", "component", "session", "session_id", s.id,
		"duration_ms", time.Since(s.started).Milliseconds())
	s.cancel()
	e.current = nil
}

// fail reports a stage failure and ends the session. Cancellation is not a
// failure: if the session's context is already done, the cancel path has
// spoken for the outcome.
func (e *Engine) fail(s *sess, stage string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failLocked(s, stage, err)
}

func (e *Engine) failLocked(s *sess, stage string, err error) {
	if e.current != s || s.ctx.Err() != nil {
		return
	}
	e.log.Error("session failed", "component", stage, "session_id", s.id, "error", err.Error())
	if CanTransition(e.state, StateError) {
		_ = e.setStateLocked(StateError)
	}
	e.publish(Event{Type: "error", Data: map[string]any{
		"session_id": s.id, "stage": stage, "message": err.Error(),
	}})
	_ = e.setStateLocked(StateIdle)
	e.publish(Event{Type: "session.finished", Data: map[string]any{"session_id": s.id}})
	s.cancel()
	e.current = nil
}

// maybeThinkLocked advances to Thinking once transcript and submission have
// both arrived, whichever order they came in.
func (e *Engine) maybeThinkLocked(s *sess) {
	if !s.transcriptReady || !s.submitted || e.current != s {
		return
	}
	if strings.TrimSpace(s.transcript) == "" {
		e.failLocked(s, "stt", fmt.Errorf("I didn't catch that — no speech was recognised"))
		return
	}
	if err := e.setStateLocked(StateThinking); err != nil {
		e.failLocked(s, "session", err)
		return
	}
	go e.think(s)
}

// transcribe runs STT on a finished recording, then hands over to the
// assistant when the session has been submitted.
func (e *Engine) transcribe(s *sess, rec audio.Recording) {
	clip, err := rec.Stop()
	if err != nil {
		e.fail(s, "audio", err)
		return
	}
	defer os.Remove(clip.WAVPath)

	start := time.Now()
	events, err := e.stt.Transcribe(s.ctx, stt.AudioInput{
		WAVPath:    clip.WAVPath,
		SampleRate: clip.SampleRate,
		Channels:   clip.Channels,
	})
	if err != nil {
		e.fail(s, "stt", err)
		return
	}
	for ev := range events {
		switch ev.Type {
		case stt.EventPartial:
			e.publish(Event{Type: "transcript.partial", Data: map[string]any{"session_id": s.id, "text": ev.Text}})
		case stt.EventFinal:
			e.log.Info("transcription finished", "component", "stt",
				"session_id", s.id, "duration_ms", time.Since(start).Milliseconds())
			e.mu.Lock()
			if e.current == s {
				s.transcript = ev.Text
				s.transcriptReady = true
				e.publish(Event{Type: "transcript.final", Data: map[string]any{"session_id": s.id, "text": ev.Text}})
				e.maybeThinkLocked(s)
			}
			e.mu.Unlock()
		case stt.EventError:
			if s.ctx.Err() == nil {
				e.fail(s, "stt", ev.Err)
			}
			return
		}
	}
}

// maxToolRounds bounds the tool-call loop so a model that keeps requesting
// tools without answering cannot loop forever.
const maxToolRounds = 6

// think drives the assistant: it streams a response — speaking each sentence
// aloud as soon as it is complete — and whenever the model requests tools it
// executes them (under the session context), feeds the results back, and
// continues, until the model produces a final answer or the round budget is
// exhausted. Prior exchanges are carried in as conversation context.
func (e *Engine) think(s *sess) {
	messages := e.conversationMessages(s.transcript)

	var toolDefs []ai.ToolDef
	if e.tools != nil && !e.tools.Empty() {
		toolDefs = e.tools.Defs()
	}

	var speaker *streamingSpeaker
	if e.opts.SpeakResponses && e.tts != nil {
		speaker = newStreamingSpeaker(e, s)
	}

	e.publish(Event{Type: "assistant.started", Data: map[string]any{"session_id": s.id, "provider": e.provider.Name()}})

	finalText := ""
	for round := 0; round < maxToolRounds; round++ {
		text, calls, ok := e.streamOnce(s, ai.ChatRequest{
			Model:       e.opts.Model,
			MaxTokens:   e.opts.MaxTokens,
			Temperature: e.opts.Temperature,
			Messages:    messages,
			Tools:       toolDefs,
		}, speaker)
		if !ok {
			e.abortSpeaker(speaker) // failed/cancelled/superseded — already reported
			return
		}
		if len(calls) == 0 {
			finalText = text
			break
		}
		if round == maxToolRounds-1 {
			e.abortSpeaker(speaker)
			e.fail(s, "assistant", fmt.Errorf("stopped after %d tool rounds without a final answer", maxToolRounds))
			return
		}

		// The model wants tools. Record its request, run each call, append
		// results, and loop.
		messages = append(messages, ai.Message{Role: ai.RoleAssistant, Content: text, ToolCalls: calls})
		for _, call := range calls {
			if s.ctx.Err() != nil {
				e.abortSpeaker(speaker)
				return
			}
			e.publish(Event{Type: "tool.started", Data: map[string]any{
				"session_id": s.id, "tool": call.Name, "arguments": call.Arguments}})
			result := e.tools.Execute(s.ctx, call)
			e.publish(Event{Type: "tool.finished", Data: map[string]any{
				"session_id": s.id, "tool": call.Name}})
			messages = append(messages, ai.Message{Role: ai.RoleTool, ToolCallID: call.ID, Content: result})
		}
	}

	e.publish(Event{Type: "assistant.finished", Data: map[string]any{"session_id": s.id, "content": finalText}})
	if finalText == "" {
		e.abortSpeaker(speaker)
		e.fail(s, "assistant", fmt.Errorf("the assistant returned an empty response"))
		return
	}

	// Wait for the last sentence to finish speaking, then record the exchange
	// so the next turn has this one as context.
	if speaker != nil {
		if err := speaker.close(); err != nil {
			if s.ctx.Err() == nil {
				e.fail(s, "tts", err)
			}
			return
		}
	}
	e.commitTurn(s.transcript, finalText)

	e.mu.Lock()
	e.finishLocked(s)
	e.mu.Unlock()
}

// streamOnce runs one provider turn, forwarding text deltas to the overlay
// and, when a speaker is present, feeding complete sentences to it as they
// form. ok is false when it already handled a terminal condition.
func (e *Engine) streamOnce(s *sess, req ai.ChatRequest, speaker *streamingSpeaker) (text string, calls []ai.ToolCall, ok bool) {
	start := time.Now()
	events, err := e.provider.Chat(s.ctx, req)
	if err != nil {
		e.fail(s, "assistant", err)
		return "", nil, false
	}
	var full strings.Builder
	var sc sentencer
	responded := false
	for ev := range events {
		switch ev.Type {
		case ai.EventDelta:
			if !responded {
				if !e.advance(s, StateResponding) {
					return "", nil, false
				}
				responded = true
				e.publish(Event{Type: "assistant.delta", Data: map[string]any{"session_id": s.id, "content": ""}})
			}
			full.WriteString(ev.Content)
			e.publish(Event{Type: "assistant.delta", Data: map[string]any{"session_id": s.id, "content": ev.Content}})
			if speaker != nil {
				for _, sentence := range sc.push(ev.Content) {
					speaker.speak(sentence)
				}
			}
		case ai.EventToolCall:
			calls = append(calls, ev.Call)
		case ai.EventError:
			if s.ctx.Err() == nil {
				e.fail(s, "assistant", ev.Err)
			}
			return "", nil, false
		case ai.EventDone:
		}
	}
	if speaker != nil {
		for _, sentence := range sc.flush() {
			speaker.speak(sentence)
		}
	}
	e.log.Info("assistant turn finished", "component", "assistant", "session_id", s.id,
		"provider", e.provider.Name(), "tool_calls", len(calls),
		"duration_ms", time.Since(start).Milliseconds())
	return strings.TrimSpace(full.String()), calls, true
}

// abortSpeaker closes a speaker without treating its result as a fresh error:
// the session already failed or was cancelled, and that path owns the events.
func (e *Engine) abortSpeaker(speaker *streamingSpeaker) {
	if speaker != nil {
		_ = speaker.close()
	}
}

// conversationMessages builds the provider message list for a new turn:
// system prompt, carried-over history (reset if the follow-up window lapsed),
// then the new user message.
func (e *Engine) conversationMessages(userText string) []ai.Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.opts.FollowUpWindow > 0 && !e.lastTurn.IsZero() &&
		time.Since(e.lastTurn) > e.opts.FollowUpWindow {
		e.history = nil
	}
	msgs := make([]ai.Message, 0, len(e.history)+2)
	if e.opts.SystemPrompt != "" {
		msgs = append(msgs, ai.Message{Role: ai.RoleSystem, Content: e.opts.SystemPrompt})
	}
	msgs = append(msgs, e.history...)
	msgs = append(msgs, ai.Message{Role: ai.RoleUser, Content: userText})
	return msgs
}

// commitTurn records a completed exchange as context for the next turn, kept
// to the configured number of turns. Intermediate tool traffic is not stored;
// the user question and the assistant's final answer are what carry meaning.
func (e *Engine) commitTurn(userText, assistantText string) {
	if e.opts.HistoryTurns <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.history = append(e.history,
		ai.Message{Role: ai.RoleUser, Content: userText},
		ai.Message{Role: ai.RoleAssistant, Content: assistantText})
	if max := e.opts.HistoryTurns * 2; len(e.history) > max {
		e.history = append([]ai.Message(nil), e.history[len(e.history)-max:]...)
	}
	e.lastTurn = time.Now()
}

// ResetConversation clears the carried-over context so the next turn starts a
// fresh thread.
func (e *Engine) ResetConversation() {
	e.mu.Lock()
	e.history = nil
	e.lastTurn = time.Time{}
	e.mu.Unlock()
}
