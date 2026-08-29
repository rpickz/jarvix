package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/tools"
)

// These tests drive the host cascade (issue #161, ADR 0064) through the real
// engine, with the two models faked at the one seam a tier is observable
// through — ai.Provider — and every ordering gated rather than slept for.
//
// Three gates do all the work, and no test in this file uses a timer:
//
//   - the answering tier's stream is held open until the test releases it, so
//     "the answer has not begun" is a fact the test controls;
//   - the grace is a channel the test closes (Options.HostGraceTimer), so "the
//     grace expired" is likewise;
//   - Engine.hostFinished fires as the host's goroutine exits, so "the host had
//     already decided" is a happens-before edge rather than a hope.
//
// The guard's own tests are next door in hostguard_test.go and touch no engine.

// ---------------------------------------------------------------------------
// The world
// ---------------------------------------------------------------------------

// callLedger records which provider was asked, in order, across both tiers.
// It exists for one assertion — that the answer's request goes out before the
// host's — which is otherwise unobservable: both are Chat calls on different
// objects from different goroutines.
type callLedger struct {
	mu    sync.Mutex
	names []string
}

func (l *callLedger) note(name string) {
	l.mu.Lock()
	l.names = append(l.names, name)
	l.mu.Unlock()
}

func (l *callLedger) order() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.names...)
}

// gatedTier is a provider whose stream produces nothing until the test lets it,
// and which unwinds with its context's error when the turn is abandoned — the
// two behaviours a real endpoint has that ai.Fake does not.
type gatedTier struct {
	name   string
	text   string
	fail   error // Chat refuses before streaming — an unreachable tier
	call   *ai.ToolCall
	ledger *callLedger

	releaseOnce sync.Once
	release     chan struct{}

	mu   sync.Mutex
	reqs []ai.ChatRequest
}

func (g *gatedTier) Name() string { return g.name }

func (g *gatedTier) Chat(ctx context.Context, req ai.ChatRequest) (<-chan ai.Event, error) {
	g.mu.Lock()
	g.reqs = append(g.reqs, req)
	g.mu.Unlock()
	if g.ledger != nil {
		g.ledger.note(g.name)
	}
	if g.fail != nil {
		return nil, g.fail
	}
	ch := make(chan ai.Event)
	go func() {
		defer close(ch)
		if g.release != nil {
			select {
			case <-g.release:
			case <-ctx.Done():
				ch <- ai.Event{Type: ai.EventError, Err: ctx.Err()}
				return
			}
		}
		if g.call != nil {
			select {
			case ch <- ai.Event{Type: ai.EventToolCall, Call: *g.call}:
			case <-ctx.Done():
			}
			return
		}
		for _, word := range strings.SplitAfter(g.text, " ") {
			select {
			case ch <- ai.Event{Type: ai.EventDelta, Content: word}:
			case <-ctx.Done():
				ch <- ai.Event{Type: ai.EventError, Err: ctx.Err()}
				return
			}
		}
		ch <- ai.Event{Type: ai.EventDone}
	}()
	return ch, nil
}

// let opens the gate. Idempotent, so it can be both a step in a test and that
// test's cleanup.
func (g *gatedTier) let() {
	if g.release == nil {
		return
	}
	g.releaseOnce.Do(func() { close(g.release) })
}

func (g *gatedTier) requests() []ai.ChatRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]ai.ChatRequest(nil), g.reqs...)
}

// manualGrace is the grace clock as a channel the test closes. A fresh channel
// per arming, so a second turn in the same test is not pre-expired by the
// first — which would make the second turn's host fire before its answer and
// turn a continuity test into a race.
type manualGrace struct {
	mu    sync.Mutex
	ch    chan time.Time
	asked time.Duration
	armed chan struct{}
}

func newManualGrace() *manualGrace {
	return &manualGrace{armed: make(chan struct{}, 8)}
}

func (g *manualGrace) timer(d time.Duration) (<-chan time.Time, func()) {
	g.mu.Lock()
	g.asked = d
	g.ch = make(chan time.Time)
	ch := g.ch
	g.mu.Unlock()
	select {
	case g.armed <- struct{}{}:
	default:
	}
	return ch, func() {}
}

