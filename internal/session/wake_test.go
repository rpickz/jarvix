package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/tools"
)

// These tests cover the engine's half of hands-free activation with the
// existing fakes: what a wake word does to the state machine, what happens
// when something interrupts between the wake word and the end of the
// sentence, and what the transcript looks like by the time anything reads it.
// No microphone is involved — the audio arrives as a finished recording,
// which is exactly how the wake listener delivers it.

// wakeClip writes a WAV the fakes can carry around and returns it as a
// finished recording, standing in for the utterance the listener captured.
func wakeClip(t *testing.T) audio.Recording {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wake.wav")
	if err := os.WriteFile(path, make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	return &fakeClipRecording{clip: audio.Clip{WAVPath: path, SampleRate: 16000, Channels: 1}}
}

type fakeClipRecording struct {
	clip      audio.Clip
	cancelled bool
}

func (r *fakeClipRecording) Stop() (audio.Clip, error) { return r.clip, nil }
func (r *fakeClipRecording) Cancel()                   { r.cancelled = true; _ = os.Remove(r.clip.WAVPath) }

// The whole path: a wake word starts a session, the captured utterance
// becomes the transcript, and the answer is produced exactly as it would be
// for a held chord. "The session proceeds exactly like a PTT session" is the
// acceptance criterion, and this is what it means in the state machine.
func TestWakeSessionRunsLikeAPushToTalkSession(t *testing.T) {
	h := newHarness(t, Options{})
	h.stt.Text = "what's my disk usage"

	id, err := h.engine.StartWake()
	if err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "recording.started")
	if state, _ := h.engine.State(); state != StateListening {
		t.Fatalf("after the wake word the engine is %q, want listening", state)
	}

	discarded, err := h.engine.FinishWake(id, wakeClip(t), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if discarded {
		t.Fatal("a two-second request was discarded")
	}
	h.waitFor(t, "recording.stopped")
	if got := h.waitFor(t, "transcript.final").Data["text"]; got != "what's my disk usage" {
		t.Errorf("transcript is %q", got)
	}
	h.waitFor(t, "session.finished")
	h.waitIdle(t)
}

// The interruption contract. Saying the wake word while Jarvix is talking
// must stop it there and then — on the wake word, not after the sentence the
// user is still saying, which is the difference between an assistant that
// listens and one that talks over you.
func TestWakeWordInterruptsSpeech(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	h.provider.Response = "Here is a long answer."
	h.tts.Chunks = [][]byte{make([]byte, 1024)}
	// The synthesizer is parked, so speech cannot finish and the turn cannot
	// end until this test lets go of it (#215). Without it the whole turn is a
	// microsecond long — the fake provider streams with no delay and the fake
	// player does not sleep — and a runner that deschedules the test goroutine
	// for a millisecond after the wake below has nothing left to interrupt.
	hold := make(chan struct{})
	h.tts.SetHold(hold)
	defer close(hold)

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("tell me a story"); err != nil {
		t.Fatal(err)
	}
	// tts.started, not assistant.started: the latter is published before the
	// provider request is even opened and proves only that think() began, while
	// this one is published as the session enters Speaking — which is the claim
	// this test is about. Held speech makes it a barrier rather than a window.
	h.waitFor(t, "tts.started")

	if _, err := h.engine.StartWake(); err != nil {
		t.Fatal(err)
	}
	// The old session is cancelled by the new one — the same route a chord
	// press takes (startSessionLocked), so interruption has exactly one
	// implementation however it was triggered.
	ev := h.waitFor(t, "session.cancelled")
	if reason, _ := ev.Data["reason"].(string); reason == "" {
		t.Error("the interruption carried no reason")
	}
	if state, _ := h.engine.State(); state != StateListening {
		t.Errorf("after interrupting, the engine is %q, want listening", state)
	}
}

// A false activation that produced no speech must leave nothing behind: the
// session is cancelled, and the audio file is deleted rather than sent to
// whisper.
func TestAbortedWakeSessionCancelsAndKeepsNothing(t *testing.T) {
	h := newHarness(t, Options{})
	id, err := h.engine.StartWake()
	if err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "recording.started")

	h.engine.AbortWake(id, "no speech after the wake word")
	ev := h.waitFor(t, "session.cancelled")
	if ev.Data["reason"] != "no speech after the wake word" {
		t.Errorf("cancel reason is %v", ev.Data["reason"])
	}
	h.waitIdle(t)
}

