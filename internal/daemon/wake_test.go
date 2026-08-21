package daemon

import (
	"context"
	"math/rand"
	"path/filepath"
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
	"github.com/rpickz/jarvix/internal/wake"
)

// The daemon's wake wiring, end to end over a real socket, with the
// microphone and the model faked. What is being tested here is the join: a
// detector firing has to become a session, an endpoint has to become a
// submitted transcript, and `wake.mute` has to reach a capture process and
// kill it. Everything either side of the join has its own tests.

type wakeFixture struct {
	client *ipc.Client
	source *wake.FakeSource
	stream *wake.FakeStream
	events <-chan session.Event
}

// startWakeDaemon runs a daemon with background listening enabled and both
// audio ends faked.
func startWakeDaemon(t *testing.T, opts ...func(*config.Config)) *wakeFixture {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock")}

	cfg := testConfig()
	cfg.Audio.MinRecordingMs = 0
	cfg.Activation.Mode = config.ModeWakeWord
	cfg.Activation.WakeRingMs = 240
	cfg.Activation.EndpointSilenceMs = 320
	cfg.Activation.MaxUtteranceSec = 2
	for _, edit := range opts {
		edit(&cfg)
	}

	source := &wake.FakeSource{}
	stream := wake.NewFakeStream(31337)
	source.Push(stream)

	d, err := New(cfg, paths, nil, Deps{
		Provider:     &ai.Fake{Response: "About half full."},
		Transcriber:  &stt.Fake{Text: "Jarvix, what's my disk usage?"},
		Synthesizer:  &tts.Fake{},
		Recorder:     &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:       &audio.FakePlayer{},
		Notifier:     &desktop.FakeNotifier{},
		OpenWindow:   func(context.Context) error { return nil },
		Compositor:   desktop.NewFakeCompositor(),
		WakeSource:   source,
		WakeDetector: func(context.Context) (wake.Detector, error) { return markerDetector(), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	client := dialDaemon(t, paths.Socket)
	return &wakeFixture{client: client, source: source, stream: stream, events: client.Events()}
}

// markerDetector recognises the loud fixture burst and nothing else — the
// same substitution internal/wake's own tests make, so the daemon tests
// describe wiring rather than acoustics.
func markerDetector() wake.Detector {
	return &wake.ScriptedDetector{Label: "fixture", ScoreFunc: func(frame []int16, _ int) (float64, error) {
		var sum float64
		for _, s := range frame {
			sum += float64(s) * float64(s)
		}
		if sum/float64(len(frame)) > 15000*15000 {
			return 0.99, nil
		}
		return 0.02, nil
	}}
}

// fixture audio, matching internal/wake's corpus: quiet room, loud wake word,
// speech-level request.
func pcm(frames int, seed int64, amplitude int) []int16 {
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // fixture, not crypto
	out := make([]int16, frames*wake.FrameSamples)
	for i := range out {
		out[i] = int16(rng.Intn(2*amplitude+1) - amplitude)
	}
	return out
}

func (f *wakeFixture) feed(t *testing.T, samples []int16) {
	t.Helper()
	for off := 0; off+wake.FrameSamples <= len(samples); off += wake.FrameSamples {
		frame := samples[off : off+wake.FrameSamples]
		accepted := make(chan bool, 1)
		go func() { accepted <- f.stream.Feed(frame) }()
		select {
		case ok := <-accepted:
			if !ok {
				t.Fatal("the capture was closed before the fixture was consumed")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out feeding the wake listener")
		}
	}
}

func (f *wakeFixture) waitEvent(t *testing.T, method string) map[string]any {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-f.events:
			if !ok {
				t.Fatalf("the connection closed while waiting for %s", method)
			}
			if ev.Type == method {
				return ev.Data
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", method)
		}
	}
}

// armed asserts that a capture process is up. It reads the status rather than
// waiting for the first `wake.changed`, deliberately: the listener starts with
// the daemon and the indicator's first event can go out before this client has
// even connected. Feeding a frame is the synchronisation — the listener has to
// have opened the capture to read one — and the status is then a fact rather
// than a race.
func (f *wakeFixture) armed(t *testing.T) map[string]any {
	t.Helper()
	report := f.status(t)
	if report["capturing"] != true || report["state"] != string(wake.StateArmed) {
		t.Fatalf("nothing is capturing: %v", report)
	}
	return report
}

// waitWakeState drains `wake.changed` until the indicator reaches want. It
// waits for the state rather than for the next event because an earlier one
// may already be sitting in this client's buffer — the listener starts with
// the daemon, and its first "armed" can predate the connection.
func (f *wakeFixture) waitWakeState(t *testing.T, want wake.State) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-f.events:
			if !ok {
				t.Fatalf("the connection closed while waiting for the indicator to reach %q", want)
			}
			if ev.Type == "wake.changed" && ev.Data["state"] == string(want) {
				return
			}
		case <-deadline:
			t.Fatalf("the indicator never reached %q", want)
		}
	}
}

