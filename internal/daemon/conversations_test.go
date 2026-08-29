package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/history"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// notifyingConvStore wraps the real file store and signals completed appends,
// so a socket test can wait for the post-session archive write instead of
// sleeping — the notifying-fake pattern from internal/history, over the real
// files because these tests inspect the state directory.
type notifyingConvStore struct {
	conversations.Store
	appended chan struct{}
}

func (n *notifyingConvStore) Append(id string, turns []conversations.Turn) (string, error) {
	landed, err := n.Store.Append(id, turns)
	select {
	case n.appended <- struct{}{}:
	default:
	}
	return landed, err
}

func (n *notifyingConvStore) awaitAppend(t *testing.T) {
	t.Helper()
	select {
	case <-n.appended:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the archive write")
	}
}

// convFixture is a daemon over a real Unix socket and a real archive
// directory, with everything else faked.
type convFixture struct {
	client   *ipc.Client
	provider *ai.Fake
	store    *notifyingConvStore
	dir      string // the conversations directory on disk
}

// startConvDaemon wires a daemon with the given retention setting over a real
// archive directory under the test's state dir.
func startConvDaemon(t *testing.T, retention string) *convFixture {
	t.Helper()
	return startConvDaemonWith(t, retention, nil)
}

// startConvDaemonWith is startConvDaemon with an injected history store, for
// the test that needs to hold the session tail open at the history write —
// the step *before* the archive flush — and observe the archive mid-gap.
func startConvDaemonWith(t *testing.T, retention string, hist history.Store) *convFixture {
	t.Helper()
	return startConvDaemonSpeaking(t, retention, hist, &tts.Fake{})
}

// startConvDaemonSpeaking additionally injects the synthesizer, for the
// interruption tests: a tts.Fake with a hold gate is how a session is kept
// deterministically mid-speech — the incident's exact posture — while the
// socket interrupts it (issue #117).
func startConvDaemonSpeaking(t *testing.T, retention string, hist history.Store, synth *tts.Fake) *convFixture {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config:  dir,
		Data:    dir,
		State:   dir,
		Runtime: dir,
		Socket:  filepath.Join(dir, "j.sock"),
	}
	cfg := testConfig()
	cfg.Audio.MinRecordingMs = 0
	cfg.Conversation.Retention = retention
	provider := &ai.Fake{Response: "An archived answer."}
	store := &notifyingConvStore{
		Store:    &conversations.FileStore{Dir: paths.ConversationsDir()},
		appended: make(chan struct{}, 16),
	}
	d, err := New(cfg, paths, nil, Deps{
		Provider:          provider,
		Transcriber:       &stt.Fake{Text: "hello computer"},
		Synthesizer:       synth,
		Recorder:          &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:            &audio.FakePlayer{},
		Notifier:          &desktop.FakeNotifier{},
		OpenWindow:        func(context.Context) error { return nil },
		Compositor:        desktop.NewFakeCompositor(),
		ConversationStore: store,
		HistoryStore:      hist,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	return &convFixture{
		client:   dialDaemon(t, paths.Socket),
		provider: provider,
		store:    store,
		dir:      paths.ConversationsDir(),
	}
}

// ask drives one text exchange over the socket and waits for the archive
// write behind it.
func (f *convFixture) ask(t *testing.T, text string) {
	t.Helper()
	if err := f.client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Call("session.submit", map[string]string{"text": text}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, f.client, "session.finished")
	f.store.awaitAppend(t)
}

// listing is the conversation.list wire shape these tests read.
type listing struct {
	Retention     bool   `json:"retention"`
	ActiveID      string `json:"active_id"`
	Conversations []struct {
		ID      string `json:"id"`
		Turns   int    `json:"turns"`
		Preview string `json:"preview"`
	} `json:"conversations"`
	Unreadable []struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	} `json:"unreadable"`
}

