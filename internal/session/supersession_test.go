package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
)

// This file covers issue #120: speech keeps up with the conversation's
// leading edge. Sentences queue on the speaker faster than a voice can say
// them, and before this feature every one of them played — in a fast
// tool-round exchange the user sat through stale narration for rounds the
// transcript had already moved past. Now each provider round is a speech
// turn, and the first sentence a newer turn commits condemns the older
// turns' unplayed queue: dropped at dequeue, audio only, with the count on
// the record (tts.superseded, and superseded_sentences in the timings).
//
// The boundaries these tests pin, because they are the contract:
//
//   - strictly cross-turn: nothing within a turn is ever skipped, however
//     far the audio lags the text;
//   - decided at commit: a round that streams no speech supersedes nothing;
//   - the sentence in flight finishes — supersession pays at the queue,
//     never at the device (see streamingSpeaker.superseded);
//   - a confirmation question always plays (it gates progress), and the
//     keep flag it rides is the same exemption a spoken receipt of an
//     executed action would use — while a stale progress reassurance drops
//     (see utterance.keep for the full aside policy);
//   - tts.started/finished stay one pair per answer, so the bar and
//     overlay state machines never see a superseded turn as a second (or a
//     missing) answer.
//
// Every ordering is gated (tts.Fake.SetHold, the speakerQueued seam, and for
// the engine-driven test a tool round held open until the voice has provably
// started), never slept for.

// recordingSynth wraps the fake synthesizer to keep every text it was asked
// to speak, in order — tts.Fake only retains the last. What reached Speak is
// exactly what supersession let through: a dropped utterance is discarded at
// dequeue, before synthesis.
type recordingSynth struct {
	*tts.Fake
	mu     sync.Mutex
	spoken []string
	// started is closed the first time any sentence reaches Speak, giving a
	// test a real happens-before edge for "the voice has begun" rather than a
	// count to poll. tts.Fake.Speaks() counts calls but names none of them,
	// and that anonymity is what let issue #154's ordering go unestablished:
	// "at least one synthesis has happened" is satisfied just as well by the
	// answer that was supposed to arrive *second*. Closed under startOnce so
	// the edge exists exactly once, however many sentences follow.
	startOnce sync.Once
	started   chan struct{}
}

func (r *recordingSynth) Speak(ctx context.Context, req tts.Request) (tts.Format, <-chan tts.Chunk, error) {
	r.mu.Lock()
	r.spoken = append(r.spoken, req.Text)
	r.mu.Unlock()
	r.startOnce.Do(func() { close(r.started) })
	return r.Fake.Speak(ctx, req)
}

// firstSpeak returns a channel closed once a sentence has actually reached the
// synthesizer — the gate anything that must happen *after* the voice starts
// waits on.
func (r *recordingSynth) firstSpeak() <-chan struct{} { return r.started }

func (r *recordingSynth) texts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.spoken...)
}

// newRecordedHarness rebuilds the harness engine around a recordingSynth (and
// an optional tool registry), the same rebuild-and-drain shape as the #111
// wedge test: the extra Shutdown cleanup runs before any gate-release
// cleanups registered after it (LIFO), so a drain never waits on a goroutine
// a still-closed gate is parking.
func newRecordedHarness(t *testing.T, reg *tools.Registry) (*harness, *recordingSynth) {
	t.Helper()
	h := newHarness(t, Options{SpeakResponses: true})
	h.tools = reg
	synth := &recordingSynth{Fake: h.tts, started: make(chan struct{})}
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	t.Cleanup(h.cancel)
	h.engine = NewEngine(h.provider, h.stt, synth, h.recorder, h.player, h.tools, nil, bus, nil,
		Options{Model: "m", SpeakResponses: true})
	t.Cleanup(func() {
		if err := h.engine.Shutdown(context.Background()); err != nil {
			t.Errorf("engine had not quiesced by the end of the test: %v", err)
		}
	})
	return h, synth
}

// holdFirstSentence gates the synthesizer so the first sentence to reach it
// parks mid-synthesis, returning the release function (idempotent, and
// registered as a cleanup so a failing test can still drain).
func holdFirstSentence(t *testing.T, h *harness) (release func()) {
	t.Helper()
	hold := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(hold) }) }
	t.Cleanup(release)
	h.tts.SetHold(hold)
	return release
}

// collectEvents drains the harness bus until terminal, returning every event
// seen in order and failing the test on any error event.
func collectEvents(t *testing.T, h *harness, terminal string) []Event {
	t.Helper()
	var out []Event
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-h.events:
			if ev.Type == "error" {
				t.Fatalf("waiting for %q, got error event: %v", terminal, ev.Data)
			}
			out = append(out, ev)
			if ev.Type == terminal {
				return out
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event %q", terminal)
		}
	}
}

