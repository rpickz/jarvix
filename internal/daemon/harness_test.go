package daemon

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
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

// dialDaemon connects to a daemon's socket once it comes up AND has answered
// on it. It is the readiness barrier for the whole package, so nothing that
// follows can be ahead of the daemon's own boot.
func dialDaemon(t *testing.T, socket string) *ipc.Client {
	t.Helper()
	client, err := awaitDaemon(socket, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// awaitDaemon is dialDaemon without the *testing.T, so the readiness rule can
// be tested rather than only relied on (TestTheHarnessWaitsForTheDaemonToAnswer).
//
// Polling for the socket is right here and nowhere else in these tests: the
// socket is a file another goroutine creates, not something this test can
// synchronise with. The ROUND TRIP after it is the part that matters, and the
// part that was missing (#179).
//
// Run binds the socket first and starts the scheduled services afterwards —
// the reminder clockwork last of all, immediately before Serve — so a
// successful dial says the socket file exists and nothing more: the kernel
// queues the connection and the request loop has not started. These tests then
// routinely reach PAST the socket into the daemon's own services (a store file
// written by hand, h.d.reminders.Rearm(), h.d.engine.StartSession()), and a
// test that did so was racing a boot it had never waited for. #179's reminder
// failure was exactly that: a reminder planted in the gap was still sitting on
// disk when reminders.Start scanned the store, so it was marked as a
// missed-while-down catch-up and the delivery spoke "While I was off …"
// instead of the late-marked line. One in ~200 runs, and it looked like a
// production ordering bug rather than a harness that returned too early.
//
// A served reply is the barrier: the request loop only runs once Run has
// reached Serve, which is after every Start has returned. It settles the
// second ordering too — the per-connection event subscription is taken inside
// serveConn, after accept — so events published after this call are guaranteed
// to be pushed to this client rather than merely likely to be. That is the
// same argument, and the same one round trip, that startActivityDaemon has
// been making on its own since the activity feed landed.
func awaitDaemon(socket string, budget time.Duration) (*ipc.Client, error) {
	deadline := time.Now().Add(budget)
	for {
		client, err := ipc.Dial(socket)
		if err == nil {
			// One round trip and no retry: a dial that succeeded proves some
			// process owns this socket, so a failure to answer is a real
			// failure and never a not-up-yet worth waiting through.
			if err := client.Call("status.get", nil, nil); err != nil {
				_ = client.Close()
				return nil, fmt.Errorf("the socket at %s took the connection but did not answer: %w",
					socket, err)
			}
			return client, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("daemon socket never came up: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTheHarnessWaitsForTheDaemonToAnswer pins the rule awaitDaemon exists for:
// a bound socket is not a booted daemon, and the harness must not say ready
// until something has answered on it (#179).
//
// The stand-in for the gap is a listener that accepts and hangs up, which is
// the daemon's own shape between server.Listen() and server.Serve() — bound,
// dialable, dispatching nothing. Hanging up rather than staying silent is what
// keeps this instant: a silent peer proves the same point at the cost of the
// client's own thirty-second call timeout, and a test that costs half a minute
// to assert a negative gets deleted.
func TestTheHarnessWaitsForTheDaemonToAnswer(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "j.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	client, err := awaitDaemon(socket, 5*time.Second)
	if err == nil {
		_ = client.Close()
		t.Fatal("the harness called a bound-but-unserved socket ready: a successful dial " +
			"proves the socket file exists, not that Run has finished starting the " +
			"clockwork behind it (#179)")
	}
}
