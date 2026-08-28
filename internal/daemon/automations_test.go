package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/automation"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// The daemon half of ADR 0032: the fire path. The scheduler's clockwork has
// its own hermetic tests in internal/automation; here the callback it fires
// is driven directly — same package, no timers — against a fully wired
// daemon, so the tier pre-check, the refusal notification, the quiet session
// entry, and the skip-when-busy are each asserted on the real wiring.

// scheduledHarness is one daemon with a scheduled stub script.
type scheduledHarness struct {
	d        *Daemon
	provider *ai.Fake
	notifier *desktop.FakeNotifier
	tts      *tts.Fake
	marker   string
	logs     *lockedBuffer
	entry    automation.Entry
}

// lockedBuffer collects log output; the daemon logs from several goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startScheduledScriptDaemon wires a daemon whose one script carries a
// schedule — a stub in a test-owned temp dir that proves it ran with a marker
// file. allow promotes script.run; the default leaves the shipped ask.
func startScheduledScriptDaemon(t *testing.T, allow, announce bool) *scheduledHarness {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "backup-notes.sh")
	marker := filepath.Join(dir, "ran.marker")
	if err := os.WriteFile(scriptPath,
		[]byte("#!/bin/sh\ntouch "+marker+"\necho 'Notes backed up.'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	cfg := testConfig()
	cfg.Scripts = []config.Script{{
		Name:     "backup notes",
		Phrases:  []string{"backup my notes"},
		Path:     scriptPath,
		Report:   "stdout",
		Schedule: "02:00",
		Announce: announce,
	}}
	if allow {
		cfg.Tools.Policy.Tool = map[string]string{"script.run": "allow"}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	h := &scheduledHarness{
		provider: &ai.Fake{Response: "should never be needed"},
		notifier: &desktop.FakeNotifier{},
		tts:      &tts.Fake{},
		marker:   marker,
		logs:     &lockedBuffer{},
		entry:    cfg.AutomationEntries()[0],
	}
	logger := slog.New(slog.NewTextHandler(h.logs, nil))
	d, err := New(cfg, paths, logger, Deps{
		Provider:    h.provider,
		Transcriber: &stt.Fake{Text: "unused"},
		Synthesizer: h.tts,
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    h.notifier,
		OpenWindow:  func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	h.d = d
	serveDaemon(t, d)
	dialDaemon(t, paths.Socket)
	return h
}

// collectBus drains bus events during fn and returns everything seen.
func collectBus(t *testing.T, d *Daemon, fn func()) []session.Event {
	t.Helper()
	events, unsubscribe := d.Bus().Subscribe()
	fn()
	unsubscribe() // closes the channel once drained below
	var seen []session.Event
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return seen
			}
			seen = append(seen, ev)
		case <-deadline:
			t.Fatal("bus events never drained")
		}
	}
}

func busEvent(seen []session.Event, eventType string) (session.Event, bool) {
	for _, ev := range seen {
		if ev.Type == eventType {
			return ev, true
		}
	}
	return session.Event{}, false
}

// TestScheduledAskTierFireIsRefusedAndNotified is the ticket's central
// policy, mutation-checked end to end: an ask-tier scheduled firing executes
// nothing — the script file is untouched — and instead produces the refusal
// event and a notification whose click target is the window-opening default
// action. The load-time warning has already fired by construction.
func TestScheduledAskTierFireIsRefusedAndNotified(t *testing.T) {
	h := startScheduledScriptDaemon(t, false, false)

	// The load-time WARNING: the user learns at boot, not at 2am.
	if logs := h.logs.String(); !strings.Contains(logs, "cannot run unattended") ||
		!strings.Contains(logs, `"script.run\" = \"allow\"`) {
		t.Errorf("no load-time warning with the fix in it; logs:\n%s", logs)
	}

	seen := collectBus(t, h.d, func() {
		h.d.fireAutomation(context.Background(), h.entry)
	})

	if _, err := os.Stat(h.marker); err == nil {
		t.Fatal("an ask-tier scheduled firing executed the script")
	}
	refused, ok := busEvent(seen, "automation.refused")
	if !ok {
		t.Fatalf("no automation.refused event; saw %v", seen)
	}
	if refused.Data["name"] != "backup notes" || refused.Data["rule"] == "" {
		t.Errorf("automation.refused = %v", refused.Data)
	}
	if _, started := busEvent(seen, "session.finished"); started {
		t.Error("a refused firing still ran a session")
	}
	// The notification rides the existing channel with its click-opens-window
	// default action. Send is dispatched on its own goroutine; the daemon's
	// post group is waited on by shutdown, so wait for it here the same way.
	deadline := time.Now().Add(5 * time.Second)
	for len(h.notifier.Sent()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no refusal notification was delivered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	n := h.notifier.Sent()[0]
	if !strings.Contains(n.Body, "backup notes") || !strings.Contains(n.Body, "needs your confirmation") {
		t.Errorf("notification body = %q", n.Body)
	}
	if len(n.Actions) != 1 || n.Actions[0].ID != desktop.DefaultActionID {
		t.Errorf("notification actions = %+v, want the window-opening default", n.Actions)
	}
	if len(h.provider.Requests) != 0 {
		t.Errorf("a refused firing made %d provider calls", len(h.provider.Requests))
	}
}

// TestScheduledAllowTierFireRunsTheNormalPathQuietly: an allow-listed entry
// fires through the ordinary session path — router, gate, runner, events —
// and executes for real (the marker file proves it), with not one sentence
// synthesized: announce defaults off, and 2am stays silent.
func TestScheduledAllowTierFireRunsTheNormalPathQuietly(t *testing.T) {
	h := startScheduledScriptDaemon(t, true, false)

	seen := collectBus(t, h.d, func() {
		h.d.fireAutomation(context.Background(), h.entry)
	})

	if _, err := os.Stat(h.marker); err != nil {
		t.Fatalf("the scheduled script never ran: %v", err)
	}
	fin, ok := busEvent(seen, "script.finished")
	if !ok || fin.Data["status"] != "ok" {
		t.Fatalf("script.finished = %v (found %v)", fin.Data, ok)
	}
	if _, ok := busEvent(seen, "session.finished"); !ok {
		t.Fatal("the clockfire returned before its session finished")
	}
	if n := h.tts.Speaks(); n != 0 {
		t.Fatalf("a quiet scheduled run synthesized %d sentences, want zero", n)
	}
	if len(h.provider.Requests) != 0 {
		t.Errorf("a scheduled script made %d provider calls", len(h.provider.Requests))
	}
}

// TestScheduledFireWithAnnounceSpeaksOverDaemonWiring: announce = true is the
// explicit opt-in, and then the run's report line is spoken.
func TestScheduledFireWithAnnounceSpeaksOverDaemonWiring(t *testing.T) {
	h := startScheduledScriptDaemon(t, true, true)

	h.d.fireAutomation(context.Background(), h.entry)

	if n := h.tts.Speaks(); n == 0 {
		t.Fatal("announce = true spoke nothing")
	}
	if h.tts.Last().Text != "Notes backed up." {
		t.Errorf("spoken = %q, want the script's report line", h.tts.Last().Text)
	}
}

// TestScheduledFireSkipsWhenAConversationIsActive: the clock yields to the
// person — a firing during an active session is reported as skipped, and the
// session in flight is untouched.
func TestScheduledFireSkipsWhenAConversationIsActive(t *testing.T) {
	h := startScheduledScriptDaemon(t, true, false)

	id, err := h.d.engine.StartSession()
	if err != nil {
		t.Fatal(err)
	}
	seen := collectBus(t, h.d, func() {
		h.d.fireAutomation(context.Background(), h.entry)
	})

	if _, err := os.Stat(h.marker); err == nil {
		t.Fatal("a firing during an active session executed anyway")
	}
	skipped, ok := busEvent(seen, "automation.skipped")
	if !ok || skipped.Data["name"] != "backup notes" {
		t.Fatalf("automation.skipped = %v (found %v)", skipped.Data, ok)
	}
	if reason, _ := skipped.Data["reason"].(string); reason == "" {
		t.Error("the skip carries no reason")
	}
	if _, current := h.d.engine.State(); current != id {
		t.Errorf("active session = %q after the skip, want %q untouched", current, id)
	}
	_ = h.d.engine.Cancel()
}

// TestAutomationsSchedulesOverSocket: the future tab's read surface —
// next-fire computed daemon-side, the announce flag echoed, and the tier
// verdict carried as would_refuse so "needs allow" is visible before 2am.
func TestAutomationsSchedulesOverSocket(t *testing.T) {
	h := startScheduledScriptDaemon(t, false, false)
	client := dialDaemon(t, h.d.paths.Socket)

	var out struct {
		Schedules []struct {
			Kind        string `json:"kind"`
			Name        string `json:"name"`
			Schedule    string `json:"schedule"`
			Announce    bool   `json:"announce"`
			NextFire    string `json:"next_fire"`
			Running     bool   `json:"running"`
			WouldRefuse bool   `json:"would_refuse"`
			Rule        string `json:"rule"`
		} `json:"schedules"`
	}
	if err := client.Call("automations.schedules", nil, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Schedules) != 1 {
		t.Fatalf("schedules = %+v", out.Schedules)
	}
	s := out.Schedules[0]
	if s.Kind != "script" || s.Name != "backup notes" || s.Schedule != "02:00" || s.Announce {
		t.Errorf("schedule = %+v", s)
	}
	next, err := time.Parse(time.RFC3339, s.NextFire)
	if err != nil {
		t.Fatalf("next_fire %q: %v", s.NextFire, err)
	}
	if until := time.Until(next); until <= 0 || until > 24*time.Hour {
		t.Errorf("next_fire = %v, want within the next day", next)
	}
	if !s.WouldRefuse || s.Rule == "" {
		t.Errorf("schedule = %+v, want would_refuse with its rule for the ask-tier entry", s)
	}
}
