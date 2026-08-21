package daemon

import (
	"os"
	"testing"
)

// daemonTempDir is t.TempDir with one difference that matters here: removal
// is best-effort.
//
// A daemon under test keeps working for a moment after the test's last
// assertion — conversation history is persisted asynchronously, *after*
// session.finished, deliberately (ADR 0011: disk I/O adds no latency to the
// spoken exchange). t.TempDir's cleanup fails the test if a file appears
// while it is removing the directory, so a daemon test that runs a session
// fails intermittently with "directory not empty" through no fault of the
// test. Ignoring the removal error costs nothing — the directory is under the
// system temp root either way — and removes a flake that has nothing to say.
func daemonTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "jarvixd-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
