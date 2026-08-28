package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// The daemon half of one-shot reminders (#141, ADR 0046): the reminders.*
// verbs over the real socket, spoken creation with no confirmation card
// through the real wiring, the deferral-to-boundary delivery, and the boot
// late fire — all on the fake engines, headless and offline.

// awaitBus waits on an already-open subscription for one event of the wanted
// type, returning it. Bounded, never polled.
func awaitBus(t *testing.T, events <-chan session.Event, eventType string) session.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("bus closed while waiting for %s", eventType)
			}
			if ev.Type == eventType {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %s event arrived", eventType)
		}
	}
}

// plantReminder writes a store file by hand with one reminder due in the
// past — the shape a daemon finds after downtime, and the hand-edit path the
// store promises to pick up.
func plantReminder(t *testing.T, dir string, due time.Time) {
	t.Helper()
	content := fmt.Sprintf(`version = 1
next_id = 2

[[reminder]]
id = "r1"
text = "call the pharmacy"
due = %s
created = %s
`, due.UTC().Format(time.RFC3339), due.Add(-time.Hour).UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, "reminders.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReminderVerbsRoundTripOverTheSocket(t *testing.T) {
	h := startFocusDaemon(t)
	client := dialDaemon(t, h.socket)

	if _, err := h.d.reminders.Create("in twenty minutes", "stretch"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.reminders.Create("in an hour", "hydrate"); err != nil {
		t.Fatal(err)
	}

	var listing struct {
		Reminders []struct {
			ID        string `json:"id"`
			Text      string `json:"text"`
			DueSpoken string `json:"due_spoken"`
		} `json:"reminders"`
		History []struct {
			Outcome string `json:"outcome"`
		} `json:"history"`
	}
	if err := client.Call("reminders.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Reminders) != 2 || listing.Reminders[0].Text != "stretch" {
		t.Fatalf("reminders.list = %+v", listing)
	}
	if listing.Reminders[0].DueSpoken == "" {
		t.Error("due_spoken missing: the tab would have to invent its own arithmetic")
	}

	var cancelled struct {
		Spoken string `json:"spoken"`
	}
	if err := client.Call("reminders.cancel",
		map[string]any{"id": listing.Reminders[0].ID}, &cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled.Spoken != "Cancelled the reminder: stretch." {
		t.Errorf("cancel spoken = %q", cancelled.Spoken)
	}
	if err := client.Call("reminders.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Reminders) != 1 || len(listing.History) != 1 ||
		listing.History[0].Outcome != "cancelled" {
		t.Fatalf("after cancel: %+v", listing)
	}

	// A refusal travels as a JSON-RPC error, never a silent success.
	if err := client.Call("reminders.cancel", map[string]any{"id": "r99"}, &cancelled); err == nil ||
		!strings.Contains(err.Error(), "no reminder matches") {
		t.Errorf("unknown cancel err = %v", err)
	}
}

// TestSpokenCreateNeedsNoConfirmationCard is the pinned no-ceremony test
// through the REAL wiring: the shipped router, the chained runner, the real
// store. "Remind me at three to call the pharmacy" routes deterministically,
// never reaches the model, raises no confirmation of any kind, and the
// spoken confirmation says which reading of "three" won.
func TestSpokenCreateNeedsNoConfirmationCard(t *testing.T) {
	h := startFocusDaemon(t)

	seen := collectBus(t, h.d, func() {
		events, unsubscribe := h.d.Bus().Subscribe()
		defer unsubscribe()
		if _, err := h.d.engine.StartSession(); err != nil {
			t.Fatal(err)
		}
		if err := h.d.engine.Submit("remind me at three to call the pharmacy"); err != nil {
			t.Fatal(err)
		}
		awaitBus(t, events, "session.finished")
	})

	executed, ok := busEvent(seen, "intent.executed")
	if !ok {
		t.Fatalf("the phrase never routed; saw %v", seen)
	}
	if executed.Data["intent"] != "reminder.set" || executed.Data["source"] != "reminder" ||
		executed.Data["status"] != "ok" {
		t.Errorf("intent.executed = %v", executed.Data)
	}
	for _, forbidden := range []string{"tool.confirmation_required", "tool.started", "tool.denied"} {
		if _, raised := busEvent(seen, forbidden); raised {
			t.Errorf("creating a reminder raised %s — the ceremony this feature removes", forbidden)
		}
	}
	if len(h.provider.Requests) != 0 {
		t.Error("a deterministic reminder phrase reached the model")
	}
	spoken := h.tts.Last().Text
	if !strings.HasPrefix(spoken, "Reminding you at three ") ||
		!strings.HasSuffix(spoken, ": call the pharmacy.") {
		t.Errorf("spoken confirmation = %q; it must say which three", spoken)
	}
	// The store holds it, resolved.
	v := h.d.reminders.Snapshot()
	if len(v.Pending) != 1 || v.Pending[0].Text != "call the pharmacy" {
		t.Fatalf("store = %+v", v.Pending)
	}
}

// TestDeliveryDefersBehindALiveSessionAndArrivesAtItsEnd is the do-not-nag
// deferral end to end: the moment arrives while a session holds the floor,
// the attempt yields, and the session's end delivers it — once, marked late
// in the spoken line.
func TestDeliveryDefersBehindALiveSessionAndArrivesAtItsEnd(t *testing.T) {
	h := startFocusDaemon(t)
	events, unsubscribe := h.d.Bus().Subscribe()
	defer unsubscribe()

	// A session holds the floor.
	if _, err := h.d.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	// The moment arrives: a hand-planted reminder five minutes past due,
	// picked up on the next wake — and the attempt is refused by the live
	// session, so it parks for the boundary.
	plantReminder(t, filepath.Dir(h.d.reminders.Path()), time.Now().Add(-5*time.Minute))
	h.d.reminders.Rearm()
	awaitBus(t, events, "reminders.changed") // reason: deferred
	if owed := h.d.reminders.Owed(); owed != 1 {
		t.Fatalf("owed while deferred = %d; the reminder was lost", owed)
	}

	// The user's session ends; the boundary watcher releases the delivery,
	// which speaks through its own scheduled session.
	if err := h.d.engine.Submit("what reminders do i have"); err != nil {
		t.Fatal(err)
	}
	awaitBus(t, events, "session.finished") // the user's session
	for {
		ev := awaitBus(t, events, "intent.executed")
		if ev.Data["intent"] == "reminder.due" {
			if ev.Data["source"] != "reminder" || ev.Data["status"] != "ok" {
				t.Errorf("delivery event = %v", ev.Data)
			}
			break
		}
	}
	awaitBus(t, events, "session.finished") // the delivery's session
	spoken := h.tts.Last().Text
	if !strings.Contains(spoken, "late: call the pharmacy.") {
		t.Errorf("deferred delivery spoke %q; want the late-marked line", spoken)
	}
	if owed := h.d.reminders.Owed(); owed != 0 {
		t.Errorf("owed after the boundary = %d", owed)
	}
	v := h.d.reminders.Snapshot()
	if len(v.History) != 1 || !v.History[0].Late {
		t.Fatalf("history = %+v; want one late-marked delivery", v.History)
	}
}

// startPlantedReminderDaemon builds a daemon over a state dir that already
// holds a reminder store — the downtime shape — and subscribes to the bus
// BEFORE Run starts, so nothing the boot fire publishes can outrun the
// subscription.
func startPlantedReminderDaemon(t *testing.T, dir string) (*focusHarness, <-chan session.Event) {
	t.Helper()
	cfg := testConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	h := &focusHarness{tts: &tts.Fake{}, provider: &ai.Fake{Response: "should never be needed"}}
	d, err := New(cfg, paths, slog.New(slog.DiscardHandler), Deps{
		Provider:    h.provider,
		Transcriber: &stt.Fake{Text: "unused"},
		Synthesizer: h.tts,
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
		Compositor:  desktop.NewFakeCompositor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.d, h.socket = d, paths.Socket
	events, unsubscribe := d.Bus().Subscribe()
	t.Cleanup(unsubscribe)
	serveDaemon(t, d)
	dialDaemon(t, paths.Socket)
	return h, events
}

// TestBootFiresAMissedReminderLate: the daemon was down at the moment. The
// next boot delivers it once — "While I was off: …" — through the ordinary
// session path, before anyone has spoken.
func TestBootFiresAMissedReminderLate(t *testing.T) {
	dir := t.TempDir()
	plantReminder(t, dir, time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC))
	h, events := startPlantedReminderDaemon(t, dir)

	for {
		ev := awaitBus(t, events, "reminders.changed")
		if ev.Data["reason"] == "fired" {
			break
		}
	}
	awaitBus(t, events, "session.finished")
	spoken := h.tts.Last().Text
	if !strings.HasPrefix(spoken, "While I was off: you asked me to remind you to call the pharmacy") {
		t.Errorf("boot fire spoke %q", spoken)
	}
	if owed := h.d.reminders.Owed(); owed != 0 {
		t.Errorf("owed after the boot fire = %d", owed)
	}
	v := h.d.reminders.Snapshot()
	if len(v.History) != 1 || v.History[0].Outcome != "fired" || !v.History[0].Late {
		t.Fatalf("history = %+v; want one late-marked boot delivery", v.History)
	}
}