func eventCount(events []Event, eventType string) int {
	n := 0
	for _, ev := range events {
		if ev.Type == eventType {
			n++
		}
	}
	return n
}

func findEvent(events []Event, eventType string) (Event, bool) {
	for _, ev := range events {
		if ev.Type == eventType {
			return ev, true
		}
	}
	return Event{}, false
}

// speakerUnderTest starts a session, parks it in Responding by the legal
// path, and hands back a speaker on it — the setup for driving supersession
// interleavings the public API cannot reach, because the goroutines it
// serialises (interject blocks its caller; awaitConfirmation parks the tool
// loop) are exactly what makes those interleavings impossible from outside.
// The guarantees still have to hold by construction, not by the current
// shape of the callers, so they are pinned here at the speaker's own seam.
func speakerUnderTest(t *testing.T, h *harness) (*streamingSpeaker, *sess) {
	t.Helper()
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	s := h.engine.current
	h.engine.forceStateLocked(StateThinking)
	h.engine.forceStateLocked(StateResponding)
	h.engine.mu.Unlock()
	return newStreamingSpeaker(h.engine, s), s
}

// TestStaleSpeechFromAnEarlierTurnIsSuperseded is the headline behaviour,
// end to end through the engine: a tool round narrates three sentences, the
// voice is still on the first when the final answer commits, and the user
// hears the first sentence finish (never cut mid-word) and then the answer —
// the two stale sentences are dropped, counted on the bus and in the
// timings, and the tts.started/finished pair stays exactly one.
func TestStaleSpeechFromAnEarlierTurnIsSuperseded(t *testing.T) {
	reg := tools.NewRegistry(nil)
	h, synth := newRecordedHarness(t, reg)
	release := holdFirstSentence(t, h)

	// The tool round is held open until the first sentence has provably
	// reached the synthesizer, and that gate is the whole point of this
	// arrangement.
	//
	// The behaviour under test is "the voice is still on turn one's sentence
	// when turn two commits". Turn two only exists once the tool returns, so
	// parking the tool on the synthesizer's own start signal makes that
	// premise a fact: while the tool is blocked, no round two exists, nothing
	// can raise the supersession floor, and the speaker has the exchange to
	// itself to dequeue sentence one and park on the hold gate inside Speak.
	//
	// It used to be asserted instead of established (issue #154). The test
	// waited for tts.Fake.Speaks() >= 1 on its own goroutine — a count with no
	// ordering to the engine's, and no name on the sentence it counted. The
	// engine can finish *both* rounds before the speaker goroutine dequeues
	// anything at all, and then all three narration sentences are stale at
	// dequeue and drop; the one synthesis the count saw was the answer's.
	// That is #120 working exactly as designed — a stale utterance is dropped
	// at dequeue, and only the *in-flight* sentence is promised — so the
	// failure was the assertion's, not the product's. It cost about one run in
	// three without -race, and passed under -race only because the detector's
	// scheduling happened to let the speaker run first, which is the only way
	// CI runs it. Do not go back to sampling a count: gate on the sentence.
	rec := &gatedTool{name: "run", result: "checked", gate: synth.firstSpeak()}
	reg.Register(rec)

	// Round one narrates three complete sentences, then calls the tool; round
	// two is the final answer. The held synthesizer keeps the voice inside
	// sentence one for the whole exchange, so sentences two and three are
	// still queued unplayed when the answer's first sentence is committed.
	h.provider.Preamble = "Checking the first thing. Checking the second thing. Checking the third thing."
	h.provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "run", Arguments: `{"command":"check"}`}},
	}
	h.provider.Response = "All done here."

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("check everything"); err != nil {
		t.Fatal(err)
	}

	events := collectEvents(t, h, "assistant.finished")
	// The whole answer is now committed to the queue (streamOnce flushed it
	// before assistant.finished was published) and the floor has risen — while
	// the voice is still inside sentence one, because the hold gate has not
	// been released and the speaker cannot reach a second Speak until it is.
	// Exactly one synthesis is therefore both a fact and the situation the
	// rest of this test is about.
	if n := h.tts.Speaks(); n != 1 {
		t.Fatalf("syntheses when the answer committed = %d, want 1: the voice must still be "+
			"inside the first sentence for supersession to have anything to drop", n)
	}
	// Releasing the audio now lets the queue drain against a floor that has
	// already risen.
	h.tts.SetHold(nil)
	release()
	events = append(events, collectEvents(t, h, "session.finished")...)
	h.waitIdle(t)

	// What was actually said: the in-flight sentence, finished, then the
	// answer. The stale middle never reached synthesis.
	want := []string{"Checking the first thing.", "All done here."}
	got := synth.texts()
	if len(got) != len(want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("spoken = %q, want %q", got, want)
		}
	}

	// The skip is on the record: one supersession event naming the winning
	// turn and the two sentences it cost.
	sup, ok := findEvent(events, "tts.superseded")
	if !ok {
		t.Fatal("no tts.superseded event was published")
	}
	if turn, _ := sup.Data["turn"].(int); turn != 2 {
		t.Errorf("tts.superseded turn = %v, want 2", sup.Data["turn"])
	}
	if dropped, _ := sup.Data["dropped"].(int); dropped != 2 {
		t.Errorf("tts.superseded dropped = %v, want 2", sup.Data["dropped"])
	}
	timings, ok := findEvent(events, "session.timings")
	if !ok {
		t.Fatal("no session.timings event was published")
	}
	if n, _ := timings.Data[StageSupersededSentences].(int); n != 2 {
		t.Errorf("timings %s = %v, want 2", StageSupersededSentences, timings.Data[StageSupersededSentences])
	}

	// The UI's state machine sees one answer: one started, one finished, one
	// playback stream, and the tool ran exactly once.
	if n := eventCount(events, "tts.started"); n != 1 {
		t.Errorf("tts.started published %d times, want 1", n)
	}
	if n := eventCount(events, "tts.finished"); n != 1 {
		t.Errorf("tts.finished published %d times, want 1", n)
	}
	chunks, plays := h.player.Played()
	if plays != 1 {
		t.Errorf("playback streams opened = %d, want 1 for the whole turn", plays)
	}
	// Supersession pays at the queue, never at the device: the sentence that
	// was in flight when the floor rose delivered all of its audio. The fake
	// synthesizer emits one chunk per Speak, so one chunk per surviving
	// sentence is "neither was cut short" stated where a user would hear it.
	if len(chunks) != len(want) {
		t.Errorf("chunks reaching the player = %d, want %d: the in-flight sentence was cut mid-audio",
			len(chunks), len(want))
	}
	if got := rec.callCount(); got != 1 {
		t.Errorf("tool ran %d times, want 1", got)
	}
}

