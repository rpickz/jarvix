package session

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
)

// This file covers issue #111, the third state wedge of the #52/#63 family.
//
// The live shape: the model narrates briefly and asks for tools in the same
// round, and the narration's only sentence leaves the sentencer at
// end-of-stream — after the tool calls, right before streamOnce returns. The
// #109 config tools invite exactly this (a short "let me check" and three
// config.list_entries calls), which is why the wave surfaced it. The tool
// loop then reads the state immediately (backToThinking), and the answer's
// claim on Speaking used to live on the speaker's run loop — so whether the
// sentence had claimed Speaking by then was a goroutine race. Losing it went:
// backToThinking forces Responding → Thinking, the speaker's late claim
// arrives as Thinking → Speaking, the table refuses it (rightly — speech goes
// Thinking → Responding → Speaking), and failLocked kills a live session
// mid-turn. The think goroutine, between its context check and the tool's
// execution, then ran the tool for a session whose failure was already
// published — the absurdity in the incident log.
//
// The fix is routing, not a wider table, exactly as #52/#53 and #63 resolved:
// the Speaking claim moved onto the enqueueing goroutine (speaker.go's
// announce), making "a committed sentence has claimed Speaking before the
// tool loop moves on" an ordering guarantee instead of a race; and a tool
// call now refuses to *begin* on a session that has already ended, checked
// under the engine lock so it is ordered against every session-ending path
// (executeTool).
//
// Every ordering here is gated, never slept for: parkedSynth holds the
// synthesizer *call* (where the old code's claim lived, so on the unpatched
// engine the claim reliably loses the race it used to lose only sometimes),
// and gatedTool holds the tool round open so the session state during
// execution can be asserted rather than sampled.

// parkedSynth wraps the fake synthesizer, parking every Speak call until the
// gate opens. It is deliberately not tts.Fake.SetHold: SetHold holds the
// audio *chunks* after Speak has returned, and the unpatched engine claimed
// Speaking between those two points — the wedge under test lives in the
// window before Speak returns, so that is where the test must be able to
// park.
type parkedSynth struct {
	*tts.Fake
	gate chan struct{}
}

func (p *parkedSynth) Speak(ctx context.Context, req tts.Request) (tts.Format, <-chan tts.Chunk, error) {
	select {
	case <-p.gate:
	case <-ctx.Done():
		return tts.Format{}, nil, ctx.Err()
	}
	return p.Fake.Speak(ctx, req)
}

// gatedTool blocks Execute until released (or the session dies), so a test
// can hold the tool round open and assert what the session looks like while
// a tool is genuinely running.
//
// The gate is receive-only because a test does not have to own it: the
// supersession test hands over a channel the *synthesizer* closes, which
// makes "the tool round stays open until the voice has started" an ordering
// rather than a hope (issue #154).
type gatedTool struct {
	name   string
	result string
	gate   <-chan struct{}

	mu    sync.Mutex
	calls int
}

