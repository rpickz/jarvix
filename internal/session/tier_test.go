package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/tools"
)

// These tests drive the whole tier feature through the real engine with the
// provider faked at the existing seam (ai.Provider), which is the only place
// a tier becomes observable: the model that got the request.
//
// The routing table's own exhaustive tests live in internal/ai. What is
// proved here is that the engine asks it the right question and does what it
// says — including on the two paths that are only reachable through a running
// turn, the failover and the context budget.

// unreachableTier is a tier whose endpoint is not there: Chat refuses before
// streaming, which is the one failure another tier may still answer.
//
// Its recording field is unexported behind an accessor that takes the same
// mutex the write does — the engine calls Chat on a session goroutine while
// the test reads from its own.
type unreachableTier struct {
	err error

	mu    sync.Mutex
	calls int
}

func (p *unreachableTier) Name() string { return "unreachable" }

func (p *unreachableTier) Chat(context.Context, ai.ChatRequest) (<-chan ai.Event, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return nil, p.err
}

func (p *unreachableTier) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// tierWorld builds a harness whose three tiers are three distinguishable
// providers, so "which one answered" is a fact rather than an inference.
type tierWorld struct {
	*harness
	instant *ai.Fake
	medium  *ai.Fake
	deep    *ai.Fake
}

func newTierWorld(t *testing.T, def ai.Tier, extra func(*Options)) *tierWorld {
	t.Helper()
	w := &tierWorld{
		instant: &ai.Fake{Response: "instant answer."},
		medium:  &ai.Fake{Response: "medium answer."},
		deep:    &ai.Fake{Response: "deep answer."},
	}
	opts := Options{
		Model: "brain-model",
		Tiers: TierSet{
			Default: def,
			Bindings: map[ai.Tier]TierBinding{
				ai.TierInstant: {Provider: w.instant, Model: "small"},
				ai.TierMedium:  {Provider: w.medium, Model: "usual"},
				ai.TierDeep:    {Provider: w.deep, Model: "strong"},
			},
		},
	}
	if extra != nil {
		extra(&opts)
	}
	w.harness = newHarness(t, opts)
	return w
}

// tierOf reads the serving tier out of a finished turn's timings event, which
// is the record itself rather than a reconstruction of it.
func tierOf(t *testing.T, seen map[string]Event) (tier, model, reason, wanted string) {
	t.Helper()
	ev, ok := seen["session.timings"]
	if !ok {
		t.Fatal("no session.timings event")
	}
	str := func(key string) string {
		v, _ := ev.Data[key].(string)
		return v
	}
	return str(StageTier), str(StageTierModel), str(StageTierReason), str(StageTierWanted)
}

// ---------------------------------------------------------------------------
// The byte-identity promise
// ---------------------------------------------------------------------------

