package session

import (
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/desktop"
)

// Engine-side desktop context (ADR 0019): where a capture lands in the
// message list, which paths pay for it, and how it is disclosed afterwards.

// contextHarness is the standard harness with a scripted collector.
func newContextHarness(t *testing.T, opts Options, items ...desktop.Item) (*harness, *desktop.FakeCollector) {
	t.Helper()
	collector := &desktop.FakeCollector{Snapshot: desktop.Snapshot{
		Items: items, Elapsed: 7 * time.Millisecond,
	}}
	opts.Context = collector
	return newHarness(t, opts), collector
}

// TestContextReachesTheModel is the headline acceptance criterion: the user
// selects an error, asks what it means, and the provider request carries the
// selection — with nothing pasted by hand.
func TestContextReachesTheModel(t *testing.T) {
	h, collector := newContextHarness(t, Options{SystemPrompt: "You are Jarvix."},
		desktop.Item{Source: desktop.SourceWindow, Text: "Alacritty — nvim engine.go", Chars: 26},
		desktop.Item{Source: desktop.SourceSelection, Text: "panic: index out of range", Chars: 25})

	h.ask(t, "what does this mean?")

	if collector.Calls() != 1 {
		t.Fatalf("collector called %d times, want once per model turn", collector.Calls())
	}
	msgs := h.provider.LastRequest.Messages
	if len(msgs) != 3 {
		t.Fatalf("messages = %d (%v), want system prompt, context, question", len(msgs), roles(msgs))
	}
	// System-adjacent: the model must read it as fact about the machine, not
	// as something the user typed.
	if msgs[1].Role != ai.RoleSystem {
		t.Errorf("context role = %s, want system", msgs[1].Role)
	}
	// Immediately before the question, so "this" and the capture are adjacent.
	if msgs[2].Role != ai.RoleUser || msgs[2].Content != "what does this mean?" {
		t.Errorf("last message = %+v, want the question", msgs[2])
	}
	for _, want := range []string{"panic: index out of range", "Alacritty — nvim engine.go",
		"--- selected text ---", "--- active window ---"} {
		if !strings.Contains(msgs[1].Content, want) {
			t.Errorf("context message missing %q:\n%s", want, msgs[1].Content)
		}
	}
}

// TestContextIsGatheredAfterTheIntentRouter is the ordering guarantee. A
// matched intent answers in microseconds without a model; making it wait on
// hyprctl and wl-paste first would hand back everything ADR 0017 bought.
func TestContextIsGatheredAfterTheIntentRouter(t *testing.T) {
	collector := &desktop.FakeCollector{Snapshot: desktop.Snapshot{
		Items: []desktop.Item{{Source: desktop.SourceWindow, Text: "Alacritty"}},
	}}
	h := newIntentHarness(t, Options{Context: collector, HistoryTurns: 4})

	h.say(t, "volume thirty") // a hit: no provider request, no context
	if collector.Calls() != 0 {
		t.Fatalf("a matched intent gathered context %d times; it must pay nothing",
			collector.Calls())
	}

	h.say(t, "why is my build failing?") // a miss: reaches the model
	if collector.Calls() != 1 {
		t.Fatalf("collector called %d times on the model path, want 1", collector.Calls())
	}
	if len(h.provider.Requests) != 1 {
		t.Fatalf("provider called %d times", len(h.provider.Requests))
	}
}

func TestNoCollectorMeansNoContextMessage(t *testing.T) {
	// The zero-cost case: with context disabled the message list is exactly
	// what it was before the feature existed.
	h := newHarness(t, Options{SystemPrompt: "You are Jarvix."})
	h.ask(t, "hello")
	msgs := h.provider.LastRequest.Messages
	if len(msgs) != 2 || msgs[0].Role != ai.RoleSystem || msgs[1].Role != ai.RoleUser {
		t.Errorf("messages = %v, want system prompt and question only", roles(msgs))
	}
}

func TestEmptyCaptureAddsNothing(t *testing.T) {
	// Nothing on screen, no compositor, empty clipboard: the turn must be
	// indistinguishable from one with context switched off.
	h, collector := newContextHarness(t, Options{SystemPrompt: "You are Jarvix."})
	h.ask(t, "hello")
	if collector.Calls() != 1 {
		t.Fatalf("collector calls = %d", collector.Calls())
	}
	if msgs := h.provider.LastRequest.Messages; len(msgs) != 2 {
		t.Errorf("messages = %v, want no empty context message", roles(msgs))
	}
}

// TestContextIsNotRemembered pins the "never persisted beyond the
// conversation history rules" requirement: a capture belongs to one turn, and
// the next turn gets a fresh one rather than a stale one carried in history.
func TestContextIsNotRemembered(t *testing.T) {
	h, collector := newContextHarness(t, Options{HistoryTurns: 8},
		desktop.Item{Source: desktop.SourceClipboard, Text: "yesterday's clipboard"})

	h.ask(t, "first question")
	h.ask(t, "second question")

	if collector.Calls() != 2 {
		t.Fatalf("collector calls = %d, want one per turn", collector.Calls())
	}
	msgs := h.provider.LastRequest.Messages
	var contexts int
	for _, m := range msgs {
		if strings.Contains(m.Content, "yesterday's clipboard") {
			contexts++
		}
	}
	if contexts != 1 {
		t.Errorf("the second turn carried %d captures, want only its own:\n%v", contexts, msgs)
	}
	// The conversation window shows turns, never captures.
	for _, turn := range h.engine.Conversation() {
		if strings.Contains(turn.Text, "yesterday's clipboard") {
			t.Errorf("a capture leaked into the conversation: %+v", turn)
		}
	}
}

