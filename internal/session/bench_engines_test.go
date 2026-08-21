//go:build engines

// Benchmarks for the release-to-first-audio budget with the REAL local
// engines: whisper.cpp and Kokoro/Piper as installed on the machine running
// them.
//
// They are behind a build tag because they are a different kind of measurement
// from the rest of bench_test.go. Those benchmarks are hermetic and belong in
// CI: they measure Jarvix's own pipeline overhead with fakes, so a regression
// is always a regression in this repository. These measure the machine — its
// CPU, its GPU backend, its page cache — and answer the question the product
// actually poses: does an answer begin within 1.5 seconds?
//
//	make bench-engines
//
// Requires a 16 kHz mono WAV of a short question and the whisper model:
//
//	JARVIX_BENCH_WAV=/path/to/question.wav \
//	JARVIX_BENCH_WHISPER_MODEL=~/.local/share/jarvix/models/whisper/ggml-base.en.bin \
//	make bench-engines
//
// The provider is a zero-delay fake on purpose: model thinking time is the
// user's choice of model, not Jarvix's latency, and including it would make
// the number say nothing about this codebase.
package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/stt/whispercpp"
	"github.com/rpickz/jarvix/internal/tts"
	"github.com/rpickz/jarvix/internal/tts/kokoro"
	"github.com/rpickz/jarvix/internal/tts/piper"
)

// clipRecorder replays a fixed WAV as if it had just been captured. The engine
// deletes the clip it transcribed, so each recording is a fresh copy.
type clipRecorder struct {
	source string
	dir    string
}

func (r *clipRecorder) Start(context.Context) (audio.Recording, error) {
	data, err := os.ReadFile(r.source)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(r.dir, "clip-"+time.Now().Format("150405.000000000")+".wav")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	return &clipRecording{path: path}, nil
}

type clipRecording struct{ path string }

func (r *clipRecording) Stop() (audio.Clip, error) {
	return audio.Clip{WAVPath: r.path, SampleRate: 16000, Channels: 1}, nil
}

func (r *clipRecording) Cancel() { _ = os.Remove(r.path) }

// engineFixture is one configuration of the real stack under measurement.
type engineFixture struct {
	name string
	stt  stt.Transcriber
	tts  tts.Synthesizer
	stop func()
}

func kokoroDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "jarvix")
}

func benchWAV(b *testing.B) string {
	b.Helper()
	path := os.Getenv("JARVIX_BENCH_WAV")
	if path == "" {
		b.Skip("set JARVIX_BENCH_WAV to a 16 kHz mono WAV of a short question")
	}
	return path
}

func whisperModel(b *testing.B) string {
	b.Helper()
	path := os.Getenv("JARVIX_BENCH_WHISPER_MODEL")
	if path == "" {
		path = filepath.Join(kokoroDir(), "models", "whisper", "ggml-base.en.bin")
	}
	if _, err := os.Stat(path); err != nil {
		b.Skipf("whisper model not found at %s", path)
	}
	return path
}

// measure runs the reference interaction n times and reports the stage
// timings, in the same vocabulary as the session.timings event.
func measure(b *testing.B, fx engineFixture, wav string) {
	if fx.stop != nil {
		defer fx.stop()
	}
	provider := &timedProvider{text: "It is half past four. That is the answer to your question."}
	player := &timedPlayer{}
	bus := NewBus(discardLogger())
	events, unsub := bus.Subscribe()
	defer unsub()
	engine := NewEngine(provider, fx.stt, fx.tts,
		&clipRecorder{source: wav, dir: b.TempDir()}, player, nil, nil, bus, discardLogger(),
		Options{Model: "bench", SpeakResponses: true})

	// One untimed interaction so the warm fixtures are warm and the cold ones
	// have their model file in the page cache: the question is warm-vs-cold
	// engine state, not warm-vs-cold disk.
	runOnce(b, engine, events, player)

	var total time.Duration
	for b.Loop() {
		total += runOnce(b, engine, events, player)
	}
	b.ReportMetric(float64(total.Milliseconds())/float64(b.N), "release-to-first-audio-ms/op")
}

func runOnce(b *testing.B, engine *Engine, events <-chan Event, player *timedPlayer) time.Duration {
	b.Helper()
	if _, err := engine.StartSession(); err != nil {
		b.Fatal(err)
	}
	if err := engine.StartVoice(); err != nil {
		b.Fatal(err)
	}
	released := time.Now()
	if _, err := engine.StopVoice(); err != nil {
		b.Fatal(err)
	}
	if err := engine.Submit(""); err != nil {
		b.Fatal(err)
	}
	var stages map[string]any
	for ev := range events {
		switch ev.Type {
		case "session.timings":
			stages = ev.Data
		case "error":
			b.Fatalf("session failed: %v", ev.Data)
		case "session.finished":
			player.mu.Lock()
			first := player.firstPCM
			player.firstPCM = time.Time{}
			player.mu.Unlock()
			b.Logf("stages: %v", stages)
			return first.Sub(released)
		}
	}
	b.Fatal("the session never finished")
	return 0
}

func BenchmarkEnginesColdWhisperKokoro(b *testing.B) {
	wav, model := benchWAV(b), whisperModel(b)
	measure(b, engineFixture{
		name: "cold",
		stt:  &whispercpp.Transcriber{Binary: "whisper-cli", ModelPath: model, Language: "en"},
		tts:  &kokoro.Synthesizer{},
	}, wav)
}

func BenchmarkEnginesWarmWhisperKokoro(b *testing.B) {
	wav, model := benchWAV(b), whisperModel(b)
	cold := &whispercpp.Transcriber{Binary: "whisper-cli", ModelPath: model, Language: "en"}
	warmSTT := &whispercpp.ServerTranscriber{
		Binary: "whisper-server", ModelPath: model, Language: "en", Cold: cold}
	warmTTS := &kokoro.WarmSynthesizer{Cold: &kokoro.Synthesizer{}}
	measure(b, engineFixture{
		name: "warm",
		stt:  warmSTT,
		tts:  warmTTS,
		stop: func() { _ = warmSTT.Close(); _ = warmTTS.Close() },
	}, wav)
}

func BenchmarkEnginesColdWhisperPiper(b *testing.B) {
	wav, model := benchWAV(b), whisperModel(b)
	measure(b, engineFixture{
		name: "cold",
		stt:  &whispercpp.Transcriber{Binary: "whisper-cli", ModelPath: model, Language: "en"},
		tts:  &piper.Synthesizer{Binary: "piper-tts", Voice: piperVoice()},
	}, wav)
}

func BenchmarkEnginesWarmWhisperPiper(b *testing.B) {
	wav, model := benchWAV(b), whisperModel(b)
	cold := &whispercpp.Transcriber{Binary: "whisper-cli", ModelPath: model, Language: "en"}
	warmSTT := &whispercpp.ServerTranscriber{
		Binary: "whisper-server", ModelPath: model, Language: "en", Cold: cold}
	warmTTS := &piper.WarmSynthesizer{
		Cold: &piper.Synthesizer{Binary: "piper-tts", Voice: piperVoice()}}
	measure(b, engineFixture{
		name: "warm",
		stt:  warmSTT,
		tts:  warmTTS,
		stop: func() { _ = warmSTT.Close(); _ = warmTTS.Close() },
	}, wav)
}

func piperVoice() string {
	if v := os.Getenv("JARVIX_BENCH_PIPER_VOICE"); v != "" {
		return v
	}
	return "en_US-amy-medium"
}