// awaitArmed blocks until the host has asked for a grace clock.
func (g *manualGrace) awaitArmed(t *testing.T) {
	t.Helper()
	select {
	case <-g.armed:
	case <-time.After(5 * time.Second):
		t.Fatal("the host never armed its grace")
	}
}

// expire fires the most recently armed grace.
func (g *manualGrace) expire() {
	g.mu.Lock()
	ch := g.ch
	g.ch = nil
	g.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

func (g *manualGrace) window() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.asked
}

// hostWorld is a harness whose instant and medium tiers are two distinguishable
// gated providers, plus the grace clock and the decision seam.
type hostWorld struct {
	*harness
	host   *gatedTier
	answer *gatedTier
	synth  *recordingSynth
	grace  *manualGrace
	ledger *callLedger

	decisions chan string
}

const hostHoldingLine = "One moment."

func newHostWorld(t *testing.T, tune func(*Options)) *hostWorld {
	t.Helper()
	w := &hostWorld{
		grace:     newManualGrace(),
		ledger:    &callLedger{},
		decisions: make(chan string, 8),
	}
	w.host = &gatedTier{name: "host", text: hostHoldingLine, ledger: w.ledger}
	w.answer = &gatedTier{name: "answer", text: "Recursion is a function calling itself.",
		release: make(chan struct{}), ledger: w.ledger}

	// The harness's fakes, then the engine rebuilt around them — the
	// rebuild-and-drain shape newRecordedHarness uses, and for its reason.
	h := newHarness(t, Options{SpeakResponses: true})
	w.synth = &recordingSynth{Fake: h.tts, started: make(chan struct{})}
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	t.Cleanup(h.cancel)

	opts := Options{
		Model:          "brain-model",
		SpeakResponses: true,
		HostGrace:      700 * time.Millisecond,
		HostGraceTimer: w.grace.timer,
		Tiers: TierSet{
			Default: ai.TierMedium,
			Bindings: map[ai.Tier]TierBinding{
				ai.TierInstant: {Provider: w.host, Model: "small"},
				ai.TierMedium:  {Provider: w.answer, Model: "usual"},
			},
		},
	}
	if tune != nil {
		tune(&opts)
	}
	h.engine = NewEngine(h.provider, h.stt, w.synth, h.recorder, h.player, h.tools, nil, bus, nil, opts)
	h.engine.hostFinished = func(outcome string) {
		select {
		case w.decisions <- outcome:
		default:
		}
	}
	t.Cleanup(func() {
		if err := h.engine.Shutdown(context.Background()); err != nil {
			t.Errorf("engine had not quiesced by the end of the test: %v", err)
		}
	})
	// Registered after the drain, so LIFO runs it first: a failing test must
	// never leave the shutdown waiting on a turn parked behind a closed gate.
	t.Cleanup(w.answer.let)
	t.Cleanup(w.grace.expire)
	w.harness = h
	return w
}

// awaitHost blocks until the host goroutine has exited and returns what it did
// ("" when it stood down without saying anything).
func (w *hostWorld) awaitHost(t *testing.T) string {
	t.Helper()
	select {
	case outcome := <-w.decisions:
		return outcome
	case <-time.After(5 * time.Second):
		t.Fatal("the host never finished")
		return ""
	}
}

// submit starts a session and submits one question, without waiting for it.
func (w *hostWorld) submit(t *testing.T, text string) {
	t.Helper()
	if _, err := w.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := w.engine.Submit(text); err != nil {
		t.Fatal(err)
	}
}

// hostRecord reads the host's three keys out of a finished turn's record.
func hostRecord(t *testing.T, seen map[string]Event) (tier, model, outcome string) {
	t.Helper()
	ev, ok := seen["session.timings"]
	if !ok {
		t.Fatal("no session.timings event")
	}
	str := func(key string) string {
		v, _ := ev.Data[key].(string)
		return v
	}
	return str(StageHostTier), str(StageHostModel), str(StageHostOutcome)
}

// ---------------------------------------------------------------------------
// The grace: who speaks, and when
// ---------------------------------------------------------------------------