// TestCaptureIsAuditableAfterwards covers the disclosure criterion: what was
// captured is answerable after the session, and the event that announces it
// carries sizes only.
func TestCaptureIsAuditableAfterwards(t *testing.T) {
	h, _ := newContextHarness(t, Options{},
		desktop.Item{Source: desktop.SourceSelection, Text: "panic: index out of range",
			Chars: 4000, Truncated: true},
		desktop.Item{Source: desktop.SourceClipboard, Text: desktop.RedactedMarker,
			Chars: 64, Redacted: true})

	if _, _, ok := h.engine.LastContext(); ok {
		t.Fatal("a capture was reported before any session ran")
	}
	seen := func() map[string]Event {
		if _, err := h.engine.StartSession(); err != nil {
			t.Fatal(err)
		}
		if err := h.engine.Submit("what is this?"); err != nil {
			t.Fatal(err)
		}
		defer h.waitIdle(t)
		return h.collectUntil(t, "session.finished")
	}()

	snap, sessionID, ok := h.engine.LastContext()
	if !ok || sessionID == "" {
		t.Fatalf("LastContext = %v, %q, %v", snap, sessionID, ok)
	}
	if len(snap.Items) != 2 || snap.Items[0].Text != "panic: index out of range" {
		t.Errorf("retained capture = %+v", snap.Items)
	}

	ev, ok := seen["context.captured"]
	if !ok {
		t.Fatal("no context.captured event")
	}
	sources, _ := ev.Data["sources"].([]map[string]any)
	if len(sources) != 2 {
		t.Fatalf("event sources = %v", ev.Data["sources"])
	}
	if sources[0]["source"] != "selection" || sources[0]["chars"] != 4000 || sources[0]["truncated"] != true {
		t.Errorf("selection metadata = %v", sources[0])
	}
	if sources[1]["redacted"] != true {
		t.Errorf("clipboard metadata = %v", sources[1])
	}
	// Sizes and flags only: events fan out to every connected client.
	for _, s := range sources {
		if _, present := s["text"]; present {
			t.Errorf("context.captured carried content: %v", s)
		}
	}
}

// TestContextGatheringIsChargedToJarvix pins the seam with the latency budget
// (ADR 0018): gathering happens between the transcript and the request, so
// without its own stage it would sit inside transcript_to_first_delta_ms —
// the one span subtracted from jarvix_ms as "the user's choice of model".
// A cost hidden inside the number it inflates is worse than no measurement.
func TestContextGatheringIsChargedToJarvix(t *testing.T) {
	var s sess
	base := time.Now()
	s.timings.captureStop = mark{at: base}
	s.timings.transcript = mark{at: base.Add(100 * time.Millisecond)}
	s.timings.contextDone = mark{at: base.Add(150 * time.Millisecond)}
	s.timings.firstDelta = mark{at: base.Add(400 * time.Millisecond)}
	s.timings.firstPCM = mark{at: base.Add(450 * time.Millisecond)}
	s.timings.audioOut = mark{at: base.Add(500 * time.Millisecond)}

	report := s.timings.report()
	if got := report[StageContext]; got != int64(50) {
		t.Errorf("%s = %v, want 50", StageContext, got)
	}
	// The model's clock starts where gathering stopped, not at the transcript.
	if got := report[StageTranscriptToDelta]; got != int64(250) {
		t.Errorf("%s = %v, want 250 (context excluded)", StageTranscriptToDelta, got)
	}
	// …so the 50ms lands in Jarvix's own number: 500 total - 250 thinking.
	if got := report[StageJarvixOverhead]; got != int64(250) {
		t.Errorf("%s = %v, want 250 (context included)", StageJarvixOverhead, got)
	}

	// With context disabled there is no stage at all — absent, not zero, so a
	// reader can tell "off" from "instant".
	var without sess
	without.timings.captureStop = mark{at: base}
	without.timings.transcript = mark{at: base.Add(100 * time.Millisecond)}
	without.timings.firstDelta = mark{at: base.Add(400 * time.Millisecond)}
	plain := without.timings.report()
	if _, present := plain[StageContext]; present {
		t.Errorf("%s reported with context disabled: %v", StageContext, plain)
	}
	if got := plain[StageTranscriptToDelta]; got != int64(300) {
		t.Errorf("%s = %v, want the full 300 without context", StageTranscriptToDelta, got)
	}
}

// TestContextStageReachesTheTimingsEvent proves the mark is actually set on a
// real turn, not merely computable.
func TestContextStageReachesTheTimingsEvent(t *testing.T) {
	h, _ := newContextHarness(t, Options{Model: "m"},
		desktop.Item{Source: desktop.SourceWindow, Text: "Alacritty"})

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("what is this?"); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	ev, ok := seen["session.timings"]
	if !ok {
		t.Fatal("no session.timings event")
	}
	if _, present := ev.Data[StageContext]; !present {
		t.Errorf("timings = %v, want a %s stage", ev.Data, StageContext)
	}
}

// roles renders a message list for failure messages.
func roles(msgs []ai.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, string(m.Role))
	}
	return out
}
