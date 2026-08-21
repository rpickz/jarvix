package session

import "fmt"

// State is the authoritative session state. There is exactly one State per
// daemon; no behaviour hangs off ad-hoc booleans.
type State string

// Session states.
const (
	StateIdle         State = "idle"
	StateListening    State = "listening"
	StateTranscribing State = "transcribing"
	StateThinking     State = "thinking"
	StateResponding   State = "responding"
	// Acting is the deterministic intent router's state (ADR 0017): the
	// transcript matched the intent table, so the session is carrying out a
	// local action instead of asking the model. It exists precisely because
	// a matched intent must NOT pass through Thinking → Responding — those
	// states mean "a provider request is open", and here none ever is.
	StateActing State = "acting"
	// AwaitingConfirmation is the permission gate's ask tier (ADR 0014): a
	// tool call needs the user's go-ahead. Jarvix has spoken (or is
	// speaking) the intent summary and the exact command is on the bus;
	// nothing executes until an affirmative arrives — a decline, timeout,
	// or interruption returns "declined" to the model instead.
	StateAwaitingConfirmation State = "awaiting_confirmation"
	StateSpeaking             State = "speaking"
	StateCancelling           State = "cancelling"
	StateError                State = "error"
)

// transitions is the complete set of legal state changes. Anything not listed
// is a programming error, caught by tests and rejected at runtime.
var transitions = map[State][]State{
	// Idle → Acting is `jarvix ask "volume thirty"`: submitted text that the
	// intent router claims, so the session never reaches the model.
	StateIdle:      {StateListening, StateThinking, StateActing},
	StateListening: {StateTranscribing, StateCancelling, StateError},
	// Transcribing → Speaking is not legal: a transcript must pass through
	// Thinking (a model answer) or Acting (a matched intent). Cancellation is
	// legal from every active state.
	StateTranscribing: {StateThinking, StateActing, StateCancelling, StateError},
	// Acting → Speaking speaks the terse acknowledgement; Acting → Idle is the
	// silent completion ("stop", or speech disabled). Acting →
	// AwaitingConfirmation is a user-defined intent whose command the
	// permission gate classified as ask — user-written commands are gated
	// exactly like the model's (ADR 0014).
	StateActing: {StateSpeaking, StateAwaitingConfirmation, StateIdle, StateCancelling, StateError},
	// Thinking → AwaitingConfirmation: the model requested a tool the
	// policy classifies as ask; execution pauses for the user.
	StateThinking: {StateResponding, StateAwaitingConfirmation, StateCancelling, StateError},
	// Responding → Thinking happens when the model streamed some text and
	// then asked to call a tool: it goes back to working before answering.
	// Performed by Engine.backToThinking — it was documented here long before
	// any code path made it, and the round that needed it (streamed text, no
	// speech claiming Speaking first) died silently instead (issue #55).
	// Responding → AwaitingConfirmation is the same moment when that tool
	// call needs the user's go-ahead.
	StateResponding: {StateSpeaking, StateThinking, StateAwaitingConfirmation, StateIdle, StateCancelling, StateError},
	// AwaitingConfirmation → Thinking: resolved (approved, declined, or
	// timed out) — the tool loop continues either way, because a decline is
	// reported to the model as a result, not an error.
	// AwaitingConfirmation → Listening: the user answers by voice; the
	// reply capture reuses the normal Listening → Transcribing path and
	// resolves from Transcribing → Thinking.
	// AwaitingConfirmation → Acting: the same resolution for a user-defined
	// intent, which returns to Acting rather than to a tool loop.
	StateAwaitingConfirmation: {StateThinking, StateActing, StateListening, StateCancelling, StateError},
	// Speaking is not the end of a turn. Streaming speech (ADR 0010, sharpened
	// by the warm workers of ADR 0018) starts on the *first complete sentence*,
	// so audio is routinely already playing while the model is still deciding
	// what else to do — including asking for tools. The table originally read
	// Speaking as "the answer is finished, it is only being said", which is why
	// a tool call arriving mid-sentence killed the session (issue #52): the
	// engine had nowhere legal to go.
	//
	// Speaking → AwaitingConfirmation is that moment for an ask-tier call: the
	// question is queued behind the audio already playing (speaker.go) so the
	// user hears one voice at a time, and the answer resolves back through
	// Thinking like any other confirmation.
	//
	// Speaking → Responding is the same moment for a call that needed no
	// confirmation: the tool ran under the speech, and the next round's first
	// token continues the answer. Without it the tool loop had no way back and
	// the session simply stopped — no error, no session.finished, nothing.
	StateSpeaking:   {StateIdle, StateResponding, StateAwaitingConfirmation, StateCancelling, StateError},
	StateCancelling: {StateIdle},
	StateError:      {StateIdle},
}

// toolRequestStates enumerates every state a tool call can be requested from,
// and is the enumeration issue #52 asked for by name. The defect there was not
// a missing table entry; it was that nobody had listed the states from which a
// tool call can actually arrive, so a state added for one feature (Speaking,
// once speech began before generation finished) silently became a state the
// permission gate could be entered from — and was not.
//
//   - Thinking: the model asked for a tool before saying anything.
//   - Responding: it streamed some text first (speech off, or the sentence is
//     not complete yet).
//   - Speaking: it streamed a complete sentence, which is already playing.
//   - Acting: a user-defined intent whose command the gate classified as ask
//     (ADR 0017) — user-written commands are gated exactly like the model's.
//
// Every state in transitions is either in this list or in the test's companion
// list of states no tool call can arrive in, and the two must together cover
// the table exactly — so a state added by the next feature cannot join without
// someone deciding, on the record, which kind it is.
var toolRequestStates = []State{
	StateThinking,
	StateResponding,
	StateSpeaking,
	StateActing,
}

// CanTransition reports whether from → to is a legal state change.
func CanTransition(from, to State) bool {
	for _, next := range transitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// Active reports whether the state belongs to an in-flight session.
func (s State) Active() bool { return s != StateIdle }

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	_, ok := transitions[s]
	return ok
}

func invalidTransition(from, to State) error {
	return fmt.Errorf("invalid state transition %s → %s", from, to)
}