// TestAFastAnswerProducesNoChatter is the pin the ticket asks for first, and
// the one that decides whether this feature is bearable to live with: when the
// answering tier starts inside the grace, the host says nothing at all, records
// nothing, and publishes nothing.
func TestAFastAnswerProducesNoChatter(t *testing.T) {
	w := newHostWorld(t, nil)
	w.answer.let() // the answer streams the moment it is asked

	w.submit(t, "explain recursion")
	if outcome := w.awaitHost(t); outcome != "" {
		t.Errorf("the host did something on a fast turn: %q", outcome)
	}
	seen := w.collectUntil(t, "session.finished")
	w.waitIdle(t)

	if _, ok := seen["assistant.host"]; ok {
		t.Error("a fast turn published assistant.host")
	}
	tier, model, outcome := hostRecord(t, seen)
	if tier != "" || model != "" || outcome != "" {
		t.Errorf("a fast turn recorded a host: %q/%q/%q", tier, model, outcome)
	}
	if got := w.synth.texts(); len(got) != 1 || got[0] != "Recursion is a function calling itself." {
		t.Errorf("spoken = %q, want the answer alone", got)
	}
	// The grace was never expired in this test, and that is the point: the
	// host stood down on the answer, not on a clock.
	if seen["assistant.finished"].Data["content"] != "Recursion is a function calling itself." {
		t.Errorf("answer = %v", seen["assistant.finished"].Data["content"])
	}
}

// TestTheHostSpeaksOverASlowAnswer is the other half: the grace expires with
// nothing from the answering tier, so the host says one short line and the
// answer arrives underneath it, on the one playback stream and in order.
func TestTheHostSpeaksOverASlowAnswer(t *testing.T) {
	w := newHostWorld(t, nil)

	w.submit(t, "explain recursion")
	w.grace.awaitArmed(t)
	w.grace.expire()
	if outcome := w.awaitHost(t); outcome != hostOutcomeHeld {
		t.Fatalf("host outcome = %q, want %q", outcome, hostOutcomeHeld)
	}
	w.answer.let()

	seen := w.collectUntil(t, "session.finished")
	w.waitIdle(t)

	want := []string{hostHoldingLine, "Recursion is a function calling itself."}
	got := w.synth.texts()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("spoken = %q, want %q — the host first, then the answer", got, want)
	}
	if _, plays := w.player.Played(); plays != 1 {
		t.Errorf("player plays = %d, want one stream (issue #53)", plays)
	}
	// The record names both, separately.
	tier, model, outcome := hostRecord(t, seen)
	if tier != string(ai.TierInstant) || model != "small" || outcome != hostOutcomeHeld {
		t.Errorf("host record = %q/%q/%q, want instant/small/held", tier, model, outcome)
	}
	answerTier, answerModel, _, _ := tierOf(t, seen)
	if answerTier != string(ai.TierMedium) || answerModel != "usual" {
		t.Errorf("answer record = %q/%q, want medium/usual", answerTier, answerModel)
	}
	// And the event says which of the two the line was, and where it came from.
	ev, ok := seen["assistant.host"]
	if !ok {
		t.Fatal("no assistant.host event")
	}
	if ev.Data["content"] != hostHoldingLine || ev.Data["kind"] != "holding" ||
		ev.Data["tier"] != string(ai.TierInstant) || ev.Data["model"] != "small" {
		t.Errorf("assistant.host = %v", ev.Data)
	}
	// The answer is the answer. The holding line is not part of it, and never
	// reaches the conversation.
	if seen["assistant.finished"].Data["content"] != "Recursion is a function calling itself." {
		t.Errorf("answer = %v — the holding line must not be in it",
			seen["assistant.finished"].Data["content"])
	}
	for _, turn := range w.engine.Conversation() {
		if strings.Contains(turn.Text, hostHoldingLine) {
			t.Errorf("the holding line reached the conversation: %q", turn.Text)
		}
	}
}

