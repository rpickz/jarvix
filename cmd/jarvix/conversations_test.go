package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/ipc"
)

// `jarvix conversations` is the CLI half of the durable archive (ADR 0027).
// The daemon methods have their own tests; here the rendering, the argument
// handling, and — because a transcript must stay under the user's control
// with jarvixd stopped — the daemon-down file fallback are pinned.

func TestRunConversationsList(t *testing.T) {
	hermeticEnv(t)
	startDaemon(t, nil, map[string]ipc.Handler{
		"conversation.list": func(json.RawMessage) (any, error) {
			return map[string]any{
				"retention": true,
				"active_id": "20260821-104500-ab12",
				"conversations": []map[string]any{
					{"id": "20260821-104500-ab12", "started": "2026-08-21T10:45:00Z",
						"last_active": "2026-08-21T10:50:00Z", "turns": 4,
						"preview": "what is on my calendar?"},
					{"id": "20260820-090000-cd34", "started": "2026-08-20T09:00:00Z",
						"last_active": "2026-08-20T09:05:00Z", "turns": 2,
						"preview": "remind me about the deploy"},
				},
				"unreadable": []map[string]any{
					{"id": "20260819-080000-ef56", "error": "parse conversation metadata: unexpected end"},
				},
			}, nil
		},
	})
	var code int
	stdout, stderr := capture(t, func() { code = run([]string{"conversations"}) })
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"20260821-104500-ab12", "what is on my calendar?", "4 turns", "2026-08-21",
		"20260820-090000-cd34", "remind me about the deploy",
		// One bad file never hides itself: the unreadable record is stated.
		"20260819-080000-ef56", "unreadable",
		// The active thread is marked, and the marker explained.
		"* = the active conversation",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}
}

// seedArchive writes a real conversation under this test's XDG state dir, as
// a daemon run would have.
func seedArchive(t *testing.T, question, answer string) (dir, id string) {
	t.Helper()
	paths := config.DefaultPaths()
	store := &conversations.FileStore{Dir: paths.ConversationsDir()}
	ts := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	id, err := store.Append("", []conversations.Turn{
		{Role: "user", Text: question, Time: ts},
		{Role: "assistant", Text: answer, Time: ts},
	})
	if err != nil {
		t.Fatal(err)
	}
	return paths.ConversationsDir(), id
}

func TestRunConversationsListFallsBackToFilesWhenDaemonIsDown(t *testing.T) {
	hermeticEnv(t)
	_, id := seedArchive(t, "asked while the daemon lived", "and answered")

	var code int
	stdout, stderr := capture(t, func() { code = run([]string{"conversations", "list"}) })
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, id) || !strings.Contains(stdout, "asked while the daemon lived") {
		t.Errorf("offline listing missing the archived conversation:\n%s", stdout)
	}
}

func TestRunConversationsShowFallsBackToFilesWhenDaemonIsDown(t *testing.T) {
	hermeticEnv(t)
	_, id := seedArchive(t, "the archived question", "the archived answer")

	var code int
	stdout, stderr := capture(t, func() { code = run([]string{"conversations", "show", id}) })
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"you: the archived question", "jarvix: the archived answer"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}
}

func TestRunConversationsDeleteFallsBackToFilesWhenDaemonIsDown(t *testing.T) {
	hermeticEnv(t)
	dir, id := seedArchive(t, "delete me offline", "done")

	var code int
	_, stderr := capture(t, func() { code = run([]string{"conversations", "delete", id}) })
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	// Deletion is proven on the state directory, daemon or no daemon.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), id) {
			t.Errorf("offline delete left %s behind", entry.Name())
		}
	}
}

func TestRunConversationsOpenNeedsTheDaemon(t *testing.T) {
	hermeticEnv(t)
	_, id := seedArchive(t, "a question", "an answer")
	var code int
	_, stderr := capture(t, func() { code = run([]string{"conversations", "open", id}) })
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "jarvixd") {
		t.Errorf("stderr = %q, want it to say the daemon is needed", stderr)
	}
}

func TestRunConversationsOpenCallsTheDaemon(t *testing.T) {
	hermeticEnv(t)
	rec := startDaemon(t, nil, map[string]ipc.Handler{
		"conversation.open": func(params json.RawMessage) (any, error) {
			var p struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(params, &p)
			if p.ID != "some-id" {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "unexpected id %q", p.ID)
			}
			return map[string]any{"id": p.ID, "turns": 6}, nil
		},
	})
	var code int
	stdout, stderr := capture(t, func() { code = run([]string{"conversations", "open", "some-id"}) })
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if got := rec.recorded(); len(got) != 1 || got[0] != "conversation.open" {
		t.Errorf("methods called = %v", got)
	}
	if !strings.Contains(stdout, "reopened some-id (6 turns)") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestRunConversationsDeleteAllOverSocket(t *testing.T) {
	hermeticEnv(t)
	rec := startDaemon(t, nil, map[string]ipc.Handler{
		"conversation.delete": func(params json.RawMessage) (any, error) {
			var p struct {
				All bool `json:"all"`
			}
			_ = json.Unmarshal(params, &p)
			if !p.All {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "expected all=true")
			}
			return map[string]any{"deleted": 3}, nil
		},
	})
	var code int
	stdout, _ := capture(t, func() { code = run([]string{"conversations", "delete", "--all"}) })
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if got := rec.recorded(); len(got) != 1 || got[0] != "conversation.delete" {
		t.Errorf("methods called = %v", got)
	}
	if !strings.Contains(stdout, "deleted 3 conversation(s)") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestRunConversationsUsage(t *testing.T) {
	hermeticEnv(t)
	for _, args := range [][]string{
		{"conversations", "show"},
		{"conversations", "open"},
		{"conversations", "delete"},
		{"conversations", "delete", "--al"},
		{"conversations", "frobnicate"},
	} {
		var code int
		_, stderr := capture(t, func() { code = run(args) })
		if code != 1 {
			t.Errorf("run(%v) = %d, want 1", args, code)
		}
		if !strings.Contains(stderr, "usage: jarvix conversations") {
			t.Errorf("run(%v) stderr = %q, want usage", args, stderr)
		}
	}
}

// The seeded file's permissions are the store's concern; the CLI must not
// loosen them just by reading. A regression here is a privacy bug.
func TestOfflineCommandsKeepFilesPrivate(t *testing.T) {
	hermeticEnv(t)
	dir, id := seedArchive(t, "private", "kept")
	capture(t, func() { run([]string{"conversations", "show", id}) })
	info, err := os.Stat(filepath.Join(dir, id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("transcript mode = %v after a read, want 0600", info.Mode().Perm())
	}
}
