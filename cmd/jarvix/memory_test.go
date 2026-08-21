package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ipc"
)

// `jarvix memory` is the CLI half of hearing and correcting the knowledge
// base (ADR 0025), and `jarvix status --last` gains the fourth audit line:
// which remembered facts the model was just given. The daemon methods have
// their own tests; here the rendering and the argument handling are pinned.

func TestRunMemoryList(t *testing.T) {
	hermeticEnv(t)
	startDaemon(t, nil, map[string]ipc.Handler{
		"memory.list": func(params json.RawMessage) (any, error) {
			var p struct {
				Query string `json:"query"`
			}
			_ = json.Unmarshal(params, &p)
			if p.Query != "" {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "unexpected query %q", p.Query)
			}
			return map[string]any{
				"enabled": true,
				"path":    "/home/u/.local/state/jarvix/memory.toml",
				"count":   2, "max": 200,
				"facts": []map[string]any{
					{"id": "m2", "content": "the staging server is called helios",
						"stored": "2026-08-01T10:00:00Z", "updated": "2026-08-21T09:00:00Z",
						"previous": []map[string]any{{
							"content": "the staging server is called atlas",
							"stored":  "2026-08-01T10:00:00Z", "superseded": "2026-08-21T09:00:00Z",
						}}},
					{"id": "m1", "content": "the user's editor is neovim",
						"stored": "2026-08-01T10:00:00Z", "updated": "2026-08-01T10:00:00Z"},
				},
			}, nil
		},
	})
	var code int
	stdout, stderr := capture(t, func() { code = run([]string{"memory", "list"}) })
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"m2", "helios", "updated 2026-08-21",
		// The supersede trail answers "when did that change".
		`previously "the staging server is called atlas"`, "2026-08-01 to 2026-08-21",
		"m1", "neovim",
		// The store is the user's: the listing says where the file is.
		"2 of 200 facts", "memory.toml",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}
}

func TestRunMemoryListQueryIsJoinedFromArgs(t *testing.T) {
	hermeticEnv(t)
	startDaemon(t, nil, map[string]ipc.Handler{
		"memory.list": func(params json.RawMessage) (any, error) {
			var p struct {
				Query string `json:"query"`
			}
			_ = json.Unmarshal(params, &p)
			if p.Query != "staging server" {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "query = %q", p.Query)
			}
			return map[string]any{"enabled": true, "facts": []map[string]any{}}, nil
		},
	})
	var code int
	stdout, stderr := capture(t, func() { code = run([]string{"memory", "list", "staging", "server"}) })
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, `no remembered fact matches "staging server"`) {
		t.Errorf("output = %q", stdout)
	}
}

func TestRunMemoryForgetSendsIDForIDs(t *testing.T) {
	hermeticEnv(t)
	startDaemon(t, nil, map[string]ipc.Handler{
		"memory.forget": func(params json.RawMessage) (any, error) {
			var p struct {
				ID    string `json:"id"`
				Query string `json:"query"`
			}
			_ = json.Unmarshal(params, &p)
			if p.ID != "m3" || p.Query != "" {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "params = %+v", p)
			}
			return map[string]any{"forgotten": true,
				"fact": map[string]any{"id": "m3", "content": "the staging server is called atlas"}}, nil
		},
	})
	var code int
	stdout, stderr := capture(t, func() { code = run([]string{"memory", "forget", "m3"}) })
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "forgotten: the staging server is called atlas") {
		t.Errorf("output = %q", stdout)
	}
}

func TestRunMemoryForgetAmbiguityListsCandidates(t *testing.T) {
	hermeticEnv(t)
	startDaemon(t, nil, map[string]ipc.Handler{
		"memory.forget": func(params json.RawMessage) (any, error) {
			var p struct {
				Query string `json:"query"`
			}
			_ = json.Unmarshal(params, &p)
			if p.Query != "staging server" {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "query = %q", p.Query)
			}
			return map[string]any{"forgotten": false, "matches": []map[string]any{
				{"id": "m1", "content": "the staging server is called atlas", "updated": "2026-08-01T10:00:00Z"},
				{"id": "m2", "content": "the staging server certificate renews in march", "updated": "2026-08-02T10:00:00Z"},
			}}, nil
		},
	})
	var code int
	stdout, stderr := capture(t, func() { code = run([]string{"memory", "forget", "staging", "server"}) })
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"several facts match", "m1", "m2", "atlas", "certificate"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}
}

func TestRunMemoryUsage(t *testing.T) {
	hermeticEnv(t)
	for _, args := range [][]string{
		{"memory"},
		{"memory", "forget"}, // forget needs a target
		{"memory", "wipe"},
	} {
		var code int
		_, stderr := capture(t, func() { code = run(args) })
		if code != 1 || !strings.Contains(stderr, "usage: jarvix memory") {
			t.Errorf("run(%v): exit = %d, stderr = %q", args, code, stderr)
		}
	}
}

func TestRunStatusLastShowsInjectedMemory(t *testing.T) {
	hermeticEnv(t)
	startDaemon(t, nil, map[string]ipc.Handler{
		"status.get": idleStatus,
		"context.last": func(json.RawMessage) (any, error) {
			return map[string]any{"captured": false}, nil
		},
		"memory.last": func(json.RawMessage) (any, error) {
			return map[string]any{
				"enabled": true, "injected": true, "session_id": "s4",
				"trimmed": 1, "total": 3, "est_tokens": 42,
				"facts": []map[string]any{
					{"id": "m1", "content": "the staging server is called helios"},
					{"id": "m2", "content": "the user's editor is neovim"},
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
		"memory:   session s4", "2 of 3 facts injected", "~42 tokens", "1 trimmed",
		// The facts themselves: the audit shows what was sent, not counts of it.
		"the staging server is called helios", "the user's editor is neovim",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}
}
