package intent

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The ExecRunner tests use coreutils that exist everywhere and change
// nothing: no test in this package may touch wpctl, hyprctl, or a terminal.

func TestExecRunnerRunsArgv(t *testing.T) {
	r := &ExecRunner{}
	if err := r.Run(context.Background(), []string{"true"}); err != nil {
		t.Errorf("Run(true): %v", err)
	}
}

func TestExecRunnerReportsFailure(t *testing.T) {
	r := &ExecRunner{}
	err := r.Run(context.Background(), []string{"false"})
	if err == nil {
		t.Fatal("a non-zero exit must be reported")
	}
	if !strings.Contains(err.Error(), "false") {
		t.Errorf("error %q does not name the command", err)
	}
}

func TestExecRunnerMissingBinaryIsSpeakable(t *testing.T) {
	r := &ExecRunner{}
	err := r.Run(context.Background(), []string{"jarvix-no-such-binary-2f9a"})
	if err == nil {
		t.Fatal("a missing binary must be reported")
	}
	// The message is spoken aloud, so it must read as a sentence rather than
	// as an exec error.
	if !strings.Contains(err.Error(), "is not installed") {
		t.Errorf("error %q is not a speakable sentence", err)
	}
}

func TestExecRunnerEmptyArgv(t *testing.T) {
	r := &ExecRunner{}
	if err := r.Run(context.Background(), nil); err == nil {
		t.Error("empty argv must be an error")
	}
	if err := r.RunShell(context.Background(), "   "); err == nil {
		t.Error("empty command must be an error")
	}
}

func TestExecRunnerShell(t *testing.T) {
	r := &ExecRunner{}
	if err := r.RunShell(context.Background(), "exit 0"); err != nil {
		t.Errorf("RunShell: %v", err)
	}
	err := r.RunShell(context.Background(), "echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("a failing command must be reported")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q does not carry the command's own message", err)
	}
}

// TestExecRunnerTimeout proves a wedged command cannot hold a session open.
func TestExecRunnerTimeout(t *testing.T) {
	r := &ExecRunner{Timeout: 20 * time.Millisecond}
	err := r.RunShell(context.Background(), "sleep 5")
	if err == nil {
		t.Fatal("a hung command must time out")
	}
	if !strings.Contains(err.Error(), "did not finish in time") {
		t.Errorf("error = %q", err)
	}
}

func TestFakeRunnerRecords(t *testing.T) {
	f := &FakeRunner{}
	if err := f.Run(context.Background(), []string{"wpctl", "set-mute"}); err != nil {
		t.Fatal(err)
	}
	if err := f.RunShell(context.Background(), "hyprlock"); err != nil {
		t.Fatal(err)
	}
	argv := f.Argv()
	if len(argv) != 1 || strings.Join(argv[0], " ") != "wpctl set-mute" {
		t.Errorf("argv = %v", argv)
	}
	if shell := f.Shell(); len(shell) != 1 || shell[0] != "hyprlock" {
		t.Errorf("shell = %v", shell)
	}
}

func TestFirstLineBoundsSpokenFailure(t *testing.T) {
	if got := firstLine("bad thing\nmore detail\nand more"); got != "bad thing" {
		t.Errorf("firstLine = %q", got)
	}
	long := strings.Repeat("x", 400)
	if got := firstLine(long); !strings.HasSuffix(got, "…") || len([]rune(got)) != 121 {
		t.Errorf("firstLine did not bound a long line: %d runes", len([]rune(got)))
	}
}
