package session

import (
	"strings"
	"sync"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// TestReconfigureSwapsCollaboratorsWhileIdle: after a reconfigure, the next
// session runs on the new provider with the new options — the "next response
// uses the new voice, no restart" contract, observed via fakes.
func TestReconfigureSwapsCollaboratorsWhileIdle(t *testing.T) {
	h := newHarness(t, Options{Model: "old-model"})
	h.ask(t, "before")
	if h.provider.LastRequest.Model != "old-model" {
		t.Fatalf("first ask used model %q", h.provider.LastRequest.Model)
	}

	newProvider := &ai.Fake{Response: "From the new provider."}
	newTTS := &tts.Fake{}
	if err := h.engine.Reconfigure(newProvider, h.stt, newTTS, h.recorder, h.player,
		Options{Model: "new-model"}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}

	oldRequests := len(h.provider.Requests)
	h.ask(t, "after")
	if len(h.provider.Requests) != oldRequests {
		t.Error("old provider still received requests after reconfigure")
	}
	if newProvider.LastRequest.Model != "new-model" {
		t.Errorf("new provider got model %q, want new-model", newProvider.LastRequest.Model)
	}
}

// TestReconfigureRefusedWhileSessionActive: a swap under a live session is
// refused and the running collaborators stay in place — the daemon never
// hot-swaps beneath an interaction.
func TestReconfigureRefusedWhileSessionActive(t *testing.T) {
	h := newHarness(t, Options{Model: "old-model"})
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	err := h.engine.Reconfigure(&ai.Fake{}, &stt.Fake{}, &tts.Fake{},
		&audio.FakeRecorder{}, &audio.FakePlayer{}, Options{Model: "new-model"})
	if err == nil {
		t.Fatal("Reconfigure succeeded with a session active")
	}
	if !strings.Contains(err.Error(), "session is active") {
		t.Errorf("error should say a session is active, got: %v", err)
	}

	// The refused swap changed nothing: the session still runs on the old
	// provider and model.
	if err := h.engine.Submit("still the old settings"); err != nil {
		t.Fatal(err)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if h.provider.LastRequest.Model != "old-model" {
		t.Errorf("session after refused reconfigure used model %q", h.provider.LastRequest.Model)
	}

	// Idle again: the swap now goes through.
	if err := h.engine.Reconfigure(&ai.Fake{}, &stt.Fake{}, &tts.Fake{},
		&audio.FakeRecorder{}, &audio.FakePlayer{}, Options{Model: "new-model"}); err != nil {
		t.Errorf("Reconfigure while idle: %v", err)
	}
}

// TestReconfigureShrinksHistory: a smaller history_turns takes effect on the
// carried conversation immediately, and zero forgets it entirely — matching
// what loadHistory enforces at boot.
func TestReconfigureShrinksHistory(t *testing.T) {
	h := newHarness(t, Options{Model: "m", HistoryTurns: 16})
	h.ask(t, "one")
	h.ask(t, "two")
	if got := len(h.engine.Conversation()); got != 4 {
		t.Fatalf("conversation has %d entries, want 4", got)
	}

	if err := h.engine.Reconfigure(h.provider, h.stt, h.tts, h.recorder, h.player,
		Options{Model: "m", HistoryTurns: 1}); err != nil {
		t.Fatal(err)
	}
	if got := len(h.engine.Conversation()); got != 2 {
		t.Errorf("after shrinking to 1 turn: %d entries, want 2", got)
	}

	if err := h.engine.Reconfigure(h.provider, h.stt, h.tts, h.recorder, h.player,
		Options{Model: "m", HistoryTurns: 0}); err != nil {
		t.Fatal(err)
	}
	if got := len(h.engine.Conversation()); got != 0 {
		t.Errorf("after disabling memory: %d entries, want 0", got)
	}
}

// TestReconfigureConcurrentWithSessions hammers reconfigure against running
// sessions. Correctness here is "no race, no panic, refusals are clean" —
// the race detector does the real judging.
func TestReconfigureConcurrentWithSessions(t *testing.T) {
	h := newHarness(t, Options{Model: "m"})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			// Errors are expected when a session is mid-flight; the point is
			// that a refused swap leaves the engine coherent.
			_ = h.engine.Reconfigure(&ai.Fake{Response: "swapped"}, h.stt, &tts.Fake{},
				h.recorder, h.player, Options{Model: "m2"})
		}
	}()
	for i := 0; i < 5; i++ {
		if _, err := h.engine.StartSession(); err != nil {
			continue // reconfigure window; fine
		}
		if err := h.engine.Submit("go"); err != nil {
			continue
		}
		h.collectUntil(t, "session.finished")
		h.waitIdle(t)
	}
	wg.Wait()
}