// TestTheGraceIsTheConfiguredWindow pins that the wait the host asks for is the
// configured one, measured from the turn's model clock rather than from some
// second, differently-anchored moment.
func TestTheGraceIsTheConfiguredWindow(t *testing.T) {
	w := newHostWorld(t, func(o *Options) { o.HostGrace = 250 * time.Millisecond })
	w.submit(t, "explain recursion")
	w.grace.awaitArmed(t)
	w.grace.expire()
	w.awaitHost(t)
	w.answer.let()
	w.collectUntil(t, "session.finished")
	w.waitIdle(t)

	if window := w.grace.window(); window <= 0 || window > 250*time.Millisecond {
		t.Errorf("grace window = %v, want the configured 250ms measured from the model clock", window)
	}
}

// TestTheAnsweringRequestGoesOutFirst pins the ordering the whole feature is
// conditional on: the host may cost the answer nothing, so its own request
// cannot be opened until the answer's has been.
func TestTheAnsweringRequestGoesOutFirst(t *testing.T) {
	w := newHostWorld(t, nil)
	w.submit(t, "explain recursion")
	w.grace.awaitArmed(t)
	w.grace.expire()
	w.awaitHost(t)
	w.answer.let()
	w.collectUntil(t, "session.finished")
	w.waitIdle(t)

	order := w.ledger.order()
	if len(order) < 2 || order[0] != "answer" || order[1] != "host" {
		t.Errorf("provider call order = %v, want the answer asked before the host", order)
	}
}

// ---------------------------------------------------------------------------
// Honesty
// ---------------------------------------------------------------------------

// TestAnAssertingHostLineIsDiscardedRatherThanSpoken is the fixture case: a
// host that ignores its prompt and answers the question. Nothing is spoken,
// nothing is published, and the record says the line was refused — because a
// silent discard would make a working guard look like a broken feature.
func TestAnAssertingHostLineIsDiscardedRatherThanSpoken(t *testing.T) {
	w := newHostWorld(t, nil)
	w.host.text = "Recursion is a function that calls itself."

	w.submit(t, "explain recursion")
	w.grace.awaitArmed(t)
	w.grace.expire()
	if outcome := w.awaitHost(t); outcome != hostOutcomeRefused {
		t.Fatalf("host outcome = %q, want %q", outcome, hostOutcomeRefused)
	}
	w.answer.let()
	seen := w.collectUntil(t, "session.finished")
	w.waitIdle(t)

	for _, spoken := range w.synth.texts() {
		if strings.Contains(spoken, "calls itself") {
			t.Fatalf("the refused line was spoken: %q", spoken)
		}
	}
	if _, ok := seen["assistant.host"]; ok {
		t.Error("a refused line was published as an event; it must not reach a client by any route")
	}
	if _, _, outcome := hostRecord(t, seen); outcome != hostOutcomeRefused {
		t.Errorf("host_outcome = %q, want refused on the record", outcome)
	}
	// And the turn is otherwise exactly the turn it would have been.
	if seen["assistant.finished"].Data["content"] != "Recursion is a function calling itself." {
		t.Errorf("answer = %v", seen["assistant.finished"].Data["content"])
	}
}

// TestTheHostHoldsNoToolsAtAll is the hard rule, pinned on the wire. The
// answering tier is offered the registry's tools; the host is offered nothing,
// on a turn where tools are demonstrably in play.
func TestTheHostHoldsNoToolsAtAll(t *testing.T) {
	w := newHostWorld(t, nil)
	registry := tools.NewRegistry(nil)
	registry.Register(&namedTool{name: "shell.run", result: "ok"})
	policy, err := tools.NewPolicy(tools.PolicyConfig{Default: "allow"})
	if err != nil {
		t.Fatal(err)
	}
	registry.SetPolicy(policy)
	w.engine.tools = registry

	w.submit(t, "explain recursion")
	w.grace.awaitArmed(t)
	w.grace.expire()
	w.awaitHost(t)
	w.answer.let()
	w.collectUntil(t, "session.finished")
	w.waitIdle(t)

	hostReqs := w.host.requests()
	if len(hostReqs) != 1 {
		t.Fatalf("host was asked %d times, want once", len(hostReqs))
	}
	if len(hostReqs[0].Tools) != 0 {
		t.Errorf("the host was offered tools: %v", hostReqs[0].Tools)
	}
	// The prompt is the instruction and the question, and nothing else: no
	// history, no capture, no memory — nothing it could state a fact from.
	want := []ai.Message{
		{Role: ai.RoleSystem, Content: hostSystemPrompt},
		{Role: ai.RoleUser, Content: "explain recursion"},
	}
	if len(hostReqs[0].Messages) != len(want) {
		t.Fatalf("host messages = %+v, want exactly %+v", hostReqs[0].Messages, want)
	}
	for i := range want {
		got := hostReqs[0].Messages[i]
		if got.Role != want[i].Role || got.Content != want[i].Content ||
			len(got.ToolCalls) != 0 || got.ToolCallID != "" {
			t.Errorf("host message %d = %+v, want %+v", i, got, want[i])
		}
	}
	// The answering tier, on the same turn, did get them — so the absence
	// above is the rule and not an empty registry.
	answerReqs := w.answer.requests()
	if len(answerReqs) == 0 || len(answerReqs[0].Tools) == 0 {
		t.Error("the answering tier was offered no tools; the pin above proves nothing")
	}
}

