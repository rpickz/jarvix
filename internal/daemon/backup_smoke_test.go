package daemon

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/backup"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// The fresh-machine acceptance criterion (issue #140), end to end: back up a
// live daemon, restore onto empty roots, boot jarvixd against them, and ask
// it — over the socket, exactly as the CLI would — whether the memory,
// vocabulary, threads, conversations, and config made it.

// smokePaths lays the roots out as XDG would: config and state as separate
// trees, so the archive's two-root structure is exercised for real.
func smokePaths(t *testing.T) config.Paths {
	t.Helper()
	dir := t.TempDir()
	return config.Paths{
		Config:  filepath.Join(dir, "config", "jarvix"),
		Data:    filepath.Join(dir, "data", "jarvix"),
		State:   filepath.Join(dir, "state", "jarvix"),
		Runtime: filepath.Join(dir, "run"),
		Socket:  filepath.Join(dir, "run", "jarvix.sock"),
	}
}

// seedSmokeRoots writes one of everything the issue names: config with a
// distinctive setting, remembered facts, taught vocabulary, a focus thread
// with a parked thought, an archived conversation, and rolling history.
func seedSmokeRoots(t *testing.T, paths config.Paths) {
	t.Helper()
	files := map[string]string{
		filepath.Join(paths.Config, "config.toml"): "[memory]\nmax_facts = 123\n",
		filepath.Join(paths.State, "memory.toml"): `version = 1
next_id = 2

[[fact]]
id = "m1"
content = "the staging server is called atlas"
stored = 2026-08-01T10:00:00Z
updated = 2026-08-01T10:00:00Z
`,
		filepath.Join(paths.State, "vocabulary.toml"): `version = 1
next_id = 2

[[entry]]
id = "w1"
phrase = "quid"
meaning = "pounds sterling"
taught = 2026-08-01T10:00:00Z
updated = 2026-08-01T10:00:00Z
`,
		filepath.Join(paths.State, "focus.toml"): `version = 1
next_thread_id = 2
next_parked_id = 2

[[thread]]
id = "t1"
name = "deep work"
created = 2026-08-01T10:00:00Z
last_activity = 2026-08-01T10:00:00Z

[[thread.parked]]
id = "p1"
text = "check the atlas deploy"
at = 2026-08-01T10:00:00Z
`,
		filepath.Join(paths.State, "history.json"):                                `{"version":1,"last_turn":"2026-08-01T10:00:00Z","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"Understood."}]}`,
		filepath.Join(paths.State, "conversations", "20260801-100000-cafe.json"):  `{"schema":1,"id":"20260801-100000-cafe","started":"2026-08-01T10:00:00Z","last":"2026-08-01T10:00:00Z","turns":2,"preview":"hello"}`,
		filepath.Join(paths.State, "conversations", "20260801-100000-cafe.jsonl"): "{\"schema\":1,\"id\":\"20260801-100000-cafe\"}\n{\"role\":\"user\",\"text\":\"hello\",\"ts\":\"2026-08-01T10:00:00Z\"}\n{\"role\":\"assistant\",\"text\":\"Understood.\",\"ts\":\"2026-08-01T10:00:01Z\"}\n",
		filepath.Join(paths.State, "conversations", "active"):                     "20260801-100000-cafe\n",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// bootSmokeDaemon builds a daemon over the given roots from the config file
// actually on disk — the restored machine boots from what the archive put
// there, never from a config this test held in memory — and returns a
// connected client plus an explicit stop.
func bootSmokeDaemon(t *testing.T, paths config.Paths) (*ipc.Client, func()) {
	t.Helper()
	cfg, err := config.Load(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	// The hermetic switches every daemon test flips: no window, selection,
	// or clipboard reads from the machine running the suite.
	cfg.Context.Window = false
	cfg.Context.Selection = false
	cfg.Context.Clipboard = false
	cfg.Audio.MinRecordingMs = 0
	d, err := New(cfg, paths, nil, Deps{
		Provider:    &ai.Fake{Response: "Understood."},
		Transcriber: &stt.Fake{Text: "hello"},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: filepath.Join(t.TempDir(), "r.wav")}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
		Compositor:  desktop.NewFakeCompositor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); _ = d.Run(ctx) }()
	stop := func() { cancel(); <-stopped }
	client := dialDaemon(t, paths.Socket)
	return client, stop
}

// verifySmokeState asks a booted daemon, over the socket, for everything the
// issue's fresh-machine criterion names.
func verifySmokeState(t *testing.T, client *ipc.Client, wantLiveFact bool) {
	t.Helper()
	var mem map[string]any
	if err := client.Call("memory.list", nil, &mem); err != nil {
		t.Fatal(err)
	}
	facts, _ := mem["facts"].([]any)
	wantFacts := 1
	if wantLiveFact {
		wantFacts = 2
	}
	if len(facts) != wantFacts {
		t.Errorf("memory.list returned %d facts, want %d: %v", len(facts), wantFacts, mem)
	}

	var vocab map[string]any
	if err := client.Call("vocabulary.list", nil, &vocab); err != nil {
		t.Fatal(err)
	}
	entries, _ := vocab["entries"].([]any)
	if len(entries) != 1 {
		t.Errorf("vocabulary.list returned %d entries, want 1", len(entries))
	} else if e := entries[0].(map[string]any); e["phrase"] != "quid" {
		t.Errorf("vocabulary entry = %v, want the taught phrase", e)
	}

	var foc map[string]any
	if err := client.Call("focus.list", nil, &foc); err != nil {
		t.Fatal(err)
	}
	threads, _ := foc["threads"].([]any)
	if len(threads) != 1 {
		t.Errorf("focus.list returned %d threads, want 1", len(threads))
	}

	var convs map[string]any
	if err := client.Call("conversation.list", nil, &convs); err != nil {
		t.Fatal(err)
	}
	list, _ := convs["conversations"].([]any)
	if len(list) != 1 {
		t.Errorf("conversation.list returned %d conversations, want 1", len(list))
	} else if c := list[0].(map[string]any); c["id"] != "20260801-100000-cafe" {
		t.Errorf("conversation = %v, want the archived one", c)
	}
}

// TestBackupRestoreSmoke is the whole lifecycle: seed, boot, mutate live,
// back up under the running daemon (the state.hold path), stop, restore
// onto empty roots, boot from them, verify over the socket.
func TestBackupRestoreSmoke(t *testing.T) {
	src := smokePaths(t)
	seedSmokeRoots(t, src)
	clientA, stopA := bootSmokeDaemon(t, src)
	stoppedA := false
	defer func() {
		if !stoppedA {
			stopA()
		}
	}()

	// A fact added through the live daemon — the backup must catch state
	// the daemon wrote, not only what was seeded.
	if err := clientA.Call("memory.add", map[string]any{"content": "added while the daemon ran"}, nil); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(t.TempDir(), "smoke.tar.gz")
	report, err := backup.Create(src, archive, backup.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Capture != backup.CaptureHeld {
		t.Errorf("capture = %q, want %q under a running daemon", report.Capture, backup.CaptureHeld)
	}
	_ = clientA.Close()
	stopA()
	stoppedA = true

	dst := smokePaths(t) // the fresh machine: nothing exists yet
	restored, err := backup.Restore(dst, archive, backup.RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.SafetyCopies) != 0 {
		t.Errorf("fresh restore made safety copies: %v", restored.SafetyCopies)
	}

	cfg, err := config.Load(dst.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Memory.MaxFacts != 123 {
		t.Errorf("restored config max_facts = %d, want the distinctive 123", cfg.Memory.MaxFacts)
	}

	clientB, stopB := bootSmokeDaemon(t, dst)
	defer stopB()
	verifySmokeState(t, clientB, true)
}

// TestFixtureArchiveRestoresAndBoots restores the committed fixture archive
// — bytes written by the Create of the day it was committed — and boots a
// daemon from the result. This is the format-compatibility pin: if a change
// to Create or Restore strands archives users already have, this fails.
func TestFixtureArchiveRestoresAndBoots(t *testing.T) {
	dst := smokePaths(t)
	report, err := backup.Restore(dst, filepath.Join("testdata", "backup-fixture-v1.tar.gz"), backup.RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Files == 0 {
		t.Fatal("fixture archive restored no files")
	}
	clientB, stop := bootSmokeDaemon(t, dst)
	defer stop()
	verifySmokeState(t, clientB, false)
}

// TestRegenerateBackupFixture rebuilds testdata/backup-fixture-v1.tar.gz
// from the smoke seed. Skipped in a normal run; set the env var when the
// seed content must change, then commit the regenerated archive:
//
//	JARVIX_REGEN_BACKUP_FIXTURE=1 go test ./internal/daemon/ -run TestRegenerateBackupFixture
func TestRegenerateBackupFixture(t *testing.T) {
	if os.Getenv("JARVIX_REGEN_BACKUP_FIXTURE") == "" {
		t.Skip("set JARVIX_REGEN_BACKUP_FIXTURE=1 to regenerate the fixture archive")
	}
	src := smokePaths(t)
	seedSmokeRoots(t, src)
	archive := filepath.Join(t.TempDir(), "fixture.tar.gz")
	fixed := time.Date(2026, 8, 28, 3, 0, 0, 0, time.UTC)
	if _, err := backup.Create(src, archive, backup.CreateOptions{Now: func() time.Time { return fixed }}); err != nil {
		t.Fatal(err)
	}
	src2, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src2.Close() }()
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(filepath.Join("testdata", "backup-fixture-v1.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, src2); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	t.Log("fixture regenerated; commit " + filepath.Join("testdata", "backup-fixture-v1.tar.gz"))
}