// With no [ai.tiers] table there is no routing, no extra event key, and no
// record key: the turn is the turn this engine has always taken. This is the
// pin the ticket asks for, and it is written as an exact comparison rather
// than a spot check because "mostly the same" is not the promise.
func TestNoTiersConfiguredIsTodaysTurnExactly(t *testing.T) {
	h := newHarness(t, Options{Model: "brain-model", SystemPrompt: "you are jarvix"})
	seen := h.askCollecting(t, "explain recursion")

	req := h.provider.LastRequest
	if req.Model != "brain-model" {
		t.Errorf("model = %q, want the [ai] model unchanged", req.Model)
	}
	if len(req.Tools) != 0 {
		t.Errorf("tools = %v, want none", req.Tools)
	}
	want := []ai.Message{
		{Role: ai.RoleSystem, Content: "you are jarvix"},
		{Role: ai.RoleUser, Content: "explain recursion"},
	}
	if len(req.Messages) != len(want) {
		t.Fatalf("messages = %+v, want exactly %+v", req.Messages, want)
	}
	for i := range want {
		got := req.Messages[i]
		if got.Role != want[i].Role || got.Content != want[i].Content ||
			len(got.ToolCalls) != 0 || got.ToolCallID != "" {
			t.Errorf("message %d = %+v, want %+v", i, got, want[i])
		}
	}
	// No tier keys anywhere. Their absence is what tells a reader there was
	// no routing decision, so a key saying "medium" here would be a claim
	// nobody made.
	for _, key := range []string{StageTier, StageTierModel, StageTierReason,
		StageTierWanted, StageTierContextDropped} {
		if _, ok := seen["session.timings"].Data[key]; ok {
			t.Errorf("session.timings carries %q with no tiers configured", key)
		}
	}
	started := seen["assistant.started"].Data
	if len(started) != 2 || started["session_id"] == nil || started["provider"] == nil {
		t.Errorf("assistant.started = %v, want only session_id and provider", started)
	}
	if got := h.engine.Thinking(); got != ai.TierNone {
		t.Errorf("Thinking() = %q with no tiers configured, want none", got)
	}
	if levels := h.engine.AvailableTiers(); len(levels) != 0 {
		t.Errorf("AvailableTiers() = %v, want none", levels)
	}
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

func TestTheDefaultTierAnswersAnOrdinaryTurn(t *testing.T) {
	w := newTierWorld(t, ai.TierMedium, nil)
	seen := w.askCollecting(t, "explain recursion")

	if len(w.medium.Requests) != 1 {
		t.Fatalf("medium served %d turns, want 1", len(w.medium.Requests))
	}
	if len(w.instant.Requests)+len(w.deep.Requests) != 0 {
		t.Error("a tier nobody asked for served the turn")
	}
	tier, model, reason, _ := tierOf(t, seen)
	if tier != "medium" || model != "usual" || reason != "default" {
		t.Errorf("record = %q/%q/%q, want medium/usual/default", tier, model, reason)
	}
	if got := w.medium.LastRequest.Model; got != "usual" {
		t.Errorf("model asked for = %q, want the tier's own", got)
	}
}

// The instant tier answers a plain turn with no tools attached — the case the
// whole feature exists for.
func TestInstantServesATurnWithNoTools(t *testing.T) {
	w := newTierWorld(t, ai.TierInstant, nil)
	seen := w.askCollecting(t, "what time is it")

	if len(w.instant.Requests) != 1 {
		t.Fatalf("instant served %d turns, want 1", len(w.instant.Requests))
	}
	tier, model, _, _ := tierOf(t, seen)
	if tier != "instant" || model != "small" {
		t.Errorf("record = %q/%q, want instant/small", tier, model)
	}
	if got := seen["assistant.started"].Data["tier"]; got != "instant" {
		t.Errorf("assistant.started tier = %v, want instant — the pending turn reads this", got)
	}
	if got := seen["assistant.started"].Data["tier_label"]; got != "Quick" {
		t.Errorf("assistant.started tier_label = %v, want Quick", got)
	}
}

// The hard rule, through the real engine: a turn whose request carries tool
// definitions is never served by the instant tier, even when instant is the
// configured default and nothing else asked for anything.
//
// #71 is why. A model too small for what it was holding narrated actions it
// had never performed; a small model with tools in its hands is that incident
// with the safety catch off.
func TestATurnThatCanCallAToolIsNeverServedByInstant(t *testing.T) {
	tool := &namedTool{name: "shell.run", result: "ok"}
	w := &tierWorld{
		instant: &ai.Fake{Response: "instant answer."},
		medium:  &ai.Fake{Response: "medium answer."},
		deep:    &ai.Fake{Response: "deep answer."},
	}
	opts := Options{
		Model: "brain-model",
		Tiers: TierSet{
			// Instant as the default *and* pinned below: neither may get past
			// the rule.
			Default: ai.TierInstant,
			Bindings: map[ai.Tier]TierBinding{
				ai.TierInstant: {Provider: w.instant, Model: "small"},
				ai.TierMedium:  {Provider: w.medium, Model: "usual"},
			},
		},
	}
	h := newGateHarness(t, opts, tool, tools.PolicyConfig{Default: "allow"})
	// The gate harness rebuilds the engine around its own provider; point the
	// tiers at the fakes it now holds.
	w.harness = h
	if _, err := h.engine.SetThinking(ai.TierInstant); err != nil {
		t.Fatal(err)
	}
	seen := w.askCollecting(t, "quick answer, what time is it")

	if got := w.instant.Requests; len(got) != 0 {
		t.Fatalf("instant was handed %d tool-carrying turns; #71 says never", len(got))
	}
	if len(w.medium.Requests) == 0 {
		t.Fatal("nothing served the turn")
	}
	if got := w.medium.LastRequest.Tools; len(got) == 0 {
		t.Fatal("the turn carried no tools, so this proves nothing")
	}
	tier, _, reason, wanted := tierOf(t, seen)
	if tier != "medium" || reason != "tools" || wanted != "instant" {
		t.Errorf("record = %q/%q/%q, want medium/tools/instant — the refusal must be legible",
			tier, reason, wanted)
	}
}

// ---------------------------------------------------------------------------
// Asking for a tier
// ---------------------------------------------------------------------------

func TestSpokenEscalationSendsOneTurnToDeep(t *testing.T) {
	w := newTierWorld(t, ai.TierMedium, nil)
	seen := w.askCollecting(t, "think hard about this, what should I do about the rota")

	if len(w.deep.Requests) != 1 {
		t.Fatalf("deep served %d turns, want 1", len(w.deep.Requests))
	}
	tier, _, reason, _ := tierOf(t, seen)
	if tier != "deep" || reason != "asked" {
		t.Errorf("record = %q/%q, want deep/asked", tier, reason)
	}
	// One spoken cue, before the answer, so a chosen wait is announced once
	// rather than narrated.
	answer, _ := seen["assistant.finished"].Data["content"].(string)
	if !strings.HasPrefix(answer, DeepThinkingCue) {
		t.Errorf("answer = %q, want it to lead with the deep cue", answer)
	}

	// And only that turn: an escalation is a request about one question, not
	// a new setting.
	w.askCollecting(t, "and the week after")
	if len(w.deep.Requests) != 1 {
		t.Errorf("deep served %d turns; an escalation must not stick", len(w.deep.Requests))
	}
	if len(w.medium.Requests) != 1 {
		t.Errorf("medium served %d turns after the escalation, want 1", len(w.medium.Requests))
	}
}

// The utterance reaches the model unchanged: the escalation annotates a turn,
// it does not claim it, and the archive should hold what the user said.
func TestAnEscalationDoesNotSwallowTheQuestion(t *testing.T) {
	w := newTierWorld(t, ai.TierMedium, nil)
	const said = "take your time and tell me what to cook"
	w.askCollecting(t, said)

	req := w.deep.LastRequest
	last := req.Messages[len(req.Messages)-1]
	if last.Role != ai.RoleUser || last.Content != said {
		t.Errorf("the model was sent %+v, want the utterance verbatim", last)
	}
}

func TestPinnedTierServesEveryTurnUntilTheConversationEnds(t *testing.T) {
	w := newTierWorld(t, ai.TierMedium, nil)
	if got, err := w.engine.SetThinking(ai.TierDeep); err != nil || got != ai.TierDeep {
		t.Fatalf("SetThinking = %q, %v", got, err)
	}
	w.askCollecting(t, "first question")
	w.askCollecting(t, "second question")
	if len(w.deep.Requests) != 2 {
		t.Fatalf("deep served %d turns, want both", len(w.deep.Requests))
	}

	// A new conversation returns to the configured default.
	w.engine.NewConversation()
	if got := w.engine.Thinking(); got != ai.TierMedium {
		t.Fatalf("Thinking() = %q after a new conversation, want the configured default", got)
	}
	w.askCollecting(t, "third question")
	if len(w.medium.Requests) != 1 {
		t.Errorf("medium served %d turns after the reset, want 1", len(w.medium.Requests))
	}
}

// The spoken pin and the window's control move the same thing. There is one
// level; this proves the phrase reaches it and that a pin costs no model call.
func TestASpokenPinMovesTheLevelAndAsksNoModel(t *testing.T) {
	w := newTierWorld(t, ai.TierMedium, func(o *Options) {
		o.Intents = mustRouter(t)
	})
	seen := w.askCollecting(t, "switch to the deep model")

	if got := w.engine.Thinking(); got != ai.TierDeep {
		t.Fatalf("Thinking() = %q, want deep", got)
	}
	total := len(w.instant.Requests) + len(w.medium.Requests) + len(w.deep.Requests)
	if total != 0 {
		t.Errorf("a pin cost %d provider calls, want none", total)
	}
	if ack, _ := seen["intent.executed"].Data["acknowledgement"].(string); ack != "Deep answers." {
		t.Errorf("acknowledgement = %q", ack)
	}
}

// A level this machine cannot serve is refused where it is asked for, not at
// answer time — which is the whole reason the control asks first.
func TestPinningAnUnconfiguredLevelIsRefusedInPlace(t *testing.T) {
	medium := &ai.Fake{Response: "medium answer."}
	h := newHarness(t, Options{
		Model: "brain-model",
		Tiers: TierSet{
			Default:  ai.TierMedium,
			Bindings: map[ai.Tier]TierBinding{ai.TierMedium: {Provider: medium, Model: "usual"}},
		},
	})
	got, err := h.engine.SetThinking(ai.TierDeep)
	if err == nil {
		t.Fatal("pinning deep with no deep tier was accepted")
	}
	if !strings.Contains(err.Error(), "deep") {
		t.Errorf("error %q does not name the level", err)
	}
	if got != ai.TierMedium {
		t.Errorf("level = %q after a refused pin, want it unmoved", got)
	}
}

// ---------------------------------------------------------------------------
// Honesty when a tier cannot answer
// ---------------------------------------------------------------------------

// Asked for, not configured: Jarvix says so and answers from the tier it has.
// Never a silent downgrade.
func TestAskingForATierThatIsNotConfiguredSaysSo(t *testing.T) {
	medium := &ai.Fake{Response: "the usual answer."}
	h := newHarness(t, Options{
		Model: "brain-model",
		Tiers: TierSet{
			Default:  ai.TierMedium,
			Bindings: map[ai.Tier]TierBinding{ai.TierMedium: {Provider: medium, Model: "usual"}},
		},
	})
	seen := h.askCollecting(t, "think hard about this, what should I do")

	answer, _ := seen["assistant.finished"].Data["content"].(string)
	if !strings.Contains(answer, "no deep model configured") {
		t.Errorf("answer = %q, want it to say the deep model is not configured", answer)
	}
	if !strings.Contains(answer, "the usual answer") {
		t.Errorf("answer = %q, want the reachable tier's answer offered too", answer)
	}
	tier, _, reason, wanted := tierOf(t, seen)
	if tier != "medium" || reason != "unavailable" || wanted != "deep" {
		t.Errorf("record = %q/%q/%q, want medium/unavailable/deep", tier, reason, wanted)
	}
}

// Configured but not answering: the same disappointment with a different
// cause, and it has to be said just as plainly.
func TestUnreachableDeepFailsOverAndNamesTheTierItCouldNotReach(t *testing.T) {
	deep := &unreachableTier{err: errors.New("dial tcp 127.0.0.1:9999: connection refused")}
	medium := &ai.Fake{Response: "the usual answer."}
	h := newHarness(t, Options{
		Model:        "brain-model",
		HistoryTurns: 4,
		Tiers: TierSet{
			Default: ai.TierMedium,
			Bindings: map[ai.Tier]TierBinding{
				ai.TierMedium: {Provider: medium, Model: "usual"},
				ai.TierDeep:   {Provider: deep, Model: "strong"},
			},
		},
	})
	if _, err := h.engine.SetThinking(ai.TierDeep); err != nil {
		t.Fatal(err)
	}
	seen := h.askCollecting(t, "what should I do")

	if deep.Calls() != 1 {
		t.Fatalf("deep was called %d times, want exactly one attempt", deep.Calls())
	}
	if len(medium.Requests) != 1 {
		t.Fatalf("medium served %d turns, want the failover", len(medium.Requests))
	}
	answer, _ := seen["assistant.finished"].Data["content"].(string)
	if !strings.Contains(answer, "couldn't reach the deep model") {
		t.Errorf("answer = %q, want it to name the tier it could not reach", answer)
	}
	tier, model, reason, wanted := tierOf(t, seen)
	if tier != "medium" || model != "usual" || wanted != "deep" {
		t.Errorf("record = %q/%q, wanted %q — the record must name what answered, not what was asked",
			tier, model, wanted)
	}
	// "unreachable", not "unavailable": deep is configured here and simply did
	// not answer, and the two have different fixes.
	if reason != "unreachable" {
		t.Errorf("reason = %q, want unreachable", reason)
	}
	// The failover sentence is transient: it describes this turn's plumbing,
	// and carrying it into the model's own context as something it said would
	// be noise the next answer has to reason around.
	turns := h.engine.Conversation()
	if len(turns) == 0 {
		t.Fatal("nothing was committed")
	}
	last := turns[len(turns)-1]
	if last.Role != "assistant" {
		t.Fatalf("last turn is %q, want the answer", last.Role)
	}
	if strings.Contains(last.Text, "couldn't reach") {
		t.Errorf("the failover sentence was recorded as the assistant's own words: %q", last.Text)
	}
}

// A tier that breaks after it has started answering is not failed over: words
// are already on the screen, and finishing them from a different model would
// be a worse kind of wrong than stopping.
func TestATierThatBreaksMidAnswerIsNotFailedOver(t *testing.T) {
	deep := &ai.Fake{Response: "half an ans", Fail: errors.New("stream broke")}
	medium := &ai.Fake{Response: "the usual answer."}
	h := newHarness(t, Options{
		Model: "brain-model",
		Tiers: TierSet{
			Default: ai.TierMedium,
			Bindings: map[ai.Tier]TierBinding{
				ai.TierMedium: {Provider: medium, Model: "usual"},
				ai.TierDeep:   {Provider: deep, Model: "strong"},
			},
		},
	})
	if _, err := h.engine.SetThinking(ai.TierDeep); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("what should I do"); err != nil {
		t.Fatal(err)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	if len(medium.Requests) != 0 {
		t.Errorf("medium picked up a half-streamed answer; that is not a failover, it is a splice")
	}
}

// ---------------------------------------------------------------------------
// The tighter context budget
// ---------------------------------------------------------------------------

// A tier with a smaller budget gets a smaller prompt — and both the model and
// the record are told, on ADR 0037's terms. A budget that trimmed silently
// would leave an answer confidently missing half the conversation.
func TestATighterTierBudgetIsDisclosedRatherThanSilent(t *testing.T) {
	instant := &ai.Fake{Response: "instant answer."}
	medium := &ai.Fake{Response: "medium answer."}
	h := newHarness(t, Options{
		Model:        "brain-model",
		HistoryTurns: 16,
		Tiers: TierSet{
			Default: ai.TierMedium,
			Bindings: map[ai.Tier]TierBinding{
				ai.TierInstant: {Provider: instant, Model: "small", HistoryTurns: 1},
				ai.TierMedium:  {Provider: medium, Model: "usual"},
			},
		},
	})
	h.askCollecting(t, "first")
	h.askCollecting(t, "second")
	h.askCollecting(t, "third")
	if _, err := h.engine.SetThinking(ai.TierInstant); err != nil {
		t.Fatal(err)
	}
	seen := h.askCollecting(t, "fourth")

	req := instant.LastRequest
	users := 0
	note := false
	for _, m := range req.Messages {
		if m.Role == ai.RoleUser {
			users++
		}
		if m.Role == ai.RoleSystem && strings.Contains(m.Content, "left out of this prompt") {
			note = true
		}
	}
	// One kept exchange plus this turn's question.
	if users != 2 {
		t.Errorf("instant was sent %d user messages, want 2 (one kept exchange + the question)", users)
	}
	if !note {
		t.Error("the trimmed prompt does not tell the model what it is missing")
	}
	dropped, ok := seen["session.timings"].Data[StageTierContextDropped]
	if !ok {
		t.Fatal("the record does not disclose the trimmed context")
	}
	if got, _ := dropped.(int); got != 2 {
		t.Errorf("%s = %v, want 2", StageTierContextDropped, dropped)
	}

	// And the tier without a budget of its own still gets everything.
	if _, err := h.engine.SetThinking(ai.TierMedium); err != nil {
		t.Fatal(err)
	}
	h.askCollecting(t, "fifth")
	if got := len(medium.LastRequest.Messages); got <= users {
		t.Errorf("medium was sent %d messages, want more than the instant tier's %d", got, users)
	}
}

// trimForTier is arithmetic and gets a table of its own, so the engine tests
// above can be about behaviour rather than about counting.
func TestTrimForTier(t *testing.T) {
	msgs := []ai.Message{
		{Role: ai.RoleSystem, Content: "prompt"},
		{Role: ai.RoleUser, Content: "q1"},
		{Role: ai.RoleAssistant, Content: "a1"},
		{Role: ai.RoleUser, Content: "q2"},
		{Role: ai.RoleAssistant, Content: "a2"},
		{Role: ai.RoleUser, Content: "now"},
	}
	for name, tc := range map[string]struct {
		budget      int
		wantLen     int
		wantDropped int
	}{
		"no budget keeps everything":       {0, 6, 0},
		"a budget above the history too":   {5, 6, 0},
		"one exchange drops the older one": {1, 4, 1},
	} {
		t.Run(name, func(t *testing.T) {
			out, dropped := trimForTier(msgs, tc.budget)
			if len(out) != tc.wantLen || dropped != tc.wantDropped {
				t.Fatalf("len=%d dropped=%d, want %d/%d", len(out), dropped, tc.wantLen, tc.wantDropped)
			}
			// The system block and the question always survive.
			if out[0].Role != ai.RoleSystem || out[len(out)-1].Content != "now" {
				t.Errorf("trim moved the frame: %+v", out)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Advisor-backed tiers
// ---------------------------------------------------------------------------

// An advisor-backed tier holds no tools, and the prompt says so. That is the
// #71 discipline applied to a tier that genuinely cannot act: without the
// line, a model that knows Jarvix can act on the desktop will say it has.
func TestAnAdvisorBackedTierIsOfferedNoTools(t *testing.T) {
	advisor := &ai.Fake{Response: "the specialist's answer."}
	medium := &ai.Fake{Response: "medium answer."}
	tool := &namedTool{name: "shell.run", result: "ok"}
	opts := Options{
		Model: "brain-model",
		Tiers: TierSet{
			Default: ai.TierMedium,
			Bindings: map[ai.Tier]TierBinding{
				ai.TierMedium: {Provider: medium, Model: "usual"},
				ai.TierDeep:   {Provider: advisor, Advisor: "claude"},
			},
		},
	}
	h := newGateHarness(t, opts, tool, tools.PolicyConfig{Default: "allow"})
	if _, err := h.engine.SetThinking(ai.TierDeep); err != nil {
		t.Fatal(err)
	}
	seen := h.askCollecting(t, "what should I do")

	req := advisor.LastRequest
	if len(req.Tools) != 0 {
		t.Errorf("an advisor tier was handed %d tools; it cannot call any", len(req.Tools))
	}
	told := false
	for _, m := range req.Messages {
		if m.Role == ai.RoleSystem && strings.Contains(m.Content, "cannot run tools") {
			told = true
		}
	}
	if !told {
		t.Error("the advisor tier was not told it cannot act on this computer")
	}
	tier, model, _, _ := tierOf(t, seen)
	if tier != "deep" || model != "advisor claude" {
		t.Errorf("record = %q/%q, want deep/advisor claude — the record names what answered",
			tier, model)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// askCollecting drives one exchange and returns every event it produced, so a
// test reads the record from the events the turn actually published rather
// than sampling state afterwards.
func (h *harness) askCollecting(t *testing.T, text string) map[string]Event {
	t.Helper()
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit(text); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	return seen
}

// mustRouter compiles the shipped intent grammar, which is what the spoken
// pins ride.
func mustRouter(t *testing.T) *intent.Router {
	t.Helper()
	r, err := intent.New(intent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