// Between a wake word and the end of the sentence, seconds pass, and anything
// can happen in them. A capture that belongs to a session which has since
// been replaced must be thrown away — including the file, which was recorded
// unavoidably and is now unwanted.
func TestWakeCaptureForASupersededSessionIsDiscarded(t *testing.T) {
	h := newHarness(t, Options{})
	id, err := h.engine.StartWake()
	if err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "recording.started")

	// Someone reaches for the keyboard mid-sentence.
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	rec := wakeClip(t).(*fakeClipRecording)
	discarded, err := h.engine.FinishWake(id, rec, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !discarded {
		t.Error("a capture from a superseded session was submitted")
	}
	if !rec.cancelled {
		t.Error("the unwanted capture was not deleted")
	}
	if _, err := os.Stat(rec.clip.WAVPath); err == nil {
		t.Error("the unwanted capture is still on disk")
	}
}

// The accidental-activation guard applies to wake sessions too. A quarter of
// a second of audio is not a request, and transcribing it costs a provider
// call to be told nothing was said.
func TestShortWakeCaptureIsDiscarded(t *testing.T) {
	h := newHarness(t, Options{MinRecording: 500 * time.Millisecond})
	id, err := h.engine.StartWake()
	if err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "recording.started")

	discarded, err := h.engine.FinishWake(id, wakeClip(t), 250*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !discarded {
		t.Fatal("a 250ms capture was transcribed")
	}
	h.waitFor(t, "session.cancelled")
	h.waitIdle(t)
	if _, stopped, _ := h.recorder.Counts(); stopped != 0 {
		t.Error("a discarded wake capture reached the transcriber")
	}
}

// A wake word while a tool confirmation is pending answers it, exactly as
// holding the chord does. Starting a new session instead would abandon the
// question the user is in the middle of answering.
func TestWakeWordAnswersAPendingConfirmation(t *testing.T) {
	tool := &namedTool{name: "shell.run", result: "ok"}
	h := newGateHarness(t, Options{ConfirmTimeout: time.Minute}, tool,
		tools.PolicyConfig{Default: tools.PolicyAsk})
	h.stt.Text = "yes"
	scriptShellCall(h, "rm -rf /tmp/x", "done")

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("delete the thing"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "tool.confirmation_required")
	_, id := h.engine.State()

	wakeID, err := h.engine.StartWake()
	if err != nil {
		t.Fatal(err)
	}
	if wakeID != id {
		t.Fatalf("the wake word started session %q; the pending one is %q", wakeID, id)
	}
	if state, _ := h.engine.State(); state != StateListening {
		t.Fatalf("state is %q, want listening", state)
	}
	if _, err := h.engine.FinishWake(wakeID, wakeClip(t), time.Second); err != nil {
		t.Fatal(err)
	}
	ev := h.waitFor(t, "tool.confirmed")
	if ev.Data["source"] != "voice" {
		t.Errorf("the confirmation was attributed to %v, want voice", ev.Data["source"])
	}
}

