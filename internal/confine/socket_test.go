package confine

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The second wall (#222, ADR 0069): a confined command cannot make a unix
// socket, so it cannot reach Jarvix's own.
//
// These are the tests for the hole that would otherwise have made the whole
// feature a net loss. Landlock closes the filesystem and defines no right over
// connecting to a unix socket, so a command confined out of `config.toml` could
// still have asked the daemon to rewrite `config.toml`.
//
// The assertion is on the LISTENER, not on the command. A dial can fail for a
// dozen reasons — nothing listening, a path typed wrong, a socket removed
// underneath it — and every one of them looks exactly like a refusal from the
// client's side. So the tests below stand up a real listener that records every
// connection it accepts and every byte it is sent, and then assert that it
// accepted nothing and received nothing. That is the server's own state,
// observed directly, which is the only thing that distinguishes "the wall
// stopped it" from "it did not happen to work".

// dialSentinel makes this test binary usable as the command inside a
// confinement. It is argv rather than an environment variable on purpose: the
// child's environment is built from nothing, so a variable would not survive
// the confinement — and being unable to smuggle one in is itself the point.
const dialSentinel = "--jarvix-test-dial-unix"

// serveTestDial is the other half of TestMain. When this binary is run as a
// confined command it tries to reach the socket it was pointed at, says what
// happened on stdout, and exits — so the thing attempting the connection is a
// real process inside a real Landlock domain under a real seccomp filter,
// rather than a unit test pretending to be one.
func serveTestDial() bool {
	if len(os.Args) < 3 || os.Args[1] != dialSentinel {
		return false
	}
	conn, err := net.DialTimeout("unix", os.Args[2], 5*time.Second)
	if err != nil {
		fmt.Printf("DIAL-REFUSED: %v\n", err)
		os.Exit(3)
	}
	_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"config.set"}` + "\n"))
	_ = conn.Close()
	fmt.Println("DIAL-CONNECTED")
	os.Exit(0)
	return true
}

// listener is a socket that remembers everything anybody sent it.
//
// The accept loop reads each connection to completion IN TURN rather than in a
// goroutine per connection. That serialisation is what makes the "nobody
// reached me" assertion sound: see untouched.
type listener struct {
	path string
	ln   net.Listener
	// served carries one entry per connection that has been accepted AND fully
	// read, in the order they arrived.
	served chan string
}

// barrier is the payload untouched sends to flush the listener.
const barrier = "BARRIER"

// newListener starts one, on a path OUTSIDE any job root.
func newListener(t *testing.T) *listener {
	t.Helper()
	// Its own directory, so the socket is nowhere near a root and the test
	// cannot accidentally prove something weaker than it means to.
	dir, err := os.MkdirTemp("", "jx-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	l := &listener{path: filepath.Join(dir, "j.sock"), served: make(chan string, 16)}
	if l.ln, err = net.Listen("unix", l.path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.ln.Close() })
	go func() {
		for {
			conn, err := l.ln.Accept()
			if err != nil {
				return
			}
			body, _ := io.ReadAll(conn)
			_ = conn.Close()
			l.served <- string(body)
		}
	}()
	return l
}

// untouched asserts that nothing but this test itself ever reached the socket,
// and it does so without waiting on a clock.
//
// Proving a negative about another process needs a happens-before edge, not a
// pause: a confined command that HAD got through would have been accepted a
// moment ago, and asking the listener straight away could read its counters
// before the accept loop had finished with it. So this makes its own
// connection and waits for that one to come back. Because the accept loop
// handles connections strictly in turn, the barrier's arrival proves every
// earlier connection has already been recorded — and the recording is then the
// whole truth about who reached this socket.
func (l *listener) untouched(t *testing.T) {
	t.Helper()
	l.send(t, barrier)
	first := l.next(t)
	if first != barrier {
		t.Fatalf("the socket was reached by something before this test's own barrier: %q\n"+
			"a confined command drove the daemon", first)
	}
	select {
	case extra := <-l.served:
		t.Errorf("the socket was also sent %q", extra)
	default:
	}
}

// reached asserts somebody DID get through, for the control case.
func (l *listener) reached(t *testing.T, want string) {
	t.Helper()
	if got := l.next(t); !strings.Contains(got, want) {
		t.Fatalf("the control case sent %q, want it to contain %q — without a control that "+
			"gets through, the test for the wall would pass whether or not the wall exists",
			got, want)
	}
}

// send writes one line to the socket from the test itself, unconfined.
func (l *listener) send(t *testing.T, payload string) {
	t.Helper()
	conn, err := net.DialTimeout("unix", l.path, 5*time.Second)
	if err != nil {
		t.Fatalf("the test could not reach its own listener, so it can prove nothing: %v", err)
	}
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}

// next takes the oldest fully-served connection, failing rather than hanging.
func (l *listener) next(t *testing.T) string {
	t.Helper()
	select {
	case body := <-l.served:
		return body
	case <-time.After(20 * time.Second):
		t.Fatal("nothing reached the listener at all, so this test proved nothing")
		return ""
	}
}

// probeInside copies this test binary into the job's root, so the confinement
// has something to exec that is inside its own boundary. Returns the command
// line to run it with.
func probeInside(t *testing.T, root, socket string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(root, "probe")
	if err := os.WriteFile(probe, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return probe + " " + dialSentinel + " " + socket
}

// TestAConfinedCommandCannotReachAUnixSocket is the property #222's boundary
// was missing: the wall that keeps a job's command out of Jarvix's
// configuration must also keep it from asking Jarvix to change that
// configuration.
//
// The command here is a real program, inside the boundary, deliberately
// speaking the daemon's own wire format at a real listener.
func TestAConfinedCommandCannotReachAUnixSocket(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	sock := newListener(t)
	command := probeInside(t, tr.root, sock.path)

	got, err := tr.run(t, command)
	if err != nil {
		t.Fatalf("the probe could not be run at all, so this test proved nothing: %v (%s)",
			err, got.Output)
	}
	// The daemon's own state first: nothing reached the socket. The command's
	// account of itself is the corroboration, not the evidence.
	sock.untouched(t)
	if strings.Contains(got.Output, "DIAL-CONNECTED") {
		t.Errorf("a confined command reached a unix socket outside its boundary: %q",
			got.Output)
	}
	if !strings.Contains(got.Output, "DIAL-REFUSED") {
		t.Errorf("output = %q, want the probe's own account of being refused — without it "+
			"this test cannot tell a wall from a probe that never ran", got.Output)
	}
}

// TestTheSameProbeReachesTheSocketWhenItIsNotConfined is the control, and it is
// not optional.
//
// Without it, the test above passes on a machine where the probe silently fails
// to start, where the listener never came up, or where the path was wrong — and
// a wall that is only ever tested from one side is a wall nobody has seen. This
// runs the identical binary against the identical socket with no confinement
// and requires it to get through.
func TestTheSameProbeReachesTheSocketWhenItIsNotConfined(t *testing.T) {
	sock := newListener(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, err := runUnconfined(t, self, dialSentinel, sock.path)
	if err != nil {
		t.Fatalf("the unconfined probe failed: %v (%s)", err, out)
	}
	if !strings.Contains(out, "DIAL-CONNECTED") {
		t.Fatalf("the unconfined probe did not connect: %q", out)
	}
	sock.reached(t, "config.set")
}

// TestAConfinedCommandCannotMakeAUnixSocketAtAll. The filter removes the
// capability at `socket(2)` rather than at `connect(2)`, because seccomp can
// read a scalar argument and cannot dereference a pointer — so `connect`'s
// address is invisible to it and `socket`'s domain is not. The consequence is
// broader than the hole it closes and is asserted rather than assumed: no unix
// socket can be created, so none can be listened on either.
func TestAConfinedCommandCannotMakeAUnixSocketAtAll(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	inside := filepath.Join(tr.root, "mine.sock")
	// python and nc are not assumed present; bash cannot make a socket at all,
	// which is itself the observation. The probe binary is used instead, asked
	// to reach a path inside its own root that nothing is listening on: if it
	// could make the socket it would fail with "connection refused", and if it
	// cannot make one at all it fails differently.
	command := probeInside(t, tr.root, inside)
	got, _ := tr.run(t, command)
	if strings.Contains(got.Output, "DIAL-CONNECTED") {
		t.Fatalf("the probe connected to something: %q", got.Output)
	}
	if !strings.Contains(got.Output, "permission denied") {
		t.Errorf("output = %q, want the socket call itself to have been refused — "+
			"a 'connection refused' here would mean the capability is still there and "+
			"only this particular listener was missing", got.Output)
	}
}

// TestOrdinaryWorkStillRunsUnderTheFilter. A wall that broke the everyday
// commands would be routed around, so the things a job actually does are pinned
// as still working: pipes, subshells, redirection, a loop, and the coreutils.
func TestOrdinaryWorkStillRunsUnderTheFilter(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	got, err := tr.run(t, `printf 'b\na\nc\n' | sort | tr -d '\n' > `+
		filepath.Join(tr.root, "out.txt")+` && for i in 1 2 3; do echo $i; done | wc -l`)
	if err != nil {
		t.Fatalf("ordinary shell work was refused: %v (%s)", err, got.Output)
	}
	if got.Exit != 0 {
		t.Fatalf("exit = %d, output = %q", got.Exit, got.Output)
	}
	if !strings.Contains(got.Output, "3") {
		t.Errorf("output = %q, want the loop's count", got.Output)
	}
	sorted, err := os.ReadFile(filepath.Join(tr.root, "out.txt"))
	if err != nil || string(sorted) != "abc" {
		t.Errorf("file = %q, %v; want the pipeline's own output", sorted, err)
	}
}

// runUnconfined runs a program with no boundary at all, for the control case.
func runUnconfined(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// Nothing of the parent's environment, so the control differs from the
	// confined case in exactly one thing: the walls.
	cmd.Env = []string{"PATH=" + safePath}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
