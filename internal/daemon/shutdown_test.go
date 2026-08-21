package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/history"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// syncBuffer collects log output written from the daemon's own goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// shutdownHarness is a fully wired daemon whose shutdown the test drives by
// hand: Run is started and stopped explicitly rather than by t.Cleanup, and
// the history store's write can be held open across the stop.
type shutdownHarness struct {
	daemon  *Daemon
	client  *ipc.Client
	store   *history.Fake
	log     *syncBuffer
	stop    context.CancelFunc
	stopped chan struct{}
	// release lets a held-open history write complete.
	release func()
}

// startShutdownDaemon boots a daemon with a gated history store. deps is
// applied last so a test can substitute the provider or the notifier.
func startShutdownDaemon(t *testing.T, tune func(d *Daemon), override func(*Deps)) *shutdownHarness {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	cfg := testConfig()
	cfg.Audio.MinRecordingMs = 0

	store := history.NewFake()
	// Buffered: the gate below is what holds a write open, so a test that is
	// not about persistence need not receive from this to keep the daemon
	// moving. Receiving from it still proves a write is under way and parked.
	store.SaveStarted = make(chan struct{}, 4)
	gate := make(chan struct{})
	store.SaveGate = gate
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	// Whatever the test asserts, nothing may be left parked on the gate.
	t.Cleanup(release)

	logs := &syncBuffer{}
	deps := Deps{
		Provider:     &ai.Fake{Response: "Answered."},
		Transcriber:  &stt.Fake{Text: "hello computer"},
		Synthesizer:  &tts.Fake{},
		Recorder:     &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:       &audio.FakePlayer{},
		Notifier:     &desktop.FakeNotifier{},
		OpenWindow:   func(context.Context) error { return nil },
		HistoryStore: store,
	}
	if override != nil {
		override(&deps)
	}
	d, err := New(cfg, paths, slog.New(slog.NewTextHandler(logs, nil)), deps)
	if err != nil {
		t.Fatal(err)
	}
	if tune != nil {
		tune(d)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = d.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-stopped })

	return &shutdownHarness{
		daemon: d, client: dialDaemon(t, paths.Socket), store: store,
		log: logs, stop: cancel, stopped: stopped, release: release,
	}
}

// quiesced reports whether every session goroutine has finished. A second
// Shutdown with an already-expired context answers that without waiting: an
// idle engine returns nil, a busy one returns the context error.
func quiesced(d *Daemon) error {
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	return d.engine.Shutdown(expired)
}

// The ticket in one test: a session finishes, the daemon is stopped
// immediately, and the exchange is still in the persisted conversation.
func TestShutdownPersistsTheLastExchange(t *testing.T) {
	h := startShutdownDaemon(t, nil, nil)
	runSession(t, h.client, "remember this exchange")

	// The session is over as far as every client is concerned, and the write
	// that would survive a restart has not landed yet. Stopping here is what
	// used to lose the exchange.
	<-h.store.SaveStarted
	h.stop()
	h.release()
	<-h.stopped

	if n := h.store.Saves(); n != 1 {
		t.Errorf("store saw %d saves by the time Run returned, want 1", n)
	}
	msgs, _, err := h.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	var persisted []string
	for _, m := range msgs {
		persisted = append(persisted, m.Content)
	}
	if !strings.Contains(strings.Join(persisted, " | "), "remember this exchange") {
		t.Errorf("persisted conversation lost the last exchange: %v", persisted)
	}
}

// A wedged write must not be able to keep jarvixd alive. With the grace period
// shortened and the write held open for good, Run still returns — and says in
// the journal which stage it gave up on.
func TestShutdownGivesUpAfterTheGracePeriod(t *testing.T) {
	h := startShutdownDaemon(t, func(d *Daemon) { d.shutdownGrace = time.Millisecond }, nil)
	runSession(t, h.client, "an exchange whose write wedges")
	<-h.store.SaveStarted // held open, and never released before Run must exit

	h.stop()
	<-h.stopped // the assertion: a hung write did not become a hung daemon

	logged := h.log.String()
	if !strings.Contains(logged, "shutdown drain gave up waiting") {
		t.Errorf("shutdown gave up silently; log was:\n%s", logged)
	}
	if !strings.Contains(logged, "stage=sessions") {
		t.Errorf("the log does not name the stage that did not settle:\n%s", logged)
	}
	if !strings.Contains(logged, "outstanding=1") {
		t.Errorf("the log does not say how much work was outstanding:\n%s", logged)
	}
}

// blockingProvider parks inside Chat until the session context is cancelled —
// a model call still streaming when the user stops the daemon.
type blockingProvider struct {
	entered chan struct{}
	left    chan struct{}
}

func (p *blockingProvider) Name() string { return "blocking" }

func (p *blockingProvider) Chat(ctx context.Context, _ ai.ChatRequest) (<-chan ai.Event, error) {
	close(p.entered)
	<-ctx.Done()
	close(p.left)
	return nil, ctx.Err()
}

func TestShutdownDrainsAMidFlightSession(t *testing.T) {
	provider := &blockingProvider{entered: make(chan struct{}), left: make(chan struct{})}
	h := startShutdownDaemon(t, nil, func(d *Deps) { d.Provider = provider })

	if err := h.client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Call("session.submit", map[string]string{"text": "answer slowly"}, nil); err != nil {
		t.Fatal(err)
	}
	<-provider.entered // the model call is running and will not end by itself

	h.stop()
	<-h.stopped

	// Non-blocking on purpose: this asserts what was already true when Run
	// returned, rather than what becomes true if the test waits a little.
	select {
	case <-provider.left:
	default:
		t.Error("Run returned while the assistant call was still running")
	}
	if err := quiesced(h.daemon); err != nil {
		t.Errorf("session goroutines were still in flight when Run returned: %v", err)
	}
}

// blockingNotifier holds a delivery open the way notify-send --wait does
// while a notification sits unclicked on screen. It ignores the context on
// purpose: a delivery that unblocked by itself would settle before the drain
// could be seen waiting for it.
type blockingNotifier struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (n *blockingNotifier) Send(context.Context, desktop.Notification) (string, error) {
	n.once.Do(func() { close(n.entered) })
	<-n.release
	return "", nil
}

// Notification delivery is post-session work of exactly the same shape as the
// history write, and shutdown has to account for all of it — otherwise the
// next feature dispatched this way reintroduces the bug.
//
// A delivery that never returns is drained like any other stuck stage: bounded
// and reported. That report is also the proof the stage is drained at all — a
// shutdown that ignored notifications would have nothing to say about them.
func TestShutdownDrainsNotificationDelivery(t *testing.T) {
	notifier := &blockingNotifier{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(notifier.release) })
	h := startShutdownDaemon(t,
		func(d *Daemon) { d.shutdownGrace = time.Millisecond },
		func(d *Deps) { d.Notifier = notifier })
	runSession(t, h.client, "notify me")
	<-notifier.entered

	h.release() // the history write is not what this test is about
	h.stop()
	<-h.stopped // a notification nobody clicked did not become a daemon nobody could stop

	logged := h.log.String()
	if !strings.Contains(logged, "stage=notifications") {
		t.Errorf("shutdown did not wait for notification delivery; log was:\n%s", logged)
	}
}
