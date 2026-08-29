package session

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/conversations"
)

// This file pins issue #117: a conversation only ends when the user says so.
// Interrupting a session — a new push-to-talk, `jarvix cancel`, the stop
// word — commits the exchange it was carrying, marked interrupted, into
// working memory and the archive, so a clarification answer always follows
// its question. The incident these tests reproduce (daemon log, 2026-08-27
// 18:49): s1 asked, Jarvix streamed a clarifying question, the user
// pushed-to-talk to answer, s1 was cancelled "interrupted by new session"
// with nothing committed, and s2's model — empty working memory, empty
// archive — told the user it lacked context.

// The incident shape, exactly: ask → clarifying question → interrupt →
// answer. The follow-up's model context must contain the full pending
// exchange, marked interrupted — never a hole the model narrates as missing
// context.
func TestInterruptedExchangeSurvivesIntoTheNextSession(t *testing.T) {
	fake := conversations.NewFake()
	h := newHarness(t, Options{Model: "test-model", HistoryTurns: 8,
		SpeakResponses: true, Archive: fake})
	// Hold speech so the clarifying question is still being spoken when the
	// interrupt lands — the incident's s1 was cancelled 24 seconds in, mid
	// (spoken) sentence. No chunk plays until the gate opens; only the
	// cancellation releases the first session.
	hold := make(chan struct{})
	h.tts.SetHold(hold)
	defer close(hold)

	h.provider.Response = "Do you mean tomorrow, or the whole week?"
	first, _ := h.engine.StartSession()
	_ = h.engine.Submit("what's on my calendar tomorrow?")
	// assistant.finished means every delta has streamed: the clarifying
	// question exists in full, and think() is parked at the held speaker.
	// Waiting for it makes the committed partial deterministic, not raced.
	h.waitFor(t, "assistant.finished")

	// The user pushes to talk to answer — the interruption itself.
	h.provider.Response = "You have one meeting, at ten."
	second, err := h.engine.StartSession()
	if err != nil {
		t.Fatal(err)
	}
	ev := h.waitFor(t, "session.cancelled")
	if ev.Data["session_id"] != first {
		t.Fatalf("cancelled %v, want %s", ev.Data["session_id"], first)
	}
	h.tts.SetHold(nil)
	if err := h.engine.Submit("tomorrow"); err != nil {
		t.Fatal(err)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	// The answering session's model context carries the whole pending
	// exchange: the question, the interrupted clarifying question (marked),
	// then the user's answer.
	msgs := h.provider.LastRequest.Messages
	if len(msgs) != 3 {
		t.Fatalf("second session sent %d messages, want 3 (question, marked clarification, answer): %s",
			len(msgs), requestContents(h.provider.LastRequest))
	}
	if msgs[0].Role != ai.RoleUser || msgs[0].Content != "what's on my calendar tomorrow?" {
		t.Errorf("context lost the interrupted question: %+v", msgs[0])
	}
	wantClarify := "Do you mean tomorrow, or the whole week?\n" + interruptedMidAnswer
	if msgs[1].Role != ai.RoleAssistant || msgs[1].Content != wantClarify {
		t.Errorf("context assistant half = %q, want %q", msgs[1].Content, wantClarify)
	}
	if msgs[2].Role != ai.RoleUser || msgs[2].Content != "tomorrow" {
		t.Errorf("answer turn = %+v", msgs[2])
	}
	// The session ids matter to the shape: the answer ran in a new session.
	if first == second {
		t.Error("interruption did not start a new session")
	}

	// And the archive retains the interrupted exchange, flagged, in the same
	// conversation as the completed one that followed.
	h.engine.SyncArchive()
	id := h.engine.ActiveConversationID()
	turns := fake.Turns(id)
	if len(turns) != 4 {
		t.Fatalf("archive holds %d turns, want 4: %+v", len(turns), turns)
	}
	if !turns[0].Interrupted || !turns[1].Interrupted {
		t.Errorf("interrupted exchange not flagged: %+v", turns[:2])
	}
	if turns[1].Text != wantClarify {
		t.Errorf("archived clarification = %q, want %q", turns[1].Text, wantClarify)
	}
	if turns[2].Interrupted || turns[3].Interrupted {
		t.Errorf("completed exchange wrongly flagged: %+v", turns[2:])
	}
}

// parkedFirstCall is a provider whose first Chat call streams nothing and
// ends only with its session — the deterministic "interrupted before any
// token" posture — and answers as the embedded Fake from then on. Its own
// type rather than a tweaked Fake field, because an interrupted session's
// think() can still be draining when the next session's Chat opens, and
// mutating a shared Fake between the two is exactly the data race the
// -race run exists to catch.
//
// parked is closed when the parked branch is entered, which is the only
// barrier that says the *first* session claimed it. Waiting for an event
// instead cannot: assistant.started is published before think() opens the
// provider request, so a first session descheduled between the two lets the
// second session take the parked branch and answer nothing (#215). A channel
// field on a fake is the sanctioned pattern for this — a send is not a data
// race, which is why testdiscipline's fake-field rule exempts them.
type parkedFirstCall struct {
	ai.Fake
	calls  atomic.Int32
	parked chan struct{}
}

func (p *parkedFirstCall) Chat(ctx context.Context, req ai.ChatRequest) (<-chan ai.Event, error) {
	if p.calls.Add(1) > 1 {
		return p.Fake.Chat(ctx, req)
	}
	ch := make(chan ai.Event, 1)
	// Exactly one caller can see Add return 1, so this closes exactly once.
	close(p.parked)
	go func() {
		defer close(ch)
		<-ctx.Done()
		ch <- ai.Event{Type: ai.EventError, Err: ctx.Err()}
	}()
	return ch, nil
}

// An interruption before the model has produced a single token still commits
// the user's turn: the assistant half is the annotation alone, because the
// history's user/assistant pairing must hold and providers reject empty
// messages.
func TestInterruptBeforeAnyAnswerCommitsTheQuestion(t *testing.T) {
	h := newHarness(t, Options{})
	provider := &parkedFirstCall{
		Fake:   ai.Fake{Response: "Tomorrow looks clear."},
		parked: make(chan struct{}),
	}
	// Rebuild the engine around the parked provider (the TestToolCallLoop
	// pattern for a harness with a non-default collaborator). The harness
	// registered its cleanup against the subscription it made, so release that
	// one here and register this one, or the replacement outlives the test.
	h.cancel()
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	t.Cleanup(h.cancel)
	h.engine = NewEngine(provider, h.stt, h.tts, h.recorder, h.player, nil, nil, bus, nil,
		Options{Model: "test-model", HistoryTurns: 8})

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("what's the weather like?")
	// The barrier is the park itself, not an event: this says the first
	// session's request reached the provider and is the call holding the parked
	// branch. assistant.started would only say think() got as far as publishing
	// it, which is before the request is opened — and a first session
	// descheduled there hands the parked branch to the second session, which
	// then answers nothing and the wait below times out (#215).
	<-provider.parked

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "session.cancelled")

	if err := h.engine.Submit("and tomorrow?"); err != nil {
		t.Fatal(err)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	msgs := provider.LastRequest.Messages
	if len(msgs) != 3 {
		t.Fatalf("follow-up sent %d messages, want 3: %s", len(msgs), requestContents(provider.LastRequest))
	}
	if msgs[0].Content != "what's the weather like?" {
		t.Errorf("interrupted question missing: %+v", msgs[0])
	}
	if msgs[1].Role != ai.RoleAssistant || msgs[1].Content != interruptedNoAnswer {
		t.Errorf("assistant half = %q, want %q", msgs[1].Content, interruptedNoAnswer)
	}
}

// `jarvix cancel` is a cancellation like any other: the exchange in flight is
// committed marked interrupted, and the next session has it as context.
func TestCancelCommitsTheInterruptedExchange(t *testing.T) {
	h := newHarness(t, Options{Model: "test-model", HistoryTurns: 8})
	h.provider.Delay = time.Hour
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("summarise my inbox")
	h.waitFor(t, "assistant.started")
	if err := h.engine.Cancel(); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "session.cancelled")
	h.waitIdle(t)

	turns := h.engine.Conversation()
	if len(turns) != 2 {
		t.Fatalf("conversation = %+v, want the committed interrupted exchange", turns)
	}
	if turns[0].Text != "summarise my inbox" || turns[1].Text != interruptedNoAnswer {
		t.Errorf("committed exchange = %+v", turns)
	}
}

