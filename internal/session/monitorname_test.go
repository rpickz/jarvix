package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/intent"
)

// The screen-name tests cover the engine half of #180, on nickname_test.go's
// terms exactly: the three phrases are claimed by the router (no provider
// call), carried out through the injected seam — never a real compositor —
// and answered with the seam's one spoken sentence.

// fakeMonitorNamer is a scripted MonitorNamer.
type fakeMonitorNamer struct {
	mu         sync.Mutex
	spoken     string
	assignErr  error
	forgetErr  error
	listSpoken string

	assigned []string // "name\x00connector" per call
	forgot   []string
	listed   int
}

func (f *fakeMonitorNamer) AssignMonitorNickname(_ context.Context, name, connector string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assigned = append(f.assigned, name+"\x00"+connector)
	return f.spoken, f.assignErr
}

func (f *fakeMonitorNamer) ForgetMonitorNickname(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgot = append(f.forgot, name)
	return f.spoken, f.forgetErr
}

func (f *fakeMonitorNamer) MonitorNicknameListing(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed++
	return f.listSpoken, nil
}

func (f *fakeMonitorNamer) calls() ([]string, []string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.assigned...), append([]string(nil), f.forgot...), f.listed
}

// newMonitorNameHarness wires an engine whose router carries the built-in
// screen-name patterns and whose seam is the fake. A nil namer exercises the
// "daemon without window tools" refusal.
func newMonitorNameHarness(t *testing.T, namer MonitorNamer) *harness {
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
		MonitorNames: namer,
	})
	return h
}

// TestMonitorNamePhraseAssignsWithoutAProviderCall: "call this monitor top"
// reaches the seam with the name and the empty connector — the deictic
// phrasing means the screen holding focus, and the seam resolves that.
func TestMonitorNamePhraseAssignsWithoutAProviderCall(t *testing.T) {
	namer := &fakeMonitorNamer{spoken: "Okay — HDMI-A-1 (3440 by 1440) is now called top."}
	h := newMonitorNameHarness(t, namer)

	seen := sayRoutine(t, h, "call this monitor top")

	if len(h.provider.Requests) != 0 {
		t.Fatalf("the provider was called %d times for a screen-name phrase", len(h.provider.Requests))
	}
	assigned, _, _ := namer.calls()
	if len(assigned) != 1 || assigned[0] != "top\x00" {
		t.Fatalf("assigned %q, want one call naming the focused screen", assigned)
	}
	ev, ok := seen["intent.executed"]
	if !ok {
		t.Fatal("no intent.executed event")
	}
	if ev.Data["intent"] != intent.MonitorNameIntentName || ev.Data["status"] != "ok" {
		t.Errorf("event = %v", ev.Data)
	}
	if h.tts.Last().Text != namer.spoken {
		t.Errorf("spoken confirmation = %q", h.tts.Last().Text)
	}
}

// TestMonitorNameRefusalIsSpokenAsSorry: the seam's spoken-ready collision
// refusal comes out as one plain sentence and the session completes normally.
func TestMonitorNameRefusalIsSpokenAsSorry(t *testing.T) {
	namer := &fakeMonitorNamer{assignErr: errors.New(
		`"current" already means something when you name a screen — it is the screen you are on; choose a different name`)}
	h := newMonitorNameHarness(t, namer)

	seen := sayRoutine(t, h, "call this monitor current")

	if seen["intent.executed"].Data["status"] != "failed" {
		t.Errorf("status = %v", seen["intent.executed"].Data["status"])
	}
	want := `Sorry, "current" already means something when you name a screen — ` +
		`it is the screen you are on; choose a different name.`
	if h.tts.Last().Text != want {
		t.Errorf("spoken = %q, want %q", h.tts.Last().Text, want)
	}
}

// TestMonitorForgetAndListingPhrasesReachTheSeam: removal and the listing are
// seam calls and spoken sentences, no model anywhere.
func TestMonitorForgetAndListingPhrasesReachTheSeam(t *testing.T) {
	namer := &fakeMonitorNamer{
		spoken:     "Okay — HDMI-A-1 is no longer called top.",
		listSpoken: "2 screens have names: bottom is DP-2 (5120 by 1440); top is HDMI-A-1 (3440 by 1440).",
	}
	h := newMonitorNameHarness(t, namer)

	sayRoutine(t, h, "forget the monitor called top")
	if h.tts.Last().Text != namer.spoken {
		t.Errorf("forget spoke %q", h.tts.Last().Text)
	}
	sayRoutine(t, h, "what are my screens called")
	if h.tts.Last().Text != namer.listSpoken {
		t.Errorf("listing spoke %q", h.tts.Last().Text)
	}
	if len(h.provider.Requests) != 0 {
		t.Fatalf("the provider was called %d times", len(h.provider.Requests))
	}
	_, forgot, listed := namer.calls()
	if len(forgot) != 1 || forgot[0] != "top" || listed != 1 {
		t.Errorf("forgot %q, listed %d", forgot, listed)
	}
}

// TestScreenNamePhrasesRefuseHonestlyWithoutTheSeam: a daemon whose window
// tools are switched off says so rather than dropping the phrase silently.
func TestScreenNamePhrasesRefuseHonestlyWithoutTheSeam(t *testing.T) {
	h := newMonitorNameHarness(t, nil)
	for _, utterance := range []string{
		"call this monitor top",
		"forget the monitor called top",
		"what are my screens called",
	} {
		seen := sayRoutine(t, h, utterance)
		if seen["intent.executed"].Data["status"] != "failed" {
			t.Errorf("%q status = %v", utterance, seen["intent.executed"].Data["status"])
		}
		if h.tts.Last().Text != "Sorry, naming screens is not available on this daemon." {
			t.Errorf("%q spoke %q", utterance, h.tts.Last().Text)
		}
	}
}
