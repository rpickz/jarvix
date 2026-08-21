package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/conversations"
)

// The conversation-search check must draw one line correctly: inactive is a
// state (OK, explained), damage is a Warn with a fix, and nothing here is
// ever a Fail — search degrades to "nothing found", never to a broken daemon.

func searchCheckPaths(t *testing.T) config.Paths {
	t.Helper()
	dir := t.TempDir()
	return config.Paths{Config: dir, Data: dir, State: dir, Runtime: dir}
}

func TestConversationSearchInactiveIsAStateNotAFailure(t *testing.T) {
	cfg := config.Default()
	cfg.Conversation.Retention = config.RetentionOff
	r := checkConversationSearch(cfg, searchCheckPaths(t))
	if r.Status != OK {
		t.Errorf("status = %v, want OK: retention off with an empty archive is a choice", r.Status)
	}
	if !strings.Contains(r.Detail, "inactive") || !strings.Contains(r.Detail, "retention is off") {
		t.Errorf("detail must say inactive and why: %q", r.Detail)
	}
}

func TestConversationSearchCountsTheArchive(t *testing.T) {
	cfg := config.Default()
	paths := searchCheckPaths(t)
	store := &conversations.FileStore{Dir: paths.ConversationsDir()}
	ts := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	if _, err := store.Append("", []conversations.Turn{{Role: "user", Text: "hello", Time: ts}}); err != nil {
		t.Fatal(err)
	}
	r := checkConversationSearch(cfg, paths)
	if r.Status != OK || !strings.Contains(r.Detail, "1 conversation(s) searchable") {
		t.Errorf("result = %+v, want OK with the count", r)
	}
}

func TestConversationSearchWarnsAboutUnreadableRecords(t *testing.T) {
	cfg := config.Default()
	paths := searchCheckPaths(t)
	if err := os.MkdirAll(paths.ConversationsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.ConversationsDir(), "bad.json"),
		[]byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := checkConversationSearch(cfg, paths)
	if r.Status != Warn {
		t.Errorf("status = %v, want Warn: an unreadable record deserves a mention, not a Fail", r.Status)
	}
	if !strings.Contains(r.Detail, "1 unreadable") || r.Fix == "" {
		t.Errorf("result = %+v, want the unreadable count and a fix", r)
	}
}