// The stop word ends the turn through CancelSpeech, not the cancel path — and
// a stopped answer is an interrupted exchange too: question and partial
// answer survive, or a follow-up to "stop" meets the incident's amnesia.
func TestStopSpeechCommitsTheInterruptedExchange(t *testing.T) {
	h := newHarness(t, Options{Model: "test-model", HistoryTurns: 8, SpeakResponses: true})
	hold := make(chan struct{})
	h.tts.SetHold(hold)
	defer close(hold)
	h.provider.Response = "A very long answer the user has heard enough of."
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("tell me everything")
	h.waitFor(t, "assistant.finished")

	if !h.engine.CancelSpeech() {
		t.Fatal("CancelSpeech reported nothing playing while speech was held")
	}
	h.waitFor(t, "session.finished")
	h.waitIdle(t)

	turns := h.engine.Conversation()
	if len(turns) != 2 {
		t.Fatalf("conversation = %+v, want the stopped exchange committed", turns)
	}
	want := "A very long answer the user has heard enough of.\n" + interruptedMidAnswer
	if turns[0].Text != "tell me everything" || turns[1].Text != want {
		t.Errorf("committed exchange = %+v, want assistant %q", turns, want)
	}
}

// A session cancelled before it ever had a transcript has no exchange to
// keep: nothing is committed, nothing is staged, nothing is written.
func TestCancelWithoutATranscriptCommitsNothing(t *testing.T) {
	fake := conversations.NewFake()
	h := newHarness(t, Options{Model: "test-model", HistoryTurns: 8, Archive: fake})
	_, _ = h.engine.StartSession()
	_ = h.engine.StartVoice()
	h.waitFor(t, "recording.started")
	if err := h.engine.Cancel(); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "session.cancelled")
	h.waitIdle(t)

	if turns := h.engine.Conversation(); len(turns) != 0 {
		t.Errorf("conversation = %+v, want empty", turns)
	}
	h.engine.SyncArchive()
	if n := fake.Appends(); n != 0 {
		t.Errorf("archive saw %d appends for a transcript-less cancel", n)
	}
}

