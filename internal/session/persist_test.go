package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/history"
)

// notifyingStore wraps a Store and signals each completed Save. Persistence
// runs after session.finished, off the engine's lock path, so tests must
// wait for it explicitly rather than assume it happened by event time.
type notifyingStore struct {
	history.Store
	saved chan struct{}
}

func notifying(s history.Store) *notifyingStore {
	return &notifyingStore{Store: s, saved: make(chan struct{}, 16)}
}

func (n *notifyingStore) Save(messages []ai.Message, lastTurn time.Time) error {
	err := n.Store.Save(messages, lastTurn)
	select {
	case n.saved <- struct{}{}:
	default:
	}
	return err
}

func (n *notifyingStore) awaitSave(t *testing.T) {
	t.Helper()
	select {
	case <-n.saved:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the history to be saved")
	}
}

// requestContents flattens a provider request for substring assertions.
func requestContents(req ai.ChatRequest) string {
	var parts []string
	for _, m := range req.Messages {
		parts = append(parts, string(m.Role)+":"+m.Content)
	}
	return strings.Join(parts, " | ")
}

func TestConversationSurvivesRestart(t *testing.T) {
	store := notifying(&history.File{Path: filepath.Join(t.TempDir(), "history.json")})
	opts := Options{HistoryTurns: 8, FollowUpWindow: time.Hour}

	h1 := newHarnessWithStore(t, opts, store)
	h1.ask(t, "why is my build failing?")
	store.awaitSave(t)

	// "Restart": a brand-new engine over the same on-disk state.
	h2 := newHarnessWithStore(t, opts, store)
	h2.ask(t, "what should I change?")
	// The restarted engine persists too, after session.finished. Waiting for
	// that write is what stops t.TempDir's cleanup racing the store's
	// temp-file-and-rename ("directory not empty"), as below.
	store.awaitSave(t)

	got := requestContents(h2.provider.LastRequest)
	if !strings.Contains(got, "why is my build failing?") {
		t.Errorf("follow-up after restart lost the prior question: %s", got)
	}
	if !strings.Contains(got, h1.provider.Response) {
		t.Errorf("follow-up after restart lost the prior answer: %s", got)
	}
	if !strings.Contains(got, "what should I change?") {
		t.Errorf("follow-up missing its own question: %s", got)
	}
}

func TestFollowUpWindowLapsesAcrossRestart(t *testing.T) {
	store := notifying(&history.File{Path: filepath.Join(t.TempDir(), "history.json")})
	opts := Options{HistoryTurns: 8, FollowUpWindow: time.Hour}

	h1 := newHarnessWithStore(t, opts, store)
	h1.ask(t, "an old question")
	store.awaitSave(t)

	// Restart with the clock two hours later: the window has lapsed while the
	// daemon was down, so the old thread must not reach the provider.
	h2 := newHarnessWithStore(t, opts, store)
	h2.engine.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	h2.ask(t, "a new question")
	store.awaitSave(t) // as above: do not let cleanup race the write

	got := requestContents(h2.provider.LastRequest)
	if strings.Contains(got, "an old question") {
		t.Errorf("lapsed thread leaked into the new conversation: %s", got)
	}
}

func TestResetConversationClearsDisk(t *testing.T) {
	fake := history.NewFake()
	opts := Options{HistoryTurns: 8, FollowUpWindow: time.Hour}

	h1 := newHarnessWithStore(t, opts, fake)
	h1.ask(t, "remember me")
	awaitOp(t, fake, "save")

	h1.engine.ResetConversation()
	if fake.Clears() != 1 {
		t.Fatalf("store cleared %d times, want 1", fake.Clears())
	}

	// A restart resurrects nothing.
	h2 := newHarnessWithStore(t, opts, fake)
	h2.ask(t, "fresh start")
	got := requestContents(h2.provider.LastRequest)
	if strings.Contains(got, "remember me") {
		t.Errorf("reset conversation came back after restart: %s", got)
	}
}

func TestCorruptHistoryFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	// A torn write, as after kill -9 without the atomic rename.
	if err := os.WriteFile(path, []byte(`{"version":1,"messages":[{"rol`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Construction must not crash, and the engine must still converse.
	store := notifying(&history.File{Path: path})
	h := newHarnessWithStore(t, Options{HistoryTurns: 8}, store)
	h.ask(t, "hello")
	if n := len(h.provider.LastRequest.Messages); n != 1 {
		t.Errorf("corrupt history produced %d messages, want just the new question", n)
	}
	// Persistence runs after session.finished; wait for the write so TempDir
	// cleanup cannot race the store's temp-file-and-rename dance.
	store.awaitSave(t)
}

func TestHistoryDisabledClearsDiskAndNeverWrites(t *testing.T) {
	fake := history.NewFake()
	fake.Seed([]ai.Message{
		{Role: ai.RoleUser, Content: "from an earlier configuration"},
		{Role: ai.RoleAssistant, Content: "an old answer"},
	}, time.Now())

	h := newHarnessWithStore(t, Options{}, fake) // HistoryTurns 0
	if fake.Clears() != 1 {
		t.Fatalf("startup with history disabled cleared %d times, want 1", fake.Clears())
	}
	h.ask(t, "standalone question")
	if fake.Saves() != 0 {
		t.Errorf("history disabled but store saw %d saves", fake.Saves())
	}
	got := requestContents(h.provider.LastRequest)
	if strings.Contains(got, "earlier configuration") {
		t.Errorf("disabled history still reached the provider: %s", got)
	}
}

func TestSaveFailureDegradesToMemoryOnly(t *testing.T) {
	fake := history.NewFake()
	fake.SaveErr = errors.New("disk full")
	opts := Options{HistoryTurns: 8, FollowUpWindow: time.Hour}

	h := newHarnessWithStore(t, opts, fake)
	h.ask(t, "first question")
	awaitOp(t, fake, "save") // attempted, failed
	h.ask(t, "second question")

	// The conversation still works in memory...
	got := requestContents(h.provider.LastRequest)
	if !strings.Contains(got, "first question") {
		t.Errorf("in-memory context lost after a failed save: %s", got)
	}
	// ...and the engine stopped hammering the broken store after one warning.
	if fake.Saves() != 1 {
		t.Errorf("store saw %d save attempts after a failure, want 1", fake.Saves())
	}
}

func TestLoadedHistoryIsCappedToConfiguredTurns(t *testing.T) {
	fake := history.NewFake()
	fake.Seed([]ai.Message{
		{Role: ai.RoleUser, Content: "oldest question"},
		{Role: ai.RoleAssistant, Content: "oldest answer"},
		{Role: ai.RoleUser, Content: "newest question"},
		{Role: ai.RoleAssistant, Content: "newest answer"},
	}, time.Now())

	// The cap shrank to one turn since that history was written.
	h := newHarnessWithStore(t, Options{HistoryTurns: 1, FollowUpWindow: time.Hour}, fake)
	h.ask(t, "follow-up")

	got := requestContents(h.provider.LastRequest)
	if strings.Contains(got, "oldest question") {
		t.Errorf("loaded history exceeds the configured cap: %s", got)
	}
	if !strings.Contains(got, "newest question") {
		t.Errorf("cap dropped the newest turns instead of the oldest: %s", got)
	}
}

// awaitOp waits for the Fake store to report an operation of the given kind.
func awaitOp(t *testing.T, fake *history.Fake, want string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case op := <-fake.Ops:
			if op == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for store op %q", want)
		}
	}
}