func (f *wakeFixture) status(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := f.client.Call("wake.status", nil, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The join, end to end: someone says the wake word, and by the time the
// daemon is finished a request has been transcribed and answered without
// anybody touching a key.
func TestWakeWordDrivesAWholeSession(t *testing.T) {
	f := startWakeDaemon(t)
	f.feed(t, pcm(4, 1, 60)) // the room
	f.feed(t, pcm(2, 2, 30000))

	detected := f.waitEvent(t, "wake.detected")
	if c, _ := detected["confidence"].(float64); c < 0.9 {
		t.Errorf("wake.detected carried confidence %v", c)
	}
	// Confidence and nothing else. A wake event fans out to every connected
	// client, so it must never carry audio or text.
	for _, forbidden := range []string{"text", "audio", "transcript", "pcm"} {
		if _, present := detected[forbidden]; present {
			t.Errorf("wake.detected carries %q", forbidden)
		}
	}

	f.feed(t, pcm(6, 3, 6000)) // "...what's my disk usage?"
	f.feed(t, pcm(6, 4, 60))   // ...and stops talking

	// Endpointing submits: no key press anywhere in this test.
	if got := f.waitEvent(t, "transcript.final")["text"]; got != "Jarvix, what's my disk usage?" {
		t.Errorf("transcript is %v", got)
	}
	f.waitEvent(t, "assistant.finished")
	f.waitEvent(t, "session.finished")
}

// `jarvix mute` over the socket. The answer must already be true when it
// arrives: the capture process gone, the report saying so, and the indicator
// changed.
func TestMuteOverIPCKillsTheCapture(t *testing.T) {
	f := startWakeDaemon(t)
	f.feed(t, pcm(1, 5, 60))
	before := f.armed(t)
	if pid, _ := before["pid"].(float64); pid != 31337 {
		t.Errorf("status names capture pid %v, want the fake's 31337", pid)
	}

	var report map[string]any
	if err := f.client.Call("wake.mute", map[string]bool{"muted": true}, &report); err != nil {
		t.Fatal(err)
	}
	if report["muted"] != true || report["capturing"] != false {
		t.Errorf("the mute reply says muted=%v capturing=%v", report["muted"], report["capturing"])
	}
	if pid, _ := report["pid"].(float64); pid != 0 {
		t.Errorf("the mute reply still names pid %v", pid)
	}
	if f.stream.Closes() == 0 {
		t.Error("wake.mute returned with the capture process still running")
	}
	f.waitWakeState(t, wake.StateMuted)
}

// A muted daemon must hear nothing at all. This is the assertion behind the
// whole feature being trustworthy: not "Jarvix ignores you", but "there is
// nothing listening".
func TestAMutedDaemonDoesNotHearTheWakeWord(t *testing.T) {
	f := startWakeDaemon(t)
	f.feed(t, pcm(1, 6, 60))
	f.armed(t)

	if err := f.client.Call("wake.mute", map[string]bool{"muted": true}, nil); err != nil {
		t.Fatal(err)
	}
	// The stream is closed, so the fixture cannot even be delivered — which
	// is the point, and is why this is not "feed a wake word and assert
	// nothing happened".
	if f.stream.Feed(pcm(1, 7, 30000)) {
		t.Error("audio was still being read after the capture process was killed")
	}
	if status := f.status(t); status["capturing"] != false || status["state"] != string(wake.StateMuted) {
		t.Errorf("a muted daemon reports %v", status)
	}
}

// status.get carries the whole story so one call answers "is my microphone
// open?" — including the pid, which is what makes the answer checkable
// against the process table rather than merely believable.
func TestStatusReportsBackgroundListening(t *testing.T) {
	f := startWakeDaemon(t)
	f.feed(t, pcm(1, 8, 60))
	f.armed(t)

	var status map[string]any
	if err := f.client.Call("status.get", nil, &status); err != nil {
		t.Fatal(err)
	}
	if status["wake_state"] != string(wake.StateArmed) {
		t.Errorf("status.get reports wake_state %v", status["wake_state"])
	}
	report, ok := status["wake"].(map[string]any)
	if !ok {
		t.Fatalf("status.get carries no wake report: %v", status["wake"])
	}
	for key, want := range map[string]any{
		"mode":    config.ModeWakeWord,
		"enabled": true,
		"running": true,
		"muted":   false,
		"word":    "jarvix",
	} {
		if report[key] != want {
			t.Errorf("wake report %s = %v, want %v", key, report[key], want)
		}
	}
	if pid, _ := report["pid"].(float64); pid != 31337 {
		t.Errorf("wake report names pid %v", pid)
	}
	if report["detector"] != "fixture" {
		t.Errorf("wake report names detector %v", report["detector"])
	}
}

// Push-to-talk mode leaves everything switched off, and the surface still
// answers: `jarvix mute` on a daemon that is not listening should say so
// rather than fail.
func TestWakeSurfaceAnswersWhenTheFeatureIsOff(t *testing.T) {
	client, _ := startDaemon(t) // the ordinary push-to-talk daemon
	var report map[string]any
	if err := client.Call("wake.mute", map[string]bool{"muted": true}, &report); err != nil {
		t.Fatal(err)
	}
	if report["enabled"] != false || report["capturing"] != false {
		t.Errorf("a push-to-talk daemon reports %v", report)
	}
	if report["mode"] != config.ModePushToTalk {
		t.Errorf("the report names mode %v", report["mode"])
	}
	var status map[string]any
	if err := client.Call("status.get", nil, &status); err != nil {
		t.Fatal(err)
	}
	if status["wake_state"] != string(wake.StateOff) {
		t.Errorf("status.get reports wake_state %v with the feature off", status["wake_state"])
	}
}

// The degradation path. A configured wake word whose detector is not
// installed must leave the daemon running and push-to-talk working, with the
// report saying plainly what is wrong — not a supervisor retrying a helper
// that was never there.
func TestAMissingDetectorDegradesToPushToTalk(t *testing.T) {
	dir := t.TempDir()
	paths := config.Paths{Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock")}
	cfg := testConfig()
	cfg.Activation.Mode = config.ModeWakeWord
	cfg.Activation.WakeCommand = []string{"jarvix-wake-that-is-not-installed"}

	d, err := New(cfg, paths, nil, Deps{
		Provider:    &ai.Fake{},
		Transcriber: &stt.Fake{Text: "hello"},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
		Compositor:  desktop.NewFakeCompositor(),
	})
	if err != nil {
		t.Fatalf("a missing wake detector stopped the daemon from being built: %v", err)
	}
	serveDaemon(t, d)
	client := dialDaemon(t, paths.Socket)

	var report map[string]any
	if err := client.Call("wake.status", nil, &report); err != nil {
		t.Fatal(err)
	}
	if report["enabled"] != true {
		t.Error("the report hides that background listening was asked for")
	}
	if report["running"] != false || report["capturing"] != false {
		t.Errorf("something is listening despite the missing detector: %v", report)
	}
	if reason, _ := report["last_reason"].(string); reason == "" {
		t.Error("the report does not say why background listening is not running")
	}

	// And the daemon is entirely usable: this is what "degrades to PTT-only"
	// has to mean.
	if err := client.Call("session.start", nil, nil); err != nil {
		t.Fatalf("push-to-talk broke because a wake detector was missing: %v", err)
	}
}
