package session

import (
	"strings"
	"testing"
	"time"
)

// Issue #191. Whisper never declines to answer: handed a capture with nothing
// in it, it returns its most likely continuation of the bias prompt, which is
// the bias prompt. internal/stt discards that; this file is about what the
// session then does with a capture that produced no words — which must be an
// honest nothing, not an incident.

// listen drives one voice capture to the end of the session and returns every
// event it produced, in order. Order matters here in a way it does not for the
// ordinary turn: the claim is partly about what is *absent*, and a map keyed
// by type would hide a second event of the same kind.
func (h *harness) listen(t *testing.T) []Event {
	t.Helper()
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.StartVoice(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.StopVoice(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit(""); err != nil {
		t.Fatal(err)
	}
	var seen []Event
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-h.events:
			seen = append(seen, ev)
			if ev.Type == "session.finished" {
				h.waitIdle(t)
				return seen
			}
		case <-deadline:
			t.Fatalf("session never finished; saw %v", typesOf(seen))
		}
	}
}

func typesOf(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Type)
	}
	return out
}

func firstOf(events []Event, eventType string) (Event, bool) {
	for _, ev := range events {
		if ev.Type == eventType {
			return ev, true
		}
	}
	return Event{}, false
}

// The headline: a capture that produced nothing ends the turn quietly. Before
// this, an empty transcript was an `error` event, which lights the urgent chip
// on the bar, the red banner in the window and a "Jarvix hit a problem"
// notification — and holds them until the next session. A user whose
// microphone is unplugged would get one fault report per press.
func TestACaptureThatProducedNothingIsNotAnError(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	h.stt.Text = "  "

	seen := h.listen(t)

	if _, ok := firstOf(seen, "error"); ok {
		t.Errorf("an empty capture published an error event; saw %v", typesOf(seen))
	}
	ev, ok := firstOf(seen, "session.nothing_heard")
	if !ok {
		t.Fatalf("no session.nothing_heard event; saw %v", typesOf(seen))
	}
	if ev.Data["reason"] != NothingHeardDefaultReason {
		t.Errorf("reason = %v, want the default when the transcriber gave none", ev.Data["reason"])
	}
	if s, _ := h.engine.State(); s != StateIdle {
		t.Errorf("state = %s, want idle", s)
	}
}

// The reason the STT adapter attached travels all the way to the record. This
// is the sentence a user debugging a microphone reads, and it is the whole
// reason the discard is allowed to be quiet: it is visible instead.
func TestTheTranscribersReasonReachesTheRecord(t *testing.T) {
	h := newHarness(t, Options{})
	h.stt.Text = ""
	h.stt.Reason = "the capture had no voiced audio (peak -inf dBFS, floor -72 dBFS)"

	seen := h.listen(t)

	ev, ok := firstOf(seen, "session.nothing_heard")
	if !ok {
		t.Fatalf("no session.nothing_heard event; saw %v", typesOf(seen))
	}
	if ev.Data["reason"] != h.stt.Reason {
		t.Errorf("reason = %v, want %q", ev.Data["reason"], h.stt.Reason)
	}
	// And the timings record carries it too, so `jarvix status --last` — where
	// someone looks after pressing the key and getting nothing — says why
	// rather than showing a turn with no stages.
	timings, ok := firstOf(seen, "session.timings")
	if !ok {
		t.Fatalf("no session.timings event; saw %v", typesOf(seen))
	}
	if timings.Data[StageNothingHeard] != h.stt.Reason {
		t.Errorf("timings[%s] = %v, want %q", StageNothingHeard,
			timings.Data[StageNothingHeard], h.stt.Reason)
	}
}

// No transcript event at all. Publishing an empty one made every surface
// render the user having said nothing out loud — a blank speech bubble is
// still a claim about what happened, and this issue is about not making
// claims about captures that carried no speech.
func TestNothingHeardInventsNoTranscript(t *testing.T) {
	h := newHarness(t, Options{})
	h.stt.Text = "   "

	for _, ev := range h.listen(t) {
		if ev.Type == "transcript.final" || ev.Type == "transcript.partial" {
			t.Errorf("published %s with %v; a capture that produced nothing has no transcript",
				ev.Type, ev.Data)
		}
	}
}

// The point of the whole change: a phantom utterance is a phantom
// instruction. Nothing must reach the model.
func TestNothingHeardOpensNoProviderRequest(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	h.stt.Text = ""

	seen := h.listen(t)

	if n := len(h.provider.Requests); n != 0 {
		t.Errorf("provider requests = %d, want 0: a capture that produced nothing is not a question", n)
	}
	for _, ev := range seen {
		if strings.HasPrefix(ev.Type, "assistant.") {
			t.Errorf("published %s; there was no exchange to have", ev.Type)
		}
	}
	if h.tts.Last().Text != "" {
		t.Errorf("spoke %q; nothing was said to answer", h.tts.Last().Text)
	}
}

// Ordering. session.finished is a session's last event and session.timings
// immediately precedes it, on this path as on every other — a client
// attributing the numbers must not race the end of the session.
//
// And session.nothing_heard comes out *before* the return to idle, which is
// load-bearing rather than incidental: the conversation window closes its
// pending row on the idle transition, so an event published after it would
// resolve a row that no longer exists and the turn would disappear in silence.
func TestNothingHeardKeepsTheEndOfSessionOrdering(t *testing.T) {
	h := newHarness(t, Options{})
	h.stt.Text = ""

	order := typesOf(h.listen(t))
	if n := len(order); n < 2 || order[n-2] != "session.timings" || order[n-1] != "session.finished" {
		t.Fatalf("session ended with %v, want …session.timings, session.finished", order)
	}
	notice, idle := -1, -1
	for i, ev := range order {
		if ev == "session.nothing_heard" && notice < 0 {
			notice = i
		}
		if ev == "state.changed" {
			idle = i
		}
	}
	if notice < 0 {
		t.Fatalf("no session.nothing_heard event; saw %v", order)
	}
	if idle >= 0 && notice > idle {
		t.Errorf("session.nothing_heard at %d came after the return to idle at %d in %v",
			notice, idle, order)
	}
}
