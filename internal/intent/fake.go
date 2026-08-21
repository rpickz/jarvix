package intent

import (
	"context"
	"sync"
)

// FakeRunner is a Runner for tests and offline development: it records what
// would have run instead of running it, so no test ever changes the machine's
// volume or opens a terminal. Safe for concurrent use — the engine calls it
// from the session goroutine while a test asserts from its own.
type FakeRunner struct {
	mu    sync.Mutex
	argv  [][]string
	shell []string
	err   error
}

// SetErr makes every subsequent call fail with err (nil restores success),
// standing in for a missing wpctl or a command that exits non-zero.
func (f *FakeRunner) SetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// Run implements Runner.
func (f *FakeRunner) Run(_ context.Context, argv []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.argv = append(f.argv, append([]string(nil), argv...))
	return f.err
}

// RunShell implements Runner.
func (f *FakeRunner) RunShell(_ context.Context, command string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shell = append(f.shell, command)
	return f.err
}

// Argv returns the fixed command lines executed so far.
func (f *FakeRunner) Argv() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.argv...)
}

// Shell returns the user-defined commands executed so far.
func (f *FakeRunner) Shell() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.shell...)
}