// TestAHostThatCallsAToolIsAbandoned covers the case the rule above cannot
// prevent: a provider that produces a tool call anyway. The line is thrown
// away whole rather than reasoned about.
func TestAHostThatCallsAToolIsAbandoned(t *testing.T) {
	w := newHostWorld(t, nil)
	w.host.call = &ai.ToolCall{ID: "c1", Name: "shell.run", Arguments: "{}"}

	w.submit(t, "explain recursion")
	w.grace.awaitArmed(t)
	w.grace.expire()
	if outcome := w.awaitHost(t); outcome != "" {
		t.Errorf("host outcome = %q, want nothing at all", outcome)
	}
	w.answer.let()
	seen := w.collectUntil(t, "session.finished")
	w.waitIdle(t)

	if _, ok := seen["assistant.host"]; ok {
		t.Error("a host that called a tool still spoke")
	}
}

// TestTheHandoffIsDecidedUnderOneLock pins the arbitration itself, at the seam
// rather than through a turn: the two claims are mutually exclusive in both
// orders, whichever goroutine gets there first.
func TestTheHandoffIsDecidedUnderOneLock(t *testing.T) {
	t.Run("the answer first", func(t *testing.T) {
		s := &sess{issued: make(chan struct{}), begun: make(chan struct{})}
		if !s.beginAnswer() {
			t.Fatal("the first answer claim was refused")
		}
		if !s.beginAnswer() {
			t.Error("a second claim from the same answer was refused")
		}
		if !answerBegan(s) {
			t.Error("answerBegan is false after the answer claimed the turn")
		}
		if s.claimHost() {
			t.Error("the host took a turn the answer had already begun")
		}
	})
	t.Run("the host first", func(t *testing.T) {
		s := &sess{issued: make(chan struct{}), begun: make(chan struct{})}
		if !s.claimHost() {
			t.Fatal("the host's claim was refused on an untouched turn")
		}
		if s.beginAnswer() {
			t.Error("the answer began under a question the host had committed to")
		}
		if answerBegan(s) {
			t.Error("a refused answer still marked the turn as begun")
		}
	})
}

// ---------------------------------------------------------------------------
// The handoff to the answer
// ---------------------------------------------------------------------------

// TestTheHoldingLineFinishesBeforeTheAnswerFollows is the mid-sentence case:
// the answer arrives while the host is still being synthesized. The sentence
// finishes, the answer follows it on the same stream, and nothing overlaps.
func TestTheHoldingLineFinishesBeforeTheAnswerFollows(t *testing.T) {
	w := newHostWorld(t, nil)
	release := holdFirstSentence(t, w.harness)

	w.submit(t, "explain recursion")
	w.grace.awaitArmed(t)
	w.grace.expire()
	// The event, not the hostFinished seam: with the synthesizer gated the
	// host's goroutine is parked *inside* holdFor until the line has been
	// handed to the player, which is exactly the state this test needs it in.
	// assistant.host is published at the decision, before that wait.
	w.waitFor(t, "assistant.host")
	// The holding line is inside the synthesizer, parked on the gate: this is
	// "mid-sentence", established rather than waited for.
	select {
	case <-w.synth.firstSpeak():
	case <-time.After(5 * time.Second):
		t.Fatal("the holding line never reached the voice")
	}
	w.answer.let()
	release()

	seen := w.collectUntil(t, "session.finished")
	w.waitIdle(t)

	want := []string{hostHoldingLine, "Recursion is a function calling itself."}
	got := w.synth.texts()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("spoken = %q, want %q — the sentence in flight is never cut", got, want)
	}
	if _, plays := w.player.Played(); plays != 1 {
		t.Errorf("player plays = %d, want one stream", plays)
	}
	if _, dropped := seen["session.timings"].Data[StageSupersededSentences]; dropped {
		t.Error("a holding line already in the synthesizer was counted as superseded")
	}
}

