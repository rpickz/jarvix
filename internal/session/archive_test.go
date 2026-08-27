package session

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/history"
	"github.com/rpickz/jarvix/internal/intent"
)

// awaitAppend waits for the archive Fake to report a completed append. The
// flush runs after session.finished, off the engine's lock path (like
// persistence, ADR 0011), so a test that reads the archive must wait for the
// write rather than assume it happened by event time.
func awaitAppend(t *testing.T, fake *conversations.Fake) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case op := <-fake.Ops:
			if op == "append" {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for an archive append")
		}
	}
}

// archiveOptions is the smallest configuration with archiving on.
func archiveOptions(archive conversations.Store, historyTurns int) Options {
	return Options{Model: "test-model", HistoryTurns: historyTurns,
		FollowUpWindow: time.Hour, Archive: archive}
}

// The central acceptance criterion: the retention cap governs what the model
// is sent, never what is kept. With a one-turn cap, a three-turn conversation
// still archives all three turns — because commitTurn stages for the archive
// *before* it trims the in-memory head.
func TestArchiveKeepsEveryTurnBeyondTheCap(t *testing.T) {
	fake := conversations.NewFake()
	h := newHarness(t, archiveOptions(fake, 1))

	for _, q := range []string{"question one", "question two", "question three"} {
		h.ask(t, q)
		awaitAppend(t, fake)
	}

	// The model saw only the capped head...
	prompt := requestContents(h.provider.LastRequest)
	if strings.Contains(prompt, "question one") {
		t.Errorf("the cap failed: the oldest turn reached the model: %s", prompt)
	}
	// ...but the archive holds the whole conversation, in order.
	id := h.engine.ActiveConversationID()
	if id == "" {
		t.Fatal("no active archived conversation after three turns")
	}
	turns := fake.Turns(id)
	if len(turns) != 6 {
		t.Fatalf("archive holds %d turns, want all 6", len(turns))
	}
	if turns[0].Text != "question one" || turns[4].Text != "question three" {
		t.Errorf("archive order wrong: first %q, fifth %q", turns[0].Text, turns[4].Text)
	}
	for i, turn := range turns {
		if turn.Time.IsZero() {
			t.Errorf("archived turn %d carries no timestamp", i)
		}
	}
}

// Archiving must run even with in-memory history disabled entirely: the cap
// (zero here) is about the model's context, and the record is about the user.
func TestArchiveRunsWithHistoryDisabled(t *testing.T) {
	fake := conversations.NewFake()
	h := newHarness(t, archiveOptions(fake, 0))
	h.ask(t, "a standalone question")
	awaitAppend(t, fake)

	id := h.engine.ActiveConversationID()
	if turns := fake.Turns(id); len(turns) != 2 {
		t.Fatalf("archive holds %d turns with history_turns=0, want 2", len(turns))
	}
	// The model, meanwhile, saw a standalone turn.
	if n := len(h.provider.LastRequest.Messages); n != 1 {
		t.Errorf("provider got %d messages, want just the question", n)
	}
}

// `jarvix new` archives the thread rather than destroying it: the turns are
// already on disk, so the reset only detaches — and the next thread lands in
// a different conversation.
func TestResetDetachesAndTheNextThreadIsANewConversation(t *testing.T) {
	fake := conversations.NewFake()
	h := newHarness(t, archiveOptions(fake, 8))

	h.ask(t, "the first thread")
	awaitAppend(t, fake)
	first := h.engine.ActiveConversationID()

	h.engine.ResetConversation()
	if got := h.engine.ActiveConversationID(); got != "" {
		t.Fatalf("reset left the engine attached to %q", got)
	}

	h.ask(t, "the second thread")
	awaitAppend(t, fake)
	second := h.engine.ActiveConversationID()
	if second == "" || second == first {
		t.Fatalf("second thread landed in %q, want a fresh conversation (first was %q)", second, first)
	}

	// The archived first thread is intact, and the new one clean.
	if turns := fake.Turns(first); len(turns) != 2 || turns[0].Text != "the first thread" {
		t.Errorf("archived first thread = %+v", turns)
	}
	if turns := fake.Turns(second); len(turns) != 2 || turns[0].Text != "the second thread" {
		t.Errorf("second thread = %+v", turns)
	}
	// And the reset did not leak the old context to the model.
	if got := requestContents(h.provider.LastRequest); strings.Contains(got, "the first thread") {
		t.Errorf("reset thread leaked into the new conversation: %s", got)
	}
}

