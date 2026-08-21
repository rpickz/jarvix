package wake

import (
	"testing"
	"time"
)

// newEndpointer builds one with the shipped defaults, so the tests below
// exercise the configuration a user actually gets.
func newEndpointer() *Endpointer {
	return &Endpointer{Silence: DefaultSilence, Lead: DefaultLead, Max: DefaultMaxUtterance}
}

// push feeds a fixture frame by frame and reports the first non-continue
// verdict and how many frames it took.
func push(e *Endpointer, pcm []int16) (Endpoint, int) {
	for i, frame := range chunk(pcm) {
		if v := e.Push(frame); v != EndpointContinue {
			return v, i + 1
		}
	}
	return EndpointContinue, len(chunk(pcm))
}

// The headline behaviour: someone speaks, stops, and the request submits
// itself after the configured silence. There is no key to release, so this is
// the only "I have finished" signal the feature has.
func TestEndpointSubmitsAfterTheConfiguredSilence(t *testing.T) {
	e := newEndpointer()
	e.Prime(roomTone(4, 1))

	spoken := framesIn(1200 * time.Millisecond)
	if v, _ := push(e, utterance(spoken, 2)); v != EndpointContinue {
		t.Fatalf("the endpointer stopped during speech: %v", v)
	}
	v, frames := push(e, roomTone(framesIn(2*time.Second), 3))
	if v != EndpointComplete {
		t.Fatalf("after speech and silence, got %v, want EndpointComplete", v)
	}
	// It must submit at the threshold, not "eventually": the wait after
	// speaking is the whole perceived cost of hands-free activation.
	want := framesIn(DefaultSilence)
	if frames < want || frames > want+2 {
		t.Errorf("submitted after %d frames of silence, want about %d (%v)", frames, want, DefaultSilence)
	}
}

// A pause between clauses is not the end of a sentence. The endpointer must
// survive one and keep capturing, or every request longer than a breath gets
// cut in half.
func TestEndpointSurvivesAPauseShorterThanTheThreshold(t *testing.T) {
	e := newEndpointer()
	e.Prime(roomTone(4, 4))
	if v, _ := push(e, utterance(framesIn(600*time.Millisecond), 5)); v != EndpointContinue {
		t.Fatal("stopped during the first clause")
	}
	pause := framesIn(DefaultSilence - 200*time.Millisecond)
	if v, _ := push(e, roomTone(pause, 6)); v != EndpointContinue {
		t.Fatalf("a %v pause ended the utterance; the threshold is %v",
			time.Duration(pause)*FrameDuration, DefaultSilence)
	}
	if v, _ := push(e, utterance(framesIn(600*time.Millisecond), 7)); v != EndpointContinue {
		t.Fatal("stopped during the second clause")
	}
	if v, _ := push(e, roomTone(framesIn(2*time.Second), 8)); v != EndpointComplete {
		t.Fatalf("got %v after the real endpoint, want EndpointComplete", v)
	}
}

// A false activation followed by nobody saying anything must not open a
// session, transcribe silence, or reach the model. Reporting it as its own
// outcome is what lets the daemon cancel quietly instead of asking whisper
// what two seconds of room tone said.
func TestEndpointReportsSilenceAfterAFalseActivation(t *testing.T) {
	e := newEndpointer()
	e.Prime(roomTone(4, 9))
	v, frames := push(e, roomTone(framesIn(5*time.Second), 10))
	if v != EndpointNoSpeech {
		t.Fatalf("got %v, want EndpointNoSpeech", v)
	}
	if want := framesIn(DefaultLead); frames < want || frames > want+2 {
		t.Errorf("gave up after %d frames, want about %d (%v)", frames, want, DefaultLead)
	}
	if e.Speech() {
		t.Error("the endpointer thinks room tone was speech")
	}
}