// TestAHoldingLineTheAnswerBeatToTheQueueIsDropped pins the other side of the
// same doctrine, at the speaker's own seam — where the interleaving is
// reachable, and where the guarantee has to hold by construction rather than
// by the current shape of the caller (the argument supersession_test makes).
//
// The queue is FIFO, so committing an answer sentence and then a holding line
// is exactly the race a loaded machine produces: the line was enqueued before
// the speaker got round to it, and by the time it did the answer had moved on.
func TestAHoldingLineTheAnswerBeatToTheQueueIsDropped(t *testing.T) {
	h, synth := newRecordedHarness(t, nil)
	sp, s := speakerUnderTest(t, h)

	sp.speak("Recursion is a function calling itself.")
	sp.holdFor(s.ctx, hostHoldingLine) // returns when the speaker drops it
	if err := sp.close(); err != nil {
		t.Fatalf("speaker: %v", err)
	}

	got := synth.texts()
	if len(got) != 1 || got[0] != "Recursion is a function calling itself." {
		t.Errorf("spoken = %q, want the answer alone — the stale holding line drops", got)
	}
	if report := s.timings.report(); report[StageSupersededSentences] != 1 {
		t.Errorf("superseded_sentences = %v, want 1", report[StageSupersededSentences])
	}
}

// TestAHoldingLineWithNoAnswerYetIsSpoken is the control for the test above:
// the same call, on a speaker no answer sentence has reached, plays.
func TestAHoldingLineWithNoAnswerYetIsSpoken(t *testing.T) {
	h, synth := newRecordedHarness(t, nil)
	sp, s := speakerUnderTest(t, h)

	sp.holdFor(s.ctx, hostHoldingLine)
	if err := sp.close(); err != nil {
		t.Fatalf("speaker: %v", err)
	}
	if got := synth.texts(); len(got) != 1 || got[0] != hostHoldingLine {
		t.Errorf("spoken = %q, want the holding line", got)
	}
}

// ---------------------------------------------------------------------------
// Clarification
// ---------------------------------------------------------------------------

// TestAClarifyingQuestionTakesTheTurnAndKeepsTheThread is #125's continuity
// applied to a question the host asked: the answer attempt is abandoned, the
// question is this turn's reply, and the user's answer to it arrives as an
// ordinary follow-up with the exchange behind it.
func TestAClarifyingQuestionTakesTheTurnAndKeepsTheThread(t *testing.T) {
	const question = "Do you mean the deploy script or the deploy thread?"
	w := newHostWorld(t, func(o *Options) { o.HistoryTurns = 8 })
	w.host.text = question

	w.submit(t, "is the deploy broken")
	w.grace.awaitArmed(t)
	w.grace.expire()
	if outcome := w.awaitHost(t); outcome != hostOutcomeClarified {
		t.Fatalf("host outcome = %q, want %q", outcome, hostOutcomeClarified)
	}
	seen := w.collectUntil(t, "session.finished")
	w.waitIdle(t)

	if seen["assistant.finished"].Data["content"] != question {
		t.Errorf("reply = %v, want the question", seen["assistant.finished"].Data["content"])
	}
	if got := w.synth.texts(); len(got) != 1 || got[0] != question {
		t.Errorf("spoken = %q, want the question alone — the answer was abandoned", got)
	}
	// The record names the host as what produced this turn, not the tier that
	// was asked and never answered.
	tier, model, reason, _ := tierOf(t, seen)
	if tier != string(ai.TierInstant) || model != "small" || reason != string(ai.ReasonHost) {
		t.Errorf("record = %q/%q/%q, want instant/small/host", tier, model, reason)
	}
	if _, _, outcome := hostRecord(t, seen); outcome != hostOutcomeClarified {
		t.Errorf("host_outcome = %q, want clarified", outcome)
	}
	if ev := seen["assistant.host"]; ev.Data["kind"] != "clarification" {
		t.Errorf("assistant.host kind = %v, want clarification", ev.Data["kind"])
	}

	// The thread continues. The next turn is answered normally — the grace is
	// freshly armed and never expired, so the answer wins it — and the model
	// is sent the question it asked and the reply to it.
	w.answer.let()
	w.host.text = hostHoldingLine
	w.submit(t, "the script")
	w.collectUntil(t, "session.finished")
	w.waitIdle(t)

	reqs := w.answer.requests()
	if len(reqs) == 0 {
		t.Fatal("the follow-up never reached the answering tier")
	}
	var thread []string
	for _, m := range reqs[len(reqs)-1].Messages {
		thread = append(thread, string(m.Role)+": "+m.Content)
	}
	joined := strings.Join(thread, "\n")
	for _, want := range []string{"user: is the deploy broken", "assistant: " + question, "user: the script"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the follow-up prompt is missing %q; the thread was stranded\n%s", want, joined)
		}
	}
}