// Two captures of one utterance is not something to arbitrate. Somebody
// holding the chord has made a deliberate gesture; a wake word heard at the
// same moment (their own voice, reaching both paths) must not cancel it.
func TestWakeWordIsIgnoredWhileAChordIsHeld(t *testing.T) {
	h := newHarness(t, Options{})
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.StartVoice(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.StartWake(); err == nil {
		t.Fatal("the wake word interrupted a held push-to-talk capture")
	}
	if state, _ := h.engine.State(); state != StateListening {
		t.Errorf("the push-to-talk capture ended up in state %q", state)
	}
}

// The wake word is inside the transcript, because the pre-roll deliberately
// contains it. Everything downstream wants it gone: the intent router matches
// whole utterances, so "Jarvix, stop" would otherwise reach the model instead
// of stopping anything.
func TestStripWakeWord(t *testing.T) {
	for _, c := range []struct{ name, in, word, want string }{
		{"leading with a comma", "Jarvix, what's my disk usage?", "jarvix", "what's my disk usage?"},
		{"no punctuation", "jarvix stop", "jarvix", "stop"},
		{"a filler in front", "Hey Jarvix, volume thirty", "jarvix", "volume thirty"},
		{"mid-sentence stays", "what did Jarvix say?", "jarvix", "what did Jarvix say?"},
		{"later in the sentence stays", "ask the Jarvix team", "jarvix", "ask the Jarvix team"},
		{"only the wake word is left alone", "Jarvix.", "jarvix", "Jarvix."},
		{"a substring is not the word", "Jarvixes are great", "jarvix", "Jarvixes are great"},
		{"no wake word configured", "Jarvix, hello", "", "Jarvix, hello"},
		{"empty transcript", "", "jarvix", ""},
	} {
		if got := stripWakeWord(c.in, c.word, nil); got != c.want {
			t.Errorf("%s: stripWakeWord(%q, %q, nil) = %q, want %q", c.name, c.in, c.word, got, c.want)
		}
	}
}

// The name is the user's to choose (issue #103): whatever [assistant] name
// and aliases hold flows through the strip under exactly the discipline the
// default name gets — leading whole words only, case and punctuation
// ignored, a following filler tolerated, and a name-only utterance left
// alone. Multi-word names ("Mister Smith") match as a word sequence, and so
// do multi-word aliases, because whisper mishears a two-word name two words
// at a time.
func TestStripWakeWordFollowsAConfiguredName(t *testing.T) {
	for _, c := range []struct {
		name    string
		in      string
		word    string
		aliases []string
		want    string
	}{
		{"custom name leading", "Hal, open the window", "Hal", nil, "open the window"},
		{"custom name case variant", "hal open the window", "HAL", nil, "open the window"},
		{"custom alias leading", "Howl, open the window", "Hal", []string{"hal", "howl"}, "open the window"},
		{"custom alias with a filler", "Hey howl, open the window", "Hal", []string{"howl"}, "open the window"},
		{"custom name mid-sentence stays", "please tell Hal I said hi", "Hal", []string{"howl"}, "please tell Hal I said hi"},
		{"only the custom name is left alone", "Hal.", "Hal", []string{"howl"}, "Hal."},
		{"multi-word name leading", "Mister Smith, what's the time?", "Mister Smith", nil, "what's the time?"},
		{"multi-word name case and punctuation", "mister smith. what's the time?", "Mister Smith", nil, "what's the time?"},
		{"multi-word name with a filler", "Hey Mister Smith, what's the time?", "Mister Smith", nil, "what's the time?"},
		{"multi-word alias", "Mr Smith, what's the time?", "Mister Smith", []string{"mr smith"}, "what's the time?"},
		{"half a multi-word name stays", "Mister, what's the time?", "Mister Smith", nil, "Mister, what's the time?"},
		{"multi-word name mid-sentence stays", "please could you call Mister Smith", "Mister Smith", nil, "please could you call Mister Smith"},
		{"only the multi-word name is left alone", "Mister Smith.", "Mister Smith", nil, "Mister Smith."},
		{"an alias that is a prefix strips the whole name", "Mister Smith, open it", "Mister Smith", []string{"mister"}, "open it"},
	} {
		if got := stripWakeWord(c.in, c.word, c.aliases); got != c.want {
			t.Errorf("%s: stripWakeWord(%q, %q, %v) = %q, want %q",
				c.name, c.in, c.word, c.aliases, got, c.want)
		}
	}
}

// shippedAliases mirrors config.Default()'s assistant aliases: the words
// whisper actually writes when it mishears "jarvix" (issue #83). Mirrored
// rather than imported so the session package keeps not depending on config;
// TestDefaultWakeAliasesAreTheKnownMishearings in internal/config pins the
// same list from the other side.
var shippedAliases = []string{"jarvis", "javax", "jarvic", "jarvicks", "jarvex"}

// A mishearing of the summons is still the summons. Every shipped alias is
// stripped under exactly the wake word's own discipline: leading whole word
// only, case and punctuation ignored — and a mid-sentence "Jarvis" (a real
// name that real sentences contain) is never touched.
func TestStripWakeWordAcceptsMishearingAliases(t *testing.T) {
	for _, alias := range shippedAliases {
		upper := strings.ToUpper(alias[:1]) + alias[1:]
		for _, c := range []struct{ name, in, want string }{
			{"leading with a comma", upper + ", volume thirty", "volume thirty"},
			{"lowercase no punctuation", alias + " stop", "stop"},
			{"a filler in front", "Hey " + upper + ", volume thirty", "volume thirty"},
			{"only the alias is left alone", upper + ".", upper + "."},
			{"a substring is not the alias", upper + "es are great", upper + "es are great"},
		} {
			if got := stripWakeWord(c.in, "jarvix", shippedAliases); got != c.want {
				t.Errorf("%s: stripWakeWord(%q, \"jarvix\", aliases) = %q, want %q",
					c.name, c.in, got, c.want)
			}
		}
	}
	// Beyond the two-word filler window an alias is a word like any other:
	// "Jarvis" is a name real sentences contain.
	for _, in := range []string{
		"tell me about Jarvis Cocker",
		"what did JavaX compile to?",
		"who is better, Jarvis or Jarvix?",
	} {
		if got := stripWakeWord(in, "jarvix", shippedAliases); got != in {
			t.Errorf("non-leading occurrence was touched: stripWakeWord(%q) = %q", in, got)
		}
	}
}

// End to end: what the model is asked is the request, not the summons. This
// is the assertion that would catch the stripping being wired to the wrong
// sessions or skipped entirely.
func TestWakeTranscriptReachesTheModelWithoutTheWakeWord(t *testing.T) {
	h := newHarness(t, Options{WakeWord: "jarvix"})
	h.stt.Text = "Jarvix, what's my disk usage?"

	id, err := h.engine.StartWake()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.FinishWake(id, wakeClip(t), 2*time.Second); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "session.finished")

	asked := h.provider.LastRequest
	last := asked.Messages[len(asked.Messages)-1]
	if last.Content != "what's my disk usage?" {
		t.Errorf("the model was asked %q; the wake word should have been stripped", last.Content)
	}
}

