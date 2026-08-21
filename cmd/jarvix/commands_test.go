package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
)

// pathsWithHistory builds a Paths whose state directory holds a saved
// conversation, so a test can assert whether `jarvix new` kept or destroyed
// it. socket is the socket path to dial.
func pathsWithHistory(t *testing.T, socket string) config.Paths {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := config.Paths{State: state, Socket: socket}
	doc := `{"version":1,"last_turn":"2026-01-01T00:00:00Z","messages":` +
		`[{"role":"user","content":"remember this"},{"role":"assistant","content":"noted"}]}`
	if err := os.WriteFile(paths.HistoryFile(), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return paths
}

func historyExists(t *testing.T, paths config.Paths) bool {
	t.Helper()
	_, err := os.Stat(paths.HistoryFile())
	return err == nil
}

// `jarvix new` clearing the on-disk conversation is only correct when the
// daemon really is down. ipc.Dial wraps every connection failure in the same
// "is it running?" message, so treating any error as "not running" made a
// permission or misconfiguration error delete the user's history — silent,
// unrecoverable data loss (raised in review of #16).
func TestNewConversationKeepsHistoryWhenDialFailsForAnotherReason(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "regular-file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Dialling through a regular file as if it were a directory fails with
	// ENOTDIR: a real error that is neither "no socket" nor "nobody home".
	paths := pathsWithHistory(t, filepath.Join(notADir, "jarvix.sock"))

	err := cmdNewConversation(paths)
	if err == nil {
		t.Fatal("an unexplained dial failure must be reported, not swallowed")
	}
	if !historyExists(t, paths) {
		t.Fatal("the saved conversation was deleted on an error that does not mean the daemon is down")
	}
}

// The flip side: when the daemon genuinely is not running, clearing the
// persisted conversation is the whole point of the command — otherwise the
// old thread resurrects at the next daemon start.
func TestNewConversationClearsHistoryWhenDaemonIsDown(t *testing.T) {
	t.Run("socket missing", func(t *testing.T) {
		paths := pathsWithHistory(t, filepath.Join(t.TempDir(), "jarvix.sock"))
		if err := cmdNewConversation(paths); err != nil {
			t.Fatalf("a missing socket means no daemon: %v", err)
		}
		if historyExists(t, paths) {
			t.Error("history must be cleared when no daemon can hold it")
		}
	})
	t.Run("socket stale", func(t *testing.T) {
		// A socket file left behind by a killed daemon: connect gets
		// ECONNREFUSED because nobody is accepting.
		stale := filepath.Join(t.TempDir(), "jarvix.sock")
		listener, err := net.Listen("unix", stale)
		if err != nil {
			t.Fatal(err)
		}
		// Close the listener but keep the file, exactly as a SIGKILL leaves it.
		listener.(*net.UnixListener).SetUnlinkOnClose(false)
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		paths := pathsWithHistory(t, stale)
		if err := cmdNewConversation(paths); err != nil {
			t.Fatalf("a stale socket means no daemon: %v", err)
		}
		if historyExists(t, paths) {
			t.Error("history must be cleared when the socket is stale")
		}
	})
}
