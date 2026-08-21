package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// startActivityDaemon is startDaemon with the daemon handle kept, so a test
// can drive the bus directly — the activity ring is assembled from bus
// events, and publishing them is how its assembly is exercised hermetically.
func startActivityDaemon(t *testing.T, cfg config.Config) (*Daemon, *ipc.Client) {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config:  dir,
		Data:    dir,
		State:   dir,
		Runtime: dir,
		Socket:  filepath.Join(dir, "j.sock"),
	}
	cfg.Audio.MinRecordingMs = 0
	d, err := New(cfg, paths, nil, Deps{
		Provider:    &ai.Fake{Response: "I have opened Firefox for you."},
		Transcriber: &stt.Fake{Text: "open firefox"},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
		Compositor:  desktop.NewFakeCompositor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	client := dialDaemon(t, paths.Socket)
	// A successful dial proves the socket exists, not that the server has
	// accepted the connection — and the per-connection event subscription
	// happens after accept, before the request loop. One round trip proves
	// both, so events published after this call are guaranteed to be pushed.
	if err := client.Call("status.get", nil, nil); err != nil {
		t.Fatal(err)
	}
	return d, client
}

// activityRowsOf calls activity.get and returns the rows.
func activityRowsOf(t *testing.T, client *ipc.Client) []map[string]any {
	t.Helper()
	var out struct {
		Rows  []map[string]any `json:"rows"`
		Limit int              `json:"limit"`
	}
	if err := client.Call("activity.get", nil, &out); err != nil {
		t.Fatal(err)
	}
	return out.Rows
}

// waitActivityRow reads the client's event stream until an activity.row whose
// label contains want arrives — the push half of the feed, and the test's
// synchronisation point: once the push is out, the ring row behind it is
// already appended.
func waitActivityRow(t *testing.T, client *ipc.Client, want string) map[string]any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-client.Events():
			if ev.Type != "activity.row" {
				continue
			}
			if label, _ := ev.Data["label"].(string); strings.Contains(label, want) {
				return ev.Data
			}
		case <-deadline:
			t.Fatalf("no activity.row containing %q arrived", want)
		}
	}
}

func TestActivityRingAssemblesBusEventsInOrder(t *testing.T) {
	d, client := startActivityDaemon(t, testConfig())
	bus := d.Bus()
	bus.Publish(session.Event{Type: "transcript.final",
		Data: map[string]any{"session_id": "s1", "text": "open firefox"}})
	bus.Publish(session.Event{Type: "tool.started",
		Data: map[string]any{"session_id": "s1", "tool": "desktop.launch_app",
			"arguments": `{"app":"firefox"}`}})
	bus.Publish(session.Event{Type: "desktop.refusal",
		Data: map[string]any{"verb": "launch", "target": "firefox", "reason": "it is not installed"}})
	bus.Publish(session.Event{Type: "error",
		Data: map[string]any{"session_id": "s1", "stage": "assistant", "message": "model exploded"}})
	waitActivityRow(t, client, "Failed at assistant")

	rows := activityRowsOf(t, client)
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4: %v", len(rows), rows)
	}
	wantLabels := []string{"You", "Tool: desktop.launch_app", "Launch refused: firefox", "Failed at assistant"}
	var lastSeq float64
	for i, row := range rows {
		if row["label"] != wantLabels[i] {
			t.Errorf("row %d label = %v, want %q", i, row["label"], wantLabels[i])
		}
		seq, _ := row["seq"].(float64)
		if seq <= lastSeq {
			t.Errorf("row %d seq = %v, not increasing past %v", i, seq, lastSeq)
		}
		lastSeq = seq
		if ts, _ := row["ts"].(string); ts == "" {
			t.Errorf("row %d has no timestamp", i)
		}
	}
	// The refusal row carries the daemon's actual reason — the sentence that
	// used to live only in journald.
	if rows[2]["detail"] != "it is not installed" || rows[2]["failed"] != true {
		t.Errorf("refusal row = %v", rows[2])
	}
	// And the launch tool's argument summary named the app, not raw JSON.
	if rows[1]["detail"] != "firefox" {
		t.Errorf("tool row detail = %v", rows[1]["detail"])
	}
}

