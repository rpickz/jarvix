package daemon

// A job's command cannot drive the daemon (#222, ADR 0069).
//
// This is the hole that would have made ADR 0068 a net loss, tested where it
// actually mattered: a real ipc.Server, on the real socket path a real daemon
// binds, with a real registered method that changes something — driven at by a
// real command inside a real confinement, through the real jobActor.Do path.
//
// The assertion is the DAEMON'S state. A dial can fail for a dozen reasons that
// have nothing to do with a wall, and every one of them looks the same from the
// command's side, so what is checked here is that the server never handled the
// request and that the setting it would have changed still says what it said
// before. The command's own account of being refused is corroboration, never
// evidence.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/jobs"
	"github.com/rpickz/jarvix/internal/tools"
)

// dialSentinel makes this test binary usable as the command inside a job.
//
// argv rather than an environment variable, because the child's environment is
// built from nothing — a variable would not survive the confinement, and being
// unable to smuggle one in is part of what is being tested elsewhere.
const dialSentinel = "--jarvix-test-drive-daemon"

// serveTestDrive is the attacker, and it is deliberately a competent one: it
// speaks the daemon's own wire format at the daemon's own socket and asks for
// the one thing #109's wall exists to prevent.
func serveTestDrive() {
	if len(os.Args) < 3 || os.Args[1] != dialSentinel {
		return
	}
	conn, err := net.DialTimeout("unix", os.Args[2], 5*time.Second)
	if err != nil {
		fmt.Printf("DRIVE-REFUSED: %v\n", err)
		os.Exit(3)
	}
	frame, _ := json.Marshal(ipcRequest{JSONRPC: "2.0", ID: 1, Method: "config.set",
		Params: map[string]any{"key": "tools.shell", "value": true}})
	_, _ = conn.Write(append(frame, '\n'))
	// Read the reply so the exit code says whether the daemon actually answered,
	// rather than only whether the bytes left this process.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 512)
	n, _ := conn.Read(buf)
	_ = conn.Close()
	fmt.Printf("DRIVE-CONNECTED: %s\n", strings.TrimSpace(string(buf[:n])))
	os.Exit(0)
}

