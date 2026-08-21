// Package session owns the conversational core of jarvixd: the authoritative
// session state machine, the event bus, and the engine that drives one
// interaction from captured speech through transcription, the assistant
// (including tool rounds), and streamed spoken output.
package session

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/history"
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
	// ConfirmTimeout is how long a pending tool confirmation waits for the
	// user before declining. Zero means DefaultConfirmTimeout.
	ConfirmTimeout time.Duration
	// RememberApprovals re-runs a user-approved command without asking again
	// for the rest of the conversation. Approvals live in memory only and
	// are cleared with the conversation — they never survive `jarvix new`,
	// the follow-up window, or a daemon restart.
	RememberApprovals bool
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

	// store persists conversation memory across daemon restarts (ADR 0011);
	// nil disables persistence. Immutable after construction, so it is read
	// without the lock.
	store history.Store
	// persistFailed latches after the first failed save: the engine degrades
	// to in-memory-only for its lifetime and warns exactly once, instead of
	// spamming a warning per exchange on a persistently broken disk.
	persistFailed atomic.Bool
	// now is the follow-up-window clock, injectable so tests can lapse the
	// window deterministically — including across a simulated restart.
	now func() time.Time
	// timer is the confirmation-timeout clock, injectable so tests can fire
	// (or withhold) the timeout deterministically. The returned stop func
	// releases the underlying timer.
	timer func(d time.Duration) (<-chan time.Time, func())
	// progressAfter is how long a slow tool call may run before Jarvix says
	// it is still working (ADR 0016). Immutable after construction; tests
	// shorten it.
	progressAfter time.Duration

	// active tracks the session goroutines (transcribe, think) that read the
	// swappable collaborators and options without holding mu. Reconfigure
	// waits on it so a swap never races a draining goroutine — a cancelled
	// session's think() can still be executing briefly after current is nil.
	active sync.WaitGroup

	mu      sync.Mutex
	state   State
	current *sess
	counter int
	// pending is the tool confirmation the session is waiting on, if any
	// (ADR 0014). Guarded by mu.
	pending *pendingConfirmation
	// approvals are commands the user already confirmed this conversation
	// (remember_for_conversation). Cleared with the conversation; guarded
	// by mu.
	approvals map[string]bool
	// reconfiguring blocks new sessions for the brief window in which
	// Reconfigure drains e.active before swapping collaborators.
	reconfiguring bool

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

	// replyCapture marks a voice capture that answers a pending tool
	// confirmation rather than asking a new question: its transcript is
	// interpreted as yes/no and resolves the confirmation instead of
	// starting a think round.
	replyCapture bool
}

// NewEngine wires the engine. logger, registry, and store may be nil (no
// tools, no persistence). Construction restores any persisted conversation
// so a follow-up asked after a daemon restart still has its context.
func NewEngine(provider ai.Provider, transcriber stt.Transcriber, synthesizer tts.Synthesizer,
	recorder audio.Recorder, player audio.Player, registry *tools.Registry,
	store history.Store, bus *Bus, logger *slog.Logger, opts Options) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	e := &Engine{
		provider: provider,
		stt:      transcriber,
		tts:      synthesizer,
		recorder: recorder,
		player:   player,
		tools:    registry,
		bus:      bus,
		log:      logger,
		opts:     opts,
		store:    store,
		now:      time.Now,
		timer: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTimer(d)
			return t.C, func() { t.Stop() }
		},
		progressAfter: DefaultToolProgressAfter,
		approvals:     make(map[string]bool),
		state:         StateIdle,
	}
	e.loadHistory()
	return e
}

