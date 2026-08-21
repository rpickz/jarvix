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

func TestCancellationFromEveryActiveState(t *testing.T) {
	active := []State{StateListening, StateTranscribing, StateThinking, StateResponding, StateSpeaking}
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
	for _, s := range []State{StateListening, StateSpeaking, StateError, StateCancelling} {
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