// ---------------------------------------------------------------------------
// Standing down
// ---------------------------------------------------------------------------

// TestAnUnreachableHostDegradesToSilenceThenTheAnswer: the host's endpoint is
// not there. Nothing is said, nothing is recorded, no error reaches the user,
// and the answering tier's turn is untouched.
func TestAnUnreachableHostDegradesToSilenceThenTheAnswer(t *testing.T) {
	w := newHostWorld(t, nil)
	w.host.fail = errors.New("connection refused")

	w.submit(t, "explain recursion")
	w.grace.awaitArmed(t)
	w.grace.expire()
	if outcome := w.awaitHost(t); outcome != "" {
		t.Errorf("an unreachable host recorded %q", outcome)
	}
	w.answer.let()
	seen := w.collectUntil(t, "session.finished")
	w.waitIdle(t)

	if _, ok := seen["error"]; ok {
		t.Errorf("an unreachable host produced an error event: %v", seen["error"].Data)
	}
	if _, ok := seen["assistant.host"]; ok {
		t.Error("an unreachable host published a line")
	}
	if tier, _, outcome := hostRecord(t, seen); tier != "" || outcome != "" {
		t.Errorf("an unreachable host left %q/%q on the record", tier, outcome)
	}
	if got := w.synth.texts(); len(got) != 1 || got[0] != "Recursion is a function calling itself." {
		t.Errorf("spoken = %q, want the answer alone", got)
	}
}

// TestQuickStandsTheHostDown: the user chose Quick, so there is no wait to
// cover and the host is never armed at all — not armed and silent, never armed.
func TestQuickStandsTheHostDown(t *testing.T) {
	w := newHostWorld(t, nil)
	w.host.let() // instant answers this turn, so it must not be gated
	if _, err := w.engine.SetThinking(ai.TierInstant); err != nil {
		t.Fatal(err)
	}

	w.submit(t, "explain recursion")
	seen := w.collectUntil(t, "session.finished")
	w.waitIdle(t)

	select {
	case outcome := <-w.decisions:
		t.Errorf("a host ran on a Quick turn and did %q", outcome)
	default:
	}
	if _, ok := seen["assistant.host"]; ok {
		t.Error("a Quick turn published assistant.host")
	}
	// The instant tier was asked exactly once, and it was asked the question —
	// not the host's instruction.
	reqs := w.host.requests()
	if len(reqs) != 1 {
		t.Fatalf("instant was asked %d times on a Quick turn, want once", len(reqs))
	}
	for _, m := range reqs[0].Messages {
		if m.Content == hostSystemPrompt {
			t.Error("the Quick turn's own answer carried the host prompt")
		}
	}
	if tier, _, _, _ := tierOf(t, seen); tier != string(ai.TierInstant) {
		t.Errorf("tier = %q, want instant", tier)
	}
}