// TestNothingWithinATurnIsEverSkipped pins the other half of the contract:
// supersession is strictly cross-turn. A single turn streaming several
// sentences — with the audio lagging the whole way, exactly the condition
// that triggers drops across turns — plays every one of them.
func TestNothingWithinATurnIsEverSkipped(t *testing.T) {
	h, synth := newRecordedHarness(t, nil)
	release := holdFirstSentence(t, h)

	h.provider.Response = "First sentence here. Second sentence here. Third sentence here."

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("tell me everything"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the first sentence reaches the synthesizer", func() bool { return h.tts.Speaks() >= 1 })
	events := collectEvents(t, h, "assistant.finished")
	h.tts.SetHold(nil)
	release()
	events = append(events, collectEvents(t, h, "session.finished")...)
	h.waitIdle(t)

	want := []string{"First sentence here.", "Second sentence here.", "Third sentence here."}
	got := synth.texts()
	if len(got) != len(want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("spoken = %q, want %q", got, want)
		}
	}
	if _, ok := findEvent(events, "tts.superseded"); ok {
		t.Error("a single turn superseded itself")
	}
	timings, ok := findEvent(events, "session.timings")
	if !ok {
		t.Fatal("no session.timings event was published")
	}
	if v, present := timings.Data[StageSupersededSentences]; present {
		t.Errorf("timings carry %s = %v for a turn that dropped nothing; the key must be absent",
			StageSupersededSentences, v)
	}
}

// TestConfirmationPromptQueuedBehindStaleSpeechStillPlays is the never-drop
// mutation check for the keep exemption: a confirmation question queued
// behind three sentences of a turn that then gets superseded still plays, in
// order, between the finishing in-flight sentence and the new turn's speech.
// The same keep flag is the pinned policy for any future receipt-of-executed-
// action aside (see utterance.keep): what gates progress or records a real
// action is never traded for freshness.
func TestConfirmationPromptQueuedBehindStaleSpeechStillPlays(t *testing.T) {
	h, synth := newRecordedHarness(t, nil)
	queued := make(chan struct{}, 16)
	h.engine.speakerQueued = func() { queued <- struct{}{} }
	release := holdFirstSentence(t, h)

	sp, s := speakerUnderTest(t, h)
	sp.speak("The first sentence is playing.")
	<-queued
	waitUntil(t, "the first sentence reaches the synthesizer", func() bool { return h.tts.Speaks() >= 1 })
	sp.speak("The second sentence is stale.")
	<-queued
	sp.speak("The third sentence is stale.")
	<-queued

	promptCtx, stopPrompt := context.WithCancel(s.ctx)
	defer stopPrompt()
	asked := make(chan struct{})
	go func() {
		sp.interject(promptCtx, "May I run the command?", true)
		close(asked)
	}()
	<-queued

	// The next turn commits: everything above that does not keep is condemned.
	sp.nextTurn()
	sp.speak("Here is the newer answer.")
	<-queued

	h.tts.SetHold(nil)
	release()
	if err := sp.close(); err != nil {
		t.Fatalf("speaker close: %v", err)
	}
	// interject returned because the question was handed to the player — not
	// abandoned, not dropped.
	<-asked

	want := []string{"The first sentence is playing.", "May I run the command?", "Here is the newer answer."}
	got := synth.texts()
	if len(got) != len(want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("spoken = %q, want %q", got, want)
		}
	}
	sup := h.waitFor(t, "tts.superseded")
	if turn, _ := sup.Data["turn"].(int); turn != 2 {
		t.Errorf("tts.superseded turn = %v, want 2", sup.Data["turn"])
	}
	if dropped, _ := sup.Data["dropped"].(int); dropped != 2 {
		t.Errorf("tts.superseded dropped = %v, want 2", sup.Data["dropped"])
	}
}

