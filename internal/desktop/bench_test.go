package desktop

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Benchmarks for the ≤300ms budget. They run against stub binaries rather
// than a live compositor, so they measure what Jarvix controls — process
// spawn, parse, redaction, truncation — on any machine, headless CI included.
// The real hyprctl/wl-paste add their own time on top; the ADR records what
// that measured on a live Hyprland session.

// discardLogger keeps benchmark output clean and keeps logging cost in the
// measurement (a handler that formats nothing still gets called).
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// benchStub writes an executable stub and returns its path.
func benchStub(b *testing.B, dir, name, script string) string {
	b.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		b.Fatal(err)
	}
	return path
}

// BenchmarkCollect is the headline number: one full capture of the two
// default sources, in parallel, including redaction and truncation.
func BenchmarkCollect(b *testing.B) {
	dir := b.TempDir()
	hyprctl := benchStub(b, dir, "hyprctl", `#!/bin/sh
printf '{"class":"Alacritty","title":"nvim internal/session/engine.go"}'
`)
	// A realistic selection: a screenful of stack trace.
	wlPaste := benchStub(b, dir, "wl-paste", `#!/bin/sh
i=0
while [ $i -lt 40 ]; do
  printf 'goroutine 1 [running]: main.handle(0xc000112233, 0x14)\n'
  i=$((i + 1))
done
`)
	c := NewCollector(Options{
		Window: true, Selection: true,
		HyprctlBinary: hyprctl, WLPasteBinary: wlPaste,
	}, discardLogger())
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if snap := c.Collect(ctx); len(snap.Items) != 2 {
			b.Fatalf("captured %v", snap.Sources())
		}
	}
}

// BenchmarkCollectAllThree adds the clipboard: a third source must cost
// nothing extra in wall time, because sources are gathered in parallel.
func BenchmarkCollectAllThree(b *testing.B) {
	dir := b.TempDir()
	hyprctl := benchStub(b, dir, "hyprctl", `#!/bin/sh
printf '{"class":"Alacritty","title":"nvim engine.go"}'
`)
	wlPaste := benchStub(b, dir, "wl-paste", `#!/bin/sh
printf 'some copied text'
`)
	c := NewCollector(Options{
		Window: true, Selection: true, Clipboard: true,
		HyprctlBinary: hyprctl, WLPasteBinary: wlPaste,
	}, discardLogger())
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if snap := c.Collect(ctx); len(snap.Items) != 3 {
			b.Fatalf("captured %v", snap.Sources())
		}
	}
}

// BenchmarkCollectDisabled is the zero-cost claim, stated as a measurement:
// with context off there is no collector, and the call the engine skips costs
// nothing at all.
func BenchmarkCollectDisabled(b *testing.B) {
	c := NewCollector(Options{}, discardLogger()) // nil
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Collect(ctx)
	}
}

// BenchmarkRedact is the per-source cost of the secret heuristics over a
// full 2 000-character capture that contains no secret — the worst case,
// since a match short-circuits.
func BenchmarkRedact(b *testing.B) {
	text := strings.Repeat("the quick brown fox jumps over the lazy dog; ", 46)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, redacted := Redact(text); redacted {
			b.Fatal("prose was redacted")
		}
	}
}

// BenchmarkSnapshotMessage measures building the message the model sees.
func BenchmarkSnapshotMessage(b *testing.B) {
	snap := Snapshot{
		Items: []Item{
			{Source: SourceWindow, Text: "Alacritty — nvim engine.go"},
			{Source: SourceSelection, Text: strings.Repeat("panic: index out of range\n", 60)},
		},
		Elapsed: 12 * time.Millisecond,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if snap.Message() == "" {
			b.Fatal("empty message")
		}
	}
}