// TestWithoutAGraceThereIsNoHost: the shipped state of an engine nobody
// configured a grace for. The instant tier is bound and still never speaks.
func TestWithoutAGraceThereIsNoHost(t *testing.T) {
	w := newHostWorld(t, func(o *Options) { o.HostGrace = 0 })
	w.answer.let()

	w.submit(t, "explain recursion")
	w.collectUntil(t, "session.finished")
	w.waitIdle(t)

	select {
	case outcome := <-w.decisions:
		t.Errorf("a host ran with no grace configured and did %q", outcome)
	default:
	}
	if len(w.host.requests()) != 0 {
		t.Errorf("the instant tier was asked %d times with no grace configured", len(w.host.requests()))
	}
}

// TestWithNoInstantTierThereIsNoHost: tiering on, instant absent. There is no
// host to be, and the turn is the turn tiering already had.
func TestWithNoInstantTierThereIsNoHost(t *testing.T) {
	w := newHostWorld(t, func(o *Options) {
		delete(o.Tiers.Bindings, ai.TierInstant)
	})
	w.answer.let()

	w.submit(t, "explain recursion")
	seen := w.collectUntil(t, "session.finished")
	w.waitIdle(t)

	select {
	case outcome := <-w.decisions:
		t.Errorf("a host ran with no instant tier and did %q", outcome)
	default:
	}
	if tier, _, outcome := hostRecord(t, seen); tier != "" || outcome != "" {
		t.Errorf("host keys on a turn with no host: %q/%q", tier, outcome)
	}
}

// TestAQuietTurnHasNoHost: a scheduled clockfire with announce off says nothing
// out loud, so a host would be a provider call spent on silence.
func TestAQuietTurnHasNoHost(t *testing.T) {
	w := newHostWorld(t, nil)
	w.answer.let()

	if _, err := w.engine.StartScheduledSession(false); err != nil {
		t.Fatal(err)
	}
	if err := w.engine.Submit("explain recursion"); err != nil {
		t.Fatal(err)
	}
	w.collectUntil(t, "session.finished")
	w.waitIdle(t)

	select {
	case outcome := <-w.decisions:
		t.Errorf("a host ran on a quiet turn and did %q", outcome)
	default:
	}
	if len(w.host.requests()) != 0 {
		t.Error("a quiet turn asked the host for a line nobody would hear")
	}
}

// TestTheHostSurvivesATurnThatDiesUnderIt: the session is cancelled while the
// host is still deciding. Nothing is spoken, nothing panics, and the engine
// quiesces — which is what the harness's own shutdown check asserts.
func TestTheHostSurvivesACancelMidDecision(t *testing.T) {
	w := newHostWorld(t, nil)

	w.submit(t, "explain recursion")
	w.grace.awaitArmed(t)
	if err := w.engine.Cancel(); err != nil {
		t.Fatal(err)
	}
	if outcome := w.awaitHost(t); outcome != "" {
		t.Errorf("a cancelled turn's host did %q", outcome)
	}
	w.waitIdle(t)
	if _, ok := findEvent(collectNothing(w), "assistant.host"); ok {
		t.Error("a cancelled turn's host still spoke")
	}
}

// collectNothing drains whatever the bus has right now, without waiting: the
// cancel test has no terminal event of its own to wait for.
func collectNothing(w *hostWorld) []Event {
	var out []Event
	for {
		select {
		case ev := <-w.events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// TestNoTiersMeansNoHostAndNoNewKeys: the byte-identity promise ADR 0063 made,
// re-checked now that a second model can speak. A configuration with no tiers
// gains no host, no event and no record key.
func TestNoTiersMeansNoHostAndNoNewKeys(t *testing.T) {
	h := newHarness(t, Options{Model: "brain-model", SpeakResponses: true,
		HostGrace: 700 * time.Millisecond})
	seen := h.askCollecting(t, "explain recursion")

	if _, ok := seen["assistant.host"]; ok {
		t.Error("an untiered configuration published assistant.host")
	}
	for _, key := range []string{StageHostTier, StageHostModel, StageHostOutcome} {
		if _, ok := seen["session.timings"].Data[key]; ok {
			t.Errorf("session.timings carries %q with no tiers configured", key)
		}
	}
}