// The #116 read-barrier guarantee holds for interrupted commits: the staging
// happens before session.cancelled is published, so a read that runs behind
// SyncArchive immediately after the interrupt already finds the exchange —
// no waiting on the (asynchronous) tail flush. This is the mechanism the
// daemon's conversation.* reads rely on, pinned at the engine seam.
func TestInterruptedCommitIsVisibleThroughTheSyncBarrier(t *testing.T) {
	fake := conversations.NewFake()
	h := newHarness(t, Options{Model: "test-model", HistoryTurns: 8, Archive: fake})
	h.provider.Delay = time.Hour
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("the question the barrier must not lose")
	h.waitFor(t, "assistant.started")
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}

	// StartSession has returned, so the interrupted commit is staged: the
	// barrier must surface it now, however far behind the tail flush is.
	h.engine.SyncArchive()
	id := h.engine.ActiveConversationID()
	if id == "" {
		t.Fatal("no active conversation after the barrier flushed the interrupted commit")
	}
	turns := fake.Turns(id)
	if len(turns) != 2 || !turns[0].Interrupted {
		t.Fatalf("archive after barrier = %+v, want the flagged interrupted exchange", turns)
	}
	if turns[0].Text != "the question the barrier must not lose" {
		t.Errorf("archived question = %q", turns[0].Text)
	}
	// Unwind the still-open second session.
	_ = h.engine.Cancel()
	h.waitIdle(t)
}

// NewConversation is the explicit end (ADR 0038): a session in flight is
// cancelled and its exchange committed — marked interrupted — into the thread
// being ended, which is archived and detached; the next thread starts clean
// in a fresh conversation.
func TestNewConversationEndsTheThreadAndArchivesTheInterruptedTail(t *testing.T) {
	fake := conversations.NewFake()
	h := newHarness(t, Options{Model: "test-model", HistoryTurns: 8,
		SpeakResponses: true, Archive: fake})
	h.ask(t, "the first exchange")
	// The sync barrier rather than op-counting: NewConversation below flushes
	// the interrupted tail itself, so the Ops channel would carry appends this
	// test cannot attribute deterministically. The barrier makes "flushed and
	// adopted" true by return, whoever did the writing.
	h.engine.SyncArchive()
	first := h.engine.ActiveConversationID()
	if first == "" {
		t.Fatal("no active conversation after the first exchange")
	}

	hold := make(chan struct{})
	h.tts.SetHold(hold)
	defer close(hold)
	h.provider.Response = "An answer cut off by New Chat."
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("a question being answered")
	h.waitFor(t, "assistant.finished")

	h.engine.NewConversation()
	ev := h.waitFor(t, "session.cancelled")
	if reason, _ := ev.Data["reason"].(string); reason != "new conversation" {
		t.Errorf("cancel reason = %q, want %q", reason, "new conversation")
	}
	h.waitFor(t, "conversation.changed")
	h.waitIdle(t)

	// The live head is empty and detached — the fresh thread starts clean.
	if turns := h.engine.Conversation(); len(turns) != 0 {
		t.Errorf("conversation after NewConversation = %+v, want empty", turns)
	}
	if got := h.engine.ActiveConversationID(); got != "" {
		t.Errorf("still attached to %q after NewConversation", got)
	}
	// The ended thread holds everything, interrupted tail included, flagged.
	turns := fake.Turns(first)
	if len(turns) != 4 {
		t.Fatalf("ended thread holds %d turns, want 4: %+v", len(turns), turns)
	}
	if turns[2].Text != "a question being answered" || !turns[2].Interrupted || !turns[3].Interrupted {
		t.Errorf("interrupted tail = %+v", turns[2:])
	}
	if !strings.HasSuffix(turns[3].Text, interruptedMidAnswer) {
		t.Errorf("archived tail answer = %q, want the interrupted annotation", turns[3].Text)
	}

	// And the next exchange lands in a different conversation.
	h.tts.SetHold(nil)
	h.provider.Response = "A fresh answer."
	h.ask(t, "the second thread")
	h.engine.SyncArchive()
	second := h.engine.ActiveConversationID()
	if second == "" || second == first {
		t.Errorf("next thread landed in %q, want a fresh conversation (ended one was %q)", second, first)
	}
	if got := requestContents(h.provider.LastRequest); strings.Contains(got, "a question being answered") {
		t.Errorf("ended thread leaked into the new conversation: %s", got)
	}
}
