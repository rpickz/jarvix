package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/session"
)

// These tests drive run() — the CLI's dispatch seam — end to end. Daemon
// commands talk to a real ipc.Server on a temp socket; nothing external runs.

// hermeticEnv points every XDG root at temp directories so run() can never
// touch the real config, state, or daemon socket.
func hermeticEnv(t *testing.T) {
	t.Helper()
	for _, env := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR"} {
		t.Setenv(env, t.TempDir())
	}
}

// capture runs fn with stdout/stderr redirected and returns what it printed.
func capture(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	readAll := func(r *os.File, into *string, wg *sync.WaitGroup) {
		defer wg.Done()
		data, _ := io.ReadAll(r)
		*into = string(data)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wOut, wErr
	var wg sync.WaitGroup
	wg.Add(2)
	go readAll(rOut, &stdout, &wg)
	go readAll(rErr, &stderr, &wg)
	defer func() {
		os.Stdout, os.Stderr = oldOut, oldErr
	}()
	fn()
	_ = wOut.Close()
	_ = wErr.Close()
	wg.Wait()
	return stdout, stderr
}

// callRecorder tracks which RPC methods a command invoked.
type callRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (c *callRecorder) record(method string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, method)
}

func (c *callRecorder) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// startDaemon runs a real IPC server at this test's socket path.
func startDaemon(t *testing.T, bus *session.Bus, handlers map[string]ipc.Handler) *callRecorder {
	t.Helper()
	rec := &callRecorder{}
	sock := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "jarvix.sock")
	srv := ipc.NewServer(sock, bus, nil)
	for method, h := range handlers {
		method, h := method, h
		srv.Handle(method, func(params json.RawMessage) (any, error) {
			rec.record(method)
			return h(params)
		})
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); srv.Close() })
	return rec
}

func ok(json.RawMessage) (any, error) { return nil, nil }

func TestRunWithoutArgsPrintsUsage(t *testing.T) {
	hermeticEnv(t)
	var code int
	stdout, _ := capture(t, func() { code = run(nil) })
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stdout, "Usage:") {
		t.Errorf("stdout = %q, want the usage text", stdout)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	hermeticEnv(t)
	var code int
	_, stderr := capture(t, func() { code = run([]string{"frobnicate"}) })
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, `unknown command "frobnicate"`) {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestRunVersionAndHelp(t *testing.T) {
	hermeticEnv(t)
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		var code int
		stdout, _ := capture(t, func() { code = run(args) })
		if code != 0 || !strings.HasPrefix(stdout, "jarvix ") {
			t.Errorf("run(%v) = %d, stdout %q", args, code, stdout)
		}
	}
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		var code int
		stdout, _ := capture(t, func() { code = run(args) })
		if code != 0 || !strings.Contains(stdout, "Usage:") {
			t.Errorf("run(%v) = %d, stdout %q", args, code, stdout)
		}
	}
}

func TestRunUsageErrors(t *testing.T) {
	hermeticEnv(t)
	cases := map[string][]string{
		"ask without question": {"ask"},
		"ptt without phase":    {"ptt"},
		"ptt bad phase":        {"ptt", "sideways"},
		"setup without target": {"setup"},
		"setup unknown target": {"setup", "bogus"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var code int
			_, stderr := capture(t, func() { code = run(args) })
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if !strings.Contains(stderr, "usage:") {
				t.Errorf("stderr = %q, want usage guidance", stderr)
			}
		})
	}
}

func TestRunConfigShowsDefaults(t *testing.T) {
	hermeticEnv(t)
	var code int
	stdout, _ := capture(t, func() { code = run([]string{"config"}) })
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"built-in defaults", "provider", "model ="} {
		if !strings.Contains(stdout, want) {
			t.Errorf("config output missing %q:\n%s", want, stdout)
		}
	}
}

func TestRunStatusWithoutDaemonFails(t *testing.T) {
	hermeticEnv(t)
	var code int
	_, stderr := capture(t, func() { code = run([]string{"status"}) })
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "is it running?") {
		t.Errorf("stderr = %q, want an actionable dial error", stderr)
	}
}

func TestRunStatusAgainstDaemon(t *testing.T) {
	hermeticEnv(t)
	startDaemon(t, nil, map[string]ipc.Handler{
		"status.get": func(json.RawMessage) (any, error) {
			return map[string]any{"state": "idle", "version": "test", "protocol": 1}, nil
		},
	})
	var code int
	stdout, _ := capture(t, func() { code = run([]string{"status"}) })
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "state:    idle") || !strings.Contains(stdout, "version:  test") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestRunCancelAndNewConversation(t *testing.T) {
	hermeticEnv(t)
	rec := startDaemon(t, nil, map[string]ipc.Handler{
		"session.cancel":     ok,
		"conversation.reset": ok,
	})
	var code int
	capture(t, func() { code = run([]string{"cancel"}) })
	if code != 0 {
		t.Fatalf("cancel exit = %d", code)
	}
	stdout, _ := capture(t, func() { code = run([]string{"new"}) })
	if code != 0 {
		t.Fatalf("new exit = %d", code)
	}
	if !strings.Contains(stdout, "fresh conversation") {
		t.Errorf("stdout = %q", stdout)
	}
	calls := rec.recorded()
	if len(calls) != 2 || calls[0] != "session.cancel" || calls[1] != "conversation.reset" {
		t.Errorf("calls = %v", calls)
	}
}