// Digital silence — every sample zero — is the case a naive noise floor gets
// wrong: the floor collapses to zero, every ratio test passes, and the
// endpointer decides the room is talking. The floor has a minimum for exactly
// this reason.
func TestEndpointTreatsDigitalSilenceAsSilence(t *testing.T) {
	e := newEndpointer()
	if v, _ := push(e, silence(framesIn(5*time.Second))); v != EndpointNoSpeech {
		t.Fatalf("got %v over pure silence, want EndpointNoSpeech", v)
	}
}

// A noisy room must not deafen the endpointer. Priming from the pre-roll —
// audio recorded before anyone addressed Jarvix, and therefore by definition
// the room — is what makes the ratio meaningful in a workshop as well as a
// study.
func TestEndpointAdaptsToALouderRoom(t *testing.T) {
	loud := func(frames int, seed int64) []int16 { return noise(frames, seed, 900) }
	e := newEndpointer()
	e.Prime(loud(6, 11))
	if v, _ := push(e, loud(framesIn(4*time.Second), 12)); v != EndpointNoSpeech {
		t.Fatalf("a loud room read as speech: %v", v)
	}

	e2 := newEndpointer()
	e2.Prime(loud(6, 13))
	if v, _ := push(e2, utterance(framesIn(1*time.Second), 14)); v != EndpointContinue {
		t.Fatalf("real speech in a loud room ended early: %v", v)
	}
	if !e2.Speech() {
		t.Error("speech in a loud room was not recognised as speech")
	}
}

// Priming takes the quietest frame of the pre-roll rather than its average,
// because the pre-roll ends with the wake word itself. Averaging that in
// would set the floor at speech level and the endpointer would hear nothing
// afterwards.
func TestEndpointPrimingIgnoresTheWakeWordInThePreRoll(t *testing.T) {
	preRoll := append(roomTone(8, 15), utterance(4, 16)...)
	e := newEndpointer()
	e.Prime(preRoll)
	if v, _ := push(e, utterance(framesIn(1*time.Second), 17)); v != EndpointContinue {
		t.Fatalf("speech after a pre-roll containing the wake word was not heard: %v", v)
	}
	if !e.Speech() {
		t.Fatal("the noise floor was primed from the wake word, so nothing sounds like speech any more")
	}
}

// Somebody talking without pause forever is bounded. Submitting what there is
// beats losing the request, and it is what caps how much audio can exist at
// one time.
func TestEndpointStopsAtTheCeiling(t *testing.T) {
	e := &Endpointer{Silence: DefaultSilence, Lead: DefaultLead, Max: 2 * time.Second}
	e.Prime(roomTone(4, 18))
	v, frames := push(e, utterance(framesIn(10*time.Second), 19))
	if v != EndpointMax {
		t.Fatalf("got %v, want EndpointMax", v)
	}
	if want := framesIn(2 * time.Second); frames < want || frames > want+2 {
		t.Errorf("stopped after %d frames, want about %d", frames, want)
	}
}

// Reset starts a new utterance but keeps the learned floor: the room has not
// changed between one request and the next, and re-learning it would spend
// the first half-second of every capture deaf.
func TestEndpointResetKeepsTheLearnedNoiseFloor(t *testing.T) {
	e := newEndpointer()
	e.Prime(noise(6, 20, 900))
	before := e.noise
	e.Reset()
	if e.noise != before {
		t.Errorf("Reset moved the noise floor from %.1f to %.1f", before, e.noise)
	}
	if e.Speech() {
		t.Error("Reset left the previous utterance's speech flag set")
	}
}

// The level function everything above rests on. Checked directly because a
// silent regression here (an integer overflow, a wrong divisor) would show up
// only as an endpointer that behaves oddly in quiet rooms.
func TestRMSLevels(t *testing.T) {
	approx(t, "silence", rms(make([]int16, FrameSamples)), 0, 0.001)
	full := make([]int16, FrameSamples)
	for i := range full {
		full[i] = 32767
	}
	approx(t, "full scale", rms(full), 32767, 1)
	if rms(nil) != 0 {
		t.Error("an empty frame must have zero level, not a division by zero")
	}
}
