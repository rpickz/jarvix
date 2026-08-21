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
		// …including when the model streamed text before calling the tool…
		{StateResponding, StateAwaitingConfirmation},
		// …and when that text has already been spoken. Streaming speech begins
		// on the first complete sentence, so this is not an edge case: it is
		// what happens whenever the model narrates before it acts (issue #52).
		{StateSpeaking, StateAwaitingConfirmation},
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
	// Each illegal row records *why* it is illegal, and the reason is the
	// load-bearing part: this very test once pinned the #52 bug as correct
	// behaviour, because a row's assumption ("Speaking means the answer is
	// finished") had been invalidated by a later feature and nothing recorded
	// what the row was assuming. A row whose reason no longer holds is a row
	// to delete, not to satisfy. Audited against the streaming-speech asides
	// (#53) and speech cancel from any state (#54) — the reasons below still
	// hold.
	illegal := [][2]State{
		{StateIdle, StateAwaitingConfirmation},      // only a tool round can ask
		{StateListening, StateAwaitingConfirmation}, // capture resolves via Transcribing
		// The question travels the answer's own playback queue as an aside
		// (#52/#53) precisely so it needs no state of its own.
		{StateAwaitingConfirmation, StateSpeaking},
		{StateAwaitingConfirmation, StateResponding}, // resolution passes through Thinking
		// Teardown goes via Cancelling/Error — including a speech cancel that
		// abandons the pending question (#54), which unwinds through
		// Cancelling like an interruption.
		{StateAwaitingConfirmation, StateIdle},
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

// TestEveryToolRequestStateReachesTheGateAndComesBack is the enumeration issue
// #52 asked for by name, and the reason it is a table rather than a case: the
// bug was not one missing entry, it was that no one had ever listed the states
// a tool call can arrive in. Speaking became such a state when streaming speech
// landed, and nothing anywhere said so — the gap was found in production.
//
// Both directions matter. A state that cannot reach AwaitingConfirmation kills
// the turn at the question; a state whose resume is not reachable kills it at
// the answer, which is the harder half to notice because the user has already
// been asked and has already said yes.
func TestEveryToolRequestStateReachesTheGateAndComesBack(t *testing.T) {
	// Where a confirmation raised from each state puts the session once it is
	// answered. A model tool round always resumes at Thinking — the tool loop
	// continues from there whatever the answer was, and going back to
	// Responding instead would need a self-transition the table refuses. A
	// user-defined intent resumes at Acting, which never reaches the model.
	resume := map[State]State{
		StateThinking:   StateThinking,
		StateResponding: StateThinking,
		StateSpeaking:   StateThinking,
		StateActing:     StateActing,
	}
	for _, from := range toolRequestStates {
		if !CanTransition(from, StateAwaitingConfirmation) {
			t.Errorf("a tool call can be requested from %s, so %s → awaiting_confirmation must be legal", from, from)
		}
		to, ok := resume[from]
		if !ok {
			t.Errorf("no resume state declared for %s; decide what answering a question from there means", from)
			continue
		}
		if !CanTransition(StateAwaitingConfirmation, to) {
			t.Errorf("a confirmation raised from %s resumes at %s, so awaiting_confirmation → %s must be legal", from, to, to)
		}
	}
}

// TestToolRequestStatesCoverEveryState is the guard that stops this class of
// defect coming back. Every state in the table has to be classified as one a
// tool call can arrive in or one it cannot, so adding a state to the machine
// forces the same question that was never asked about Speaking. A new state
// left out of both lists fails here rather than in someone's living room.
func TestToolRequestStatesCoverEveryState(t *testing.T) {
	// The states no tool call can arrive in, each with the reason it cannot.
	// The reason is the point: "it has never happened" is not one of them.
	noToolCalls := map[State]string{
		StateIdle:                 "no turn is in flight",
		StateListening:            "the microphone is open; nothing has been said yet",
		StateTranscribing:         "the words are still being recognised; neither the model nor the router has seen them",
		StateAwaitingConfirmation: "already at the gate — tool calls are gated one at a time",
		StateCancelling:           "the session is being torn down",
		StateError:                "the session has already failed",
	}
	inToolStates := map[State]bool{}
	for _, s := range toolRequestStates {
		if inToolStates[s] {
			t.Errorf("%s is listed twice in toolRequestStates", s)
		}
		inToolStates[s] = true
		if !s.Valid() {
			t.Errorf("toolRequestStates names %s, which is not a state", s)
		}
		if why, both := noToolCalls[s]; both {
			t.Errorf("%s is classified both ways (%q); it can only be one", s, why)
		}
	}
	for s := range transitions {
		if !inToolStates[s] && noToolCalls[s] == "" {
			t.Errorf("%s is classified neither way: decide whether a tool call can be "+
				"requested from it, add it to toolRequestStates or to this test's list, "+
				"and give it a legal path to awaiting_confirmation if it needs one", s)
		}
	}
	for s := range noToolCalls {
		if !s.Valid() {
			t.Errorf("this test names %s, which is not a state", s)
		}
	}
}

// TestSpeakingIsNotTheEndOfATurn pins the other half of #52: speech starts
// before generation finishes, so the tool loop has to be able to carry on
// underneath it. Without Speaking → Responding a tool call that needed no
// confirmation at all left the session wedged in Speaking forever — no error,
// no session.finished, nothing for the user to see except an assistant that
// stopped mid-answer.
func TestSpeakingIsNotTheEndOfATurn(t *testing.T) {
	legal := [][2]State{
		// The next round's first token, after a tool ran under the speech.
		{StateSpeaking, StateResponding},
		// The answer really is over.
		{StateSpeaking, StateIdle},
	}
	for _, pair := range legal {
		if !CanTransition(pair[0], pair[1]) {
			t.Errorf("expected %s → %s to be legal", pair[0], pair[1])
		}
	}
	// Audited against background wake listening (ADR 0024): a wake word — or
	// a spoken "stop" — heard while Speaking starts a *new* session, which
	// interrupts this one; no same-session path from Speaking to Listening or
	// Acting exists, so these reasons still hold.
	illegal := [][2]State{
		{StateSpeaking, StateListening},    // speech never opens the microphone
		{StateSpeaking, StateTranscribing}, //
		{StateSpeaking, StateActing},       // a matched intent never follows a model answer
		{StateSpeaking, StateSpeaking},     // one continuous stream per turn
	}
	for _, pair := range illegal {
		if CanTransition(pair[0], pair[1]) {
			t.Errorf("expected %s → %s to be illegal", pair[0], pair[1])
		}
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
