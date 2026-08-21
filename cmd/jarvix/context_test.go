package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/session"
)

// `jarvix status --last` is the audit surface: after any interaction the user
// can ask what it cost *and* what Jarvix saw, and get the exact text that was
// sent. Both halves come from the one flag, so both are asserted together.

// idleStatus is a minimal status.get payload with a latency report, so these
// tests exercise the merged --last path rather than half of it.
func idleStatus(json.RawMessage) (any, error) {
	return map[string]any{
		"state": "idle", "version": "test", "protocol": 1,
		"last_timings": map[string]any{
			"session_id":                     "s4",
			session.StageContext:             2,
			session.StageReleaseToFirstAudio: 900,
		},
	}, nil
}

func TestRunStatusLastShowsWhatWasCaptured(t *testing.T) {
	hermeticEnv(t)
	startDaemon(t, nil, map[string]ipc.Handler{
		"status.get": idleStatus,
		"context.last": func(json.RawMessage) (any, error) {
			return map[string]any{
				"captured":    true,
				"session_id":  "s4",
				"age_sec":     0,
				"duration_ms": 2,
				"sources": []map[string]any{
					{"source": "window", "text": "Alacritty — nvim engine.go", "chars": 26},
					{"source": "selection", "text": "panic: index out of range", "chars": 4000, "truncated": true},
					{"source": "clipboard", "text": "[looks like a secret — not shared]", "chars": 64, "redacted": true},
				},
			}, nil
		},
	})
	var code int
	stdout, stderr := capture(t, func() { code = run([]string{"status", "--last"}) })
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		// The latency budget…
		"desktop context gathered", "release → first audio (total)",
		// …and what was gathered, verbatim.
		"context:  session s4", "gathered in 2ms",
		"Alacritty — nvim engine.go",
		"panic: index out of range", "truncated",
		"[looks like a secret — not shared]", "redacted",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}
}

func TestRunStatusLastWithNothingCaptured(t *testing.T) {
	hermeticEnv(t)
	startDaemon(t, nil, map[string]ipc.Handler{
		"status.get": idleStatus,
		"context.last": func(json.RawMessage) (any, error) {
			return map[string]any{"captured": false}, nil
		},
	})
	var code int
	stdout, stderr := capture(t, func() { code = run([]string{"status", "--last"}) })
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "no desktop context has been captured yet") {
		t.Errorf("output = %q", stdout)
	}
}

func TestRunStatusWithoutLastSkipsTheContextCall(t *testing.T) {
	hermeticEnv(t)
	// Plain `jarvix status` must not ask what Jarvix saw: the capture is shown
	// when it is asked for, not printed at anyone who checks the daemon state.
	rec := startDaemon(t, nil, map[string]ipc.Handler{
		"status.get": idleStatus,
		"context.last": func(json.RawMessage) (any, error) {
			return map[string]any{"captured": false}, nil
		},
	})
	var code int
	stdout, _ := capture(t, func() { code = run([]string{"status"}) })
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(stdout, "context:") {
		t.Errorf("plain status printed context:\n%s", stdout)
	}
	if calls := strings.Join(rec.recorded(), ","); calls != "status.get" {
		t.Errorf("calls = %q, want status.get only", calls)
	}
}

func TestRunStatusRejectsUnknownFlags(t *testing.T) {
	hermeticEnv(t)
	var code int
	_, stderr := capture(t, func() { code = run([]string{"status", "--everything"}) })
	if code != 1 || !strings.Contains(stderr, "usage:") {
		t.Errorf("exit = %d, stderr = %q", code, stderr)
	}
}
