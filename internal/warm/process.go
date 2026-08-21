package warm

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// terminateGrace is how long a child gets to exit after its stdin is closed
// and it has been asked to stop, before the process group is killed outright.
// Short on purpose: shutdown must not make logging out feel slow, and every
// engine here is restartable from cold.
const terminateGrace = 500 * time.Millisecond

// Process is a supervised long-lived child: its own process group, a stdin it
// is fed requests on, and a stdout it answers on.
//
// The process group is the whole point of the type. A persistent engine helper
// is exactly the shape of process that becomes an orphan — the Python Kokoro
// helper spawns ONNX threads, whisper-server outlives a killed parent
// perfectly happily — so Jarvix never kills a pid, it kills a group, and every
// descendant goes with it.
type Process struct {
	cmd *exec.Cmd
	// Stdin carries requests to the child. Closing it is the polite stop
	// signal for a helper that loops over its input.
	Stdin io.WriteCloser
	// Stdout carries the child's framed replies. Buffered because every
	// protocol here is "header line, then payload".
	Stdout *bufio.Reader
	// Stderr is the child's diagnostic stream, left for the caller to drain
	// (an undrained pipe eventually blocks the child).
	Stderr io.ReadCloser

	stdout io.ReadCloser

	closeOnce sync.Once
	waited    chan struct{}
	waitErr   error
}

// ProcessSpec describes a child to start.
type ProcessSpec struct {
	// Path is the executable; Args are its arguments (argv[1:]).
	Path string
	Args []string
	// Env replaces the environment when non-nil; nil inherits.
	Env []string
	// Dir is the working directory; empty inherits.
	Dir string
	// StdoutSize is the read buffer for framed replies. Zero uses 64 KiB,
	// which comfortably holds one PCM frame header plus its payload.
	StdoutSize int
}

// StartProcess launches a supervised child in its own process group.
func StartProcess(spec ProcessSpec) (*Process, error) {
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = spec.Dir
	// Own process group: Close kills the group, so a helper that forked
	// workers of its own cannot leave orphans behind.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start %s: %w", spec.Path, err)
	}

	size := spec.StdoutSize
	if size <= 0 {
		size = 64 << 10
	}
	p := &Process{
		cmd:    cmd,
		Stdin:  stdin,
		Stdout: bufio.NewReaderSize(stdout, size),
		Stderr: stderr,
		stdout: stdout,
		waited: make(chan struct{}),
	}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.waited)
	}()
	return p, nil
}

// PID implements Child.
func (p *Process) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Exited is closed once the child has been reaped — the seam a reader
// goroutine uses to tell "the engine died" from "the engine is thinking".
func (p *Process) Exited() <-chan struct{} { return p.waited }

// ExitError reports why the child exited, once Exited is closed.
func (p *Process) ExitError() error { return p.waitErr }

// Close stops the child: stdin closed (the protocol's own quit signal), then
// SIGTERM to the whole process group, then SIGKILL to it if the grace period
// lapses. Safe to call more than once; safe to call on a child that already
// died.
func (p *Process) Close() {
	p.closeOnce.Do(func() {
		_ = p.Stdin.Close()
		p.signalGroup(syscall.SIGTERM)
		select {
		case <-p.waited:
		case <-time.After(terminateGrace):
			p.signalGroup(syscall.SIGKILL)
			<-p.waited
		}
		_ = p.stdout.Close()
		_ = p.Stderr.Close()
	})
}

// signalGroup delivers sig to the child's whole process group. The negative
// pid is what makes it a group signal; a failure means the group is already
// gone, which is the outcome we wanted.
func (p *Process) signalGroup(sig syscall.Signal) {
	if p.cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		// Fall back to the single process: Setpgid can only fail in ways that
		// leave us a plain child, and killing that is still better than not.
		_ = p.cmd.Process.Signal(sig)
	}
}

// DrainStderr consumes a child's stderr so the pipe never fills and blocks it,
// keeping the last few lines for an error message. Engines are chatty on
// stderr (ggml backend banners, ONNX warnings) and none of it belongs in the
// journal at info level.
func DrainStderr(r io.Reader, keep int) *StderrTail {
	tail := &StderrTail{keep: keep}
	go func() {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			tail.add(scanner.Text())
		}
	}()
	return tail
}

// StderrTail holds the last lines a child wrote to stderr, so a crash can be
// explained with the engine's own words.
type StderrTail struct {
	keep int

	mu    sync.Mutex
	lines []string
}

func (t *StderrTail) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > t.keep {
		t.lines = t.lines[len(t.lines)-t.keep:]
	}
}

// String returns the retained lines, newest last.
func (t *StderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := ""
	for i, l := range t.lines {
		if i > 0 {
			out += "; "
		}
		out += l
	}
	return out
}

// LookPath resolves a binary the way the adapters do, so "not installed"
// fails at spawn time with a message naming the binary rather than deep
// inside a protocol read.
func LookPath(bin string) (string, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("%s not found on PATH", bin)
	}
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	return path, nil
}