// A real session through the real engine: the ring ends up holding the turn
// as the user would review it — their words, the model being consulted, the
// answer, and the text-only marker, because the fake provider answered with
// a claim of action and called no tool. This is the incident from the issue,
// reproduced and made visible.
func TestActivityRingRecordsATextOnlyTurn(t *testing.T) {
	_, client := startActivityDaemon(t, testConfig())
	runSession(t, client, "open firefox")

	// Rows are appended by a subscriber the session does not wait for; poll
	// the reconciliation path, which is exactly what a window would render.
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows := activityRowsOf(t, client)
		var labels []string
		for _, row := range rows {
			labels = append(labels, row["label"].(string))
		}
		joined := strings.Join(labels, " | ")
		// The timings row is the session's last (it immediately precedes
		// session.finished), so once it is in the ring the whole turn is.
		if strings.Contains(joined, "Timings") {
			for _, want := range []string{"You", "Asking fake", "Jarvix", "Text-only turn — no tools ran"} {
				if !strings.Contains(joined, want) {
					t.Errorf("rows missing %q: %s", want, joined)
				}
			}
			if strings.Contains(joined, "Tool:") {
				t.Errorf("no tool ran, but a tool row appeared: %s", joined)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the turn's rows never settled; rows: %s", joined)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The ring is bounded by ui.activity_rows and keeps the newest rows — the
// slow-client rule's other half: activity.get returns what the daemon holds,
// never more than configured.
func TestActivityRingIsBounded(t *testing.T) {
	cfg := testConfig()
	cfg.UI.ActivityRows = 3
	d, client := startActivityDaemon(t, cfg)
	for i := 0; i < 8; i++ {
		d.Bus().Publish(session.Event{Type: "transcript.final",
			Data: map[string]any{"text": "question " + string(rune('a'+i))}})
	}
	waitActivityRow(t, client, "You") // rows exist…
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows := activityRowsOf(t, client)
		if len(rows) == 3 && rows[2]["detail"] == "question h" {
			if rows[0]["detail"] != "question f" {
				t.Errorf("oldest kept row = %v, want question f", rows[0]["detail"])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ring never settled at the bound: %v", rows)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// `jarvix new` clears the feed only when the user asked it to; by default
// activity survives a reset, because "what did it just do?" is usually asked
// after starting fresh.
func TestActivityClearsOnResetOnlyWhenConfigured(t *testing.T) {
	t.Run("configured on", func(t *testing.T) {
		cfg := testConfig()
		cfg.UI.ActivityClearOnNew = true
		d, client := startActivityDaemon(t, cfg)
		d.Bus().Publish(session.Event{Type: "transcript.final",
			Data: map[string]any{"text": "before the reset"}})
		waitActivityRow(t, client, "You")
		if err := client.Call("conversation.reset", nil, nil); err != nil {
			t.Fatal(err)
		}
		d.Bus().Publish(session.Event{Type: "transcript.final",
			Data: map[string]any{"text": "after the reset"}})
		waitActivityRow(t, client, "You")
		rows := activityRowsOf(t, client)
		if len(rows) != 1 || rows[0]["detail"] != "after the reset" {
			t.Errorf("rows after a configured clear = %v, want only the post-reset row", rows)
		}
	})
	t.Run("default off", func(t *testing.T) {
		d, client := startActivityDaemon(t, testConfig())
		d.Bus().Publish(session.Event{Type: "transcript.final",
			Data: map[string]any{"text": "before the reset"}})
		waitActivityRow(t, client, "You")
		if err := client.Call("conversation.reset", nil, nil); err != nil {
			t.Fatal(err)
		}
		d.Bus().Publish(session.Event{Type: "transcript.final",
			Data: map[string]any{"text": "after the reset"}})
		waitActivityRow(t, client, "You")
		rows := activityRowsOf(t, client)
		if len(rows) != 2 {
			t.Errorf("rows after a default reset = %v, want both to survive", rows)
		}
	})
}

// The privacy contracts, end to end: events salted with content that must
// never surface cross the real bus, the real subscriber, and the real IPC
// method — and no row repeats it. Pairs with the vocabulary-level mutation
// tests in internal/desktop.
func TestActivityGetNeverServesPrivateContent(t *testing.T) {
	const secret = "hunter2-the-door-code-is-4312"
	d, client := startActivityDaemon(t, testConfig())
	bus := d.Bus()
	bus.Publish(session.Event{Type: "typing.audit",
		Data: map[string]any{"tool": "typing.type_text", "window": "kitty",
			"chars": 29, "outcome": "typed", "text": secret}})
	bus.Publish(session.Event{Type: "tool.started",
		Data: map[string]any{"tool": "memory.remember",
			"arguments": `{"content":"` + secret + `"}`}})
	bus.Publish(session.Event{Type: "memory.injected",
		Data: map[string]any{"facts": 1, "trimmed": 0, "total": 1,
			"est_tokens": 9, "content": secret}})
	waitActivityRow(t, client, "Remembered facts offered")

	rows := activityRowsOf(t, client)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3: %v", len(rows), rows)
	}
	for i, row := range rows {
		for key, value := range row {
			if s, ok := value.(string); ok && strings.Contains(s, secret) {
				t.Errorf("row %d field %s leaked the content: %q", i, key, s)
			}
		}
	}
	// Redaction says something, not nothing: lengths and counts survive.
	if rows[0]["label"] != "Typed 29 characters" {
		t.Errorf("typing row = %v", rows[0])
	}
	if !strings.Contains(rows[1]["detail"].(string), "29 characters (content not shown)") {
		t.Errorf("memory tool row = %v", rows[1])
	}
}

// A fresh daemon serves an empty feed as an array, not null — the window
// renders result.rows without special cases.
func TestActivityGetEmptyIsAnArray(t *testing.T) {
	client, _ := startDaemon(t)
	var out map[string]any
	if err := client.Call("activity.get", nil, &out); err != nil {
		t.Fatal(err)
	}
	rows, ok := out["rows"].([]any)
	if !ok {
		t.Fatalf("rows = %#v, want a JSON array even when empty", out["rows"])
	}
	if len(rows) != 0 {
		t.Errorf("fresh daemon has %d rows", len(rows))
	}
	if limit, _ := out["limit"].(float64); int(limit) != config.Default().UI.ActivityRows {
		t.Errorf("limit = %v, want the default bound", out["limit"])
	}
}