func TestRunAskFollowsSessionToTheEnd(t *testing.T) {
	hermeticEnv(t)
	bus := session.NewBus(nil)
	var gotText string
	var mu sync.Mutex
	rec := startDaemon(t, bus, map[string]ipc.Handler{
		"session.start": ok,
		"session.submit": func(params json.RawMessage) (any, error) {
			var p struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(params, &p)
			mu.Lock()
			gotText = p.Text
			mu.Unlock()
			// The daemon answers asynchronously after accepting the submit.
			go func() {
				bus.Publish(session.Event{Type: "assistant.delta", Data: map[string]any{"content": "The answer"}})
				bus.Publish(session.Event{Type: "assistant.delta", Data: map[string]any{"content": " is 42."}})
				bus.Publish(session.Event{Type: "assistant.finished", Data: map[string]any{"content": "The answer is 42."}})
				bus.Publish(session.Event{Type: "session.finished", Data: map[string]any{}})
			}()
			return nil, nil
		},
	})

	var code int
	stdout, _ := capture(t, func() { code = run([]string{"ask", "what is the answer?"}) })
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "The answer is 42.") {
		t.Errorf("stdout = %q, want the streamed answer", stdout)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotText != "what is the answer?" {
		t.Errorf("submitted text = %q", gotText)
	}
	calls := rec.recorded()
	if len(calls) != 2 || calls[0] != "session.start" || calls[1] != "session.submit" {
		t.Errorf("calls = %v", calls)
	}
}

func TestRunAskSurfacesSessionErrors(t *testing.T) {
	hermeticEnv(t)
	bus := session.NewBus(nil)
	startDaemon(t, bus, map[string]ipc.Handler{
		"session.start": ok,
		"session.submit": func(json.RawMessage) (any, error) {
			go bus.Publish(session.Event{Type: "error", Data: map[string]any{
				"message": "model exploded", "stage": "assistant"}})
			return nil, nil
		},
	})
	var code int
	_, stderr := capture(t, func() { code = run([]string{"ask", "boom"}) })
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "model exploded") || !strings.Contains(stderr, "assistant") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestRunPTTStartBeginsListening(t *testing.T) {
	hermeticEnv(t)
	rec := startDaemon(t, nil, map[string]ipc.Handler{
		"session.start": ok,
		"voice.start":   ok,
	})
	var code int
	capture(t, func() { code = run([]string{"ptt", "start"}) })
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	calls := rec.recorded()
	if len(calls) != 2 || calls[0] != "session.start" || calls[1] != "voice.start" {
		t.Errorf("calls = %v", calls)
	}
}

func TestRunPTTStopSubmits(t *testing.T) {
	hermeticEnv(t)
	rec := startDaemon(t, nil, map[string]ipc.Handler{
		"voice.stop":     func(json.RawMessage) (any, error) { return map[string]any{"discarded": false}, nil },
		"session.submit": ok,
	})
	var code int
	capture(t, func() { code = run([]string{"ptt", "stop"}) })
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	calls := rec.recorded()
	if len(calls) != 2 || calls[0] != "voice.stop" || calls[1] != "session.submit" {
		t.Errorf("calls = %v", calls)
	}
}

func TestRunPTTStopSkipsSubmitWhenDiscarded(t *testing.T) {
	hermeticEnv(t)
	rec := startDaemon(t, nil, map[string]ipc.Handler{
		"voice.stop":     func(json.RawMessage) (any, error) { return map[string]any{"discarded": true}, nil },
		"session.submit": ok,
	})
	var code int
	capture(t, func() { code = run([]string{"ptt", "stop"}) })
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	calls := rec.recorded()
	if len(calls) != 1 || calls[0] != "voice.stop" {
		t.Errorf("calls = %v, want only voice.stop for a discarded tap", calls)
	}
}

func TestRunPTTToggle(t *testing.T) {
	cases := map[string]struct {
		status    map[string]any
		wantCalls []string
	}{
		"idle starts listening": {
			status:    map[string]any{"state": "idle", "ptt": "cli"},
			wantCalls: []string{"status.get", "session.start", "voice.start"},
		},
		"listening submits": {
			status:    map[string]any{"state": "listening", "ptt": "cli"},
			wantCalls: []string{"status.get", "voice.stop", "session.submit"},
		},
		"daemon-owned chord is a no-op": {
			status:    map[string]any{"state": "idle", "ptt": "daemon"},
			wantCalls: []string{"status.get"},
		},
		"speaking interrupts into listening": {
			status:    map[string]any{"state": "speaking", "ptt": "cli"},
			wantCalls: []string{"status.get", "session.start", "voice.start"},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			hermeticEnv(t)
			rec := startDaemon(t, nil, map[string]ipc.Handler{
				"status.get":     func(json.RawMessage) (any, error) { return c.status, nil },
				"session.start":  ok,
				"voice.start":    ok,
				"session.submit": ok,
				"voice.stop":     func(json.RawMessage) (any, error) { return map[string]any{"discarded": false}, nil },
			})
			var code int
			capture(t, func() { code = run([]string{"ptt", "toggle"}) })
			if code != 0 {
				t.Fatalf("exit = %d", code)
			}
			calls := rec.recorded()
			if strings.Join(calls, ",") != strings.Join(c.wantCalls, ",") {
				t.Errorf("calls = %v, want %v", calls, c.wantCalls)
			}
		})
	}
}

func TestSplitLines(t *testing.T) {
	got := splitLines("one\ntwo\nthree")
	if len(got) != 3 || got[0] != "one" || got[2] != "three" {
		t.Errorf("splitLines = %q", got)
	}
	if got := splitLines("single"); len(got) != 1 || got[0] != "single" {
		t.Errorf("splitLines = %q", got)
	}
}
