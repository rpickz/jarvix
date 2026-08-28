package transcript

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Window-to-directory resolution (#137): the /proc walk is exercised against
// a procfs shape built in a tempdir — pids, stat lines, cwd symlinks — so no
// test ever reads the real process table.

// fakeProc builds a procfs root. Each entry is pid → (ppid, cwd target).
func fakeProc(t *testing.T, procs map[int]struct {
	ppid int
	cwd  string
}) string {
	t.Helper()
	root := t.TempDir()
	for pid, p := range procs {
		dir := filepath.Join(root, fmt.Sprint(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// The comm field deliberately contains a space and parentheses in one
		// entry (see callers), because the stat parse must anchor on the LAST
		// ')' — the documented trap in /proc/<pid>/stat.
		stat := fmt.Sprintf("%d (proc %d (x)) S %d 1 1 0 -1 4194560", pid, pid, p.ppid)
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
		if p.cwd != "" {
			if err := os.Symlink(p.cwd, filepath.Join(dir, "cwd")); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Non-process entries are always present in a real /proc and must be
	// skipped, not tripped over.
	if err := os.WriteFile(filepath.Join(root, "meminfo"), []byte("MemTotal: 1 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestReadWindowPrefersTheShellOverDeeperChildren is the ordering decision:
// the emulator (pid 100) launched in a directory with its own stale
// transcripts, the shell (200) and the agent (300) sit in the project, and a
// tool child (350) is off in a worktree with transcripts of its own. The
// shell's directory must win — shallowest first, the window process last.
func TestReadWindowPrefersTheShellOverDeeperChildren(t *testing.T) {
	launchDir := t.TempDir()  // the emulator's own cwd — least credible
	projectDir := t.TempDir() // where the shell and the agent live
	toolDir := t.TempDir()    // a tool child's worktree — wrong session

	root := t.TempDir()
	writeClaudeTranscript(t, root, launchDir, "s.jsonl", "finished.jsonl", fixedNow.Add(-time.Hour))
	writeClaudeTranscript(t, root, projectDir, "s.jsonl", "awaiting.jsonl", fixedNow.Add(-time.Hour))
	writeClaudeTranscript(t, root, toolDir, "s.jsonl", "midtask.jsonl", fixedNow.Add(-time.Hour))

	proc := fakeProc(t, map[int]struct {
		ppid int
		cwd  string
	}{
		100: {1, launchDir},
		200: {100, projectDir},
		300: {200, projectDir},
		350: {300, toolDir},
	})
	f := &Finder{ClaudeDir: root, ProcDir: proc, Now: func() time.Time { return fixedNow }}
	tail, err := f.ReadWindow(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if tail.State != StateNeedsYou {
		t.Errorf("state = %q; the walk did not resolve the shell's directory first", tail.State)
	}
}

// TestReadWindowFallsBackToTheWindowProcess: with no descendants hosting a
// session, the window process's own directory is still tried — last.
func TestReadWindowFallsBackToTheWindowProcess(t *testing.T) {
	launchDir := t.TempDir()
	root := t.TempDir()
	writeClaudeTranscript(t, root, launchDir, "s.jsonl", "finished.jsonl", fixedNow.Add(-time.Hour))
	proc := fakeProc(t, map[int]struct {
		ppid int
		cwd  string
	}{
		100: {1, launchDir},
		200: {100, t.TempDir()}, // a shell somewhere with no session
	})
	f := &Finder{ClaudeDir: root, ProcDir: proc, Now: func() time.Time { return fixedNow }}
	tail, err := f.ReadWindow(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if tail.State != StateDone {
		t.Errorf("state = %q", tail.State)
	}
}

// TestReadWindowWithNoSessionAnywhereIsAbsence: a window whose whole tree
// hosts nothing reads as ErrNoSession — the silent title fallback, never an
// error a recap would have to admit.
func TestReadWindowWithNoSessionAnywhereIsAbsence(t *testing.T) {
	proc := fakeProc(t, map[int]struct {
		ppid int
		cwd  string
	}{
		100: {1, t.TempDir()},
	})
	f := &Finder{
		ClaudeDir: filepath.Join(t.TempDir(), "claude"),
		ProcDir:   proc,
		Now:       func() time.Time { return fixedNow },
	}
	if _, err := f.ReadWindow(context.Background(), 100); !errors.Is(err, ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession", err)
	}
	if _, err := f.ReadWindow(context.Background(), 0); !errors.Is(err, ErrNoSession) {
		t.Errorf("a zero pid = %v, want ErrNoSession", err)
	}
}

// TestCandidateCwdsOrderAndBounds pins the walk itself: shallow before deep,
// the window process last, duplicates dropped, vanished children skipped.
func TestCandidateCwdsOrderAndBounds(t *testing.T) {
	shellDir, agentDir, launchDir := t.TempDir(), t.TempDir(), t.TempDir()
	proc := fakeProc(t, map[int]struct {
		ppid int
		cwd  string
	}{
		100: {1, launchDir},
		200: {100, shellDir},
		300: {200, agentDir},
		310: {200, shellDir}, // duplicate cwd — dropped
		320: {200, ""},       // no cwd link — a vanished child, skipped
	})
	f := &Finder{ProcDir: proc}
	got := f.candidateCwds(100)
	want := []string{shellDir, agentDir, launchDir}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if !strings.HasSuffix(got[i], filepath.Base(want[i])) {
			t.Errorf("candidate[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
