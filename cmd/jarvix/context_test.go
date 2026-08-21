package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ipc"
)

// `jarvix status --last` is the audit surface: after any session the user can
// ask what Jarvix saw and get the exact text that was sent.

func TestRunStatusLastShowsWhatWasCaptured(t *testing.T) {
	hermeticEnv(t)
	startDaemon(t, nil, map[string]ipc.Handler{
		"context.last": func(json.RawMessage) (any, error) {
			return map[string]any{
				"captured":    true,
				"session_id":  "s4",
				"age_sec":     0,
				"duration_ms": 18,
				"sources": []map[string]any{
					{"source": "window", "text": "Alacritty — nvim engine.go", "chars": 26},
					{"source": "selection", "text": "panic: index out of range", "chars": 4000, "truncated": true},
					{"source": "clipboard", "text": "[looks like a secret — not shared]", "chars": 64, "redacted": true},
				},
			}, nil
		},
	})
	var code int
	stdout, _ := capture(t, func() { code = run([]string{"status", "--last"}) })
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{
		"session s4", "gathered in 18ms",
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
		"context.last": func(json.RawMessage) (any, error) {
			return map[string]any{"captured": false}, nil
		},
	})
	var code int
	stdout, _ := capture(t, func() { code = run([]string{"status", "--last"}) })
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "no desktop context") {
		t.Errorf("output = %q", stdout)
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
