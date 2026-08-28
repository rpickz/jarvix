package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ipc"
)

func TestRunApprovalsListRendersEachGrant(t *testing.T) {
	hermeticEnv(t)
	startDaemon(t, nil, map[string]ipc.Handler{
		"approvals.list": func(json.RawMessage) (any, error) {
			return map[string]any{
				"path": "/home/me/.config/jarvix/config.toml",
				"approved": []map[string]any{
					{"pattern": "docker ps", "source": "card", "scope": "always",
						"uses": 4, "added": "2026-08-20T09:30:00Z",
						"last_used": "2026-08-27T11:00:00Z"},
					{"pattern": "kubectl get pods", "source": "hand", "scope": "always", "uses": 0},
					{"pattern": "jq", "source": "card", "scope": "conversation", "uses": 0},
				},
			}, nil
		},
	})
	var code int
	stdout, stderr := capture(t, func() { code = run([]string{"approvals", "list"}) })
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"docker ps",
		"added 2026-08-20",
		"used 4 times, last 2026-08-27",
		"kubectl get pods",
		"added by hand",
		// The row that most deserves revoking says so.
		"never used",
		"jq",
		"just this conversation",
		"forget one with: jarvix approvals forget <pattern>",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("listing is missing %q:\n%s", want, stdout)
		}
	}
}

func TestRunApprovalsListWithNothingGranted(t *testing.T) {
	hermeticEnv(t)
	startDaemon(t, nil, map[string]ipc.Handler{
		"approvals.list": func(json.RawMessage) (any, error) {
			return map[string]any{"path": "/x", "approved": []map[string]any{}}, nil
		},
	})
	var code int
	stdout, stderr := capture(t, func() { code = run([]string{"approvals"}) })
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "every command still asks first") {
		t.Errorf("output = %q", stdout)
	}
}

func TestRunApprovalsForgetJoinsTheWords(t *testing.T) {
	hermeticEnv(t)
	startDaemon(t, nil, map[string]ipc.Handler{
		"approvals.forget": func(params json.RawMessage) (any, error) {
			var p struct {
				Pattern string `json:"pattern"`
			}
			_ = json.Unmarshal(params, &p)
			// Unquoted words arrive as separate arguments and must be
			// rejoined: a pattern is words.
			if p.Pattern != "docker ps" {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "pattern = %q", p.Pattern)
			}
			return map[string]any{"forgotten": true, "pattern": "docker ps", "scope": "always"}, nil
		},
	})
	var code int
	stdout, stderr := capture(t, func() { code = run([]string{"approvals", "forget", "docker", "ps"}) })
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "forgotten: docker ps") ||
		!strings.Contains(stdout, "will ask again") {
		t.Errorf("output = %q", stdout)
	}
}

func TestRunApprovalsForgetUnknownPatternListsWhatThereIs(t *testing.T) {
	hermeticEnv(t)
	startDaemon(t, nil, map[string]ipc.Handler{
		"approvals.forget": func(json.RawMessage) (any, error) {
			return map[string]any{"forgotten": false, "pattern": "nope",
				"approved": []map[string]any{{"pattern": "docker ps"}}}, nil
		},
	})
	var code int
	stdout, stderr := capture(t, func() { code = run([]string{"approvals", "forget", "nope"}) })
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "nothing was forgotten") ||
		!strings.Contains(stdout, "docker ps") {
		t.Errorf("output = %q", stdout)
	}
}

// There is no `approvals add`, and the usage line says the two verbs there
// are. A standing grant is made on the confirmation card, where the exact
// rule is shown beside the command that provoked it.
func TestRunApprovalsUsage(t *testing.T) {
	hermeticEnv(t)
	for _, args := range [][]string{
		{"approvals", "forget"}, // forget needs a pattern
		{"approvals", "add", "docker", "ps"},
		{"approvals", "allow", "docker"},
		{"approvals", "wipe"},
	} {
		var code int
		_, stderr := capture(t, func() { code = run(args) })
		if code != 1 || !strings.Contains(stderr, "usage: jarvix approvals") {
			t.Errorf("run(%v): exit = %d, stderr = %q", args, code, stderr)
		}
	}
}
