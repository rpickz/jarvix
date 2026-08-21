package warm

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// installStub writes an executable shell script and returns its path. Every
// process test here runs a real child: the properties under test — process
// groups, orphans, signals — do not exist in a fake.
func installStub(t *testing.T, name, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// alive reports whether a pid is still running (signal 0 probes existence).
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// awaitGone polls until a pid disappears. Polling, not sleeping: the test
// asserts the outcome and returns as soon as it holds.
func awaitGone(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s (pid %d) is still running", what, pid)
}

func TestProcessRoundTripsLineWise(t *testing.T) {
	// The shape every persistent engine helper has: read a line, answer a line.
	bin := installStub(t, "echoer", `#!/bin/sh
while IFS= read -r line; do
  printf 'REPLY %s\n' "$line"
done
`)
	proc, err := StartProcess(ProcessSpec{Path: bin})
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Close()

	for _, want := range []string{"first", "second", "third"} {
		if _, err := fmt.Fprintln(proc.Stdin, want); err != nil {
			t.Fatal(err)
		}
		line, err := proc.Stdout.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(line); got != "REPLY "+want {
			t.Fatalf("reply = %q, want %q", got, "REPLY "+want)
		}
	}
	// One process served all three: that is the whole point of a warm worker.
	if !alive(proc.PID()) {
		t.Error("the worker exited between utterances")
	}
}

func TestCloseKillsTheWholeProcessGroup(t *testing.T) {
	// The stub forks a long-lived grandchild and reports its pid, which is
	// exactly the shape that leaks: killing the direct child alone would leave
	// the grandchild running with no parent to reap it.
	bin := installStub(t, "forker", `#!/bin/sh
sleep 300 &
printf '%s\n' "$!"
while IFS= read -r _; do :; done
`)
	proc, err := StartProcess(ProcessSpec{Path: bin})
	if err != nil {
		t.Fatal(err)
	}
	line, err := proc.Stdout.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	if !alive(grandchild) {
		t.Fatal("the stub's grandchild never started")
	}
	child := proc.PID()

	proc.Close()

	awaitGone(t, child, "the worker")
	awaitGone(t, grandchild, "the worker's grandchild")
}

func TestCloseIsIdempotent(t *testing.T) {
	bin := installStub(t, "sleeper", `#!/bin/sh
exec sleep 300
`)
	proc, err := StartProcess(ProcessSpec{Path: bin})
	if err != nil {
		t.Fatal(err)
	}
	pid := proc.PID()
	proc.Close()
	proc.Close() // a supervisor may retire a child that already died
	awaitGone(t, pid, "the worker")
}

func TestExitedFiresWhenTheChildDies(t *testing.T) {
	bin := installStub(t, "quitter", `#!/bin/sh
exit 7
`)
	proc, err := StartProcess(ProcessSpec{Path: bin})
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Close()
	select {
	case <-proc.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("Exited never fired")
	}
	if proc.ExitError() == nil {
		t.Error("a non-zero exit must be reported")
	}
}

func TestStderrTailKeepsTheLastLines(t *testing.T) {
	bin := installStub(t, "noisy", `#!/bin/sh
i=1
while [ $i -le 12 ]; do echo "line $i" >&2; i=$((i+1)); done
echo done
while IFS= read -r _; do :; done
`)
	proc, err := StartProcess(ProcessSpec{Path: bin})
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Close()
	tail := DrainStderr(proc.Stderr, 3)
	// The stdout marker orders the assertion after the stderr writes: the stub
	// writes every stderr line before it, so no sleep is needed to wait for
	// output that has already happened.
	if _, err := proc.Stdout.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(tail.String(), "line 12") {
			break
		}
		time.Sleep(time.Millisecond)
	}
	got := tail.String()
	if !strings.Contains(got, "line 12") || strings.Contains(got, "line 9") {
		t.Errorf("tail = %q, want only the last three lines", got)
	}
}

func TestStartProcessReportsAMissingBinary(t *testing.T) {
	_, err := StartProcess(ProcessSpec{Path: filepath.Join(t.TempDir(), "nope")})
	if err == nil {
		t.Fatal("want an error for a binary that does not exist")
	}
}

func TestLookPathNamesTheMissingBinary(t *testing.T) {
	_, err := LookPath("jarvix-does-not-exist")
	if err == nil || !strings.Contains(err.Error(), "jarvix-does-not-exist") {
		t.Errorf("err = %v, want the binary named", err)
	}
}
