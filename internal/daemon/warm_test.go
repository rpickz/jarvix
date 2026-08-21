package daemon

import (
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/stt/whispercpp"
	"github.com/rpickz/jarvix/internal/tts"
	"github.com/rpickz/jarvix/internal/tts/kokoro"
	"github.com/rpickz/jarvix/internal/tts/piper"
	"github.com/rpickz/jarvix/internal/warm"
)

func testPaths(t *testing.T) config.Paths {
	t.Helper()
	dir := t.TempDir()
	return config.Paths{Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock")}
}

func TestFillDepsBuildsWarmAdaptersWhenWarmModeIsOn(t *testing.T) {
	cfg := config.Default()
	cfg.TTS.Provider = "kokoro"
	deps, workers, err := fillDeps(cfg, testPaths(t), Deps{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := deps.Transcriber.(*whispercpp.ServerTranscriber); !ok {
		t.Errorf("transcriber = %T, want the warm whisper adapter", deps.Transcriber)
	}
	if _, ok := deps.Synthesizer.(*kokoro.WarmSynthesizer); !ok {
		t.Errorf("synthesizer = %T, want the warm kokoro adapter", deps.Synthesizer)
	}
	// Both must be tracked, or nothing kills them at shutdown.
	if got := workers.Status(); len(got) != 2 {
		t.Fatalf("tracked warm workers = %d, want 2", len(got))
	}
	for _, s := range workers.Status() {
		if s.Running {
			// Building deps must not spawn anything: a daemon nobody speaks to
			// should never load a model.
			t.Errorf("worker %q was started at construction", s.Name)
		}
	}
}

func TestFillDepsBuildsTheWarmPiperAdapter(t *testing.T) {
	cfg := config.Default() // piper is the default TTS
	deps, workers, err := fillDeps(cfg, testPaths(t), Deps{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := deps.Synthesizer.(*piper.WarmSynthesizer); !ok {
		t.Errorf("synthesizer = %T, want the warm piper adapter", deps.Synthesizer)
	}
	if got := len(workers.Status()); got != 2 {
		t.Errorf("tracked warm workers = %d, want 2", got)
	}
}

func TestFillDepsKeepsTheColdAdaptersWhenWarmModeIsOff(t *testing.T) {
	cfg := config.Default()
	cfg.Performance.WarmEngines = false
	cfg.TTS.Provider = "kokoro"
	deps, workers, err := fillDeps(cfg, testPaths(t), Deps{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// warm_engines = false must be exactly the pre-ADR-0017 daemon.
	if _, ok := deps.Transcriber.(*whispercpp.Transcriber); !ok {
		t.Errorf("transcriber = %T, want the per-question whisper-cli adapter", deps.Transcriber)
	}
	if _, ok := deps.Synthesizer.(*kokoro.Synthesizer); !ok {
		t.Errorf("synthesizer = %T, want the per-utterance kokoro adapter", deps.Synthesizer)
	}
	if got := len(workers.Status()); got != 0 {
		t.Errorf("tracked warm workers = %d, want none", got)
	}
}

func TestFillDepsLeavesInjectedCollaboratorsAlone(t *testing.T) {
	// A test fake cannot be rebuilt from config, and must never be wrapped in
	// a warm adapter that would try to spawn a process for it.
	cfg := config.Default()
	injected := Deps{Transcriber: &stt.Fake{}, Synthesizer: &tts.Fake{}}
	deps, workers, err := fillDeps(cfg, testPaths(t), injected, nil)
	if err != nil {
		t.Fatal(err)
	}
	if deps.Transcriber != injected.Transcriber || deps.Synthesizer != injected.Synthesizer {
		t.Error("injected collaborators were replaced")
	}
	if got := len(workers.Status()); got != 0 {
		t.Errorf("tracked warm workers = %d, want none", got)
	}
}

// closingEngine records that the daemon shut it down.
type closingEngine struct {
	name   string
	closed atomic.Bool
}

func (e *closingEngine) WarmStatus() warm.Status {
	return warm.Status{Name: e.name, Running: !e.closed.Load(), PID: 4242, RSSBytes: 165 << 20}
}

func (e *closingEngine) Close() error {
	e.closed.Store(true)
	return nil
}

func TestWarmWorkersCloseKillsEveryEngine(t *testing.T) {
	a, b := &closingEngine{name: "whisper"}, &closingEngine{name: "kokoro"}
	workers := warmWorkers{engines: []warmEngine{a, b}}
	workers.Close()
	if !a.closed.Load() || !b.closed.Load() {
		t.Error("Close must reach every warm engine; a missed one is an orphan process")
	}
}

func TestWarmReportRendersEveryWorker(t *testing.T) {
	d := &Daemon{warm: warmWorkers{engines: []warmEngine{&closingEngine{name: "whisper"}}}}
	report := d.warmReport()
	if len(report) != 1 {
		t.Fatalf("report = %v", report)
	}
	entry := report[0]
	if entry["name"] != "whisper" || entry["running"] != true || entry["rss_mb"] != uint64(165) {
		t.Errorf("entry = %v", entry)
	}
	// closeWarm both kills the workers and forgets them, so a reload cannot
	// report the previous configuration's processes.
	d.closeWarm()
	if got := d.warmReport(); len(got) != 0 {
		t.Errorf("report after shutdown = %v, want empty", got)
	}
}

func TestLastTimingsSurviveTheSessionThatProducedThem(t *testing.T) {
	d := &Daemon{}
	if d.lastTimingsReport() != nil {
		t.Error("no session has finished; there is nothing to report")
	}
	source := map[string]any{"session_id": "s1", "release_to_first_audio_ms": int64(289)}
	d.setLastTimings(source)
	// The event's map is shared with every other bus subscriber, so the daemon
	// must be holding a copy.
	source["release_to_first_audio_ms"] = int64(9999)
	got := d.lastTimingsReport()
	if got["release_to_first_audio_ms"] != int64(289) {
		t.Errorf("last timings = %v; the event's map was retained by reference", got)
	}
}
