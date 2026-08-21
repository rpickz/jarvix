package session

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/tools"
)

// This file covers issue #55: a refused state transition is never silent.
// advance() returns false for two very different reasons — the session was
// superseded (fine, the cancel path already spoke) and the transition was
// refused (a programming error that just abandoned a live turn) — and every
// caller used to treat them identically. Two real bugs (#52) hid behind that
// indistinguishable false, one of them wedging a session in Speaking forever
// with no error, no session.finished, and no answer.

// lockedBuffer is a concurrency-safe io.Writer for capturing engine logs:
// session goroutines log from several goroutines at once, and the race
// detector rightly objects to a bare bytes.Buffer.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog swaps the harness engine's logger for one that records every
// line at debug level and above. The log/event distinction issues #54 and #55
// specify is asserted against this capture, never by eye. Call it before any
// session starts.
func captureLog(h *harness) *lockedBuffer {
	buf := &lockedBuffer{}
	h.engine.log = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return buf
}

// TestRefusedTransitionIsLoudAndTerminal is the core of #55: a transition the
// table refuses, requested for a live session, must (a) log at error level
// naming the from-state, the to-state and the session, and (b) end the turn
// properly — an error event and session.finished on the bus, the engine back
// at Idle — because the alternative demonstrated in production was a session
// abandoned in a non-terminal state with a failure signature of nothing at
// all.
func TestRefusedTransitionIsLoudAndTerminal(t *testing.T) {
	h := newHarness(t, Options{})
	logs := captureLog(h)
	id, _ := h.engine.StartSession()
	h.engine.mu.Lock()
	s := h.engine.current
	h.engine.mu.Unlock()

	// Idle → Speaking is not in the table; a stage advancing there is a bug.
	if h.engine.advance(s, StateSpeaking) {
		t.Fatal("an illegal transition was allowed")
	}

	out := logs.String()
	for _, want := range []string{"state transition refused", "level=ERROR",
		"from=idle", "to=speaking", "session_id=" + id} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal log is missing %q:\n%s", want, out)
		}
	}
	seen := h.collectUntil(t, "session.finished")
	if _, ok := seen["error"]; !ok {
		t.Error("a refused transition killed the turn without an error event")
	}
	h.waitIdle(t)
	if state, cur := h.engine.State(); state != StateIdle || cur != "" {
		t.Errorf("state = %s session = %q after a refusal; want idle and no session", state, cur)
	}
}

// TestSupersededSessionUnwindsQuietly is the other half of the distinction: a
// stage advancing on behalf of a session the user has interrupted must stay
// quiet — the cancel path already reported it, and an error (or even a
// warning) on every interruption would train everyone to ignore the one that
// matters. An operator reading the journal sees "session cancelled" for the
// interruption and "state transition refused" for the bug, never one dressed
// as the other.
func TestSupersededSessionUnwindsQuietly(t *testing.T) {
	h := newHarness(t, Options{})
	logs := captureLog(h)
	_, _ = h.engine.StartSession()
	h.engine.mu.Lock()
	s1 := h.engine.current
	h.engine.mu.Unlock()
	if _, err := h.engine.StartSession(); err != nil { // supersedes s1
		t.Fatal(err)
	}

	// A perfectly legal-looking advance for the superseded session: quiet no.
	if h.engine.advance(s1, StateThinking) {
		t.Fatal("a superseded session advanced")
	}

	counts := h.countUntil(t, "session.cancelled")
	if counts["error"] != 0 {
		t.Errorf("supersession published %d error events; the cancel path owns the reporting", counts["error"])
	}
	if out := logs.String(); strings.Contains(out, "level=ERROR") {
		t.Errorf("a superseded unwind logged at error level:\n%s", out)
	}
	_ = h.engine.Cancel() // clean up the superseding session
	h.waitIdle(t)
}

