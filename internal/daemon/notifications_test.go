package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// windowSpy records conversation-window open requests from notification
// clicks. Opens happen on a dispatch goroutine, hence the lock.
type windowSpy struct {
	mu    sync.Mutex
	opens int
}

func (w *windowSpy) open(context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.opens++
	return nil
}

func (w *windowSpy) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.opens
}

// startNotifyDaemon is startDaemon with control over the notification
// config and collaborators.
func startNotifyDaemon(t *testing.T, cfg config.Config, notifier desktop.Notifier, opener func(context.Context) error, provider *ai.Fake) *ipc.Client {
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
		Provider:    provider,
		Transcriber: &stt.Fake{Text: "hello computer"},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    notifier,
		OpenWindow:  opener,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = d.Run(ctx) }()

	var client *ipc.Client
	deadline := time.Now().Add(5 * time.Second)
	for {
		client, err = ipc.Dial(paths.Socket)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon socket never came up: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// runSession drives one text session to its terminal event.
func runSession(t *testing.T, client *ipc.Client, text string) {
	t.Helper()
	if err := client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call("session.submit", map[string]string{"text": text}, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-client.Events():
			if ev.Type == "session.finished" || ev.Type == "session.cancelled" {
				return
			}
		case <-deadline:
			t.Fatal("session never finished")
		}
	}
}

