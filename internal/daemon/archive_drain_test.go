package daemon

// The daemon half of #74: an intent turn — a routine run, a layout capture —
// does its archive write on runIntent's tail, after session.finished, and the
// shutdown drain must cover it exactly as it covers think()'s history write
// (#29/#42). These tests drive the whole flake as a live daemon: the gated
// conversations.Fake holds the write open so the race is real rather than
// hoped-for, and no test sleeps.

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// bootArchiveDaemon wires a daemon over the given conversation store and
// compositor windows, returning the client plus the explicit stop/stopped
// pair so the test can drive shutdown by hand and assert what was true when
// Run returned.
func bootArchiveDaemon(t *testing.T, cfg config.Config, store conversations.Store,
	tune func(*Daemon), windows ...desktop.Window) (*ipc.Client, context.CancelFunc, chan struct{}, *syncBuffer) {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	logs := &syncBuffer{}
	d, err := New(cfg, paths, slog.New(slog.NewTextHandler(logs, nil)), Deps{
		Provider:          &ai.Fake{Response: "should never be needed"},
		Transcriber:       &stt.Fake{Text: "unused"},
		Synthesizer:       &tts.Fake{},
		Recorder:          &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:            &audio.FakePlayer{},
		Notifier:          &desktop.FakeNotifier{},
		OpenWindow:        func(context.Context) error { return nil },
		Compositor:        desktop.NewFakeCompositor(windows...),
		ConversationStore: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tune != nil {
		tune(d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = d.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-stopped })
	return dialDaemon(t, paths.Socket), cancel, stopped, logs
}

// awaitArchiveAppend waits for the conversation fake to report a completed
// append. The flush runs after session.finished, off the engine's lock path,
// so a test that needs it landed must wait for the store to say so (the
// awaitOp pattern from internal/session).
func awaitArchiveAppend(t *testing.T, fake *conversations.Fake) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case op := <-fake.Ops:
			if op == "append" {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for an archive append")
		}
	}
}

// The production-facing assertion of #74: the daemon is stopped immediately
// after a capture-then-run sequence — the run turn's archive write still in
// flight — and the archive holds both turns. This is the exact shape of the
// flaking loop (TestSpokenCaptureMakesTheRoutineImmediatelyRunnable): the
// capture turn triggers the deferred engine rebuild, the routine runs on the
// rebuilt engine, and the write Run must wait for is the rebuilt engine's.
func TestShutdownAfterCaptureThenRunArchivesBothTurns(t *testing.T) {
	store := conversations.NewFake()
	store.AppendStarted = make(chan struct{}, 4)
	gate := make(chan struct{})
	store.AppendGate = gate
	var once sync.Once
	releaseAll := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(releaseAll)

	cfg := testConfig()
	cfg.Audio.MinRecordingMs = 0
	client, stop, stopped, _ := bootArchiveDaemon(t, cfg, store, nil,
		desktop.Window{Address: "0xs", Class: "sh", Workspace: 2, AcceptsInput: true, Focused: true})

	// Turn one: the spoken capture.
	if err := client.Call("session.text", map[string]string{"text": "save this as morning setup"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "session.finished")
	// The capture turn's own flush is parked; one send releases exactly one
	// Append. It must land before the deferred reload can rebuild the engine —
	// Reconfigure drains session tails first — so wait for the store to report
	// it, then for the rebuild's announcement.
	<-store.AppendStarted
	gate <- struct{}{}
	awaitArchiveAppend(t, store)
	waitForEvent(t, client, "config.changed")

	// Turn two: the captured routine, run on the rebuilt engine.
	if err := client.Call("routines.run", map[string]string{"name": "morning setup"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "session.finished")
	<-store.AppendStarted // the run turn's write is in flight, parked

	// Stop with the write parked, releasing it only once the shutdown drain is
	// already running. Run must not return before the write lands.
	stop()
	releaseAll()
	<-stopped

	if n := store.Appends(); n != 2 {
		t.Errorf("archive saw %d appends by the time Run returned, want 2", n)
	}
	turns := store.Turns(store.Active())
	if len(turns) != 4 {
		t.Fatalf("archive holds %d turns after capture-then-run, want both exchanges' 4", len(turns))
	}
	if turns[0].Text != "save this as morning setup" || turns[2].Text != "morning setup" {
		t.Errorf("archive lost a turn: first %q, third %q", turns[0].Text, turns[2].Text)
	}
}

// The bounded-drain report for the intent tail, and the deterministic
// mutation check at the daemon level: a wedged archive write on a routine
// turn is drained like a wedged history write — bounded by the grace period,
// then named in the journal. Spawn runIntent outside the engine's quiesce
// group again and the drain has nothing to wait for: Run returns without the
// give-up report and every assertion below fails.
func TestShutdownGivesUpOnAWedgedRoutineArchiveWrite(t *testing.T) {
	store := conversations.NewFake()
	store.AppendStarted = make(chan struct{}, 4)
	gate := make(chan struct{})
	store.AppendGate = gate
	t.Cleanup(func() { close(gate) })

	cfg := testConfig()
	cfg.Audio.MinRecordingMs = 0
	cfg.Routines = []config.Routine{{
		Name:    "morning setup",
		Phrases: []string{"morning setup"},
		Steps:   []config.RoutineStep{{App: "firefox", Workspace: 2}},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	client, stop, stopped, logs := bootArchiveDaemon(t, cfg, store,
		func(d *Daemon) { d.shutdownGrace = time.Millisecond },
		desktop.Window{Address: "0xf", Class: "firefox", Workspace: 5})

	if err := client.Call("routines.run", map[string]string{"name": "morning setup"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "session.finished")
	<-store.AppendStarted // wedged: never released before Run must exit

	stop()
	<-stopped // the bound is the assertion: a wedged write is not a hung daemon

	logged := logs.String()
	if !strings.Contains(logged, "shutdown drain gave up waiting") {
		t.Errorf("shutdown gave up silently; log was:\n%s", logged)
	}
	if !strings.Contains(logged, "stage=sessions") {
		t.Errorf("the log does not name the sessions stage:\n%s", logged)
	}
	if !strings.Contains(logged, "outstanding=1") {
		t.Errorf("the log does not count the parked archive flush:\n%s", logged)
	}
}
