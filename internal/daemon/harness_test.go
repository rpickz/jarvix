package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ipc"
)

// serveDaemon runs d for the length of the test and stops it cleanly at the
// end: cancel the context, then wait for Run to return.
//
// The waiting is the point. jarvixd does work after the user-visible part of
// an interaction is over — the conversation history is written once the
// session has finished, off the engine's lock (ADR 0011) — and Run drains
// that work before returning. A test that only cancelled would race its own
// t.TempDir cleanup against the daemon's last write and fail with "TempDir
// RemoveAll: directory not empty", which is exactly the flake this drain was
// built to remove (#29).
//
// Cleanups run last-registered-first, so calling this *after* t.TempDir puts
// the daemon's stop ahead of the directory's removal.
func serveDaemon(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = d.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
}

// dialDaemon connects to a daemon's socket once it comes up. Polling is right
// here and nowhere else in these tests: the socket is a file another process
// creates, not a goroutine this test can synchronise with.
func dialDaemon(t *testing.T, socket string) *ipc.Client {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		client, err := ipc.Dial(socket)
		if err == nil {
			t.Cleanup(func() { _ = client.Close() })
			return client
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon socket never came up: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
