package session

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/intent"
)

// The engine's half of the return briefing (#150, ADR 0050). What is asserted
// here is only what the engine owns: which sessions count as the user being
// here, where the offer lands, and that the spoken briefing does not become
// conversation memory. Everything about *what* a briefing says is tested in
// internal/briefing.

// fakeReturning records the engine's calls and scripts the two answers.
type fakeReturning struct {
	mu sync.Mutex

	offer          string
	offerTransient bool
	briefing       string
	err            error

	arrivals  []time.Time
	offerAsks int
	briefings int
}

func (f *fakeReturning) Arrive(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.arrivals = append(f.arrivals, now)
}

func (f *fakeReturning) OfferLine(context.Context) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offerAsks++
	return f.offer, f.offerTransient
}

func (f *fakeReturning) Briefing(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.briefings++
	return f.briefing, f.err
}

func (f *fakeReturning) seen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.arrivals)
}

func (f *fakeReturning) asked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.offerAsks
}

func (f *fakeReturning) briefed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.briefings
}

// TestTheOfferRidesTheAnswerItFollows: one appended sentence, in the ear, in
// the event, and in the record — one answer, not a second turn nobody asked
// for.
func TestTheOfferRidesTheAnswerItFollows(t *testing.T) {
	ret := &fakeReturning{offer: "I've got a briefing when you want it."}
	h := newHarness(t, Options{
		SpeakResponses: true,
		HistoryTurns:   4,
		FollowUpWindow: time.Hour,
		Returning:      ret,
	})
	h.provider.Response = "Recursion is a function calling itself."

	seen := func() map[string]Event {
		if _, err := h.engine.StartSession(); err != nil {
			t.Fatal(err)
		}
		if err := h.engine.Submit("explain recursion"); err != nil {
			t.Fatal(err)
		}
		out := h.collectUntil(t, "session.finished")
		h.waitIdle(t)
		return out
	}()

	content, _ := seen["assistant.finished"].Data["content"].(string)
	if !strings.HasSuffix(content, ret.offer) {
		t.Errorf("assistant.finished content = %q, want it to end with the offer", content)
	}
	if !strings.HasPrefix(content, "Recursion is a function calling itself.") {
		t.Errorf("the answer itself did not survive the append: %q", content)
	}
	turns := h.engine.Conversation()
	if len(turns) != 2 || !strings.HasSuffix(turns[1].Text, ret.offer) {
		t.Errorf("the record does not carry what was spoken: %+v", turns)
	}
	// The speaker was handed the offer as its own sentence — enqueued last,
	// so the fake's most recent request IS the offer — rather than the
	// sentence merely being written into a record nobody heard.
	if last := h.tts.Last().Text; !strings.Contains(last, "briefing when you want it") {
		t.Errorf("the last thing spoken was %q, not the offer", last)
	}
}

// TestNoOfferMeansAnUntouchedAnswer: the overwhelmingly common case must be
// byte-identical to a daemon without this feature.
func TestNoOfferMeansAnUntouchedAnswer(t *testing.T) {
	ret := &fakeReturning{}
	h := newHarness(t, Options{HistoryTurns: 4, FollowUpWindow: time.Hour, Returning: ret})
	h.provider.Response = "Recursion is a function calling itself."
	h.ask(t, "explain recursion")

	turns := h.engine.Conversation()
	if len(turns) != 2 || turns[1].Text != "Recursion is a function calling itself." {
		t.Errorf("an answer with no offer was altered: %+v", turns)
	}
	if ret.asked() != 1 {
		t.Errorf("the offer was asked for %d times, want once per answer", ret.asked())
	}
}

// TestAClockfireIsNotTheUserComingBack is the seam's whole point. A reminder
// speaking at three in the morning is the daemon talking to itself: counting
// it as a sighting would erase the night the briefing exists to describe, and
// appending an offer to it would spend that night's one offer on an empty
// room.
func TestAClockfireIsNotTheUserComingBack(t *testing.T) {
	ret := &fakeReturning{offer: "I've got a briefing when you want it."}
	h := newHarness(t, Options{HistoryTurns: 4, FollowUpWindow: time.Hour, Returning: ret})
	h.provider.Response = "The routine ran."

	if _, err := h.engine.StartScheduledSession(true); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("routine check"); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	if ret.seen() != 0 {
		t.Errorf("a clockfire was recorded as %d sightings of the user", ret.seen())
	}
	if ret.asked() != 0 {
		t.Errorf("a clockfire was offered a briefing %d times", ret.asked())
	}
	if content, _ := seen["assistant.finished"].Data["content"].(string); content != "The routine ran." {
		t.Errorf("a clockfire's answer was altered: %q", content)
	}
}

