package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/focus"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// The daemon half of focus threads (#123, ADR 0041): the focus.* verbs over
// the real socket, and the firing path — a reminder speaks through the
// ordinary scheduled-session path, and the do-not-nag rule drops it with a
// report while a session holds the microphone.

// focusHarness is one daemon with a fake desktop and a spoken-word recorder.
type focusHarness struct {
	d        *Daemon
	tts      *tts.Fake
	provider *ai.Fake
	socket   string
}

func startFocusDaemon(t *testing.T) *focusHarness {
	t.Helper()
	return startFocusDaemonWith(t, testConfig(), desktop.Window{
		Address: "0xa", Class: "Alacritty", Title: "make test", Focused: true,
	})
}

// startFocusDaemonWith is the harness with the configuration and the desktop
// chosen by the test — the AI-session recap tests (#124) need the context
// window source on and a window class of their choosing.
func startFocusDaemonWith(t *testing.T, cfg config.Config, windows ...desktop.Window) *focusHarness {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
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
		Compositor:  desktop.NewFakeCompositor(windows...),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.d, h.socket = d, paths.Socket
	serveDaemon(t, d)
	dialDaemon(t, paths.Socket)
	return h
}

// TestFocusVerbsRoundTripOverTheSocket drives the whole verb surface — the
// Focus tab's contract — through a real client.
func TestFocusVerbsRoundTripOverTheSocket(t *testing.T) {
	h := startFocusDaemon(t)
	client := dialDaemon(t, h.socket)

	var created struct {
		Thread struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"thread"`
		Spoken string `json:"spoken"`
	}
	if err := client.Call("focus.create",
		map[string]any{"name": "the ci refactor", "windows": 1}, &created); err != nil {
		t.Fatal(err)
	}
	if created.Thread.ID == "" || !strings.Contains(created.Spoken, "Anchored to Alacritty") {
		t.Fatalf("focus.create = %+v", created)
	}

	var parked struct {
		Spoken string `json:"spoken"`
	}
	if err := client.Call("focus.park", map[string]any{"text": "reply to dan"}, &parked); err != nil {
		t.Fatal(err)
	}

	var second struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := client.Call("focus.create", map[string]any{"name": "deploy"}, &second); err != nil {
		t.Fatal(err)
	}

	var switched struct {
		Recap string `json:"recap"`
	}
	if err := client.Call("focus.switch", map[string]any{"thread": "ci refactor"}, &switched); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(switched.Recap, "One parked: reply to dan") {
		t.Errorf("recap = %q", switched.Recap)
	}

	var session struct {
		Spoken string `json:"spoken"`
	}
	if err := client.Call("focus.session.start",
		map[string]any{"thread": "deploy", "minutes": 25}, &session); err != nil {
		t.Fatal(err)
	}

	var listing struct {
		Active  string `json:"active"`
		Threads []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Active      bool   `json:"active"`
			ParkedCount int    `json:"parked_count"`
			Anchors     []struct {
				App  string `json:"app"`
				Gone bool   `json:"gone"`
			} `json:"anchors"`
		} `json:"threads"`
		Session *struct {
			ThreadName   string `json:"thread_name"`
			Phase        string `json:"phase"`
			RemainingSec int    `json:"remaining_sec"`
		} `json:"session"`
	}
	if err := client.Call("focus.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Threads) != 2 || listing.Active != second.Thread.ID {
		t.Fatalf("focus.list = %+v", listing)
	}
	if listing.Threads[0].Name != "deploy" || !listing.Threads[0].Active {
		t.Errorf("the active thread is not first: %+v", listing.Threads)
	}
	if listing.Session == nil || listing.Session.ThreadName != "deploy" ||
		listing.Session.Phase != "running" || listing.Session.RemainingSec <= 0 {
		t.Errorf("session = %+v", listing.Session)
	}
	if listing.Threads[1].ParkedCount != 1 || listing.Threads[1].Anchors[0].App != "Alacritty" {
		t.Errorf("thread annotations = %+v", listing.Threads[1])
	}

	if err := client.Call("focus.session.end", nil, &session); err != nil {
		t.Fatal(err)
	}
	var remind struct {
		Spoken string `json:"spoken"`
	}
	if err := client.Call("focus.remind",
		map[string]any{"thread": "ci refactor", "minutes": 45}, &remind); err != nil {
		t.Fatal(err)
	}
	var ended struct {
		Spoken string `json:"spoken"`
	}
	if err := client.Call("focus.end", map[string]any{"thread": "deploy"}, &ended); err != nil {
		t.Fatal(err)
	}
	// A refusal travels as a JSON-RPC error, never a silent success.
	if err := client.Call("focus.switch", map[string]any{"thread": "vanished"}, &switched); err == nil ||
		!strings.Contains(err.Error(), "no thread is called") {
		t.Errorf("unknown thread err = %v", err)
	}
}

// TestFocusFiringSpeaksThroughTheSessionPath: an idle daemon fires a
// check-in and the recap arrives as a spoken scheduled session — router,
// events, activity, exactly as if the user had asked.
func TestFocusFiringSpeaksThroughTheSessionPath(t *testing.T) {
	h := startFocusDaemon(t)
	th, _, err := h.d.focus.Create(context.Background(), "deploy", 0)
	if err != nil {
		t.Fatal(err)
	}

	seen := collectBus(t, h.d, func() {
		h.d.fireFocus(context.Background(), focus.Firing{
			Kind: focus.FiringReminder, Thread: th,
		})
	})

	executed, ok := busEvent(seen, "intent.executed")
	if !ok {
		t.Fatalf("the firing never routed; saw %v", seen)
	}
	if executed.Data["intent"] != "focus.check" || executed.Data["source"] != "focus" {
		t.Errorf("intent.executed = %v", executed.Data)
	}
	if _, finished := busEvent(seen, "session.finished"); !finished {
		t.Error("the firing's session never finished")
	}
	// fireFocus blocks until the session ended, and the engine speaks before
	// it finishes — so the read is ordered, not polled.
	if !strings.HasPrefix(h.tts.Last().Text, "Deploy:") {
		t.Errorf("spoken check-in = %q", h.tts.Last().Text)
	}
}

// TestFocusFiringIsSkippedWhileASessionIsLive is the daemon half of
// do-not-nag: a live session keeps the microphone, the firing is dropped
// with a report, and nothing queues for later.
func TestFocusFiringIsSkippedWhileASessionIsLive(t *testing.T) {
	h := startFocusDaemon(t)
	th, _, err := h.d.focus.Create(context.Background(), "deploy", 0)
	if err != nil {
		t.Fatal(err)
	}
	// A session holds the floor.
	if _, err := h.d.engine.StartSession(); err != nil {
		t.Fatal(err)
	}

	seen := collectBus(t, h.d, func() {
		h.d.fireFocus(context.Background(), focus.Firing{
			Kind: focus.FiringReminder, Thread: th,
		})
	})

	skipped, ok := busEvent(seen, "focus.skipped")
	if !ok {
		t.Fatalf("no focus.skipped event; saw %v", seen)
	}
	if skipped.Data["kind"] != "reminder" || skipped.Data["reason"] == "" {
		t.Errorf("focus.skipped = %v", skipped.Data)
	}
	if _, routed := busEvent(seen, "intent.executed"); routed {
		t.Error("a skipped firing still ran")
	}
}

// TestFocusFiringWithAnUnroutableNameIsSkippedNotSentToTheModel guards the
// one gap between store and grammar: a hand-edited name too long for the
// phrase table must never reach the model as an unattended question.
func TestFocusFiringWithAnUnroutableNameIsSkippedNotSentToTheModel(t *testing.T) {
	h := startFocusDaemon(t)
	seen := collectBus(t, h.d, func() {
		h.d.fireFocus(context.Background(), focus.Firing{
			Kind: focus.FiringReminder,
			Thread: focus.Thread{
				ID: "t9", Name: "a name of far too many words to ever route",
			},
		})
	})
	if _, started := busEvent(seen, "session.finished"); started {
		t.Error("an unroutable firing still ran a session")
	}
	if len(h.provider.Requests) != 0 {
		t.Error("an unroutable firing reached the model")
	}
}

// TestFocusSwitchRecapsAnAnchoredAISession is the AI-session recap (#124)
// end to end through the daemon's wiring: the anchored terminal's live
// identity line reaches the model inside the pinned prompt, the spoken
// recap is the model's summary, and nothing transient lands in the store.
// The model is the provider fake — the repo's provider seam — and the
// desktop is the compositor fake, so the test runs headless and offline.
func TestFocusSwitchRecapsAnAnchoredAISession(t *testing.T) {
	cfg := testConfig()
	// The recap rides the desktop-context window consent; the compositor is
	// a fake, so enabling it runs no hyprctl anywhere.
	cfg.Context.Window = true
	h := startFocusDaemonWith(t, cfg, desktop.Window{
		Address: "0xa", Class: "Alacritty",
		Title: "✳ fixing the CI workflow — claude", Focused: true,
	})
	summary := "The CI fix is committed and the workflow is green. Next step is pushing the branch."
	h.provider.Response = summary

	ctx := context.Background()
	if _, _, err := h.d.focus.Create(ctx, "the ci refactor", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.d.focus.Create(ctx, "deploy", 0); err != nil {
		t.Fatal(err)
	}
	_, recap, err := h.d.focus.Switch(ctx, "ci refactor")
	if err != nil {
		t.Fatal(err)
	}
	if recap != summary {
		t.Errorf("recap = %q\nwant    %q", recap, summary)
	}

	req := h.provider.LastRequest
	if len(req.Messages) != 1 || req.Messages[0].Role != ai.RoleUser {
		t.Fatalf("recap request shape = %+v", req.Messages)
	}
	if !strings.Contains(req.Messages[0].Content, "✳ fixing the CI workflow — claude") {
		t.Errorf("the live window title never reached the prompt:\n%s", req.Messages[0].Content)
	}
	if !strings.Contains(req.Messages[0].Content, "--- window content ---") {
		t.Errorf("the capture is not delimited in the prompt")
	}
	if req.MaxTokens != recapMaxTokens {
		t.Errorf("recap MaxTokens = %d, want %d", req.MaxTokens, recapMaxTokens)
	}
	if len(req.Tools) != 0 {
		t.Errorf("a recap request advertised tools: %+v", req.Tools)
	}

	// Transient means transient: the summary exists in speech alone.
	stored, err := os.ReadFile(h.d.focus.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "green") {
		t.Errorf("the summary reached the thread store:\n%s", string(stored))
	}
}

// TestFocusSwitchLeavesABrowserAnchorAlone is the trigger policy's daemon
// half: a thread anchored to a browser keeps the core ticket's templated
// recap, and the page is never read to the model — even with the window
// source enabled.
func TestFocusSwitchLeavesABrowserAnchorAlone(t *testing.T) {
	cfg := testConfig()
	cfg.Context.Window = true
	h := startFocusDaemonWith(t, cfg, desktop.Window{
		Address: "0xb", Class: "firefox", Title: "GitHub — pull request #124", Focused: true,
	})

	ctx := context.Background()
	if _, _, err := h.d.focus.Create(ctx, "reviews", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.d.focus.Create(ctx, "deploy", 0); err != nil {
		t.Fatal(err)
	}
	_, recap, err := h.d.focus.Switch(ctx, "reviews")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(recap, "Back on reviews") {
		t.Errorf("a browser anchor changed the recap: %q", recap)
	}
	if len(h.provider.Requests) != 0 {
		t.Error("a browser anchor was read to the model")
	}
}

// TestFocusRecapRespectsTheWindowConsent: with the desktop-context window
// source off — Jarvix's eyes closed by configuration — a terminal anchor
// gets the templated recap, silently, and the model is never asked.
func TestFocusRecapRespectsTheWindowConsent(t *testing.T) {
	h := startFocusDaemon(t) // testConfig keeps every context source off
	ctx := context.Background()
	if _, _, err := h.d.focus.Create(ctx, "the ci refactor", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.d.focus.Create(ctx, "deploy", 0); err != nil {
		t.Fatal(err)
	}
	_, recap, err := h.d.focus.Switch(ctx, "ci refactor")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(recap, "Back on the ci refactor") {
		t.Errorf("recap = %q", recap)
	}
	if len(h.provider.Requests) != 0 {
		t.Error("the recap read a window the user had switched off")
	}
}
