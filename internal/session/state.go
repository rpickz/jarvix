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
	StateSpeaking     State = "speaking"
	StateCancelling   State = "cancelling"
	StateError        State = "error"
)

// transitions is the complete set of legal state changes. Anything not listed
// is a programming error, caught by tests and rejected at runtime.
var transitions = map[State][]State{
	StateIdle:      {StateListening, StateThinking},
	StateListening: {StateTranscribing, StateCancelling, StateError},
	// Transcribing → Speaking is not legal: a transcript must pass through
	// Thinking. Cancellation is legal from every active state.
	StateTranscribing: {StateThinking, StateCancelling, StateError},
	StateThinking:     {StateResponding, StateCancelling, StateError},
	// Responding → Thinking happens when the model streamed some text and
	// then asked to call a tool: it goes back to working before answering.
	StateResponding: {StateSpeaking, StateThinking, StateIdle, StateCancelling, StateError},
	StateSpeaking:     {StateIdle, StateCancelling, StateError},
	StateCancelling:   {StateIdle},
	StateError:        {StateIdle},
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
