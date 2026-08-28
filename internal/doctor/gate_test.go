package doctor

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ipc"
)

// The gate is the subset of doctor that decides an upgrade's fate (#139).
// These tests pin its contract: the same check functions the doctor runs,
// covering socket, protocol, and both voice-loop probes — and nothing that
// could roll back a good build for an unrelated reason (no network, no
// PipeWire, no plugin checks).

func TestGateChecksOnHealthyMachineAreGreen(t *testing.T) {
	cfg, paths := healthyWorld(t)
	results := GateChecks(cfg, paths)

	want := []string{
		"jarvixd running",
		"protocol match",
		"whisper.cpp transcribes",
		"piper synthesizes",
	}
	if len(results) != len(want) {
		t.Fatalf("gate ran %d checks, want %d: %+v", len(results), len(want), results)
	}
	for i, name := range want {
		if results[i].Name != name {
			t.Errorf("check %d = %q, want %q", i, results[i].Name, name)
		}
		if results[i].Status != OK {
			t.Errorf("%s: status %v, detail %q", name, results[i].Status, results[i].Detail)
		}
	}
	if !Healthy(results) {
		t.Errorf("healthy machine failed the gate: %+v", results)
	}
}

func TestGateChecksFailWhenDaemonIsDown(t *testing.T) {
	cfg, paths := healthyWorld(t)
	paths.Socket = filepath.Join(t.TempDir(), "nobody-home.sock")

	results := GateChecks(cfg, paths)
	if Healthy(results) {
		t.Fatalf("gate passed with no daemon: %+v", results)
	}
	daemon := resultByName(t, results, "jarvixd running")
	if daemon.Status != Fail {
		t.Errorf("jarvixd running = %+v", daemon)
	}
	proto := resultByName(t, results, "protocol match")
	if proto.Status != Fail {
		t.Errorf("protocol match = %+v", proto)
	}
}

// A torn install — CLI and daemon from different build generations — is
// exactly what an interrupted upgrade leaves behind, and the mismatch must
// be named, not surface later as a strange call failure.
func TestProtocolMismatchFailsTheGate(t *testing.T) {
	cfg, paths := healthyWorld(t)
	paths.Socket = filepath.Join(t.TempDir(), "mismatch.sock")
	srv := ipc.NewServer(paths.Socket, nil, nil)
	srv.Handle("status.get", func(json.RawMessage) (any, error) {
		return map[string]any{"state": "idle", "version": "test", "protocol": 99}, nil
	})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); srv.Close() })

	r := checkProtocol(cfg, paths)
	if r.Status != Fail {
		t.Fatalf("mismatched protocol passed: %+v", r)
	}
	if !strings.Contains(r.Detail, "protocol 99") || !strings.Contains(r.Detail, "mismatched") {
		t.Errorf("detail does not name the mismatch: %q", r.Detail)
	}
}