func (f *convFixture) list(t *testing.T) listing {
	t.Helper()
	var l listing
	if err := f.client.Call("conversation.list", nil, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestConversationListOverSocket(t *testing.T) {
	f := startConvDaemon(t, config.RetentionOn)
	f.ask(t, "the first conversation")
	if err := f.client.Call("conversation.reset", nil, nil); err != nil {
		t.Fatal(err)
	}
	f.ask(t, "the second conversation")

	l := f.list(t)
	if !l.Retention {
		t.Error("listing reports retention off")
	}
	if len(l.Conversations) != 2 {
		t.Fatalf("listed %d conversations, want 2", len(l.Conversations))
	}
	// Newest first, with the metadata a listing promises.
	if l.Conversations[0].Preview != "the second conversation" ||
		l.Conversations[1].Preview != "the first conversation" {
		t.Errorf("listing order/previews wrong: %+v", l.Conversations)
	}
	for _, c := range l.Conversations {
		if c.Turns != 2 || c.ID == "" {
			t.Errorf("conversation entry incomplete: %+v", c)
		}
	}
	if l.ActiveID != l.Conversations[0].ID {
		t.Errorf("active_id = %q, want the newest conversation %q", l.ActiveID, l.Conversations[0].ID)
	}
}

func TestConversationReadIsReadOnly(t *testing.T) {
	f := startConvDaemon(t, config.RetentionOn)
	f.ask(t, "something to reread")
	id := f.list(t).Conversations[0].ID

	var conv struct {
		ID    string `json:"id"`
		Turns []struct {
			Role string `json:"role"`
			Text string `json:"text"`
			TS   string `json:"ts"`
		} `json:"turns"`
	}
	if err := f.client.Call("conversation.read", map[string]string{"id": id}, &conv); err != nil {
		t.Fatal(err)
	}
	if len(conv.Turns) != 2 {
		t.Fatalf("read %d turns, want 2", len(conv.Turns))
	}
	if conv.Turns[0].Role != "user" || conv.Turns[0].Text != "something to reread" {
		t.Errorf("first turn = %+v", conv.Turns[0])
	}
	if conv.Turns[0].TS == "" {
		t.Error("turn carries no timestamp")
	}
	// Reading changed nothing: same active thread, same listing.
	if got := f.list(t); got.ActiveID != id || len(got.Conversations) != 1 {
		t.Errorf("a read-only read changed the listing: %+v", got)
	}
}

func TestConversationOpenContinuesTheThread(t *testing.T) {
	f := startConvDaemon(t, config.RetentionOn)
	f.ask(t, "the archived question")
	archived := f.list(t).Conversations[0].ID
	if err := f.client.Call("conversation.reset", nil, nil); err != nil {
		t.Fatal(err)
	}
	f.ask(t, "an unrelated thread")

	var opened struct {
		ID    string `json:"id"`
		Turns int    `json:"turns"`
	}
	if err := f.client.Call("conversation.open", map[string]string{"id": archived}, &opened); err != nil {
		t.Fatal(err)
	}
	if opened.ID != archived || opened.Turns != 2 {
		t.Errorf("open reported %+v", opened)
	}

	// A follow-up continues the reopened conversation with its context...
	f.ask(t, "and a follow-up")
	got := ""
	for _, m := range f.provider.LastRequest.Messages {
		got += string(m.Role) + ":" + m.Content + " | "
	}
	if !strings.Contains(got, "the archived question") {
		t.Errorf("follow-up after reopen lost the archived context: %s", got)
	}
	if strings.Contains(got, "unrelated thread") {
		t.Errorf("the other thread leaked into the reopened one: %s", got)
	}
	// ...and lands in the reopened record on disk.
	l := f.list(t)
	if l.ActiveID != archived {
		t.Errorf("active thread = %q after reopen, want %q", l.ActiveID, archived)
	}
	for _, c := range l.Conversations {
		if c.ID == archived && c.Turns != 4 {
			t.Errorf("reopened conversation holds %d turns, want 4", c.Turns)
		}
	}
}

func TestConversationDeleteRemovesFromDiskAndListing(t *testing.T) {
	f := startConvDaemon(t, config.RetentionOn)
	f.ask(t, "delete this thread")
	id := f.list(t).Conversations[0].ID

	var result struct {
		Deleted int `json:"deleted"`
	}
	if err := f.client.Call("conversation.delete", map[string]string{"id": id}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", result.Deleted)
	}
	// Proven on the state directory: nothing of the conversation remains.
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), id) {
			t.Errorf("deletion left %s on disk", entry.Name())
		}
	}
	l := f.list(t)
	if len(l.Conversations) != 0 || len(l.Unreadable) != 0 {
		t.Errorf("deleted conversation still listed: %+v", l)
	}
	// Deleting the active conversation also reset the live thread, so the
	// next turn cannot rebuild the record from working memory.
	if l.ActiveID != "" {
		t.Errorf("active thread survived its own deletion: %q", l.ActiveID)
	}
	f.ask(t, "a brand new question")
	got := ""
	for _, m := range f.provider.LastRequest.Messages {
		got += m.Content + " | "
	}
	if strings.Contains(got, "delete this thread") {
		t.Errorf("deleted conversation lingered in the model context: %s", got)
	}
}

