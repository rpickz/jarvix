package session

// The speak-again path (issue #122), tested hermetically: address resolution
// against the record, the standard-pipeline promise, the pinned precedence
// (live speech wins; the newest replay wins), every stop path, and the
// untouched conversation record. Deterministic throughout — speech is held in
// progress with the tts.Fake's gate, never with timers.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
)

// replayTurns is a fixed two-turn conversation for the tests below.
func replayHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, Options{SpeakResponses: true, HistoryTurns: 8, FollowUpWindow: time.Hour})
	h.ask(t, "explain recursion")
	return h
}

func TestReplaySpeaksRecordedTurnThroughStandardPipeline(t *testing.T) {
	h := replayHarness(t)
	before := h.engine.Conversation()
	speaksBefore := h.tts.Speaks()

	turn, role, err := h.engine.ReplaySpeech(2, "assistant")
	if err != nil {
		t.Fatal(err)
	}
	if turn != 2 || role != "assistant" {
		t.Fatalf("replay resolved (%d, %s), want (2, assistant)", turn, role)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	// The standard speech surface: Speaking was claimed, the tts bookends
	// published, and the record's own event names the turn.
	for _, want := range []string{"tts.started", "tts.finished", "speech.replayed"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("missing event %q", want)
		}
	}
	if got := seen["speech.replayed"].Data["turn"]; got != 2 {
		t.Errorf("speech.replayed turn = %v, want 2", got)
	}
	// The same synthesizer path spoke the recorded text.
	if h.tts.Speaks() <= speaksBefore {
		t.Error("replay never reached the synthesizer")
	}
	if h.tts.Last().Text != "Recursion is a function calling itself." {
		t.Errorf("tts got %q", h.tts.Last().Text)
	}
	// The conversation record is untouched: a replay is not a new turn.
	after := h.engine.Conversation()
	if len(after) != len(before) {
		t.Fatalf("replay changed the record: %d turns, was %d", len(after), len(before))
	}
	for i := range after {
		if after[i] != before[i] {
			t.Errorf("turn %d changed: %+v -> %+v", i+1, before[i], after[i])
		}
	}
}

