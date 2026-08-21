package session

import "testing"

func TestHappyPathTransitions(t *testing.T) {
	path := []State{
		StateIdle, StateListening, StateTranscribing,
		StateThinking, StateResponding, StateSpeaking, StateIdle,
	}
	for i := 0; i < len(path)-1; i++ {
		if !CanTransition(path[i], path[i+1]) {
			t.Errorf("expected %s → %s to be legal", path[i], path[i+1])
		}
	}
}

func TestConfirmationTransitions(t *testing.T) {
	legal := [][2]State{
		// A tool call that needs confirmation pauses the model's turn…
		{StateThinking, StateAwaitingConfirmation},
		// …including when the model streamed text before calling the tool.
		{StateResponding, StateAwaitingConfirmation},
		// Approved, declined, and timed out all resume the tool loop: a
		// decline is a result for the model, not an error.
		{StateAwaitingConfirmation, StateThinking},
		// A spoken reply reuses the normal capture path…
		{StateAwaitingConfirmation, StateListening},
		{StateListening, StateTranscribing},
		// …and resolves from Transcribing back into the tool loop.
		{StateTranscribing, StateThinking},
		// Interruption and failure work like every other active state.
		{StateAwaitingConfirmation, StateCancelling},
		{StateAwaitingConfirmation, StateError},
	}
	for _, pair := range legal {
		if !CanTransition(pair[0], pair[1]) {
			t.Errorf("expected %s → %s to be legal", pair[0], pair[1])
		}
	}
	illegal := [][2]State{
		{StateIdle, StateAwaitingConfirmation},      // only a tool round can ask
		{StateListening, StateAwaitingConfirmation}, // capture resolves via Transcribing
		{StateSpeaking, StateAwaitingConfirmation},
		{StateAwaitingConfirmation, StateSpeaking},   // the prompt is spoken without a state change
		{StateAwaitingConfirmation, StateResponding}, // resolution passes through Thinking
		{StateAwaitingConfirmation, StateIdle},       // teardown goes via Cancelling/Error
		{StateError, StateAwaitingConfirmation},
	}
	for _, pair := range illegal {
		if CanTransition(pair[0], pair[1]) {
			t.Errorf("expected %s → %s to be illegal", pair[0], pair[1])
		}
	}
	if !StateAwaitingConfirmation.Active() {
		t.Error("awaiting_confirmation is an active state")
	}
	if !StateAwaitingConfirmation.Valid() {
		t.Error("awaiting_confirmation must validate")
	}
}

// TestActingTransitions pins the deterministic intent router's state (ADR
// 0015). The illegal half is the point: Acting exists so a matched intent can
// never be mistaken for — or drift into — a model turn.
func TestActingTransitions(t *testing.T) {
	legal := [][2]State{
		// Submitted text the router claims (`jarvix ask "volume thirty"`)…
		{StateIdle, StateActing},
		// …and the same from a voice transcript.
		{StateTranscribing, StateActing},
		// The terse acknowledgement is spoken like any other answer…
		{StateActing, StateSpeaking},
		// …or the intent completes in silence ("stop", speak_responses off).
		{StateActing, StateIdle},
		// A user-defined intent's command can need the permission gate, and
		// resolution returns to Acting rather than to a tool loop.
		{StateActing, StateAwaitingConfirmation},
		{StateAwaitingConfirmation, StateActing},
		// Interruption and failure work like every other active state.
		{StateActing, StateCancelling},
		{StateActing, StateError},
	}
	for _, pair := range legal {
		if !CanTransition(pair[0], pair[1]) {
			t.Errorf("expected %s → %s to be legal", pair[0], pair[1])
		}
	}
	illegal := [][2]State{
		// The whole point: a matched intent never reaches the model.
		{StateActing, StateThinking},
		{StateActing, StateResponding},
		{StateThinking, StateActing},   // once thinking, the model owns the turn
		{StateResponding, StateActing}, //
		{StateListening, StateActing},  // a capture must be transcribed first
		{StateSpeaking, StateActing},
		{StateError, StateActing},
		{StateCancelling, StateActing},
		{StateActing, StateListening},    // the router never starts a capture
		{StateActing, StateTranscribing}, //
		{StateActing, StateActing},       // one action per session
	}
	for _, pair := range illegal {
		if CanTransition(pair[0], pair[1]) {
			t.Errorf("expected %s → %s to be illegal", pair[0], pair[1])
		}
	}
	if !StateActing.Active() {
		t.Error("acting is an active state")
	}
	if !StateActing.Valid() {
		t.Error("acting must validate")
	}
}

func TestCancellationFromEveryActiveState(t *testing.T) {
	active := []State{StateListening, StateTranscribing, StateThinking,
		StateResponding, StateAwaitingConfirmation, StateActing, StateSpeaking}
	for _, s := range active {
		if !CanTransition(s, StateCancelling) {
			t.Errorf("%s must allow cancellation", s)
		}
	}
	if !CanTransition(StateCancelling, StateIdle) {
		t.Error("cancelling must return to idle")
	}
}

func TestIllegalTransitions(t *testing.T) {
	illegal := [][2]State{
		{StateIdle, StateSpeaking},
		{StateIdle, StateResponding},
		{StateListening, StateThinking},    // must transcribe first
		{StateTranscribing, StateSpeaking}, // must think first
		{StateSpeaking, StateListening},
		{StateIdle, StateCancelling}, // nothing to cancel
		{StateError, StateThinking},
	}
	for _, pair := range illegal {
		if CanTransition(pair[0], pair[1]) {
			t.Errorf("expected %s → %s to be illegal", pair[0], pair[1])
		}
	}
}

func TestActiveAndValid(t *testing.T) {
	if StateIdle.Active() {
		t.Error("idle is not active")
	}
	for _, s := range []State{StateListening, StateSpeaking, StateActing, StateError, StateCancelling} {
		if !s.Active() {
			t.Errorf("%s should be active", s)
		}
	}
	if State("bogus").Valid() {
		t.Error("bogus state must not validate")
	}
	if !StateThinking.Valid() {
		t.Error("thinking must validate")
	}
}
