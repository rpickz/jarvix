package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// The state.hold/state.release verbs (ADR 0045): the daemon-side half of
// `jarvix backup`'s consistency story. What is pinned here is the contract
// the backup CLI relies on — hold answered means nothing under the state
// root moves until release or TTL — proven through the same socket the CLI
// uses, against the daemon's real stores.

// startStateDaemon boots a hermetic daemon and dials it twice: holding and
// writing must ride separate connections, because each connection dispatches
// serially and a parked write must never be able to queue the release that
// would unpark it behind itself.
func startStateDaemon(t *testing.T) (holder, writer *ipc.Client) {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	cfg := testConfig()
	cfg.Audio.MinRecordingMs = 0
	d, err := New(cfg, paths, nil, Deps{
		Provider:    &ai.Fake{Response: "Understood."},
		Transcriber: &stt.Fake{Text: "hello"},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
		Compositor:  desktop.NewFakeCompositor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	return dialDaemon(t, paths.Socket), dialDaemon(t, paths.Socket)
}

func TestStateHoldAndReleaseRoundTrip(t *testing.T) {
	client, _ := startStateDaemon(t)

	var held map[string]any
	if err := client.Call("state.hold", nil, &held); err != nil {
		t.Fatal(err)
	}
	if held["held"] != true {
		t.Errorf("state.hold = %v, want held: true", held)
	}
	if held["ttl_ms"] != float64(DefaultHoldTTL.Milliseconds()) {
		t.Errorf("ttl_ms = %v, want the default %d", held["ttl_ms"], DefaultHoldTTL.Milliseconds())
	}

	// One backup at a time: a second hold is refused, with the reason.
	if err := client.Call("state.hold", nil, nil); err == nil ||
		!strings.Contains(err.Error(), "already held") {
		t.Errorf("second hold: got %v, want an already-held refusal", err)
	}

	var released map[string]any
	if err := client.Call("state.release", nil, &released); err != nil {
		t.Fatal(err)
	}
	if released["held"] != false {
		t.Errorf("state.release = %v, want held: false", released)
	}
	// Releasing again is idempotent — the caller's question is "may writes
	// flow?", answerable identically however many times it is asked.
	if err := client.Call("state.release", nil, nil); err != nil {
		t.Errorf("repeated release errored: %v", err)
	}
	// And a fresh hold works after release.
	if err := client.Call("state.hold", nil, nil); err != nil {
		t.Errorf("hold after release: %v", err)
	}
}

func TestStateHoldRefusesAbsurdTTL(t *testing.T) {
	client, _ := startStateDaemon(t)
	err := client.Call("state.hold", map[string]any{"ttl_ms": MaxHoldTTL.Milliseconds() * 10}, nil)
	if err == nil || !strings.Contains(err.Error(), "ttl_ms") {
		t.Errorf("got %v, want a ttl_ms refusal", err)
	}
}

// TestStateHoldBlocksAStoreWriteUntilRelease drives a real store write into
// the held gate over the socket: memory.add from the second connection parks
// until the first connection releases — the write is delayed, never lost.
func TestStateHoldBlocksAStoreWriteUntilRelease(t *testing.T) {
	holder, writer := startStateDaemon(t)

	if err := holder.Call("state.hold", nil, nil); err != nil {
		t.Fatal(err)
	}
	added := make(chan error, 1)
	go func() {
		added <- writer.Call("memory.add", map[string]any{"content": "written under hold"}, nil)
	}()
	select {
	case err := <-added:
		t.Fatalf("memory.add completed while state was held (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}

	if err := holder.Call("state.release", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-added:
		if err != nil {
			t.Fatalf("memory.add failed after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("memory.add still blocked after release")
	}

	var list map[string]any
	if err := writer.Call("memory.list", nil, &list); err != nil {
		t.Fatal(err)
	}
	facts, _ := list["facts"].([]any)
	if len(facts) != 1 {
		t.Errorf("facts = %v, want the one written under hold", list["facts"])
	}
}

// The TTL is the safety net for a backup that dies mid-copy: the gate
// reopens on its own and the parked write completes.
func TestStateHoldTTLReleasesOnItsOwn(t *testing.T) {
	holder, writer := startStateDaemon(t)

	if err := holder.Call("state.hold", map[string]any{"ttl_ms": 100}, nil); err != nil {
		t.Fatal(err)
	}
	// Issued while held; must complete once the TTL fires, with no release.
	if err := writer.Call("memory.add", map[string]any{"content": "outlives the ttl"}, nil); err != nil {
		t.Fatalf("memory.add after TTL expiry: %v", err)
	}
}