// notifyingArchive wraps a real Store and signals completed appends — the
// same pattern notifyingStore uses for history, for the same reason: the
// write is post-session, so restart tests must wait for it, never sleep.
type notifyingArchive struct {
	conversations.Store
	appended chan struct{}
}

func notifyingConversations(s conversations.Store) *notifyingArchive {
	return &notifyingArchive{Store: s, appended: make(chan struct{}, 16)}
}

func (n *notifyingArchive) Append(id string, turns []conversations.Turn) (string, error) {
	landed, err := n.Store.Append(id, turns)
	select {
	case n.appended <- struct{}{}:
	default:
	}
	return landed, err
}

func (n *notifyingArchive) awaitAppend(t *testing.T) {
	t.Helper()
	select {
	case <-n.appended:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the archive write")
	}
}

// A restart in the middle of a conversation keeps appending to the same
// archived record — the `active` pointer plus the persisted head are what tie
// the rebooted engine back to its conversation.
func TestRestartContinuesTheSameArchivedConversation(t *testing.T) {
	dir := t.TempDir()
	archive := notifyingConversations(&conversations.FileStore{Dir: filepath.Join(dir, "conversations")})
	store := notifying(&history.File{Path: filepath.Join(dir, "history.json")})

	h1 := newHarnessWithStore(t, archiveOptions(archive, 8), store)
	h1.ask(t, "before the restart")
	store.awaitSave(t)
	archive.awaitAppend(t)

	// "Restart": a fresh engine over the same disk.
	h2 := newHarnessWithStore(t, archiveOptions(archive, 8), store)
	h2.ask(t, "after the restart")
	archive.awaitAppend(t)

	metas, unreadable, err := archive.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(unreadable) != 0 {
		t.Fatalf("unreadable conversations after restart: %v", unreadable)
	}
	if len(metas) != 1 {
		t.Fatalf("restart forked the conversation: %d archived, want 1", len(metas))
	}
	conv, err := archive.Read(metas[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.Turns) != 4 {
		t.Fatalf("conversation holds %d turns across the restart, want 4", len(conv.Turns))
	}
	if conv.Turns[0].Text != "before the restart" || conv.Turns[2].Text != "after the restart" {
		t.Errorf("turns out of order across restart: %q, %q", conv.Turns[0].Text, conv.Turns[2].Text)
	}
}

// The full `jarvix new` acceptance path against the real file store: archive
// on reset, restart, and the listing still has the whole thread — while the
// new thread starts clean.
func TestNewThenRestartListsTheArchivedConversationIntact(t *testing.T) {
	dir := t.TempDir()
	archive := notifyingConversations(&conversations.FileStore{Dir: filepath.Join(dir, "conversations")})
	store := notifying(&history.File{Path: filepath.Join(dir, "history.json")})

	h1 := newHarnessWithStore(t, archiveOptions(archive, 8), store)
	h1.ask(t, "an exchange worth keeping")
	archive.awaitAppend(t)
	h1.engine.ResetConversation()

	h2 := newHarnessWithStore(t, archiveOptions(archive, 8), store)
	h2.ask(t, "a fresh thread")
	archive.awaitAppend(t)

	if got := requestContents(h2.provider.LastRequest); strings.Contains(got, "worth keeping") {
		t.Errorf("archived thread leaked into the new one: %s", got)
	}
	metas, _, err := archive.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 {
		t.Fatalf("listed %d conversations, want the archived one and the fresh one", len(metas))
	}
	// Newest first: the fresh thread leads, the archived one follows, intact.
	older, err := archive.Read(metas[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(older.Turns) != 2 || older.Turns[0].Text != "an exchange worth keeping" {
		t.Errorf("archived conversation not intact after restart: %+v", older.Turns)
	}
}

// Reopening honours the context budget: the model gets the most recent turns,
// the archive keeps everything, and follow-ups continue the reopened record.
func TestReopenContinuesContextWithinBudget(t *testing.T) {
	fake := conversations.NewFake()
	base := time.Now().Add(-24 * time.Hour)
	archived := []conversations.Turn{
		{Role: "user", Text: "oldest question", Time: base},
		{Role: "assistant", Text: "oldest answer", Time: base},
		{Role: "user", Text: "middle question", Time: base.Add(time.Minute)},
		{Role: "assistant", Text: "middle answer", Time: base.Add(time.Minute)},
		{Role: "user", Text: "newest question", Time: base.Add(2 * time.Minute)},
		{Role: "assistant", Text: "newest answer", Time: base.Add(2 * time.Minute)},
	}
	fake.Seed(conversations.Meta{ID: "old-conv", Started: base, LastActive: base.Add(2 * time.Minute)}, archived)

	// Budget: two exchanges. The daemon hands the engine the full record; the
	// engine keeps what fits.
	h := newHarness(t, archiveOptions(fake, 2))
	msgs := make([]ai.Message, 0, len(archived))
	for _, turn := range archived {
		msgs = append(msgs, ai.Message{Role: ai.Role(turn.Role), Content: turn.Text})
	}
	h.engine.AdoptConversation("old-conv", msgs, nil)

	h.ask(t, "a follow-up")
	awaitAppend(t, fake)

	got := requestContents(h.provider.LastRequest)
	if !strings.Contains(got, "newest question") || !strings.Contains(got, "middle question") {
		t.Errorf("reopened context missing recent turns: %s", got)
	}
	if strings.Contains(got, "oldest question") {
		t.Errorf("reopened context exceeded the budget: %s", got)
	}
	// The follow-up extended the *reopened* conversation, not a new one.
	turns := fake.Turns("old-conv")
	if len(turns) != 8 {
		t.Fatalf("reopened conversation holds %d turns, want 8", len(turns))
	}
	if turns[6].Text != "a follow-up" {
		t.Errorf("follow-up did not append to the reopened record: %q", turns[6].Text)
	}
	// Reopening is an explicit act: a day-old last-active must not trip the
	// follow-up window on the very next question (the context reached the
	// provider above, which is the proof).
}

// With retention off the engine has no archive at all: nothing staged,
// nothing written, and `jarvix new` behaves exactly as before the archive
// existed. The daemon tests prove the state directory stays empty end to end;
// here the guarantee is that no archive call can even be attempted.
func TestNilArchiveNeverWrites(t *testing.T) {
	fake := conversations.NewFake()
	h := newHarness(t, Options{Model: "test-model", HistoryTurns: 8, FollowUpWindow: time.Hour})
	h.ask(t, "an unrecorded exchange")
	h.engine.ResetConversation()
	h.ask(t, "another")

	// The fake was never handed to the engine — Options.Archive is nil — so
	// the assertion is on the engine's own state: no conversation id, ever.
	if id := h.engine.ActiveConversationID(); id != "" {
		t.Errorf("retention off but the engine attached to conversation %q", id)
	}
	if n := fake.Appends(); n != 0 {
		t.Errorf("archive saw %d appends with retention off", n)
	}
}

// gatedArchive holds an archive append open across a shutdown, mirroring
// gatedStore for history: the drain must wait for the flush in flight.
func gatedArchive(t *testing.T) (fake *conversations.Fake, release func()) {
	t.Helper()
	fake = conversations.NewFake()
	fake.AppendStarted = make(chan struct{})
	gate := make(chan struct{})
	fake.AppendGate = gate
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return fake, release
}

// Archiving rides the shutdown-drained path (#29): a restart landing between
// session.finished and the archive flush loses nothing, because Shutdown
// waits for the tail the flush runs on.
func TestShutdownWaitsForThePendingArchiveWrite(t *testing.T) {
	fake, release := gatedArchive(t)
	h := newHarness(t, archiveOptions(fake, 8))
	h.ask(t, "the exchange a restart must not lose")

	// Finished, idle, published — and the archive write is in flight, held.
	<-fake.AppendStarted

	done := make(chan error, 1)
	go func() { done <- h.engine.Shutdown(context.Background()) }()
	release()
	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// If Shutdown returned before the flush completed, this reads a hole.
	if n := fake.Appends(); n != 1 {
		t.Errorf("archive saw %d appends by the time Shutdown returned, want 1", n)
	}
	id := h.engine.ActiveConversationID()
	if turns := fake.Turns(id); len(turns) != 2 {
		t.Errorf("archived %d turns by shutdown, want 2", len(turns))
	}
}

// routineArchiveOptions is archiveOptions for an intent turn: a router that
// knows one routine, the fake runner, and the archive under test — the
// smallest configuration in which runIntent's tail writes the archive (#74).
func routineArchiveOptions(t *testing.T, runner RoutineRunner, archive conversations.Store) Options {
	t.Helper()
	router, err := intent.New(intent.Options{Routines: []intent.RoutinePhrases{
		{Name: "morning setup", Phrases: []string{"morning setup"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	opts := archiveOptions(archive, 8)
	opts.Intents = router
	opts.IntentRunner = &intent.FakeRunner{}
	opts.Routines = runner
	return opts
}

// runRoutineTurn drives one routine phrase through the engine to
// session.finished — which finishLocked publishes only after the engine is
// back in idle, so no polling is needed on top of it.
func runRoutineTurn(t *testing.T, h *harness) {
	t.Helper()
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("morning setup"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "session.finished")
}

// An intent turn's archive flush lives on runIntent's tail exactly as a model
// turn's lives on think()'s — and it rides the same shutdown drain. This is
// #74: the routine/capture path spawned that tail untracked, so a daemon stop
// could race the archive append it still owed and lose the exchange.
//
// The bounded assertion doubles as the mutation check: spawn runIntent
// outside e.active again and the expired Shutdown returns nil with nothing
// in flight, failing this test deterministically.
func TestShutdownWaitsForARoutineTurnsArchiveWrite(t *testing.T) {
	fake, release := gatedArchive(t)
	runner := &fakeRoutines{summary: "Morning setup: all five apps placed."}
	h := newHarness(t, routineArchiveOptions(t, runner, fake))
	runRoutineTurn(t, h)

	// Finished, idle, published — and the archive write is in flight, held.
	<-fake.AppendStarted

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.engine.Shutdown(expired); err == nil {
		t.Fatal("Shutdown returned nil with a routine turn's archive write still in flight")
	}
	if n := h.engine.InFlight(); n != 1 {
		t.Errorf("InFlight = %d, want the 1 parked archive flush", n)
	}

	done := make(chan error, 1)
	go func() { done <- h.engine.Shutdown(context.Background()) }()
	release()
	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// If Shutdown returned before the flush completed, this reads a hole.
	if n := fake.Appends(); n != 1 {
		t.Errorf("archive saw %d appends by the time Shutdown returned, want 1", n)
	}
	id := h.engine.ActiveConversationID()
	if turns := fake.Turns(id); len(turns) != 2 {
		t.Errorf("archived %d turns by shutdown, want the routine exchange's 2", len(turns))
	}
}

// The rebuilt-engine case of the same invariant — #74's exact shape: a layout
// capture's deferred reload rebuilds the engine's collaborators between turns
// (#62), and the routine then runs on the rebuilt engine. Reconfigure swaps
// options in place, so the swapped-in archive's write must be tracked by the
// same drain as the original's — an engine rebuild must never orphan
// post-session work (#29's invariant, extended).
func TestRebuiltEngineArchiveWriteStillDrainsOnShutdown(t *testing.T) {
	fake, release := gatedArchive(t)
	runner := &fakeRoutines{summary: "Morning setup: all five apps placed."}
	// Boot with one archive, then rebuild onto the gated one — the swap a
	// config reload performs after a capture turn.
	h := newHarness(t, routineArchiveOptions(t, runner, conversations.NewFake()))
	if err := h.engine.Reconfigure(h.provider, h.stt, h.tts, h.recorder, h.player,
		routineArchiveOptions(t, runner, fake)); err != nil {
		t.Fatal(err)
	}
	runRoutineTurn(t, h)

	<-fake.AppendStarted // the rebuilt engine's flush is in flight, held

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.engine.Shutdown(expired); err == nil {
		t.Fatal("Shutdown returned nil with the rebuilt engine's archive write still in flight")
	}
	if n := h.engine.InFlight(); n != 1 {
		t.Errorf("InFlight = %d, want the 1 parked archive flush", n)
	}

	done := make(chan error, 1)
	go func() { done <- h.engine.Shutdown(context.Background()) }()
	release()
	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if n := fake.Appends(); n != 1 {
		t.Errorf("the rebuilt engine's archive saw %d appends by the time Shutdown returned, want 1", n)
	}
	id := h.engine.ActiveConversationID()
	if turns := fake.Turns(id); len(turns) != 2 {
		t.Errorf("archived %d turns by shutdown, want the routine exchange's 2", len(turns))
	}
}

// A failing archive degrades exactly as failing persistence does: one
// warning, then in-memory-only — the conversation itself never suffers.
func TestArchiveFailureDegradesQuietly(t *testing.T) {
	fake := conversations.NewFake()
	fake.AppendErr = errors.New("disk full")
	h := newHarness(t, archiveOptions(fake, 8))

	h.ask(t, "first question")
	awaitAppend(t, fake) // attempted, failed
	h.ask(t, "second question")

	// The conversation still works, context intact...
	got := requestContents(h.provider.LastRequest)
	if !strings.Contains(got, "first question") {
		t.Errorf("in-memory context lost after a failed archive write: %s", got)
	}
	// ...and the engine stopped hammering the broken archive after one try.
	if n := fake.Appends(); n != 1 {
		t.Errorf("archive saw %d append attempts after a failure, want 1", n)
	}
}