// TestStaleReassuranceAsideIsDropped pins the other side of the aside policy:
// a "still working" reassurance queued during a turn the answer then moves
// past is stale comfort — the work it reassures about has finished, or the
// newer sentence could not have been committed — and it drops. Its waiter is
// still released: a dropped aside must never wedge the goroutine that
// interjected it.
func TestStaleReassuranceAsideIsDropped(t *testing.T) {
	h, synth := newRecordedHarness(t, nil)
	queued := make(chan struct{}, 16)
	h.engine.speakerQueued = func() { queued <- struct{}{} }
	release := holdFirstSentence(t, h)

	sp, s := speakerUnderTest(t, h)
	sp.speak("The first sentence is playing.")
	<-queued
	waitUntil(t, "the first sentence reaches the synthesizer", func() bool { return h.tts.Speaks() >= 1 })

	reassured := make(chan struct{})
	go func() {
		sp.interject(s.ctx, "Still working on it.", false)
		close(reassured)
	}()
	<-queued

	sp.nextTurn()
	sp.speak("Here is the result.")
	<-queued

	h.tts.SetHold(nil)
	release()
	if err := sp.close(); err != nil {
		t.Fatalf("speaker close: %v", err)
	}
	<-reassured

	want := []string{"The first sentence is playing.", "Here is the result."}
	got := synth.texts()
	if len(got) != len(want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("spoken = %q, want %q", got, want)
		}
	}
	sup := h.waitFor(t, "tts.superseded")
	if dropped, _ := sup.Data["dropped"].(int); dropped != 1 {
		t.Errorf("tts.superseded dropped = %v, want 1 (the stale reassurance)", sup.Data["dropped"])
	}
	if s.timings.report()[StageSupersededSentences] != 1 {
		t.Errorf("timings %s = %v, want 1", StageSupersededSentences,
			s.timings.report()[StageSupersededSentences])
	}
}

// TestARoundThatCommitsNoSpeechSupersedesNothing pins that supersession is
// decided at commit, not at the round boundary: opening a new turn without
// committing a sentence (a round of pure tool calls) drops nothing — there
// is no newer speech the queue would be holding back, and dropping would buy
// silence, not freshness.
func TestARoundThatCommitsNoSpeechSupersedesNothing(t *testing.T) {
	h, synth := newRecordedHarness(t, nil)
	queued := make(chan struct{}, 16)
	h.engine.speakerQueued = func() { queued <- struct{}{} }
	release := holdFirstSentence(t, h)

	sp, _ := speakerUnderTest(t, h)
	sp.speak("The first sentence is playing.")
	<-queued
	waitUntil(t, "the first sentence reaches the synthesizer", func() bool { return h.tts.Speaks() >= 1 })
	sp.speak("The second sentence is still wanted.")
	<-queued

	// A silent round: the turn advances, no sentence is committed for it.
	sp.nextTurn()

	h.tts.SetHold(nil)
	release()
	if err := sp.close(); err != nil {
		t.Fatalf("speaker close: %v", err)
	}

	want := []string{"The first sentence is playing.", "The second sentence is still wanted."}
	got := synth.texts()
	if len(got) != len(want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("spoken = %q, want %q", got, want)
		}
	}
	// Every event of this speaker is on the bus before close() returned
	// (tts.superseded, when owed, precedes the synthesis of the surviving
	// sentence), so a drained channel really is proof of absence.
	for {
		select {
		case ev := <-h.events:
			if ev.Type == "tts.superseded" {
				t.Fatalf("a silent round superseded speech: %v", ev.Data)
			}
			continue
		default:
		}
		break
	}
}
