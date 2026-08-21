package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/desktop"
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
		Synthesizer:       &tts.Fake{},
		Recorder:          &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:            &audio.FakePlayer{},
		Notifier:          &desktop.FakeNotifier{},
		OpenWindow:        func(context.Context) error { return nil },
		Compositor:        desktop.NewFakeCompositor(),
		ConversationStore: store,
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
