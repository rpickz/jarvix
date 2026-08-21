package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/history"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// gatedStore is a Fake whose Save can be held open for the duration of a
// shutdown. Persistence runs *after* session.finished and off the engine's
// lock (ADR 0011), so the only way to test that shutdown waits for it is to
// arrange for a write to be genuinely in flight — not to hope one is.
func gatedStore(t *testing.T) (store *history.Fake, release func()) {
	t.Helper()
	store = history.NewFake()
	store.SaveStarted = make(chan struct{})
	gate := make(chan struct{})
	store.SaveGate = gate
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	// Always released at the end: a test that fails before releasing must not
	// leave a goroutine parked on the gate for the rest of the package run.
	t.Cleanup(release)
	return store, release
}

func persistOptions() Options {
	return Options{Model: "test-model", HistoryTurns: 8, FollowUpWindow: time.Hour}
}

// The defect this ticket fixes: a daemon stopped just after an exchange lost
// that exchange, because the write that records it had not happened yet.
func TestShutdownWaitsForThePendingHistoryWrite(t *testing.T) {
	store, release := gatedStore(t)
	h := newHarnessWithStore(t, persistOptions(), store)
	h.ask(t, "remember this exchange")

	// The session is over — finished, idle, every event published — and yet
	// the write that would survive a restart has not landed. That gap is the
	// bug.
	<-store.SaveStarted

	done := make(chan error, 1)
	go func() { done <- h.engine.Shutdown(context.Background()) }()
	release()
	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// No further waiting here on purpose: if Shutdown returned before the
	// write completed, these read the state it left behind.
	if n := store.Saves(); n != 1 {
		t.Errorf("store saw %d saves by the time Shutdown returned, want 1", n)
	}
	msgs, _, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	var persisted []string
	for _, m := range msgs {
		persisted = append(persisted, m.Content)
	}
	if !strings.Contains(strings.Join(persisted, " | "), "remember this exchange") {
		t.Errorf("persisted history lost the last exchange: %v", persisted)
	}
}

// The bound, and the proof that the wait is real. With the write held open
// forever and the context already expired, a Shutdown that did not wait would
// return nil — so the timeout is the assertion, and no timer is needed to
// observe it.
func TestShutdownIsBoundedByItsContext(t *testing.T) {
	store, release := gatedStore(t)
	h := newHarnessWithStore(t, persistOptions(), store)
	h.ask(t, "an exchange whose write wedges")
	<-store.SaveStarted

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	err := h.engine.Shutdown(expired)
	if err == nil {
		t.Fatal("Shutdown returned nil with a history write still in flight")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Shutdown error = %v, want a context error the caller can log", err)
	}
	if n := h.engine.InFlight(); n != 1 {
		t.Errorf("InFlight = %d, want the 1 stuck goroutine the log should name", n)
	}
	// Giving up is not carrying on: the engine is still shut.
	if _, err := h.engine.StartSession(); err == nil {
		t.Error("a session started after the drain gave up")
	}

	// Let the wedged write finish, and the engine is quiescent again — which a
	// second Shutdown reports even with an expired context.
	release()
	awaitOp(t, store, "save")
	if err := h.engine.Shutdown(expired); err != nil {
		t.Errorf("Shutdown after the write completed: %v", err)
	}
}

// blockingProvider parks inside Chat until the session context is cancelled,
// which is what a real provider call looks like when the user walks away
// mid-answer. entered and left make the goroutine's lifetime observable.
type blockingProvider struct {
	entered  chan struct{}
	left     chan struct{}
	entering sync.Once
	leaving  sync.Once
}

func (p *blockingProvider) Name() string { return "blocking" }

func (p *blockingProvider) Chat(ctx context.Context, _ ai.ChatRequest) (<-chan ai.Event, error) {
	// Once-guarded so a regression that starts a *second* session reports
	// itself as a failed assertion rather than a panic on a closed channel.
	p.entering.Do(func() { close(p.entered) })
	<-ctx.Done()
	p.leaving.Do(func() { close(p.left) })
	return nil, ctx.Err()
}