// ipcRequest is the wire shape, spelled out here rather than imported, because
// the attacker in this test should need nothing from Jarvix's own packages —
// anything that can write JSON to a socket can do what it does.
type ipcRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int            `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

// standIn is a daemon-side setting a confined command must not be able to
// change, and a record of whether anybody asked.
type standIn struct {
	mu      sync.Mutex
	asked   int
	setting string
}

func (s *standIn) handle(json.RawMessage) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked++
	s.setting = "rewritten by whoever asked"
	return map[string]any{"ok": true}, nil
}

// unchanged is the whole assertion: nobody asked, and nothing moved.
func (s *standIn) unchanged(t *testing.T, was string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.asked != 0 {
		t.Errorf("the daemon handled %d config writes from a job's command; it must handle none",
			s.asked)
	}
	if s.setting != was {
		t.Errorf("the setting now reads %q, want %q — a job's command reconfigured Jarvix",
			s.setting, was)
	}
}

// listeningDaemon builds the usual command daemon and puts a real ipc.Server on
// its real socket path, with one method that changes something.
func listeningDaemon(t *testing.T) (*Daemon, string, *standIn) {
	t.Helper()
	d, root, _ := commandDaemon(t)
	state := &standIn{setting: "as the user left it"}
	server := ipc.NewServer(d.paths.Socket, nil, nil)
	server.Handle("config.set", state.handle)
	if err := server.Listen(); err != nil {
		t.Fatalf("the test's own daemon socket would not bind, so nothing here is proved: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = server.Serve(ctx) }()
	t.Cleanup(server.Close)
	return d, root, state
}

// driveCommand copies this test binary into the job's root — the confinement
// has to have something inside its own boundary to exec — and returns the
// command line that points it at the daemon.
func driveCommand(t *testing.T, root, socket string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(root, "drive")
	if err := os.WriteFile(probe, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return probe + " " + dialSentinel + " " + socket
}

// waitForSocket makes one connection of the test's own, so that everything the
// command might have sent has already been handled by the time the assertion
// runs.
//
// It is a happens-before edge rather than a pause. ipc.Server dispatches each
// connection's requests in order on that connection's own goroutine, and this
// one is opened AFTER the command has exited — so a request the command had
// managed to send would have been accepted before this connection was, and the
// reply to this one cannot arrive until the server has got that far.
func waitForSocket(t *testing.T, socket string) {
	t.Helper()
	client, err := ipc.Dial(socket)
	if err != nil {
		t.Fatalf("the test could not reach its own daemon, so it can prove nothing: %v", err)
	}
	defer func() { _ = client.Close() }()
	// Any method will do; an unknown one is answered by the same dispatch loop.
	_ = client.Call("no.such.method", nil, nil)
}

// TestAJobsCommandCannotDriveTheDaemonThroughItsSocket.
//
// The wall that keeps a command out of config.toml has to keep it from asking
// the daemon to rewrite config.toml, or #109's wall is reachable through a
// shell and the confinement is a net loss.
func TestAJobsCommandCannotDriveTheDaemonThroughItsSocket(t *testing.T) {
	boundaryOrSkip(t)
	d, root, state := listeningDaemon(t)
	command := driveCommand(t, root, d.paths.Socket)

	result, err := runStep(t, d, commandScope(t, root), command)
	if err != nil {
		t.Fatalf("the command could not be run at all, so this test proved nothing: %v", err)
	}
	waitForSocket(t, d.paths.Socket)
	state.unchanged(t, "as the user left it")

	if strings.Contains(result.Said, "DRIVE-CONNECTED") {
		t.Errorf("a job's command reached the daemon: %q", result.Said)
	}
	if !strings.Contains(result.Said, "DRIVE-REFUSED") {
		t.Errorf("said = %q, want the command's own account of being refused — without it "+
			"this test cannot tell a wall from a command that never started", result.Said)
	}
	if !result.Failed {
		t.Errorf("result = %+v, want the step recorded as failed so no report can claim it",
			result)
	}
}

// TestTheSameCommandDrivesTheDaemonWhenItIsNotConfined is the control, and
// without it the test above would pass on a machine where the probe never ran,
// the socket never bound, or the method was never registered.
//
// It is also the honest statement of what this wall is and is not: the daemon's
// socket has no authentication, and a program of the same user that is NOT
// inside a job's confinement can still drive it exactly as it always could.
// That is the CLI and the window working; it is also the limit of the claim.
func TestTheSameCommandDrivesTheDaemonWhenItIsNotConfined(t *testing.T) {
	d, root, state := listeningDaemon(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, _ := runPlainCommand(t, self, dialSentinel, d.paths.Socket)
	if !strings.Contains(out, "DRIVE-CONNECTED") {
		t.Fatalf("the unconfined probe did not reach the daemon: %q", out)
	}
	waitForSocket(t, d.paths.Socket)
	state.mu.Lock()
	asked := state.asked
	state.mu.Unlock()
	if asked == 0 {
		t.Fatal("the unconfined probe reached the socket but the daemon never handled its " +
			"request, so the confined test would pass whether or not the wall exists")
	}
	_ = root
}

// TestEveryExistingClientStillReachesTheDaemon.
//
// The wall is in the confined child, not in the daemon, and this is what that
// buys: nothing about how a client connects has changed, so the CLI's own
// transport reaches the socket with no handshake, no key and no new step. The
// Quickshell window writes the same newline-delimited JSON-RPC by hand over the
// same path, which is why it needed no change either — a raw connection is used
// here as its stand-in, because the property being pinned is that a plain
// socket write still works.
func TestEveryExistingClientStillReachesTheDaemon(t *testing.T) {
	d, _, state := listeningDaemon(t)

	// The Go transport every CLI verb and `jarvix backup` use.
	client, err := ipc.Dial(d.paths.Socket)
	if err != nil {
		t.Fatalf("the CLI's own transport can no longer reach the daemon: %v", err)
	}
	var out map[string]any
	if err := client.Call("config.set", map[string]any{"key": "x"}, &out); err != nil {
		t.Fatalf("the CLI's own transport was refused: %v", err)
	}
	_ = client.Close()

	// The window's transport: a bare connection and a hand-written frame.
	conn, err := net.DialTimeout("unix", d.paths.Socket, 5*time.Second)
	if err != nil {
		t.Fatalf("a plain socket write can no longer reach the daemon, which is how the "+
			"Quickshell window talks to it: %v", err)
	}
	frame, _ := json.Marshal(ipcRequest{JSONRPC: "2.0", ID: 7, Method: "config.set"})
	if _, err := conn.Write(append(frame, '\n')); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	_ = conn.Close()
	if err != nil || n == 0 {
		t.Fatalf("the daemon did not answer a hand-written frame: %v", err)
	}
	if !strings.Contains(string(buf[:n]), `"result"`) {
		t.Errorf("reply = %q, want the daemon's ordinary answer", buf[:n])
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.asked != 2 {
		t.Errorf("the daemon handled %d requests from its two existing clients, want 2",
			state.asked)
	}
}

// TestNoConfinableScopeCanContainTheDaemonsSocket.
//
// The seccomp filter is the wall, and this is the second lock on the same door:
// a scope whose root reached the socket's directory would also reach Jarvix's
// configuration, and confine.Spec.Check refuses it. The two roots below are the
// ones that would matter — the runtime directory itself, and `/`, which is the
// case a prefix test written the obvious way silently fails to catch.
func TestNoConfinableScopeCanContainTheDaemonsSocket(t *testing.T) {
	d, _, _ := commandDaemon(t)
	actor := &jobActor{d: d}
	for _, root := range []string{filepath.Dir(d.paths.Socket), "/"} {
		scope := jobs.Scope{Tools: []string{tools.ShellToolName}, Roots: []string{root}}
		_, err := actor.Subject(context.Background(), scope,
			jobs.Step{Tool: tools.ShellToolName, Args: `{"command":"echo hi"}`})
		var unconfinable *jobs.ErrUnconfinable
		if !asJobsUnconfinable(err, &unconfinable) {
			t.Errorf("a job scoped to %q was allowed to run commands; from there it could "+
				"read the daemon's own directory. error = %v", root, err)
		}
	}
}
