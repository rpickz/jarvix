package session

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/intent"
)

// The engine's half of the situation report (#196, ADR 0061). What is asserted
// here is only what the engine owns: that a routed phrase is answered without a
// provider round-trip, that the composed report does not become conversation
// memory, and that a daemon without the service refuses out loud. Everything
// about *what* a report says is tested in internal/situation.

// fakeOperating scripts the one answer and counts the asks.
type fakeOperating struct {
	mu sync.Mutex

	report  string
	err     error
	reports int
}

func (f *fakeOperating) Situation(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports++
	return f.report, f.err
}

func (f *fakeOperating) asked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reports
}

// TestASituationPhraseNeverReachesTheModel. It is a deterministic intent, and
// the whole point of ADR 0017 is that a fixed phrase with one right outcome
// does not spend a provider round-trip discovering that.
func TestASituationPhraseNeverReachesTheModel(t *testing.T) {
	for _, phrase := range []string{"whats going on", "where are we", "status report"} {
		op := &fakeOperating{report: "Nothing needs you."}
		router, err := intent.New(intent.Options{})
		if err != nil {
			t.Fatal(err)
		}
		h := newHarness(t, Options{
			HistoryTurns: 4, FollowUpWindow: time.Hour,
			Intents: router, Operating: op,
		})
		h.ask(t, phrase)
		if n := len(h.provider.Requests); n != 0 {
			t.Errorf("%q reached the provider %d times", phrase, n)
		}
		if op.asked() != 1 {
			t.Errorf("%q composed %d reports, want 1", phrase, op.asked())
		}
	}
}

// TestTheSituationReportIsNotConversationMemory. A report is a description of a
// moment, and a moment that has passed is the single most misleading thing a
// later turn could be handed: "the session on X is waiting on you" committed at
// nine o'clock is false by half past, and a model reading it back would state
// it with the confidence of something it was told.
func TestTheSituationReportIsNotConversationMemory(t *testing.T) {
	const salt = "SECRET-SITUATION-CONTENT"
	op := &fakeOperating{report: "Right now: one waiting on you. " +
		"The AI session on " + salt + " is waiting on you."}
	router, err := intent.New(intent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Options{
		SpeakResponses: true, HistoryTurns: 4, FollowUpWindow: time.Hour,
		Intents: router, Operating: op,
	})
	h.ask(t, "whats going on")

	if op.asked() != 1 {
		t.Fatalf("the report was composed %d times", op.asked())
	}
	// It was spoken.
	if last := h.tts.Last().Text; !strings.Contains(last, salt) {
		t.Errorf("the last thing spoken was %q, not the report's own words", last)
	}
	// And it is not in the record.
	turns := h.engine.Conversation()
	if len(turns) != 2 {
		t.Fatalf("turns = %+v", turns)
	}
	for _, turn := range turns {
		if strings.Contains(turn.Text, salt) {
			t.Errorf("the report became conversation memory: %q", turn.Text)
		}
	}
	if turns[1].Text != situationRecord {
		t.Errorf("assistant turn = %q, want the stand-in %q", turns[1].Text, situationRecord)
	}
	if turns[0].Text != "whats going on" {
		t.Errorf("the question left the record too: %q", turns[0].Text)
	}
}

// TestTheTwoAccountsKeepTheirOwnStandIns. The briefing and the report are two
// features with two records, and a turn that said which one happened is worth
// more than one that said an account was given.
func TestTheTwoAccountsKeepTheirOwnStandIns(t *testing.T) {
	if situationRecord == briefingRecord {
		t.Fatal("the two stand-ins are the same sentence")
	}
	op := &fakeOperating{report: "Nothing needs you."}
	ret := &fakeReturning{briefing: "Nothing since you were last here, just now."}
	router, err := intent.New(intent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Options{
		HistoryTurns: 8, FollowUpWindow: time.Hour,
		Intents: router, Operating: op, Returning: ret,
	})
	h.ask(t, "what did i miss")
	h.ask(t, "whats going on")

	turns := h.engine.Conversation()
	if len(turns) != 4 {
		t.Fatalf("turns = %+v", turns)
	}
	if turns[1].Text != briefingRecord {
		t.Errorf("the briefing's record = %q", turns[1].Text)
	}
	if turns[3].Text != situationRecord {
		t.Errorf("the report's record = %q", turns[3].Text)
	}
}

// TestASituationlessDaemonRefusesHonestly. Disabled must mean absent: a nil
// seam makes the phrase a spoken refusal, never a silent drop.
func TestASituationlessDaemonRefusesHonestly(t *testing.T) {
	router, err := intent.New(intent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Options{
		SpeakResponses: true, HistoryTurns: 4, FollowUpWindow: time.Hour,
		Intents: router,
	})
	h.ask(t, "where are we")
	turns := h.engine.Conversation()
	if len(turns) != 2 || !strings.Contains(turns[1].Text, "not available on this daemon") {
		t.Errorf("turns = %+v, want an honest spoken refusal", turns)
	}
}