// waitForNotifications polls the fake until n notifications arrived —
// dispatch runs on its own goroutine after session.finished.
func waitForNotifications(t *testing.T, fake *desktop.FakeNotifier, n int) []desktop.Notification {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if sent := fake.Sent(); len(sent) >= n {
			return sent
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected %d notifications, got %d", n, len(fake.Sent()))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFinishedSessionNotifiesWithAnswerPreview(t *testing.T) {
	fake := &desktop.FakeNotifier{}
	client := startNotifyDaemon(t, config.Default(), fake, (&windowSpy{}).open,
		&ai.Fake{Response: "Recursion is a function calling itself."})

	runSession(t, client, "explain recursion")

	sent := waitForNotifications(t, fake, 1)
	n := sent[0]
	if n.Summary != "Jarvix answered" {
		t.Errorf("summary = %q", n.Summary)
	}
	if !strings.HasPrefix(n.Body, "Recursion is a function") {
		t.Errorf("body = %q, want the answer preview", n.Body)
	}
	if len(n.Actions) != 1 || n.Actions[0].ID != desktop.DefaultActionID || n.Actions[0].Label != "Open" {
		t.Errorf("actions = %+v, want the default Open action", n.Actions)
	}
}

func TestNotificationPreviewTruncatesLongAnswers(t *testing.T) {
	fake := &desktop.FakeNotifier{}
	long := strings.Repeat("all work and no play makes jarvix a dull daemon ", 5)
	client := startNotifyDaemon(t, config.Default(), fake, (&windowSpy{}).open, &ai.Fake{Response: long})

	runSession(t, client, "ramble")

	n := waitForNotifications(t, fake, 1)[0]
	if got := len([]rune(n.Body)); got > notificationPreviewLimit+1 { // +1 for the ellipsis
		t.Errorf("body is %d runes, want at most %d", got, notificationPreviewLimit+1)
	}
	if !strings.HasSuffix(n.Body, "…") {
		t.Errorf("truncated body should end with an ellipsis: %q", n.Body)
	}
}

func TestNotificationPreviewOffHidesContent(t *testing.T) {
	fake := &desktop.FakeNotifier{}
	cfg := config.Default()
	cfg.UI.NotificationPreview = false
	client := startNotifyDaemon(t, cfg, fake, (&windowSpy{}).open,
		&ai.Fake{Response: "The secret answer."})

	runSession(t, client, "tell me a secret")

	n := waitForNotifications(t, fake, 1)[0]
	if n.Summary != "Jarvix answered" || n.Body != "" {
		t.Errorf("notification leaked content: %+v", n)
	}
}

func TestNotificationsDisabledSendsNothing(t *testing.T) {
	fake := &desktop.FakeNotifier{}
	cfg := config.Default()
	cfg.UI.Notifications = false
	client := startNotifyDaemon(t, cfg, fake, (&windowSpy{}).open, &ai.Fake{Response: "Quiet."})

	runSession(t, client, "hi")

	// Give a wrongly started watcher time to misbehave before asserting.
	time.Sleep(100 * time.Millisecond)
	if sent := fake.Sent(); len(sent) != 0 {
		t.Errorf("notifications disabled, but %d were sent", len(sent))
	}
}

func TestErrorSessionNotifiesStageAndMessage(t *testing.T) {
	fake := &desktop.FakeNotifier{}
	client := startNotifyDaemon(t, config.Default(), fake, (&windowSpy{}).open,
		&ai.Fake{Response: "irrelevant", Fail: errors.New("model exploded")})

	runSession(t, client, "boom")

	n := waitForNotifications(t, fake, 1)[0]
	if n.Summary != "Jarvix hit a problem" {
		t.Errorf("summary = %q", n.Summary)
	}
	if !strings.Contains(n.Body, "assistant") || !strings.Contains(n.Body, "model exploded") {
		t.Errorf("body = %q, want failing stage and message", n.Body)
	}
}

func TestErrorNotificationWithoutPreviewKeepsStageOnly(t *testing.T) {
	fake := &desktop.FakeNotifier{}
	cfg := config.Default()
	cfg.UI.NotificationPreview = false
	client := startNotifyDaemon(t, cfg, fake, (&windowSpy{}).open,
		&ai.Fake{Fail: errors.New("secret-ish detail")})

	runSession(t, client, "boom")

	n := waitForNotifications(t, fake, 1)[0]
	if !strings.Contains(n.Body, "assistant") {
		t.Errorf("body = %q, want the failing stage", n.Body)
	}
	if strings.Contains(n.Body, "secret-ish detail") {
		t.Errorf("preview disabled, but the message leaked: %q", n.Body)
	}
}

func TestNotificationClickOpensWindow(t *testing.T) {
	fake := &desktop.FakeNotifier{InvokeAction: desktop.DefaultActionID}
	spy := &windowSpy{}
	client := startNotifyDaemon(t, config.Default(), fake, spy.open, &ai.Fake{Response: "Hello."})

	runSession(t, client, "hi")

	deadline := time.Now().Add(5 * time.Second)
	for spy.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("notification click never opened the window")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDismissedNotificationLeavesWindowClosed(t *testing.T) {
	fake := &desktop.FakeNotifier{} // InvokeAction "" = dismissed/expired
	spy := &windowSpy{}
	client := startNotifyDaemon(t, config.Default(), fake, spy.open, &ai.Fake{Response: "Hello."})

	runSession(t, client, "hi")

	waitForNotifications(t, fake, 1)
	time.Sleep(50 * time.Millisecond) // let a wrong open() happen if it would
	if spy.count() != 0 {
		t.Errorf("window opened %d times without a click", spy.count())
	}
}

func TestAbsentNotificationDaemonDegradesQuietly(t *testing.T) {
	fake := &desktop.FakeNotifier{Err: errors.New("no notification daemon on the bus")}
	spy := &windowSpy{}
	client := startNotifyDaemon(t, config.Default(), fake, spy.open, &ai.Fake{Response: "Hello."})

	// The session must still complete normally; delivery failure is log-only.
	runSession(t, client, "hi")
	waitForNotifications(t, fake, 1)
	if spy.count() != 0 {
		t.Errorf("window opened despite failed delivery")
	}
	// And the daemon keeps working for the next session.
	runSession(t, client, "again")
	waitForNotifications(t, fake, 2)
}

func TestConversationGetReturnsTurns(t *testing.T) {
	fake := &desktop.FakeNotifier{}
	client := startNotifyDaemon(t, config.Default(), fake, (&windowSpy{}).open,
		&ai.Fake{Response: "Recursion is a function calling itself."})

	runSession(t, client, "explain recursion")

	var out struct {
		Turns []struct {
			Role string `json:"role"`
			Text string `json:"text"`
		} `json:"turns"`
		State     string `json:"state"`
		SessionID string `json:"session_id"`
	}
	if err := client.Call("conversation.get", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.State != "idle" {
		t.Errorf("state = %q", out.State)
	}
	if len(out.Turns) != 2 {
		t.Fatalf("turns = %+v, want user+assistant", out.Turns)
	}
	if out.Turns[0].Role != "user" || out.Turns[0].Text != "explain recursion" {
		t.Errorf("turn 0 = %+v", out.Turns[0])
	}
	if out.Turns[1].Role != "assistant" || out.Turns[1].Text != "Recursion is a function calling itself." {
		t.Errorf("turn 1 = %+v", out.Turns[1])
	}
}

// A window is opened by clicking the notification, which happens after the
// session — and its `error` event — is over. The snapshot must therefore
// carry the failure, or an error notification opens onto a blameless idle
// conversation and the user never learns what went wrong.
func TestConversationGetReportsTheLastFailure(t *testing.T) {
	fake := &desktop.FakeNotifier{}
	client := startNotifyDaemon(t, config.Default(), fake, (&windowSpy{}).open,
		&ai.Fake{Fail: errors.New("model exploded")})

	runSession(t, client, "explain recursion")
	waitForNotifications(t, fake, 1)

	var out struct {
		State        string `json:"state"`
		ErrorStage   string `json:"error_stage"`
		ErrorMessage string `json:"error_message"`
	}
	if err := client.Call("conversation.get", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.ErrorStage != "assistant" {
		t.Errorf("error_stage = %q, want the failing stage", out.ErrorStage)
	}
	if !strings.Contains(out.ErrorMessage, "model exploded") {
		t.Errorf("error_message = %q, want the failure reason", out.ErrorMessage)
	}
}

// The banner must not outlive the failure: once a new session is under way
// the previous error is history.
func TestConversationGetClearsTheFailureOnTheNextSession(t *testing.T) {
	fake := &desktop.FakeNotifier{}
	provider := &ai.Fake{Fail: errors.New("model exploded")}
	client := startNotifyDaemon(t, config.Default(), fake, (&windowSpy{}).open, provider)

	runSession(t, client, "first")
	waitForNotifications(t, fake, 1)

	provider.Fail = nil
	provider.Response = "All better."
	runSession(t, client, "second")
	waitForNotifications(t, fake, 2)

	var out struct {
		ErrorStage   string `json:"error_stage"`
		ErrorMessage string `json:"error_message"`
	}
	if err := client.Call("conversation.get", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.ErrorStage != "" || out.ErrorMessage != "" {
		t.Errorf("stale failure reported: stage=%q message=%q", out.ErrorStage, out.ErrorMessage)
	}
}

func TestConversationGetEmptyIsAnArray(t *testing.T) {
	client, _ := startDaemon(t)
	var out map[string]any
	if err := client.Call("conversation.get", nil, &out); err != nil {
		t.Fatal(err)
	}
	turns, ok := out["turns"].([]any)
	if !ok {
		t.Fatalf("turns = %#v, want a JSON array even when empty", out["turns"])
	}
	if len(turns) != 0 {
		t.Errorf("fresh daemon has %d turns", len(turns))
	}
}