func TestReplayDefaultsToNewestAssistantTurn(t *testing.T) {
	h := replayHarness(t)
	h.provider.Response = "A second answer entirely."
	h.ask(t, "and again")

	turn, role, err := h.engine.ReplaySpeech(0, "")
	if err != nil {
		t.Fatal(err)
	}
	if turn != 4 || role != "assistant" {
		t.Fatalf("default replay resolved (%d, %s), want the newest assistant turn (4)", turn, role)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if h.tts.Last().Text != "A second answer entirely." {
		t.Errorf("tts got %q", h.tts.Last().Text)
	}
}

func TestReplayIsRefusedWhileConversationSpeaks(t *testing.T) {
	h := replayHarness(t)
	hold := make(chan struct{})
	h.tts.SetHold(hold)
	defer close(hold)
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("a follow-up")
	h.waitFor(t, "tts.started")

	// Live conversation speech wins: the replay is refused, the live turn
	// untouched.
	if _, _, err := h.engine.ReplaySpeech(2, "assistant"); !errors.Is(err, ErrReplayBusy) {
		t.Fatalf("replay during live speech: err = %v, want ErrReplayBusy", err)
	}
	if s, _ := h.engine.State(); s != StateSpeaking {
		t.Errorf("live turn disturbed: state = %s", s)
	}
	h.engine.CancelSpeech()
	h.waitFor(t, "session.finished")
	h.waitIdle(t)
}

func TestReplayIsRefusedWhileConversationThinks(t *testing.T) {
	h := replayHarness(t)
	h.provider.Delay = 50 * time.Millisecond
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("a slow one")
	h.waitFor(t, "assistant.started")

	if _, _, err := h.engine.ReplaySpeech(2, "assistant"); !errors.Is(err, ErrReplayBusy) {
		t.Fatalf("replay during a live turn: err = %v, want ErrReplayBusy", err)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)
}

func TestReplayIsSupersededByNewLiveSession(t *testing.T) {
	h := replayHarness(t)
	hold := make(chan struct{})
	h.tts.SetHold(hold)
	defer close(hold)
	if _, _, err := h.engine.ReplaySpeech(2, "assistant"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "tts.started")

	// The user speaks again: the replay is cancelled instantly, exactly as an
	// interrupted session is, and the new turn proceeds.
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	ev := h.waitFor(t, "session.cancelled")
	if reason, _ := ev.Data["reason"].(string); !strings.Contains(reason, "new session") {
		t.Errorf("reason = %q", reason)
	}
	h.tts.SetHold(nil)
	h.provider.Response = "The follow-up answer."
	if err := h.engine.Submit("a follow-up"); err != nil {
		t.Fatal(err)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	// The record carries the live exchange and nothing from the replay.
	turns := h.engine.Conversation()
	if len(turns) != 4 {
		t.Fatalf("turns = %d, want 4: %+v", len(turns), turns)
	}
	if turns[3].Text != "The follow-up answer." {
		t.Errorf("turn 4 = %+v", turns[3])
	}
}

func TestSecondReplaySupersedesFirst(t *testing.T) {
	h := replayHarness(t)
	spokenBefore := h.tts.Speaks()
	hold := make(chan struct{})
	h.tts.SetHold(hold)
	defer close(hold)
	if _, _, err := h.engine.ReplaySpeech(2, "assistant"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "tts.started")
	// tts.started is published at enqueue, with the Speaking claim (issue
	// #111), so it says the sentence is committed to the queue — not that the
	// synthesizer has been reached. Last() below names the *most recent* Speak
	// of the whole fake, so a first replay whose call has not happened yet can
	// still make it after the second replay's and leave Last() reporting the
	// superseded text ("Recursion is a function calling itself.", about one
	// run in twenty). Park the first replay inside Speak on the hold gate
	// before superseding it: its one sentence is then provably already spoken
	// for, and cannot reappear behind the second's. Same unestablished
	// ordering as issue #154, same fix.
	waitUntil(t, "the first replay reaches the synthesizer",
		func() bool { return h.tts.Speaks() > spokenBefore })

	// Clicking another message wins: the first replay is superseded, the
	// second plays whole.
	h.tts.SetHold(nil)
	turn, role, err := h.engine.ReplaySpeech(1, "user")
	if err != nil {
		t.Fatal(err)
	}
	if turn != 1 || role != "user" {
		t.Fatalf("second replay resolved (%d, %s), want (1, user)", turn, role)
	}
	ev := h.waitFor(t, "session.cancelled")
	if reason, _ := ev.Data["reason"].(string); !strings.Contains(reason, "another replay") {
		t.Errorf("reason = %q", reason)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if got := seen["speech.replayed"].Data["turn"]; got != 1 {
		t.Errorf("speech.replayed turn = %v, want 1", got)
	}
	// The spoken form of a bare line gains its terminal period from the same
	// normalizer live speech uses — the standard-pipeline promise.
	if h.tts.Last().Text != "explain recursion." {
		t.Errorf("tts got %q", h.tts.Last().Text)
	}
	if len(h.engine.Conversation()) != 2 {
		t.Error("replays changed the record")
	}
}

func TestCancelSpeechStopsReplayLikeAnySpeech(t *testing.T) {
	h := replayHarness(t)
	hold := make(chan struct{})
	h.tts.SetHold(hold)
	defer close(hold)
	if _, _, err := h.engine.ReplaySpeech(2, "assistant"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "tts.started")

	if !h.engine.CancelSpeech() {
		t.Fatal("CancelSpeech reported nothing playing during a replay")
	}
	ev := h.waitFor(t, "tts.finished")
	if ev.Data["interrupted"] != true {
		t.Errorf("tts.finished data = %v", ev.Data)
	}
	h.waitFor(t, "session.finished")
	h.waitIdle(t)
	if len(h.engine.Conversation()) != 2 {
		t.Error("a stopped replay changed the record")
	}
}

func TestCancelStopsReplay(t *testing.T) {
	h := replayHarness(t)
	hold := make(chan struct{})
	h.tts.SetHold(hold)
	defer close(hold)
	if _, _, err := h.engine.ReplaySpeech(2, "assistant"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "tts.started")

	if err := h.engine.Cancel(); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "session.cancelled")
	h.waitIdle(t)
	if len(h.engine.Conversation()) != 2 {
		t.Error("a cancelled replay changed the record")
	}
}

func TestReplaySpeaksInterruptedTurnAsRecorded(t *testing.T) {
	h := replayHarness(t)
	hold := make(chan struct{})
	h.tts.SetHold(hold)
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("a question that gets cut off")
	h.waitFor(t, "tts.started")
	h.engine.CancelSpeech()
	h.waitFor(t, "session.finished")
	h.waitIdle(t)
	close(hold)
	h.tts.SetHold(nil)

	turns := h.engine.Conversation()
	if len(turns) != 4 || !strings.Contains(turns[3].Text, "interrupted") {
		t.Fatalf("expected an interrupted turn 4, got %+v", turns)
	}
	// Replayable like any turn, spoken exactly as the record shows it —
	// annotation included, because that is the on-screen text.
	if _, _, err := h.engine.ReplaySpeech(4, "assistant"); err != nil {
		t.Fatal(err)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if !strings.Contains(h.tts.Last().Text, "interrupted") {
		t.Errorf("the annotation was not spoken; last tts text %q", h.tts.Last().Text)
	}
}

func TestReplayConfirmationTurnSpeaksSummaryOnly(t *testing.T) {
	h := replayHarness(t)
	// A resolved permission-gate record, planted the way a resolution plants
	// it: under the lock, between the exchange's halves.
	h.engine.mu.Lock()
	h.engine.recordConfirmationLocked(&pendingConfirmation{
		tool:    "run",
		command: "rm -rf /tmp/scratch",
		summary: "May I remove the scratch directory?",
		timeout: 30 * time.Second,
	}, "approved", "cli")
	h.engine.mu.Unlock()

	turns := h.engine.Conversation()
	if len(turns) != 3 || turns[2].Role != "confirmation" {
		t.Fatalf("expected a confirmation turn 3, got %+v", turns)
	}
	if _, _, err := h.engine.ReplaySpeech(3, "confirmation"); err != nil {
		t.Fatal(err)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if !strings.Contains(h.tts.Last().Text, "scratch directory") {
		t.Errorf("summary was not spoken: %q", h.tts.Last().Text)
	}
	if strings.Contains(h.tts.Last().Text, "rm -rf") {
		t.Errorf("the verbatim command must not be spoken: %q", h.tts.Last().Text)
	}
}

func TestReplayRefusalsForBadAddresses(t *testing.T) {
	h := replayHarness(t)
	if _, _, err := h.engine.ReplaySpeech(7, ""); err == nil {
		t.Error("out-of-range turn accepted")
	}
	if _, _, err := h.engine.ReplaySpeech(1, "assistant"); err == nil {
		t.Error("role mismatch accepted — a stale address would speak the wrong turn")
	}
	// Refusals start nothing: the engine is still idle with no session.
	if s, id := h.engine.State(); s != StateIdle || id != "" {
		t.Errorf("refusal left state %s session %q", s, id)
	}
}

func TestReplayRefusedWithSpeechOff(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: false, HistoryTurns: 8})
	h.ask(t, "quiet question")
	if _, _, err := h.engine.ReplaySpeech(2, "assistant"); err == nil {
		t.Error("replay accepted with speech disabled")
	}
}

func TestReplayRefusedOnEmptyConversation(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true, HistoryTurns: 8})
	if _, _, err := h.engine.ReplaySpeech(0, ""); err == nil {
		t.Error("default replay accepted with nothing recorded")
	}
}

func TestReplayWorksOnAdoptedConversation(t *testing.T) {
	// The rebuilt-history case (issue #122's #118 dependency): a conversation
	// restored through AdoptConversation — the conversation.open path — keeps
	// positional identity, so the same address speaks the same turn.
	h := newHarness(t, Options{SpeakResponses: true, HistoryTurns: 8, FollowUpWindow: time.Hour})
	msgs := []ai.Message{
		{Role: ai.RoleUser, Content: "what did we archive?"},
		{Role: ai.RoleAssistant, Content: "An archived answer, restored whole."},
	}
	h.engine.AdoptConversation("c-reopened", msgs, nil, nil)

	turn, role, err := h.engine.ReplaySpeech(2, "assistant")
	if err != nil {
		t.Fatal(err)
	}
	if turn != 2 || role != "assistant" {
		t.Fatalf("adopted replay resolved (%d, %s)", turn, role)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if h.tts.Last().Text != "An archived answer, restored whole." {
		t.Errorf("tts got %q", h.tts.Last().Text)
	}
}