func (g *gatedTool) Name() string            { return g.name }
func (g *gatedTool) Description() string     { return "fake config tool" }
func (g *gatedTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (g *gatedTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	select {
	case <-g.gate:
	case <-ctx.Done():
	}
	return g.result, nil
}

func (g *gatedTool) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// TestToolRoundSentenceAtStreamEndDoesNotKillTheSession is the regression
// test for #111 itself. A round whose narration only completes at
// end-of-stream, followed by tool calls, must (a) have claimed Speaking
// before the tool loop moves on — so the state during the tool's execution is
// Speaking, never Thinking with a claim still in flight — and (b) finish the
// whole turn normally: tool executed, answer spoken on the one stream the
// narration opened, no error, engine idle and able to serve the next turn.
//
// Before the fix this failed with the incident's exact signature: an error
// event carrying "invalid state transition thinking → speaking", the session
// dead mid-turn, and the tool executing after the failure was published.
func TestToolRoundSentenceAtStreamEndDoesNotKillTheSession(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	speakGate := make(chan struct{})
	toolGate := make(chan struct{})
	tool := &gatedTool{name: "config.list_entries", result: "no entries", gate: toolGate}
	h.tools = tools.NewRegistry(nil)
	h.tools.Register(tool)
	synth := &parkedSynth{Fake: h.tts, gate: speakGate}
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	t.Cleanup(h.cancel)
	h.engine = NewEngine(h.provider, h.stt, synth, h.recorder, h.player, h.tools, nil, bus, nil,
		Options{Model: "m", SpeakResponses: true})
	// Drain the rebuilt engine at the end (the newHarness cleanup only knows
	// the engine it built). Registered before the gate-openers so it runs
	// after them: cleanups are LIFO, and a drain must never wait on a
	// goroutine a still-closed gate is parking.
	t.Cleanup(func() {
		if err := h.engine.Shutdown(context.Background()); err != nil {
			t.Errorf("engine had not quiesced by the end of the test: %v", err)
		}
	})
	var openOnce, runOnce sync.Once
	openSpeech := func() { openOnce.Do(func() { close(speakGate) }) }
	releaseTool := func() { runOnce.Do(func() { close(toolGate) }) }
	t.Cleanup(openSpeech)
	t.Cleanup(releaseTool)

	// The trigger shape: a narration with no terminator, so its one sentence
	// leaves the sentencer only at flush — after the round's tool calls, on
	// the very edge of streamOnce returning to the tool loop.
	h.provider.Preamble = "Let me check your configuration"
	h.provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "config.list_entries", Arguments: `{"family":"routines"}`}},
	}
	h.provider.Response = "You have no routine entries."

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("what routines do I have"); err != nil {
		t.Fatal(err)
	}

	// tool.started is published by the same goroutine that ran backToThinking
	// a moment earlier, so by the time it arrives the tool loop has committed
	// its reading of the state — and the narration's claim must already have
	// landed. (waitFor fails the test on any error event, so an unpatched
	// engine cannot slip its failure past this wait.)
	h.waitFor(t, "tool.started")
	if state, _ := h.engine.State(); state != StateSpeaking {
		t.Errorf("state during the tool round = %s, want speaking: the committed "+
			"sentence must claim Speaking before the tool loop moves on", state)
	}

	// Only now may the synthesizer proceed — while the tool round is still
	// open, which is the incident's interleaving. On the fixed engine the
	// claim already happened above and this merely lets the audio flow to the
	// player; on the unpatched engine this was the moment the late claim
	// fired from Thinking and killed the session, with the tool still running
	// into the void. Waiting on the player (or the failure) before releasing
	// the tool keeps the two orderings apart deterministically: the next
	// round cannot begin and legalise the late claim by reaching Responding
	// first.
	openSpeech()
	waitUntil(t, "the narration's audio reaches the player", func() bool {
		select {
		case ev := <-h.events:
			if ev.Type == "error" {
				t.Fatalf("the session failed the moment the speaker was released: %v", ev.Data)
			}
		default:
		}
		_, plays := h.player.Played()
		return plays >= 1
	})
	releaseTool()

	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)
	if counts["error"] != 0 {
		t.Errorf("the session failed instead of finishing: %d error events", counts["error"])
	}
	if got := tool.callCount(); got != 1 {
		t.Errorf("tool ran %d times, want 1", got)
	}
	// The narration opened the turn's one playback stream and the answer
	// continued on it: one voice, gaplessly, exactly the #52/#53 discipline.
	if _, plays := h.player.Played(); plays != 1 {
		t.Errorf("playback streams opened = %d, want 1 for the whole turn", plays)
	}

	// Recovery is part of the contract: whatever this turn went through, the
	// next one must work (the incident's daemon did start s3 — keep it so).
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("and now?"); err != nil {
		t.Fatal(err)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)
}

// TestToolNeverBeginsOnASessionThatAlreadyEnded pins the other half of #111's
// incident log: "session failed" followed by the tool executing anyway. The
// tool loop's own guard reads the session context without the lock, and the
// failure that killed the session came from another goroutine — so the check
// could pass an instant before the failure landed, and the tool then began
// for a turn whose failure was already on the bus. executeTool now decides
// "may this call begin?" under the engine lock, which orders it against every
// session-ending path; this test drives that interleaving directly: the
// session has failed (failure published, engine idle), and the tool loop —
// which checked the context before any of that — asks to execute its next
// call.
func TestToolNeverBeginsOnASessionThatAlreadyEnded(t *testing.T) {
	rec := &recordingTool{result: "ran"}
	h := newHarness(t, Options{})
	h.tools = tools.NewRegistry(nil)
	h.tools.Register(rec)
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	t.Cleanup(h.cancel)
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, h.tools, nil, bus, nil,
		Options{Model: "m"})

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	s := h.engine.current
	h.engine.mu.Unlock()

	// The session fails on another goroutine's behalf, exactly as the refused
	// transition did in the incident: failure event, session.finished, engine
	// back at Idle with no current session.
	h.engine.fail(s, "session", errors.New("synthetic failure"))
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	// The tool loop, having checked s.ctx before the failure landed, reaches
	// its next call. Nothing may execute, and the loop must be told to stop.
	result, ok := h.engine.gateAndExecute(s,
		ai.ToolCall{ID: "c1", Name: "run", Arguments: `{}`}, spokenTurn{})
	if ok {
		t.Error("gateAndExecute reported the loop may continue on a failed session")
	}
	if result != "" {
		t.Errorf("a failed session's tool call produced a result: %q", result)
	}
	if rec.calls != 0 {
		t.Errorf("a tool began executing %d times on a session whose failure was already published", rec.calls)
	}
}