// End to end for the mishearing path (issue #83): whisper wrote "Jarvis", the
// user said "Jarvix", and what the model is asked is still just the request.
func TestWakeTranscriptStripsAMisheardName(t *testing.T) {
	h := newHarness(t, Options{WakeWord: "jarvix", WakeAliases: shippedAliases})
	h.stt.Text = "Jarvis, what's my disk usage?"

	id, err := h.engine.StartWake()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.FinishWake(id, wakeClip(t), 2*time.Second); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "session.finished")

	asked := h.provider.LastRequest
	last := asked.Messages[len(asked.Messages)-1]
	if last.Content != "what's my disk usage?" {
		t.Errorf("the model was asked %q; the misheard wake word should have been stripped", last.Content)
	}
}

// Push-to-talk transcripts are untouched. Someone who says "Jarvix" into a
// held chord meant to say it, and a feature that quietly edited every
// transcript would be a surprise nobody asked for.
func TestPushToTalkTranscriptsKeepEveryWord(t *testing.T) {
	h := newHarness(t, Options{WakeWord: "jarvix"})
	h.stt.Text = "Jarvix, what's my disk usage?"
	h.ask(t, "Jarvix, what's my disk usage?")

	asked := h.provider.LastRequest
	last := asked.Messages[len(asked.Messages)-1]
	if last.Content != "Jarvix, what's my disk usage?" {
		t.Errorf("a typed question was edited: %q", last.Content)
	}
}
