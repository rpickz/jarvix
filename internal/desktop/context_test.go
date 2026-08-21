package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Collector tests. Nothing here needs a compositor, a Wayland session, or
// anything on screen: the guarantees about *which* binaries run are proved
// with stub binaries on PATH, and everything else with fake gatherers.

// contextStubs puts recording stubs for hyprctl and wl-paste on PATH and
// returns the file each invocation is appended to. A source that is switched
// off must leave no line in it — that is the disabled-source guarantee,
// stated as evidence rather than as a comment.
func contextStubs(t *testing.T) (record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "invocations")
	t.Setenv("JARVIX_STUB_RECORD", record)
	stubs := map[string]string{
		"hyprctl": `#!/bin/sh
printf 'hyprctl %s\n' "$*" >> "$JARVIX_STUB_RECORD"
printf '{"class":"Alacritty","title":"nvim engine.go"}'
`,
		"wl-paste": `#!/bin/sh
printf 'wl-paste %s\n' "$*" >> "$JARVIX_STUB_RECORD"
case "$*" in
  *--primary*) printf 'panic: index out of range' ;;
  *)           printf 'copied text' ;;
esac
`,
	}
	for name, script := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return record
}

// invocations returns the stub log (empty when nothing ran at all).
func invocations(t *testing.T, record string) string {
	t.Helper()
	data, err := os.ReadFile(record)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestNewCollectorIsNilWithEverySourceOff(t *testing.T) {
	// Nil is the zero-cost contract: the daemon checks for it and never wires
	// a collector at all, so a disabled feature costs a session nothing.
	if c := NewCollector(Options{}, nil); c != nil {
		t.Fatalf("NewCollector with no sources = %v, want nil", c)
	}
	// And the nil receiver is still safe, because an interface holding a
	// typed nil is exactly how a disabled feature comes back as a panic.
	var c *Collector
	if snap := c.Collect(context.Background()); len(snap.Items) != 0 {
		t.Errorf("nil collector captured %v", snap.Items)
	}
}

func TestCollectorRunsOnlyEnabledSources(t *testing.T) {
	record := contextStubs(t)
	// The shipped defaults: window and selection on, clipboard off.
	c := NewCollector(Options{Window: true, Selection: true}, nil)
	snap := c.Collect(context.Background())

	if got := snap.Sources(); strings.Join(got, ",") != "window,selection" {
		t.Fatalf("sources = %v", got)
	}
	if snap.Items[0].Text != "Alacritty — nvim engine.go" {
		t.Errorf("window = %q", snap.Items[0].Text)
	}
	if snap.Items[1].Text != "panic: index out of range" {
		t.Errorf("selection = %q", snap.Items[1].Text)
	}

	ran := invocations(t, record)
	if !strings.Contains(ran, "hyprctl activewindow -j") {
		t.Errorf("hyprctl was not invoked: %q", ran)
	}
	if !strings.Contains(ran, "--primary") {
		t.Errorf("primary selection was not read: %q", ran)
	}
	// The disabled-source guarantee: wl-paste ran exactly once, for the
	// selection. A clipboard read would be a second, flagless invocation.
	if got := strings.Count(ran, "wl-paste"); got != 1 {
		t.Errorf("wl-paste invoked %d times, want 1 (clipboard is off):\n%s", got, ran)
	}
}

func TestDisabledSourceIsNeverExecuted(t *testing.T) {
	record := contextStubs(t)
	// Clipboard only: hyprctl must not run at all, not even to be ignored.
	c := NewCollector(Options{Clipboard: true}, nil)
	snap := c.Collect(context.Background())

	if got := snap.Sources(); strings.Join(got, ",") != "clipboard" {
		t.Fatalf("sources = %v", got)
	}
	ran := invocations(t, record)
	if strings.Contains(ran, "hyprctl") {
		t.Errorf("a disabled source was executed: %q", ran)
	}
	if strings.Contains(ran, "--primary") {
		t.Errorf("a disabled source was executed: %q", ran)
	}
}

func TestCollectorWithEverySourceOffRunsNothing(t *testing.T) {
	record := contextStubs(t)
	if c := NewCollector(Options{}, nil); c != nil {
		c.Collect(context.Background())
	}
	if ran := invocations(t, record); ran != "" {
		t.Errorf("something ran with context disabled: %q", ran)
	}
}

func TestCollectDegradesSilentlyOnFailureAndEmptiness(t *testing.T) {
	c := NewCollectorFrom([]Gatherer{
		&FakeGatherer{Src: SourceWindow, Err: errors.New("no compositor here")},
		&FakeGatherer{Src: SourceSelection, Text: "   \n  "}, // nothing highlighted
		&FakeGatherer{Src: SourceClipboard, Text: "still useful"},
	}, Options{}, nil)

	snap := c.Collect(context.Background())
	if got := snap.Sources(); strings.Join(got, ",") != "clipboard" {
		t.Fatalf("sources = %v, want the healthy source only", got)
	}
	if snap.Items[0].Text != "still useful" {
		t.Errorf("clipboard = %q", snap.Items[0].Text)
	}
}

func TestCollectStopsAtTheTimeout(t *testing.T) {
	hung := &FakeGatherer{Src: SourceSelection, Blocked: true}
	c := NewCollectorFrom([]Gatherer{
		&FakeGatherer{Src: SourceWindow, Text: "Firefox — inbox"},
		hung,
	}, Options{Timeout: 30 * time.Millisecond}, nil)

	snap := c.Collect(context.Background())
	if got := snap.Sources(); strings.Join(got, ",") != "window" {
		t.Fatalf("sources = %v, want the hung source dropped", got)
	}
	// The budget is a ceiling on the whole attempt, not per source.
	if snap.Elapsed > 5*time.Second {
		t.Errorf("gathering took %v, far past the budget", snap.Elapsed)
	}
	if hung.Calls() != 1 {
		t.Errorf("hung source gathered %d times", hung.Calls())
	}
}

func TestCollectHonoursCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the session died before gathering began
	c := NewCollectorFrom([]Gatherer{&FakeGatherer{Src: SourceWindow, Blocked: true}},
		Options{Timeout: time.Hour}, nil)
	snap := c.Collect(ctx)
	if len(snap.Items) != 0 {
		t.Errorf("items = %v, want none from a cancelled capture", snap.Items)
	}
}