// loadHistory restores persisted conversation memory at construction, before
// any session can run. Persistence must never stop the daemon from booting:
// a corrupt or unreadable file downgrades to a warning and an empty history.
// With memory disabled the on-disk history is removed instead, so nothing
// from an earlier configuration lingers on disk.
func (e *Engine) loadHistory() {
	if e.store == nil {
		return
	}
	if e.opts.HistoryTurns <= 0 {
		if err := e.store.Clear(); err != nil {
			e.log.Warn("could not remove persisted conversation history",
				"component", "session", "error", err.Error())
		}
		return
	}
	msgs, lastTurn, err := e.store.Load()
	if err != nil {
		e.log.Warn("conversation history could not be loaded; starting fresh",
			"component", "session", "error", err.Error())
		return
	}
	if len(msgs) == 0 {
		return
	}
	// The configured turn cap may have shrunk since the history was written.
	if max := e.opts.HistoryTurns * 2; len(msgs) > max {
		msgs = msgs[len(msgs)-max:]
	}
	e.history = msgs
	e.lastTurn = lastTurn
	e.log.Debug("conversation history loaded", "component", "session", "turns", len(msgs)/2)
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
	if e.reconfiguring {
		// Milliseconds at most: Reconfigure only drains goroutine tails.
		return "", fmt.Errorf("new settings are being applied; try again in a moment")
	}
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

// StartVoice begins microphone capture for the active session. While a tool
// confirmation is pending it captures the user's answer instead: the same
// Listening → Transcribing path runs, but the transcript resolves the
// confirmation (yes/no) rather than starting a new question.
func (e *Engine) StartVoice() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.current
	if s == nil {
		return fmt.Errorf("no active session; call session.start first")
	}
	replyCapture := e.state == StateAwaitingConfirmation && e.pending != nil
	if err := e.setStateLocked(StateListening); err != nil {
		return err
	}
	if replyCapture {
		// The user is engaging: the confirmation timeout no longer applies,
		// and the transcript gates below must wait for the reply, not reuse
		// the original question's.
		e.pending.engaged = true
		s.replyCapture = true
		s.transcript = ""
		s.transcriptReady = false
		s.submitted = false
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
	e.active.Add(1)
	go func() { defer e.active.Done(); e.transcribe(s, rec) }()
	return false, nil
}

// Submit marks the session ready to proceed to the assistant. With text, the
// transcript step is skipped entirely (the `jarvix ask` path); without text
// the session proceeds once transcription finishes. While a tool
// confirmation is pending, submitted text is the user's answer to it —
// affirmative approves, anything else declines.
func (e *Engine) Submit(text string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.current
	if s == nil {
		return fmt.Errorf("no active session; call session.start first")
	}
	if text != "" && e.state == StateAwaitingConfirmation && e.pending != nil {
		e.publish(Event{Type: "transcript.final", Data: map[string]any{"session_id": s.id, "text": text}})
		e.resolveConfirmationLocked(isAffirmative(text), "text")
		return nil
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
	e.clearPendingLocked("interrupted")
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
	e.clearPendingLocked("error")
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
	if s.replyCapture {
		// The transcript answers a pending tool confirmation, not a new
		// question. An empty or unrecognised reply declines: the safe
		// reading of anything that is not a clear yes.
		s.replyCapture = false
		e.resolveConfirmationLocked(isAffirmative(s.transcript), "voice")
		return
	}
	if e.state != StateIdle && e.state != StateTranscribing {
		// A duplicate submission (e.g. a stray session.submit while the
		// session is already thinking or awaiting confirmation) must not
		// start a second think round.
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
	e.active.Add(1)
	go func() { defer e.active.Done(); e.think(s) }()
}

// transcribe runs STT on a finished recording, then hands over to the
// assistant when the session has been submitted.
func (e *Engine) transcribe(s *sess, rec audio.Recording) {
	clip, err := rec.Stop()
	if err != nil {
		e.fail(s, "audio", err)
		return
	}
	defer func() { _ = os.Remove(clip.WAVPath) }()

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

		// The model wants tools. Record its request, gate and run each call,
		// append results, and loop. The permission gate (ADR 0014) sits in
		// front of every execution: denied and declined calls still produce
		// a result message, so the model can answer gracefully instead of
		// the session dying.
		messages = append(messages, ai.Message{Role: ai.RoleAssistant, Content: text, ToolCalls: calls})
		for _, call := range calls {
			if s.ctx.Err() != nil {
				e.abortSpeaker(speaker)
				return
			}
			result, ok := e.gateAndExecute(s, call)
			if !ok {
				e.abortSpeaker(speaker)
				return
			}
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

	// Persist only after the session has fully finished, off the lock path:
	// disk I/O adds zero latency to the spoken exchange.
	e.persistHistory()
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
		e.now().Sub(e.lastTurn) > e.opts.FollowUpWindow {
		e.log.Debug("conversation history expired", "component", "session",
			"turns", len(e.history)/2)
		e.history = nil
		e.lastTurn = time.Time{}
		// Remembered tool approvals are conversation-scoped: a new thread
		// must ask again.
		e.approvals = make(map[string]bool)
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
	e.lastTurn = e.now()
}

// persistHistory writes the conversation to disk so it survives a daemon
// restart. It runs after session.finished, never under mu (beyond a brief
// snapshot), and treats failure as degradation: the engine keeps working in
// memory and warns exactly once. Turn contents are never logged.
func (e *Engine) persistHistory() {
	if e.store == nil || e.opts.HistoryTurns <= 0 || e.persistFailed.Load() {
		return
	}
	e.mu.Lock()
	msgs := append([]ai.Message(nil), e.history...)
	lastTurn := e.lastTurn
	e.mu.Unlock()
	if err := e.store.Save(msgs, lastTurn); err != nil {
		if e.persistFailed.CompareAndSwap(false, true) {
			e.log.Warn("conversation history could not be saved; continuing in memory only",
				"component", "session", "error", err.Error())
		}
		return
	}
	e.log.Debug("conversation history saved", "component", "session", "turns", len(msgs)/2)
}

// Turn is one utterance of the current conversation as a client should
// display it. Only what carries meaning for a reader is included: committed
// user/assistant exchanges plus the in-flight user question — no system
// prompt, no tool traffic.
type Turn struct {
	Role string `json:"role"` // "user" or "assistant"
	Text string `json:"text"`
}

// Conversation returns the turns of the current conversation, oldest first,
// for the conversation window (the `conversation.get` IPC method). The
// active session's transcript is included as soon as it is known, so a
// window opened mid-session shows the question being answered; the streamed
// answer itself reaches clients via assistant.delta events. The lazy
// follow-up-window reset is deliberately not applied here: this reports what
// happened, not what the next turn will remember. Never nil — an empty
// conversation is an empty slice, so clients always see a JSON array.
func (e *Engine) Conversation() []Turn {
	e.mu.Lock()
	defer e.mu.Unlock()
	turns := make([]Turn, 0, len(e.history)+1)
	for _, m := range e.history {
		turns = append(turns, Turn{Role: string(m.Role), Text: m.Content})
	}
	if s := e.current; s != nil && s.transcriptReady && strings.TrimSpace(s.transcript) != "" {
		turns = append(turns, Turn{Role: string(ai.RoleUser), Text: s.transcript})
	}
	return turns
}

// ResetConversation clears the carried-over context — in memory and on disk —
// so the next turn starts a fresh thread and a later restart resurrects
// nothing. Remembered tool approvals die with the conversation too: they are
// scoped to it by design and never persist.
func (e *Engine) ResetConversation() {
	e.mu.Lock()
	e.history = nil
	e.lastTurn = time.Time{}
	e.approvals = make(map[string]bool)
	e.mu.Unlock()
	if e.store == nil {
		return
	}
	if err := e.store.Clear(); err != nil {
		e.log.Warn("could not remove persisted conversation history",
			"component", "session", "error", err.Error())
		return
	}
	e.log.Debug("conversation history cleared", "component", "session")
}

// Reconfigure swaps the engine's collaborators and options for a new
// configuration without a daemon restart. It refuses while a session is
// active: adapters are only ever swapped between sessions, never under one —
// a reload that cannot apply keeps the running configuration untouched.
//
// Idle state alone is not enough to swap safely: session goroutines read the
// swapped fields without holding mu, and a finished or cancelled session's
// think()/transcribe() may still be draining after current went nil. So
// Reconfigure briefly blocks new sessions, waits for every tracked session
// goroutine to exit, and only then swaps — making the swap invisible both to
// running code and to the race detector.
func (e *Engine) Reconfigure(provider ai.Provider, transcriber stt.Transcriber,
	synthesizer tts.Synthesizer, recorder audio.Recorder, player audio.Player,
	opts Options) error {
	e.mu.Lock()
	if e.reconfiguring {
		e.mu.Unlock()
		return fmt.Errorf("new settings are already being applied")
	}
	if e.current != nil || e.state != StateIdle {
		e.mu.Unlock()
		return fmt.Errorf("a session is active; the new settings apply once it finishes (run config.reload)")
	}
	e.reconfiguring = true
	e.mu.Unlock()

	// No new session can start now; drain the tails of past sessions.
	e.active.Wait()

	e.mu.Lock()
	defer e.mu.Unlock()
	e.reconfiguring = false
	e.provider = provider
	e.stt = transcriber
	e.tts = synthesizer
	e.recorder = recorder
	e.player = player
	e.opts = opts

	// Conversation memory follows the new limits immediately, mirroring what
	// loadHistory enforces at construction.
	if opts.HistoryTurns <= 0 {
		e.history = nil
		e.lastTurn = time.Time{}
		if e.store != nil {
			if err := e.store.Clear(); err != nil {
				e.log.Warn("could not remove persisted conversation history",
					"component", "session", "error", err.Error())
			}
		}
	} else if max := opts.HistoryTurns * 2; len(e.history) > max {
		e.history = append([]ai.Message(nil), e.history[len(e.history)-max:]...)
	}
	e.log.Info("engine reconfigured", "component", "session",
		"provider", provider.Name(), "tts", synthesizer.Name(), "model", opts.Model)
	return nil
}