func TestConversationDeleteAll(t *testing.T) {
	f := startConvDaemon(t, config.RetentionOn)
	f.ask(t, "one")
	if err := f.client.Call("conversation.reset", nil, nil); err != nil {
		t.Fatal(err)
	}
	f.ask(t, "two")

	var result struct {
		Deleted int `json:"deleted"`
	}
	if err := f.client.Call("conversation.delete", map[string]bool{"all": true}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 2 {
		t.Errorf("deleted = %d, want 2", result.Deleted)
	}
	entries, err := os.ReadDir(f.dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("delete --all left %d entries in the state directory", len(entries))
	}
}

func TestRetentionOffArchivesNothing(t *testing.T) {
	f := startConvDaemon(t, config.RetentionOff)
	// ask() waits for an archive write that must never come; drive the
	// exchange directly instead.
	if err := f.client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Call("session.submit", map[string]string{"text": "off the record"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, f.client, "session.finished")
	// `jarvix new` behaves as today: reset succeeds, nothing is archived.
	if err := f.client.Call("conversation.reset", nil, nil); err != nil {
		t.Fatal(err)
	}
	// The reset's own drain has run by the time it returns; the state
	// directory is the proof that nothing was ever written.
	if _, err := os.Stat(f.dir); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(f.dir)
		t.Fatalf("retention off but the archive directory exists with %d entries", len(entries))
	}
	l := f.list(t)
	if l.Retention {
		t.Error("listing claims retention is on")
	}
	if len(l.Conversations) != 0 {
		t.Errorf("retention off but %d conversations archived", len(l.Conversations))
	}
}

func TestCorruptConversationIsReportedAndTheRestList(t *testing.T) {
	f := startConvDaemon(t, config.RetentionOn)
	f.ask(t, "the survivor")
	if err := f.client.Call("conversation.reset", nil, nil); err != nil {
		t.Fatal(err)
	}
	f.ask(t, "the casualty")
	casualty := f.list(t).Conversations[0].ID

	// Corrupt the newest conversation's metadata on disk, as a torn write or
	// a hand edit would.
	if err := os.WriteFile(filepath.Join(f.dir, casualty+".json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	l := f.list(t)
	if len(l.Conversations) != 1 || l.Conversations[0].Preview != "the survivor" {
		t.Fatalf("the corrupt file hid the library: %+v", l.Conversations)
	}
	if len(l.Unreadable) != 1 || l.Unreadable[0].ID != casualty || l.Unreadable[0].Error == "" {
		t.Fatalf("corrupt conversation not reported: %+v", l.Unreadable)
	}
}

func TestConversationMethodsRejectBadParams(t *testing.T) {
	f := startConvDaemon(t, config.RetentionOn)
	for _, method := range []string{"conversation.read", "conversation.open", "conversation.delete"} {
		if err := f.client.Call(method, nil, nil); err == nil {
			t.Errorf("%s accepted empty params", method)
		}
	}
	if err := f.client.Call("conversation.open", map[string]string{"id": "no-such"}, nil); err == nil {
		t.Error("conversation.open accepted an unknown id")
	}
}

// searchReply is the conversation.search wire shape these tests read.
type searchReply struct {
	Retention bool   `json:"retention"`
	ActiveID  string `json:"active_id"`
	Results   []struct {
		ID      string `json:"id"`
		Turn    int    `json:"turn"`
		Role    string `json:"role"`
		TS      string `json:"ts"`
		Passage string `json:"passage"`
		Current bool   `json:"current"`
	} `json:"results"`
	Matched  int `json:"matched"`
	Searched int `json:"searched"`
	Skipped  []struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	} `json:"skipped"`
}

func (f *convFixture) search(t *testing.T, query string) searchReply {
	t.Helper()
	var r searchReply
	if err := f.client.Call("conversation.search", map[string]string{"query": query}, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestConversationSearchOverSocket(t *testing.T) {
	f := startConvDaemon(t, config.RetentionOn)
	f.ask(t, "what did we decide about the deployment approach?")
	archived := f.list(t).Conversations[0].ID
	if err := f.client.Call("conversation.reset", nil, nil); err != nil {
		t.Fatal(err)
	}
	f.ask(t, "an unrelated thread about kittens")

	// A past conversation is found with the references a client needs to
	// open it and land on the turn — and it is not marked current.
	r := f.search(t, "deployment approach")
	if !r.Retention || r.Searched != 2 {
		t.Fatalf("search reply = %+v, want retention on over 2 conversations", r)
	}
	if len(r.Results) != 1 {
		t.Fatalf("results = %+v, want the one archived hit", r.Results)
	}
	hit := r.Results[0]
	if hit.ID != archived || hit.Turn != 1 || hit.Role != "user" || hit.TS == "" {
		t.Errorf("hit = %+v, want conversation %s turn 1 by user with a timestamp", hit, archived)
	}
	if !strings.Contains(hit.Passage, "deployment approach") {
		t.Errorf("passage lost the match: %q", hit.Passage)
	}
	if hit.Current {
		t.Error("an archived conversation is marked as the current one")
	}

	// The live head is part of the corpus, and its hits say so.
	r = f.search(t, "kittens")
	if len(r.Results) != 1 || !r.Results[0].Current {
		t.Fatalf("live-head search = %+v, want one current hit", r.Results)
	}
	if r.Results[0].ID != r.ActiveID {
		t.Errorf("current hit in %q but active is %q", r.Results[0].ID, r.ActiveID)
	}

	// No matches is an empty result, not an error.
	r = f.search(t, "completely absent words")
	if len(r.Results) != 0 || r.Searched != 2 {
		t.Errorf("no-match reply = %+v", r)
	}

	// A query is required.
	if err := f.client.Call("conversation.search", nil, nil); err == nil {
		t.Error("conversation.search accepted empty params")
	}
	if err := f.client.Call("conversation.search", map[string]string{"query": "  "}, nil); err == nil {
		t.Error("conversation.search accepted a blank query")
	}
}

// TestConversationSearchSeesTheAcknowledgedTurn pins the ordering guarantee
// issue #115 is about: a turn acknowledged on the socket — session.finished
// published — is visible to conversation.search and conversation.list, as
// the *current* conversation, however slowly the session tail runs.
//
// The mechanism under test: the archive flush runs on the session tail after
// session.finished (ADR 0011), and the append that creates a conversation is
// also what adopts its id as the live thread's. A read landing in that gap
// used to miss the just-finished exchange, or find it with active_id still
// "" — the TestConversationSearchOverSocket / TestConversationListOverSocket
// CI flakes, which only a starved runner's scheduling could open wide enough
// to see. This test opens the gap deterministically instead: the history
// write sits on the same tail *before* the archive flush, so a gated history
// Save (the shutdown-drain tests' idiom) parks the tail with the exchange
// committed and acknowledged but nothing yet in the archive. The daemon's
// read-side barrier (Engine.SyncArchive) must do the flush itself before
// answering; without it, this fails every run — no stress, no sleeps.
func TestConversationSearchSeesTheAcknowledgedTurn(t *testing.T) {
	hist := history.NewFake()
	// Buffered, so a Save's start-announcement never blocks the tail if an
	// assertion fails before the test gets to receive it.
	hist.SaveStarted = make(chan struct{}, 4)
	gate := make(chan struct{})
	hist.SaveGate = gate
	f := startConvDaemonWith(t, config.RetentionOn, hist)
	// If the test fails while the tail is parked at the gate, the daemon's
	// shutdown drain would wait on that Save forever. Cleanups run
	// last-registered-first, so this release runs before serveDaemon's stop.
	t.Cleanup(func() { close(gate) })
	awaitSaveParked := func() {
		t.Helper()
		select {
		case <-hist.SaveStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for the history write to start")
		}
	}

	// One archived conversation first, its tail serviced promptly, so the
	// search below runs over a corpus and not just the live head.
	if err := f.client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Call("session.submit", map[string]string{"text": "what did we decide about the deployment approach?"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, f.client, "session.finished")
	awaitSaveParked()
	gate <- struct{}{}
	f.store.awaitAppend(t)
	if err := f.client.Call("conversation.reset", nil, nil); err != nil {
		t.Fatal(err)
	}

	// The pinch: the kittens exchange finishes on the socket, then its tail
	// is held at the history write. The exchange is staged, session.finished
	// has been seen, and the archive knows nothing of it — exactly the state
	// a starved CI runner kept catching.
	if err := f.client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Call("session.submit", map[string]string{"text": "an unrelated thread about kittens"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, f.client, "session.finished")
	awaitSaveParked()

	// Search must see the acknowledged turn, in the current conversation.
	r := f.search(t, "kittens")
	if r.Searched != 2 {
		t.Errorf("searched %d conversations, want the archive and the live head", r.Searched)
	}
	if len(r.Results) != 1 || !r.Results[0].Current {
		t.Fatalf("post-acknowledgement search = %+v, want one current hit", r.Results)
	}
	if r.ActiveID == "" || r.Results[0].ID != r.ActiveID {
		t.Errorf("current hit in %q but active is %q", r.Results[0].ID, r.ActiveID)
	}

	// And the listing must agree — the sibling active_id flake's assertion,
	// made deterministic by the same held-open gap.
	l := f.list(t)
	if len(l.Conversations) != 2 {
		t.Fatalf("listed %d conversations, want 2", len(l.Conversations))
	}
	if l.Conversations[0].Preview != "an unrelated thread about kittens" || l.Conversations[0].Turns != 2 {
		t.Errorf("newest listing entry = %+v, want the just-acknowledged exchange", l.Conversations[0])
	}
	if l.ActiveID != l.Conversations[0].ID {
		t.Errorf("active_id = %q, want the newest conversation %q", l.ActiveID, l.Conversations[0].ID)
	}

	// Release the tail: its own flush finds nothing pending (the barrier
	// already wrote it) and the daemon shuts down clean.
	gate <- struct{}{}
}

func TestConversationSearchWithRetentionOff(t *testing.T) {
	f := startConvDaemon(t, config.RetentionOff)
	r := f.search(t, "anything at all")
	if r.Retention {
		t.Error("search claims retention is on")
	}
	if r.Searched != 0 || len(r.Results) != 0 || len(r.Skipped) != 0 {
		t.Errorf("empty archive search = %+v, want nothing searched and nothing broken", r)
	}
}

func TestConversationSearchToolRegisteredAsAllow(t *testing.T) {
	f := startConvDaemon(t, config.RetentionOn)
	var status struct {
		Policy struct {
			Tools map[string]string `json:"tools"`
		} `json:"policy"`
		Conversations struct {
			Retention bool   `json:"retention"`
			Archived  int    `json:"archived"`
			Search    string `json:"search"`
		} `json:"conversations"`
	}
	if err := f.client.Call("status.get", nil, &status); err != nil {
		t.Fatal(err)
	}
	// The tool is offered to the model and gated as a read: allow, like
	// desktop.list_windows.
	if got := status.Policy.Tools["conversations.search"]; got != "allow" {
		t.Errorf("conversations.search tier = %q, want allow", got)
	}
	// Status reports search as a state: active here, since retention is on.
	if status.Conversations.Search != "active" || !status.Conversations.Retention {
		t.Errorf("status conversations = %+v, want active with retention on", status.Conversations)
	}
}

func TestStatusReportsSearchInactiveNotBroken(t *testing.T) {
	f := startConvDaemon(t, config.RetentionOff)
	var status struct {
		Conversations struct {
			Retention bool   `json:"retention"`
			Archived  int    `json:"archived"`
			Search    string `json:"search"`
		} `json:"conversations"`
	}
	if err := f.client.Call("status.get", nil, &status); err != nil {
		t.Fatal(err)
	}
	if status.Conversations.Search != "inactive" || status.Conversations.Retention ||
		status.Conversations.Archived != 0 {
		t.Errorf("status conversations = %+v, want inactive with retention off", status.Conversations)
	}
}

// The incident over the socket (issue #117): a session interrupted by a new
// session.start — the push-to-talk shape from the daemon log — has its
// exchange committed marked interrupted, and a conversation.search issued the
// moment session.cancelled is seen already finds it. This is the #116
// read-barrier guarantee extended to interrupted commits: the incident's s2
// searched and got results=0; this pins that it now gets the exchange.
func TestInterruptedTurnIsSearchableTheMomentItIsCancelled(t *testing.T) {
	synth := &tts.Fake{}
	hold := make(chan struct{})
	synth.SetHold(hold)
	defer close(hold)
	f := startConvDaemonSpeaking(t, config.RetentionOn, nil, synth)
	f.provider.Response = "Do you mean tomorrow, or the whole week?"

	if err := f.client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Call("session.submit",
		map[string]string{"text": "what's on my calendar tomorrow?"}, nil); err != nil {
		t.Fatal(err)
	}
	// Every delta has streamed and the held speaker keeps the session alive:
	// the interrupt below always lands mid-turn, never after a clean finish.
	waitForEvent(t, f.client, "assistant.finished")

	// The user pushes to talk to answer the clarifying question.
	if err := f.client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, f.client, "session.cancelled")

	// Immediately — no waiting on the tail flush. The daemon's read barrier
	// must surface the interrupted exchange to the same client that just saw
	// it cancelled.
	r := f.search(t, "calendar tomorrow")
	if len(r.Results) == 0 {
		t.Fatal("the interrupted exchange is not searchable — the incident's results=0, reproduced")
	}
	if r.ActiveID == "" {
		t.Error("active_id is empty while the interrupted exchange is on disk")
	}
	hit := r.Results[0]
	if !hit.Current || !strings.Contains(hit.Passage, "calendar") {
		t.Errorf("hit = %+v, want a current hit on the interrupted question", hit)
	}

	// The record says the exchange was cut, not completed: both halves carry
	// the wire flag, and the assistant half ends in the annotation.
	var read struct {
		Turns []struct {
			Role        string `json:"role"`
			Text        string `json:"text"`
			Interrupted bool   `json:"interrupted"`
		} `json:"turns"`
	}
	if err := f.client.Call("conversation.read", map[string]string{"id": r.ActiveID}, &read); err != nil {
		t.Fatal(err)
	}
	if len(read.Turns) != 2 || !read.Turns[0].Interrupted || !read.Turns[1].Interrupted {
		t.Fatalf("archived turns = %+v, want both flagged interrupted", read.Turns)
	}
	if !strings.Contains(read.Turns[1].Text, "interrupted") {
		t.Errorf("assistant half carries no annotation: %q", read.Turns[1].Text)
	}
	// Unwind the second session so teardown is quiet.
	if err := f.client.Call("session.cancel", nil, nil); err != nil {
		t.Fatal(err)
	}
}

// conversation.new is the explicit end, over the socket: it cancels the
// session in flight, commits its exchange (marked interrupted) into the
// thread being ended, archives and detaches that thread, and the next
// exchange starts a fresh conversation (ADR 0038). `jarvix new`, the
// window's New chat button, and the bar menu item are all thin clients of
// exactly this call.
func TestConversationNewEndsTheThreadCleanly(t *testing.T) {
	synth := &tts.Fake{}
	hold := make(chan struct{})
	defer close(hold)
	f := startConvDaemonSpeaking(t, config.RetentionOn, nil, synth)

	f.ask(t, "the first question")
	first := f.list(t).ActiveID
	if first == "" {
		t.Fatal("no active conversation after the first exchange")
	}

	// A second exchange, held mid-speech so New Chat lands on a live session.
	synth.SetHold(hold)
	f.provider.Response = "An answer New Chat cuts off."
	if err := f.client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Call("session.submit",
		map[string]string{"text": "a question new chat interrupts"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, f.client, "assistant.finished")

	if err := f.client.Call("conversation.new", nil, nil); err != nil {
		t.Fatal(err)
	}
	cancelled := waitForEvent(t, f.client, "session.cancelled")
	if reason, _ := cancelled["reason"].(string); reason != "new conversation" {
		t.Errorf("cancel reason = %q, want %q", reason, "new conversation")
	}
	waitForEvent(t, f.client, "conversation.changed")

	// The live head is empty and detached; the ended thread holds everything,
	// interrupted tail included.
	if turns := conversationTurns(t, f.client); len(turns) != 0 {
		t.Errorf("conversation.get after conversation.new = %+v, want empty", turns)
	}
	l := f.list(t)
	if l.ActiveID != "" || len(l.Conversations) != 1 {
		t.Fatalf("listing after conversation.new = %+v, want one detached conversation", l)
	}
	if l.Conversations[0].Turns != 4 {
		t.Errorf("ended thread holds %d turns, want 4 (completed + interrupted)", l.Conversations[0].Turns)
	}
	var read struct {
		Turns []struct {
			Role        string `json:"role"`
			Text        string `json:"text"`
			Interrupted bool   `json:"interrupted"`
		} `json:"turns"`
	}
	if err := f.client.Call("conversation.read", map[string]string{"id": first}, &read); err != nil {
		t.Fatal(err)
	}
	if len(read.Turns) != 4 || read.Turns[0].Interrupted || !read.Turns[2].Interrupted || !read.Turns[3].Interrupted {
		t.Fatalf("ended thread = %+v, want the completed exchange unflagged and the cut one flagged", read.Turns)
	}

	// The next utterance starts a fresh conversation.
	synth.SetHold(nil)
	f.provider.Response = "A fresh answer."
	f.ask(t, "a fresh thread")
	l = f.list(t)
	if len(l.Conversations) != 2 {
		t.Fatalf("listing after the fresh exchange = %+v, want two conversations", l)
	}
	if l.ActiveID == "" || l.ActiveID == first {
		t.Errorf("fresh thread landed in %q, want a new conversation (ended one was %q)", l.ActiveID, first)
	}
}

// ---------------------------------------------------------------------------
// The acknowledged-turn guarantee, for every reader
// ---------------------------------------------------------------------------

// parkingHistory holds exactly one Save open and lets every other one
// through. It is the probe the #115/#116 fix introduced, made reusable
// (issue #173).
//
// The archive flush runs on the session tail after session.finished
// (ADR 0011), and the history write sits on that same tail immediately
// before it. Parking that write therefore parks the tail with the exchange
// committed and acknowledged — session.finished published, the client has
// seen it — and the archive still knowing nothing about it. That is the
// window #115 was, held open on demand instead of waited for on a starved
// runner: on a daemon without the read-side barrier every assertion below
// fails on every run, with no stress, no repetition and no sleeps.
//
// Only the FIRST save after Arm is held, and that is what makes this work
// for conversation.open. Reopening persists the adopted head, so a gate that
// held every save would park the very call it is meant to be testing; the
// one that matters is the tail's, and the ones the reader makes on its own
// behalf must pass.
type parkingHistory struct {
	history.Store
	started chan struct{}
	release chan struct{}
	once    sync.Once

	mu    sync.Mutex
	armed bool
}

func newParkingHistory() *parkingHistory {
	return &parkingHistory{
		Store:   history.NewFake(),
		started: make(chan struct{}, 4),
		release: make(chan struct{}),
	}
}

// Arm holds the next Save until Release.
func (p *parkingHistory) Arm() {
	p.mu.Lock()
	p.armed = true
	p.mu.Unlock()
}

// Release lets the held Save finish. Safe to call more than once, which
// matters because it runs both in the test and from a cleanup — a tail
// parked when an assertion fails would otherwise hold the shutdown drain for
// ever.
func (p *parkingHistory) Release() { p.once.Do(func() { close(p.release) }) }

func (p *parkingHistory) Save(messages []ai.Message, lastTurn time.Time) error {
	p.mu.Lock()
	hold := p.armed
	p.armed = false
	p.mu.Unlock()
	if hold {
		p.started <- struct{}{}
		<-p.release
	}
	return p.Store.Save(messages, lastTurn)
}

// parked is a daemon whose session tail can be held open on demand.
type parked struct {
	*convFixture
	hist *parkingHistory
}

func newParkedDaemon(t *testing.T) *parked {
	t.Helper()
	hist := newParkingHistory()
	f := startConvDaemonWith(t, config.RetentionOn, hist)
	// Cleanups run last-registered-first, so this releases before
	// serveDaemon's stop waits on the drain.
	t.Cleanup(hist.Release)
	return &parked{convFixture: f, hist: hist}
}

// askAndPark drives one exchange to session.finished and leaves its tail
// held at the history write: committed, acknowledged, unarchived.
func (p *parked) askAndPark(t *testing.T, text string) {
	t.Helper()
	p.hist.Arm()
	if err := p.client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := p.client.Call("session.submit", map[string]string{"text": text}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, p.client, "session.finished")
	select {
	case <-p.hist.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the session tail to reach the history write")
	}
}

// TestEveryArchiveReaderSeesTheAcknowledgedTurn is the ticket's criterion in
// full: the read-your-acknowledged-writes guarantee (#116) proven end to end
// for every conversation.* reader, not only the two whose CI flakes provoked
// it. Each reader takes the barrier itself, and each subtest holds the
// window open deterministically rather than hoping for a slow runner.
func TestEveryArchiveReaderSeesTheAcknowledgedTurn(t *testing.T) {
	// list — the sibling flake: the conversation was there and active_id was
	// still "", because the append that creates a conversation is also what
	// adopts its id, and both happen on the tail.
	t.Run("list", func(t *testing.T) {
		p := newParkedDaemon(t)
		p.askAndPark(t, "the question the listing must already know about")

		l := p.list(t)
		if len(l.Conversations) != 1 {
			t.Fatalf("listed %d conversations, want the acknowledged one", len(l.Conversations))
		}
		if l.Conversations[0].Turns != 2 ||
			l.Conversations[0].Preview != "the question the listing must already know about" {
			t.Errorf("listing entry = %+v", l.Conversations[0])
		}
		if l.ActiveID != l.Conversations[0].ID {
			t.Errorf("active_id = %q, want the conversation on disk %q", l.ActiveID, l.Conversations[0].ID)
		}
		p.hist.Release()
	})

	// search — the original: a hit found but current:false, or not found.
	t.Run("search", func(t *testing.T) {
		p := newParkedDaemon(t)
		p.askAndPark(t, "an unrelated thread about kittens")

		r := p.search(t, "kittens")
		if len(r.Results) != 1 || !r.Results[0].Current {
			t.Fatalf("post-acknowledgement search = %+v, want one current hit", r.Results)
		}
		if r.ActiveID == "" || r.Results[0].ID != r.ActiveID {
			t.Errorf("current hit in %q but active is %q", r.Results[0].ID, r.ActiveID)
		}
		if r.Matched != 1 {
			t.Errorf("matched = %d, want the one hit", r.Matched)
		}
		p.hist.Release()
	})

	// read — the transcript view opened the instant a turn finishes must
	// include that turn, or the user watches their own words fail to appear.
	t.Run("read", func(t *testing.T) {
		p := newParkedDaemon(t)
		p.ask(t, "the first question")
		id := p.list(t).ActiveID
		p.askAndPark(t, "the question that must already be readable")

		var conv struct {
			Turns []struct {
				Role string `json:"role"`
				Text string `json:"text"`
			} `json:"turns"`
		}
		if err := p.client.Call("conversation.read", map[string]string{"id": id}, &conv); err != nil {
			t.Fatal(err)
		}
		if len(conv.Turns) != 4 {
			t.Fatalf("read %d turns, want the acknowledged exchange included: %+v", len(conv.Turns), conv.Turns)
		}
		if conv.Turns[2].Text != "the question that must already be readable" {
			t.Errorf("third turn = %+v, want the just-acknowledged question", conv.Turns[2])
		}
		p.hist.Release()
	})

	// open — reopening must adopt the whole record, the just-finished turn
	// included, or the model continues a conversation missing its last
	// exchange.
	t.Run("open", func(t *testing.T) {
		p := newParkedDaemon(t)
		p.ask(t, "the first question")
		id := p.list(t).ActiveID
		p.askAndPark(t, "the question the reopen must carry")

		var opened struct {
			ID    string `json:"id"`
			Turns int    `json:"turns"`
		}
		if err := p.client.Call("conversation.open", map[string]string{"id": id}, &opened); err != nil {
			t.Fatal(err)
		}
		if opened.ID != id || opened.Turns != 4 {
			t.Errorf("reopen = %+v, want %q with all four turns", opened, id)
		}
		p.hist.Release()
	})

	// delete — the one where the barrier's absence is not a display bug.
	// Without it the wipe finds an empty directory, answers "nothing to
	// delete", and the tail then writes the conversation the user just
	// destroyed onto the disk behind them.
	t.Run("delete", func(t *testing.T) {
		p := newParkedDaemon(t)
		p.askAndPark(t, "the conversation the user asks to destroy")

		var result struct {
			Deleted int `json:"deleted"`
		}
		if err := p.client.Call("conversation.delete", map[string]bool{"all": true}, &result); err != nil {
			t.Fatal(err)
		}
		if result.Deleted != 1 {
			t.Fatalf("deleted = %d, want the acknowledged conversation", result.Deleted)
		}
		// The tail runs now, and must find nothing left to write.
		p.hist.Release()
		if err := p.client.Call("conversation.reset", nil, nil); err != nil {
			t.Fatal(err)
		}
		l := p.list(t)
		if len(l.Conversations) != 0 || len(l.Unreadable) != 0 {
			t.Fatalf("a destroyed conversation came back: %+v", l)
		}
		entries, err := os.ReadDir(p.dir)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".jsonl") {
				t.Errorf("the destroyed conversation's transcript is still on disk: %s", entry.Name())
			}
		}
	})
}