func TestCollectGathersSourcesInParallel(t *testing.T) {
	// Every gatherer waits on the same release channel, and the release only
	// happens once all three have started. Sequential gathering would deadlock
	// until the (generous) timeout and lose every source; parallel gathering
	// returns all three. No sleeps, no timing assertions.
	release := make(chan struct{})
	gatherers := []Gatherer{
		&FakeGatherer{Src: SourceWindow, Text: "w", Started: make(chan struct{}), Release: release},
		&FakeGatherer{Src: SourceSelection, Text: "s", Started: make(chan struct{}), Release: release},
		&FakeGatherer{Src: SourceClipboard, Text: "c", Started: make(chan struct{}), Release: release},
	}
	go func() {
		for _, g := range gatherers {
			<-g.(*FakeGatherer).Started
		}
		close(release)
	}()

	c := NewCollectorFrom(gatherers, Options{Timeout: 10 * time.Second}, nil)
	snap := c.Collect(context.Background())
	if got := snap.Sources(); strings.Join(got, ",") != "window,selection,clipboard" {
		t.Fatalf("sources = %v, want all three (gathering is not parallel)", got)
	}
}

func TestCollectTruncatesAtTheCapWithAMarker(t *testing.T) {
	long := strings.Repeat("é", 50) // multi-byte: the cut must land on a rune
	c := NewCollectorFrom([]Gatherer{&FakeGatherer{Src: SourceSelection, Text: long}},
		Options{MaxChars: 10}, nil)

	snap := c.Collect(context.Background())
	item := snap.Items[0]
	if !item.Truncated {
		t.Fatalf("item = %+v, want truncated", item)
	}
	if item.Chars != 50 {
		t.Errorf("chars = %d, want the pre-truncation length", item.Chars)
	}
	if want := strings.Repeat("é", 10) + truncationMarker; item.Text != want {
		t.Errorf("text = %q, want %q", item.Text, want)
	}
	// The marker travels inside the text, so it reaches the model too.
	if !strings.Contains(snap.Message(), "[truncated]") {
		t.Errorf("message = %q, want the truncation marker", snap.Message())
	}
}

func TestCollectRedactsSecretsBeforeAnythingElse(t *testing.T) {
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\n" + strings.Repeat("b3BlbnNzaC1rZXktdjEA", 20)
	c := NewCollectorFrom([]Gatherer{&FakeGatherer{Src: SourceClipboard, Text: key}},
		Options{MaxChars: 10}, nil)

	item := c.Collect(context.Background()).Items[0]
	if !item.Redacted || item.Text != RedactedMarker {
		t.Fatalf("item = %+v, want the redaction marker", item)
	}
	// Redaction replaces the whole value, so truncation has nothing to do —
	// and no fragment of the key can survive in the marker.
	if item.Truncated {
		t.Errorf("a redacted item must not also be truncated: %+v", item)
	}
	if strings.Contains(item.Text, "BEGIN") {
		t.Errorf("text = %q, want no fragment of the key", item.Text)
	}
}

func TestSnapshotMessageDelimitsEachSource(t *testing.T) {
	snap := Snapshot{Items: []Item{
		{Source: SourceWindow, Text: "Alacritty — nvim engine.go"},
		{Source: SourceSelection, Text: "panic: index out of range"},
	}}
	msg := snap.Message()
	for _, want := range []string{
		contextPreamble,
		"--- active window ---\nAlacritty — nvim engine.go\n--- end active window ---",
		"--- selected text ---\npanic: index out of range\n--- end selected text ---",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

func TestEmptySnapshotHasNoMessage(t *testing.T) {
	// An empty capture must cost the request nothing at all — not even a
	// message saying there is nothing.
	if msg := (Snapshot{}).Message(); msg != "" {
		t.Errorf("message = %q, want empty", msg)
	}
}

func TestSourceLabels(t *testing.T) {
	for source, want := range map[Source]string{
		SourceWindow:    "active window",
		SourceSelection: "selected text",
		SourceClipboard: "clipboard",
		Source("other"): "other",
	} {
		if got := source.Label(); got != want {
			t.Errorf("%s.Label() = %q, want %q", source, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in        string
		max       int
		want      string
		truncated bool
	}{
		{"short", 10, "short", false},
		{"exactly10!", 10, "exactly10!", false},
		{"one too many", 3, "one" + truncationMarker, true},
		{"héllo wörld", 5, "héllo" + truncationMarker, true},
	}
	for _, c := range cases {
		got, truncated := truncate(c.in, c.max)
		if got != c.want || truncated != c.truncated {
			t.Errorf("truncate(%q, %d) = %q, %v; want %q, %v",
				c.in, c.max, got, truncated, c.want, c.truncated)
		}
	}
}
