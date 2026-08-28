package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/intent"
)

// The nickname tests cover the engine half of #126: "call this window
// builds" and "what are my windows called" are claimed by the router (no
// provider call), carried out through the injected seam — never a real
// desktop — and answered with the seam's one spoken sentence. Refusals come
// back as the standard "Sorry, …" framing of the seam's spoken-ready error.

// fakeNamer is a scripted WindowNamer.
type fakeNamer struct {
	mu         sync.Mutex
	spoken     string
	assignErr  error
	listSpoken string
	listErr    error

	assigned []string // "reference\x00name" per call
	listed   int
}

func (f *fakeNamer) AssignNickname(_ context.Context, reference, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assigned = append(f.assigned, reference+"\x00"+name)
	return f.spoken, f.assignErr
}

func (f *fakeNamer) NicknameListing(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed++
	return f.listSpoken, f.listErr
}

func (f *fakeNamer) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.assigned...)
}

func (f *fakeNamer) listings() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listed
}

// newNicknameHarness wires an engine whose router carries the built-in
// nickname patterns (they always compile) and whose window-name seam is the
// fake. A nil namer exercises the "daemon without window tools" refusal.
func newNicknameHarness(t *testing.T, namer WindowNamer) *harness {
	t.Helper()
	router, err := intent.New(intent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Options{})
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	t.Cleanup(h.cancel)
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, nil, nil, bus, nil, Options{
		Model: "m", SpeakResponses: true, HistoryTurns: 8,
		ConfirmTimeout: 5 * time.Second,
		Intents:        router, IntentRunner: &intent.FakeRunner{},
		WindowNames: namer,
	})
	return h
}

// TestNicknamePhraseAssignsWithoutAProviderCall is the assignment acceptance
// criterion end to end: the phrase reaches the seam with the spoken name and
// the focused-window reference, no model is consulted, and the seam's soft
// confirmation is the one thing spoken.
func TestNicknamePhraseAssignsWithoutAProviderCall(t *testing.T) {
	namer := &fakeNamer{spoken: "Okay — the code window is now called builds."}
	h := newNicknameHarness(t, namer)

	seen := sayRoutine(t, h, "call this window builds")

	if len(h.provider.Requests) != 0 {
		t.Fatalf("the provider was called %d times for a nickname phrase", len(h.provider.Requests))
	}
	if calls := namer.calls(); len(calls) != 1 || calls[0] != "\x00builds" {
		t.Fatalf("assigned %q, want one call with the focused-window reference and the name", calls)
	}
	ev, ok := seen["intent.executed"]
	if !ok {
		t.Fatal("no intent.executed event")
	}
	if ev.Data["intent"] != intent.WindowNameIntentName || ev.Data["status"] != "ok" {
		t.Errorf("event = %v", ev.Data)
	}
	if ev.Data["acknowledgement"] != namer.spoken {
		t.Errorf("acknowledgement = %v", ev.Data["acknowledgement"])
	}
	if h.tts.Last().Text != namer.spoken {
		t.Errorf("spoken confirmation = %q", h.tts.Last().Text)
	}
}

// TestNicknameRefusalIsSpokenAsSorry: the seam's spoken-ready refusal comes
// out as one plain "Sorry, …" sentence and the session completes normally.
func TestNicknameRefusalIsSpokenAsSorry(t *testing.T) {
	namer := &fakeNamer{assignErr: errors.New(`"mute" is already the built-in intent "volume.mute"; choose a different name`)}
	h := newNicknameHarness(t, namer)

	seen := sayRoutine(t, h, "call this window mute")

	if seen["intent.executed"].Data["status"] != "failed" {
		t.Errorf("status = %v", seen["intent.executed"].Data["status"])
	}
	want := `Sorry, "mute" is already the built-in intent "volume.mute"; choose a different name.`
	if h.tts.Last().Text != want {
		t.Errorf("spoken = %q, want %q", h.tts.Last().Text, want)
	}
}

// TestWindowNamesPhraseListsWithoutAProviderCall: the listing phrase is a
// seam call and a spoken sentence, no model anywhere.
func TestWindowNamesPhraseListsWithoutAProviderCall(t *testing.T) {
	namer := &fakeNamer{listSpoken: "1 window has a name: builds is Alacritty — go test."}
	h := newNicknameHarness(t, namer)

	seen := sayRoutine(t, h, "what are my windows called")

	if len(h.provider.Requests) != 0 {
		t.Fatalf("the provider was called %d times for a listing phrase", len(h.provider.Requests))
	}
	if namer.listings() != 1 {
		t.Fatalf("listings = %d, want one", namer.listings())
	}
	if ev := seen["intent.executed"]; ev.Data["intent"] != intent.WindowNamesIntentName {
		t.Errorf("event = %v", ev.Data)
	}
	if h.tts.Last().Text != namer.listSpoken {
		t.Errorf("spoken = %q", h.tts.Last().Text)
	}
}

// TestNicknamesWithoutTheSeamRefuseHonestly: a daemon whose window tools are
// off says so — never a silent drop, never a model call.
func TestNicknamesWithoutTheSeamRefuseHonestly(t *testing.T) {
	h := newNicknameHarness(t, nil)
	sayRoutine(t, h, "call this window builds")
	if h.tts.Last().Text != "Sorry, naming windows is not available on this daemon." {
		t.Errorf("spoken = %q", h.tts.Last().Text)
	}
	if len(h.provider.Requests) != 0 {
		t.Fatalf("the provider was consulted for a claimed phrase")
	}
}