// TestADeterministicIntentIsStillTheUserBeingHere. "Volume thirty" at nine in
// the morning ends the night as surely as a question does, even though it
// never reaches the model.
func TestADeterministicIntentIsStillTheUserBeingHere(t *testing.T) {
	ret := &fakeReturning{}
	router, err := intent.New(intent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Options{
		HistoryTurns: 4, FollowUpWindow: time.Hour,
		Intents: router, Returning: ret,
	})
	h.ask(t, "stop talking")
	if ret.seen() != 1 {
		t.Errorf("a routed intent was recorded as %d sightings, want 1", ret.seen())
	}
}

// TestTheSpokenBriefingIsNotConversationMemory. The account is transient: the
// exchange stays in the record, its content does not, so no later turn is
// sent a briefing it can quote back or build on.
func TestTheSpokenBriefingIsNotConversationMemory(t *testing.T) {
	const salt = "SECRET-BRIEFING-CONTENT"
	ret := &fakeReturning{briefing: "Since you were last here nine hours ago: one finished. " +
		"The session on " + salt + " has finished."}
	router, err := intent.New(intent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Options{
		SpeakResponses: true, HistoryTurns: 4, FollowUpWindow: time.Hour,
		Intents: router, Returning: ret,
	})
	h.ask(t, "what did i miss")

	if ret.briefed() != 1 {
		t.Fatalf("the briefing was composed %d times", ret.briefed())
	}
	// It was spoken: the salted sentence is the last one, so the fake's most
	// recent request carries it.
	if last := h.tts.Last().Text; !strings.Contains(last, salt) {
		t.Errorf("the last thing spoken was %q, not the briefing's own words", last)
	}
	// And it is not in the record.
	turns := h.engine.Conversation()
	if len(turns) != 2 {
		t.Fatalf("turns = %+v", turns)
	}
	for _, turn := range turns {
		if strings.Contains(turn.Text, salt) {
			t.Errorf("the briefing became conversation memory: %q", turn.Text)
		}
	}
	if turns[1].Text != briefingRecord {
		t.Errorf("assistant turn = %q, want the stand-in %q", turns[1].Text, briefingRecord)
	}
	if turns[0].Text != "what did i miss" {
		t.Errorf("the question left the record too: %q", turns[0].Text)
	}
}

// TestABriefingPhraseNeverReachesTheModel. It is a deterministic intent, and
// the whole point of ADR 0017 is that a fixed phrase with one right outcome
// does not spend a provider round-trip discovering that.
func TestABriefingPhraseNeverReachesTheModel(t *testing.T) {
	ret := &fakeReturning{briefing: "Nothing while you were away."}
	router, err := intent.New(intent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Options{
		HistoryTurns: 4, FollowUpWindow: time.Hour,
		Intents: router, Returning: ret,
	})
	h.ask(t, "give me the briefing")
	if n := len(h.provider.Requests); n != 0 {
		t.Errorf("the provider was called %d times for a routed briefing phrase", n)
	}
}

// TestSpeakOnReturnIsSpokenButNotRecorded. The whole account arrives on the
// first answer, in the ear and in the event, and the conversation keeps only
// the answer the user actually asked for — the transience rule applied to the
// one path that would otherwise smuggle a briefing into memory.
func TestSpeakOnReturnIsSpokenButNotRecorded(t *testing.T) {
	const salt = "SECRET-OVERNIGHT-DETAIL"
	ret := &fakeReturning{
		offer:          "Since you were last here nine hours ago: one finished. The session on " + salt + " has finished.",
		offerTransient: true,
	}
	h := newHarness(t, Options{
		SpeakResponses: true, HistoryTurns: 4, FollowUpWindow: time.Hour,
		Returning: ret,
	})
	h.provider.Response = "Recursion is a function calling itself."

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("explain recursion"); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	if content, _ := seen["assistant.finished"].Data["content"].(string); !strings.Contains(content, salt) {
		t.Errorf("what was said was not published: %q", content)
	}
	if last := h.tts.Last().Text; !strings.Contains(last, salt) {
		t.Errorf("the last thing spoken was %q, not the briefing", last)
	}
	turns := h.engine.Conversation()
	if len(turns) != 2 {
		t.Fatalf("turns = %+v", turns)
	}
	if strings.Contains(turns[1].Text, salt) {
		t.Errorf("a spoken-on-return briefing became conversation memory: %q", turns[1].Text)
	}
	if turns[1].Text != "Recursion is a function calling itself." {
		t.Errorf("the answer itself did not survive: %q", turns[1].Text)
	}
}

// TestABriefinglessDaemonRefusesHonestly. Disabled must mean absent: a nil
// seam makes the phrase a spoken refusal, never a silent drop.
func TestABriefinglessDaemonRefusesHonestly(t *testing.T) {
	router, err := intent.New(intent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Options{
		SpeakResponses: true, HistoryTurns: 4, FollowUpWindow: time.Hour,
		Intents: router,
	})
	h.ask(t, "what did i miss")
	turns := h.engine.Conversation()
	if len(turns) != 2 || !strings.Contains(turns[1].Text, "not available on this daemon") {
		t.Errorf("turns = %+v, want an honest spoken refusal", turns)
	}
}
