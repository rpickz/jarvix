package daemon

import (
	"context"
	"path/filepath"
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

// Issue #191, end to end through the daemon. The STT adapter discards a
// capture that carried no voice and a transcript that was only the bias
// prompt handed back; what has to be true afterwards is that the user can
// still *see* the press happened. This file pins the two surfaces that
// promise it: the activity feed says a capture produced nothing and why, and
// nothing anywhere claims Jarvix answered.

// silentCaptureDaemon wires a daemon whose transcriber reports a capture that
// produced no words, and hands back the pieces needed to drain it: shutting
// the daemon down is what makes "no notification was posted" a fact rather
// than a race, because the drain waits on the post-session goroutine that
// would have sent one.
func silentCaptureDaemon(t *testing.T, reason string) (client *ipc.Client, sent *desktop.FakeNotifier, drain func()) {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config:  dir,
		Data:    dir,
		State:   dir,
		Runtime: dir,
		Socket:  filepath.Join(dir, "j.sock"),
	}
	cfg := testConfig()
	cfg.Audio.MinRecordingMs = 0
	notifier := &desktop.FakeNotifier{}
	d, err := New(cfg, paths, nil, Deps{
		Provider:    &ai.Fake{Response: "this answer must never be reached"},
		Transcriber: &stt.Fake{Text: "", Reason: reason},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    notifier,
		OpenWindow:  func(context.Context) error { return nil },
		Compositor:  desktop.NewFakeCompositor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = d.Run(ctx)
	}()
	stop := func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Error("the daemon did not shut down")
		}
	}
	t.Cleanup(stop)
	client = dialDaemon(t, paths.Socket)
	if err := client.Call("status.get", nil, nil); err != nil {
		t.Fatal(err)
	}
	return client, notifier, stop
}

// press drives one push-to-talk capture to the end of its session.
func press(t *testing.T, client *ipc.Client) {
	t.Helper()
	for _, verb := range []string{"session.start", "voice.start", "voice.stop"} {
		if err := client.Call(verb, nil, nil); err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
	}
	if err := client.Call("session.submit", map[string]string{"text": ""}, nil); err != nil {
		t.Fatal(err)
	}
}

// The visible half of the fix. Discarding the hallucination is only defensible
// because the discard is *reported*: a user whose microphone is muted has to
// be able to see that pressing the key produced nothing, and the invented
// transcript was what denied them that.
func TestTheActivityFeedRecordsACaptureThatProducedNothing(t *testing.T) {
	const reason = "the capture had no voiced audio (peak -inf dBFS, floor -72 dBFS)"
	client, _, _ := silentCaptureDaemon(t, reason)

	press(t, client)

	// The pushed row is the barrier: once it is out, the ring behind it is
	// already appended.
	row := waitActivityRow(t, client, desktop.PendingTurnNothingHeard)
	if got, _ := row["detail"].(string); got != reason {
		t.Errorf("row detail = %q, want the measurement %q", got, reason)
	}
	if failed, _ := row["failed"].(bool); failed {
		t.Error("the row is marked failed; a muted microphone is not a fault")
	}
	if kind, _ := row["kind"].(string); kind != desktop.ActivityKindWake {
		t.Errorf("row kind = %q, want %q — the glyph should point at the microphone",
			kind, desktop.ActivityKindWake)
	}

	for _, r := range activityRowsOf(t, client) {
		if kind, _ := r["kind"].(string); kind == desktop.ActivityKindError {
			t.Errorf("an error row was recorded: %v", r)
		}
		if kind, _ := r["kind"].(string); kind == desktop.ActivityKindYou {
			t.Errorf("a transcript row was recorded for a capture with no words: %v", r)
		}
	}
}

// And nothing claims an answer. Before this change an empty transcript failed
// the session, which posted "Jarvix hit a problem" and held it until the next
// press; the naive fix — finishing quietly — posts "Jarvix answered" instead,
// which is a different false statement about the same silence.
func TestACaptureThatProducedNothingNotifiesNothing(t *testing.T) {
	client, notifier, drain := silentCaptureDaemon(t, "no speech was recognised")

	press(t, client)
	waitActivityRow(t, client, desktop.PendingTurnNothingHeard)

	// The drain waits on the very goroutine a notification would be sent
	// from, so after it "none were sent" is settled rather than merely not
	// yet observed.
	drain()
	if sent := notifier.Sent(); len(sent) != 0 {
		t.Errorf("notifications = %+v, want none for a capture that produced nothing", sent)
	}
}