func TestShutdownCancelsAndDrainsASessionInFlight(t *testing.T) {
	provider := &blockingProvider{entered: make(chan struct{}), left: make(chan struct{})}
	bus := NewBus(nil)
	events, unsubscribe := bus.Subscribe()
	t.Cleanup(unsubscribe)
	eng := NewEngine(provider, &stt.Fake{Text: "unused"}, &tts.Fake{},
		&audio.FakeRecorder{Clip: audio.Clip{WAVPath: t.TempDir() + "/rec.wav"}},
		&audio.FakePlayer{}, nil, nil, bus, nil, Options{Model: "test-model"})

	if _, err := eng.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := eng.Submit("a question that never gets answered"); err != nil {
		t.Fatal(err)
	}
	<-provider.entered // the assistant call is running and will not return on its own

	if err := eng.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Non-blocking, deliberately: these assert what was already true when
	// Shutdown returned, not what becomes true if the test waits.
	select {
	case <-provider.left:
	default:
		t.Error("Shutdown returned while the assistant call was still running")
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := eng.Shutdown(expired); err != nil {
		t.Errorf("session goroutines were still in flight when Shutdown returned: %v", err)
	}

	if state, id := eng.State(); state != StateIdle || id != "" {
		t.Errorf("state after shutdown = %s/%q, want idle with no session", state, id)
	}
	if _, err := eng.StartSession(); err == nil {
		t.Error("a new session started after shutdown")
	}
	if !sawEvent(events, "session.cancelled") {
		t.Error("the in-flight session was drained without a session.cancelled event")
	}
}

// A typed turn (ADR 0021) reaches the engine by a different door — SubmitText
// composes start+submit under one lock — but it is the same session
// afterwards. Both halves of that are asserted here, because a second entry
// point is exactly how a shutdown gap gets reopened: the door has to be shut
// to typing too, and a typed session in flight has to be drained like a spoken
// one.
func TestShutdownDrainsAndRefusesTypedTurns(t *testing.T) {
	provider := &blockingProvider{entered: make(chan struct{}), left: make(chan struct{})}
	bus := NewBus(nil)
	_, unsubscribe := bus.Subscribe()
	t.Cleanup(unsubscribe)
	eng := NewEngine(provider, &stt.Fake{Text: "unused"}, &tts.Fake{},
		&audio.FakeRecorder{Clip: audio.Clip{WAVPath: t.TempDir() + "/rec.wav"}},
		&audio.FakePlayer{}, nil, nil, bus, nil, Options{Model: "test-model"})

	if _, err := eng.SubmitText("a typed question that never gets answered"); err != nil {
		t.Fatal(err)
	}
	<-provider.entered

	if err := eng.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-provider.left:
	default:
		t.Error("Shutdown returned while a typed turn was still with the provider")
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := eng.Shutdown(expired); err != nil {
		t.Errorf("a typed turn's goroutines were still in flight when Shutdown returned: %v", err)
	}

	// The refusal lives in startSessionLocked, so typing is turned away on the
	// same terms as speaking rather than slipping past a StartSession-only guard.
	if _, err := eng.SubmitText("another typed question"); err == nil {
		t.Error("a typed turn started a session after shutdown")
	} else if errors.Is(err, ErrEmptyText) {
		t.Errorf("typed turn refused for the wrong reason: %v", err)
	}
}

// sawEvent reports whether the (already published) events include one of the
// given type. It never blocks: everything it looks for happened before the
// call, so waiting could only hide a missing event behind a timeout.
func sawEvent(events <-chan Event, want string) bool {
	for {
		select {
		case ev := <-events:
			if ev.Type == want {
				return true
			}
		default:
			return false
		}
	}
}

// Shutdown on an engine that never did anything must be instant and quiet,
// including on the second call: jarvixd stopping after a boot-and-idle day is
// the common case, not the interesting one.
func TestShutdownOfAnIdleEngineIsImmediate(t *testing.T) {
	h := newHarness(t, Options{Model: "test-model"})
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 2 {
		if err := h.engine.Shutdown(expired); err != nil {
			t.Fatalf("Shutdown %d of an idle engine: %v", i, err)
		}
	}
}
