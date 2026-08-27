package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ipc"
)

// The window CLI (#126) is a thin client of the windows.* verbs: these tests
// pin the wire calls and the rendering, against a scripted daemon.

func TestRunWindowsListsNicknames(t *testing.T) {
	hermeticEnv(t)
	rec := startDaemon(t, nil, map[string]ipc.Handler{
		"windows.list": func(json.RawMessage) (any, error) {
			return map[string]any{"windows": []map[string]any{
				{"app": "code", "title": "engine.go", "workspace": "1", "focused": true, "nickname": "builds"},
				{"app": "firefox", "title": "GitHub", "workspace": "2", "focused": false},
			}}, nil
		},
	})
	var code int
	stdout, stderr := capture(t, func() { code = run([]string{"windows"}) })
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if got := rec.recorded(); len(got) != 1 || got[0] != "windows.list" {
		t.Errorf("methods called = %v", got)
	}
	for _, want := range []string{"builds", "code — engine.go", "(focused)", "firefox — GitHub", "workspace 2"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, missing %q", stdout, want)
		}
	}
}

func TestRunWindowsNameSendsTheReference(t *testing.T) {
	hermeticEnv(t)
	rec := startDaemon(t, nil, map[string]ipc.Handler{
		"windows.name": func(params json.RawMessage) (any, error) {
			var p struct {
				Name   string `json:"name"`
				Window string `json:"window"`
			}
			_ = json.Unmarshal(params, &p)
			if p.Name != "builds" || p.Window != "the terminal" {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "unexpected params %+v", p)
			}
			return map[string]any{"spoken": "Okay — the Alacritty window is now called builds."}, nil
		},
	})
	var code int
	stdout, stderr := capture(t, func() { code = run([]string{"windows", "name", "builds", "the", "terminal"}) })
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if got := rec.recorded(); len(got) != 1 || got[0] != "windows.name" {
		t.Errorf("methods called = %v", got)
	}
	if !strings.Contains(stdout, "now called builds") {
		t.Errorf("stdout = %q, want the daemon's confirmation printed", stdout)
	}
}

func TestRunWindowsRefusalPrintsTheReason(t *testing.T) {
	hermeticEnv(t)
	startDaemon(t, nil, map[string]ipc.Handler{
		"windows.name": func(json.RawMessage) (any, error) {
			return nil, ipc.Errorf(ipc.CodeSessionError,
				`"mute" is already the built-in intent "volume.mute"; choose a different name`)
		},
	})
	var code int
	_, stderr := capture(t, func() { code = run([]string{"windows", "name", "mute"}) })
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "volume.mute") {
		t.Errorf("stderr = %q, want the collision owner printed", stderr)
	}
}

func TestRunWindowsUsage(t *testing.T) {
	hermeticEnv(t)
	for _, args := range [][]string{
		{"windows", "name"},
		{"windows", "--nope"},
	} {
		var code int
		_, stderr := capture(t, func() { code = run(args) })
		if code != 1 {
			t.Errorf("%v: exit = %d, want 1", args, code)
		}
		if !strings.Contains(stderr, "usage: jarvix windows") {
			t.Errorf("%v: stderr = %q, want usage", args, stderr)
		}
	}
}
