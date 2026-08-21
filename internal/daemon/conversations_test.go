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
