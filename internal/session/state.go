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
	StateIdle:      {StateListening, StateThinking},
	StateListening: {StateTranscribing, StateCancelling, StateError},
	// Transcribing → Speaking is not legal: a transcript must pass through
	// Thinking. Cancellation is legal from every active state.
	StateTranscribing: {StateThinking, StateCancelling, StateError},
	// Thinking → AwaitingConfirmation: the model requested a tool the
	// policy classifies as ask; execution pauses for the user.
	StateThinking: {StateResponding, StateAwaitingConfirmation, StateCancelling, StateError},
	// Responding → Thinking happens when the model streamed some text and
	// then asked to call a tool: it goes back to working before answering.
	// Responding → AwaitingConfirmation is the same moment when that tool
	// call needs the user's go-ahead.
	StateResponding: {StateSpeaking, StateThinking, StateAwaitingConfirmation, StateIdle, StateCancelling, StateError},
	// AwaitingConfirmation → Thinking: resolved (approved, declined, or
	// timed out) — the tool loop continues either way, because a decline is
	// reported to the model as a result, not an error.
	// AwaitingConfirmation → Listening: the user answers by voice; the
	// reply capture reuses the normal Listening → Transcribing path and
	// resolves from Transcribing → Thinking.
	StateAwaitingConfirmation: {StateThinking, StateListening, StateCancelling, StateError},
	StateSpeaking:             {StateIdle, StateCancelling, StateError},
	StateCancelling:           {StateIdle},
	StateError:                {StateIdle},
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
