// Benchmarks for the interaction-latency budget. Everything here runs against
// fakes: the numbers measure Jarvix's own pipeline overhead, never the STT/AI/
// TTS engines, so a regression in review is always a regression in our code.
package session

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// timedProvider streams a fixed response and records when the first delta was
// handed to the engine.
type timedProvider struct {
	text string

	mu         sync.Mutex
	firstDelta time.Time
}

func (p *timedProvider) Name() string { return "bench" }

func (p *timedProvider) Chat(ctx context.Context, req ai.ChatRequest) (<-chan ai.Event, error) {
	ch := make(chan ai.Event)
	go func() {
		defer close(ch)
		first := true
		for _, w := range strings.SplitAfter(p.text, " ") {
			if first {
				p.mu.Lock()
				p.firstDelta = time.Now()
				p.mu.Unlock()
				first = false
			}
			select {
			case ch <- ai.Event{Type: ai.EventDelta, Content: w}:
			case <-ctx.Done():
				return
			}
		}
		ch <- ai.Event{Type: ai.EventDone}
	}()
	return ch, nil
}

// timedPlayer records when the first PCM chunk reached the player, then
// drains the stream like real playback would.
type timedPlayer struct {
	mu       sync.Mutex
	firstPCM time.Time
}

func (p *timedPlayer) Play(ctx context.Context, sampleRate, channels int, chunks <-chan []byte) error {
	first := true
	for range chunks {
		if first {
			p.mu.Lock()
			p.firstPCM = time.Now()
			p.mu.Unlock()
			first = false
		}
	}
	return nil
}

// timedPlayer deliberately ignores audio.Trace: it *is* the measurement, and
// taking the mark itself keeps the benchmark independent of the plumbing under
// test.
var _ audio.Player = (*timedPlayer)(nil)

// BenchmarkFirstDeltaToFirstPCM measures THE latency seam of the product:
// from the fake provider emitting its first text delta to the first PCM chunk
// being handed to the audio player — i.e. sentencer + streaming speaker +
// fake synthesis handoff. This is the metric the future warm-engine
// optimisation work must move; keep the definition stable.
func BenchmarkFirstDeltaToFirstPCM(b *testing.B) {
	provider := &timedProvider{text: "The answer is ready. More detail follows in a second sentence."}
	player := &timedPlayer{}
	bus := NewBus(discardLogger())
	events, unsub := bus.Subscribe()
	defer unsub()
	engine := NewEngine(provider, &stt.Fake{}, &tts.Fake{}, &audio.FakeRecorder{}, player,
		nil, nil, bus, discardLogger(), Options{Model: "bench", SpeakResponses: true})

	b.ReportAllocs()
	var total time.Duration
	for b.Loop() {
		if _, err := engine.StartSession(); err != nil {
			b.Fatal(err)
		}
		if err := engine.Submit("benchmark question"); err != nil {
			b.Fatal(err)
		}
		for ev := range events {
			if ev.Type == "session.finished" {
				break
			}
			if ev.Type == "error" {
				b.Fatalf("session failed: %v", ev.Data)
			}
		}
		provider.mu.Lock()
		t0 := provider.firstDelta
		provider.mu.Unlock()
		player.mu.Lock()
		t1 := player.firstPCM
		player.mu.Unlock()
		total += t1.Sub(t0)
	}
	b.ReportMetric(float64(total.Nanoseconds())/float64(b.N), "first-delta-to-first-pcm-ns/op")
}

// BenchmarkReleaseToFirstAudio measures the product's headline number over our
// own pipeline: push-to-talk release to the first PCM chunk reaching the
// player, with fake engines so what is measured is Jarvix's overhead and
// nothing else.
//
// It is the companion to the per-session `session.timings` report (ADR 0018):
// the event tells a user what their machine did with real engines, and this
// tells a reviewer whether a change made our share of that budget worse. The
// engine numbers themselves belong to whisper and kokoro and are recorded in
// the ADR, not here — a benchmark that shelled out to them would measure the
// machine it ran on.
func BenchmarkReleaseToFirstAudio(b *testing.B) {
	provider := &timedProvider{text: "The answer is ready. More detail follows in a second sentence."}
	player := &timedPlayer{}
	bus := NewBus(discardLogger())
	events, unsub := bus.Subscribe()
	defer unsub()
	engine := NewEngine(provider, &stt.Fake{Text: "what time is it"}, &tts.Fake{},
		&audio.FakeRecorder{}, player, nil, nil, bus, discardLogger(),
		Options{Model: "bench", SpeakResponses: true})

	b.ReportAllocs()
	var total time.Duration
	for b.Loop() {
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
		for ev := range events {
			if ev.Type == "session.finished" {
				break
			}
			if ev.Type == "error" {
				b.Fatalf("session failed: %v", ev.Data)
			}
		}
		player.mu.Lock()
		firstPCM := player.firstPCM
		player.firstPCM = time.Time{}
		player.mu.Unlock()
		total += firstPCM.Sub(released)
	}
	b.ReportMetric(float64(total.Nanoseconds())/float64(b.N), "release-to-first-audio-ns/op")
}

// BenchmarkSentencer measures sentence-splitter throughput over a streaming
// token feed — it sits on the hot path of every spoken response.
func BenchmarkSentencer(b *testing.B) {
	text := strings.Repeat("This is a spoken sentence of answer text. It streams token by token! Does it split cleanly? Yes: it does.\n", 8)
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	for b.Loop() {
		var sc sentencer
		rest := text
		for len(rest) > 0 {
			n := 6 // ~ a short token
			if n > len(rest) {
				n = len(rest)
			}
			_ = sc.push(rest[:n])
			rest = rest[n:]
		}
		_ = sc.flush()
	}
}

// BenchmarkBusFanout measures event fan-out cost to N subscribers — the cost
// the engine pays on every published event with N IPC clients connected.
func BenchmarkBusFanout(b *testing.B) {
	for _, n := range []int{1, 8, 64} {
		b.Run(map[int]string{1: "1sub", 8: "8subs", 64: "64subs"}[n], func(b *testing.B) {
			bus := NewBus(discardLogger())
			var wg sync.WaitGroup
			unsubs := make([]func(), 0, n)
			for i := 0; i < n; i++ {
				events, unsub := bus.Subscribe()
				unsubs = append(unsubs, unsub)
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range events {
					}
				}()
			}
			ev := Event{Type: "assistant.delta", Data: map[string]any{"content": "chunk"}}
			b.ReportAllocs()
			for b.Loop() {
				bus.Publish(ev)
			}
			b.StopTimer()
			// Unsubscribing closes each channel so the drain goroutines exit.
			for _, unsub := range unsubs {
				unsub()
			}
			wg.Wait()
		})
	}
}