// TestStreamedToolRoundWithSpeechOffReturnsToThinking wires down the table's
// Responding → Thinking entry, which was documented from the start and
// performed by no code path — the dead-entry smell #52's author flagged. It is
// not dead; it is the transition this exact turn needs: the model streams a
// preamble (Responding), asks for an allow-tier tool, and with speech off
// nothing ever claims Speaking — so without the return to Thinking, the next
// round's first token is refused as Responding → Responding and the turn used
// to die in perfect silence.
func TestStreamedToolRoundWithSpeechOffReturnsToThinking(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "3 containers"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{}) // speech off
	scriptShellCall(h, "docker ps", "Three containers are running.")
	h.provider.Preamble = "Let me check. "

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("what's running in docker")

	var states []string
	errors := 0
	deadline := time.After(5 * time.Second)
	for len(states) == 0 || states[len(states)-1] != "idle" {
		select {
		case ev := <-h.events:
			if ev.Type == "state.changed" {
				states = append(states, ev.Data["state"].(string))
			}
			if ev.Type == "error" {
				errors++
			}
		case <-deadline:
			t.Fatalf("states so far: %v", states)
		}
	}
	h.waitIdle(t)
	if errors != 0 {
		t.Errorf("the session failed instead of finishing: %d error events", errors)
	}
	// Thinking, preamble (Responding), back to work (Thinking), the answer
	// (Responding), done. The middle "thinking" is the entry under test.
	want := []string{"thinking", "responding", "thinking", "responding", "idle"}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("states = %v, want %v", states, want)
	}
	if rec.calls != 1 {
		t.Errorf("tool ran %d times, want 1", rec.calls)
	}
}

// TestEveryTurnEndReachesATerminalPublish is the invariant the issue asks for,
// rather than one more instance of it: however a turn ends — cleanly, in
// failure, cancelled, interrupted, silenced, or through a refused transition —
// the session publishes a terminal event (session.finished or
// session.cancelled) and the engine returns to Idle with no current session.
// The forbidden outcome is the one #52 shipped: a non-terminal state, no
// pending work, and nothing on the bus to tell anyone.
func TestEveryTurnEndReachesATerminalPublish(t *testing.T) {
	tests := []struct {
		name  string
		opts  Options
		drive func(t *testing.T, h *harness)
	}{
		{
			name: "normal finish",
			opts: Options{SpeakResponses: true},
			drive: func(t *testing.T, h *harness) {
				_, _ = h.engine.StartSession()
				_ = h.engine.Submit("hi")
			},
		},
		{
			name: "provider failure",
			opts: Options{},
			drive: func(t *testing.T, h *harness) {
				h.provider.Fail = errors.New("model exploded")
				_, _ = h.engine.StartSession()
				_ = h.engine.Submit("hi")
			},
		},
		{
			name: "tts failure",
			opts: Options{SpeakResponses: true},
			drive: func(t *testing.T, h *harness) {
				h.tts.Fail = errors.New("synth exploded")
				_, _ = h.engine.StartSession()
				_ = h.engine.Submit("hi")
			},
		},
		{
			name: "cancelled mid-speech",
			opts: Options{SpeakResponses: true},
			drive: func(t *testing.T, h *harness) {
				h.tts.SetHold(make(chan struct{}))
				_, _ = h.engine.StartSession()
				_ = h.engine.Submit("hi")
				h.waitFor(t, "tts.started")
				_ = h.engine.Cancel()
			},
		},
		{
			name: "speech cancelled mid-turn",
			opts: Options{SpeakResponses: true},
			drive: func(t *testing.T, h *harness) {
				h.tts.SetHold(make(chan struct{}))
				_, _ = h.engine.StartSession()
				_ = h.engine.Submit("hi")
				h.waitFor(t, "tts.started")
				if !h.engine.CancelSpeech() {
					t.Fatal("CancelSpeech found nothing to stop")
				}
			},
		},
		{
			name: "interrupted by a new session",
			opts: Options{SpeakResponses: true},
			drive: func(t *testing.T, h *harness) {
				h.tts.SetHold(make(chan struct{}))
				_, _ = h.engine.StartSession()
				_ = h.engine.Submit("hi")
				h.waitFor(t, "tts.started")
				_, _ = h.engine.StartSession() // supersedes: s1 must still end loudly
				_ = h.engine.Cancel()          // and the second session ends too
			},
		},
		{
			name: "refused transition",
			opts: Options{},
			drive: func(t *testing.T, h *harness) {
				_, _ = h.engine.StartSession()
				h.engine.mu.Lock()
				s := h.engine.current
				h.engine.mu.Unlock()
				h.engine.advance(s, StateSpeaking) // illegal from Idle
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.opts)
			tc.drive(t, h)

			deadline := time.After(5 * time.Second)
			for done := false; !done; {
				select {
				case ev := <-h.events:
					if ev.Type == "session.finished" || ev.Type == "session.cancelled" {
						done = true
					}
				case <-deadline:
					t.Fatal("the turn ended without a terminal publish — the silent wedge issue #55 forbids")
				}
			}
			h.waitIdle(t)
			if state, id := h.engine.State(); state != StateIdle || id != "" {
				t.Errorf("state = %s session = %q; want idle with no session", state, id)
			}
		})
	}
}
