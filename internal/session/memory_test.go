package session

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/memory"
	"github.com/rpickz/jarvix/internal/tools"
)

// Engine-side knowledge base (ADR 0025): where the remembered-facts block
// lands in the message list, which paths pay for it, how it is disclosed
// afterwards — and, end to end over a real store, that a fact remembered
// through the tool survives a daemon restart.

// fakeInjector is a scripted MemoryInjector that counts consultations, so a
// test can assert which paths consult memory at all.
type fakeInjector struct {
	mu        sync.Mutex
	injection memory.Injection
	calls     int
}

func (f *fakeInjector) Inject() memory.Injection {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.injection
}

func (f *fakeInjector) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newMemoryHarness is the standard harness with a scripted injector.
func newMemoryHarness(t *testing.T, opts Options, inj memory.Injection) (*harness, *fakeInjector) {
	t.Helper()
	injector := &fakeInjector{injection: inj}
	opts.Memory = injector
	return newHarness(t, opts), injector
}

// TestMemoryReachesTheModel is the headline acceptance criterion: a stored
// fact is available to the model on a later, unrelated turn — asserted
// through the fake provider's recorded request, block placement included.
func TestMemoryReachesTheModel(t *testing.T) {
	h, injector := newMemoryHarness(t, Options{SystemPrompt: "You are Jarvix."},
		memory.Injection{
			Message: "Remembered facts: things the user asked you to remember.\n\n" +
				"- [m1, stored 2026-08-01] the staging server is called atlas",
			Total: 1,
		})

	h.ask(t, "how do I deploy to staging?")

	if injector.Calls() != 1 {
		t.Fatalf("injector consulted %d times, want once per model turn", injector.Calls())
	}
	msgs := h.provider.LastRequest.Messages
	if len(msgs) != 3 {
		t.Fatalf("messages = %d (%v), want system prompt, memory, question", len(msgs), roles(msgs))
	}
	// A system message directly after the system prompt: standing knowledge,
	// read as ground truth, ahead of any history.
	if msgs[1].Role != ai.RoleSystem || !strings.Contains(msgs[1].Content, "atlas") {
		t.Errorf("memory message = %+v, want the injected block as system", msgs[1])
	}
	if msgs[2].Role != ai.RoleUser || msgs[2].Content != "how do I deploy to staging?" {
		t.Errorf("last message = %+v, want the question", msgs[2])
	}
}

// TestMemorySitsBeforeHistoryAndContext pins the full ordering in one turn:
// system prompt, memory, history, desktop context, question. Memory is
// standing knowledge for the whole thread; the capture describes only this
// moment and stays adjacent to the question.
func TestMemorySitsBeforeHistoryAndContext(t *testing.T) {
	h, _ := newMemoryHarness(t, Options{SystemPrompt: "sys", HistoryTurns: 4},
		memory.Injection{Message: "Remembered facts: block", Total: 1})

	h.ask(t, "first question")
	h.ask(t, "second question")

	msgs := h.provider.LastRequest.Messages
	want := []struct {
		role     ai.Role
		contains string
	}{
		{ai.RoleSystem, "sys"},
		{ai.RoleSystem, "Remembered facts"},
		{ai.RoleUser, "first question"},
		{ai.RoleAssistant, ""},
		{ai.RoleUser, "second question"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("messages = %v, want %d in order", roles(msgs), len(want))
	}
	for i, w := range want {
		if msgs[i].Role != w.role || !strings.Contains(msgs[i].Content, w.contains) {
			t.Errorf("message[%d] = {%s, %q}, want {%s, ...%q...}",
				i, msgs[i].Role, msgs[i].Content, w.role, w.contains)
		}
	}
}

// TestMemoryDisabledMeansAbsent is the disabled-mode criterion, proven the
// same way disabled context sources are: nil injector, no block, no event —
// the turn is byte-identical to one before the feature existed.
func TestMemoryDisabledMeansAbsent(t *testing.T) {
	h := newHarness(t, Options{SystemPrompt: "sys"}) // Options.Memory nil

	h.ask(t, "hello")

	for _, m := range h.provider.LastRequest.Messages {
		if strings.Contains(m.Content, "Remembered facts") {
			t.Errorf("a disabled memory injected a block: %q", m.Content)
		}
	}
	if inj, _, taken := h.engine.LastMemory(); taken {
		t.Errorf("LastMemory = %+v, want nothing recorded with memory disabled", inj)
	}
}

// TestEmptyInjectionCostsNoMessage: an enabled memory with nothing to say
// must not spend a message saying it.
func TestEmptyInjectionCostsNoMessage(t *testing.T) {
	h, _ := newMemoryHarness(t, Options{SystemPrompt: "sys"}, memory.Injection{})
	h.ask(t, "hello")
	if got := len(h.provider.LastRequest.Messages); got != 2 {
		t.Errorf("messages = %d (%v), want system prompt and question only",
			got, roles(h.provider.LastRequest.Messages))
	}
	// The consultation is still recorded: "nothing was injected" is an
	// audit answer.
	if _, _, taken := h.engine.LastMemory(); !taken {
		t.Error("an empty injection was not retained for the audit")
	}
}

// TestMemoryIsNotConsultedForRoutedIntents: the deterministic router answers
// without a model, so it must not pay even the stat(2) — the same guarantee
// desktop context makes (ADR 0017/0019).
func TestMemoryIsNotConsultedForRoutedIntents(t *testing.T) {
	injector := &fakeInjector{injection: memory.Injection{Message: "Remembered facts: block"}}
	h := newIntentHarness(t, Options{Memory: injector, HistoryTurns: 4})

	h.say(t, "volume thirty") // a hit: no provider request, no consultation
	if injector.Calls() != 0 {
		t.Fatalf("a matched intent consulted memory %d times; it must pay nothing",
			injector.Calls())
	}

	h.say(t, "why is my build failing?") // a miss: reaches the model
	if injector.Calls() != 1 {
		t.Fatalf("injector consulted %d times on the model path, want 1", injector.Calls())
	}
}

// TestMemoryInjectedEventCarriesCountsOnly: the bus event is the public
// announcement, and fact content must never ride on it.
func TestMemoryInjectedEventCarriesCountsOnly(t *testing.T) {
	h, _ := newMemoryHarness(t, Options{}, memory.Injection{
		Message:   "Remembered facts:\n- [m1] the secret staging server is atlas",
		Facts:     []memory.Fact{{ID: "m1", Content: "the secret staging server is atlas"}},
		Trimmed:   2,
		Total:     3,
		EstTokens: 21,
	})

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("hello"); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	ev, ok := seen["memory.injected"]
	if !ok {
		t.Fatal("no memory.injected event was published")
	}
	if ev.Data["facts"] != 1 || ev.Data["trimmed"] != 2 || ev.Data["total"] != 3 || ev.Data["est_tokens"] != 21 {
		t.Errorf("event data = %v, want the counts", ev.Data)
	}
	payload, err := json.Marshal(ev.Data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "atlas") {
		t.Errorf("event leaked fact content: %s", payload)
	}
}

// TestLastMemoryIsTheAuditTrail: after the turn, LastMemory answers "which
// facts was the model given" with the facts themselves and the session they
// went to.
func TestLastMemoryIsTheAuditTrail(t *testing.T) {
	injection := memory.Injection{
		Message: "Remembered facts:\n- [m1] the staging server is called atlas",
		Facts:   []memory.Fact{{ID: "m1", Content: "the staging server is called atlas"}},
		Total:   1,
	}
	h, _ := newMemoryHarness(t, Options{}, injection)

	if _, _, taken := h.engine.LastMemory(); taken {
		t.Fatal("LastMemory reported an injection before any turn")
	}
	h.ask(t, "how do I deploy?")

	inj, sessionID, taken := h.engine.LastMemory()
	if !taken || sessionID == "" {
		t.Fatalf("LastMemory = (%+v, %q, %v), want the turn's injection", inj, sessionID, taken)
	}
	if len(inj.Facts) != 1 || inj.Facts[0].Content != "the staging server is called atlas" {
		t.Errorf("audited facts = %+v", inj.Facts)
	}
}

// TestRememberedFactSurvivesRestart is the persistence acceptance criterion,
// end to end: the model calls memory.remember through the registry, the turn
// completes, the engine shuts down (the drained path, #29), and a fresh Book
// over the same file — a daemon restart — still holds the fact. It holds it
// even *before* the shutdown, because the tool writes synchronously inside
// the turn: a fact is on disk before the model can claim it is remembered.
func TestRememberedFactSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.toml")
	book := memory.NewBook(path, memory.BookOptions{}, nil)

	h := newHarness(t, Options{})
	h.tools = tools.NewRegistry(nil)
	mem := tools.NewMemory(tools.MemoryOptions{Book: book, Source: func() string {
		_, id := h.engine.State()
		return id
	}})
	for _, tl := range mem.Tools() {
		h.tools.Register(tl)
	}
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	t.Cleanup(h.cancel)
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, h.tools, nil, bus, nil,
		Options{Model: "m", Memory: book})

	h.provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "memory.remember",
			Arguments: `{"content":"the staging server is called atlas"}`}},
	}
	h.provider.Response = "I'll remember that the staging server is called atlas."
	h.ask(t, "remember that the staging server is called atlas")

	// On disk already — before any shutdown, because the write is
	// synchronous inside the tool round.
	facts := book.List("")
	if len(facts) != 1 || facts[0].Content != "the staging server is called atlas" {
		t.Fatalf("facts after the turn = %+v", facts)
	}
	if facts[0].Source == "" {
		t.Error("fact has no source turn reference")
	}

	// The restart: a fresh store over the same path, as a new daemon would
	// build. (The harness cleanup also runs a full engine Shutdown drain.)
	reopened := memory.NewBook(path, memory.BookOptions{}, nil)
	inj := reopened.Inject()
	if !strings.Contains(inj.Message, "the staging server is called atlas") {
		t.Errorf("after restart the injection is missing the fact:\n%q", inj.Message)
	}
}

// TestSupersedeEndToEnd drives the whole correction flow through the engine
// and the gate: the conflicting remember comes back with candidates, the
// model re-calls with update_id, and the next turn's injection carries the
// new value — one fact, trail kept.
func TestSupersedeEndToEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.toml")
	book := memory.NewBook(path, memory.BookOptions{}, nil)

	h := newHarness(t, Options{})
	h.tools = tools.NewRegistry(nil)
	mem := tools.NewMemory(tools.MemoryOptions{Book: book})
	for _, tl := range mem.Tools() {
		h.tools.Register(tl)
	}
	// The real gate, default ask: remember must run *silently* through its
	// built-in allow tier — a confirmation prompt here would hang the test,
	// which is exactly the point of asserting through the full stack.
	policy, err := tools.NewPolicy(tools.PolicyConfig{Default: tools.PolicyAsk})
	if err != nil {
		t.Fatal(err)
	}
	h.tools.SetPolicy(policy)
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	t.Cleanup(h.cancel)
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, h.tools, nil, bus, nil,
		Options{Model: "m", Memory: book})

	if _, _, err := book.Add("the staging server is called atlas", "s1"); err != nil {
		t.Fatal(err)
	}

	// Round 0: the model tries a plain remember and is handed the conflict.
	// Round 1: it decides — update_id. Round 2: the spoken confirmation.
	h.provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "memory.remember",
			Arguments: `{"content":"the staging server is called helios"}`}},
		{{ID: "c2", Name: "memory.remember",
			Arguments: `{"content":"the staging server is called helios","update_id":"m1"}`}},
	}
	h.provider.Response = "Updated: the staging server is helios."
	h.ask(t, "actually the staging server is called helios")

	// The conflict round reached the model with the candidates.
	conflictSeen := false
	for _, req := range h.provider.Requests {
		for _, m := range req.Messages {
			if m.Role == ai.RoleTool && strings.Contains(m.Content, "Not stored yet") &&
				strings.Contains(m.Content, "m1") {
				conflictSeen = true
			}
		}
	}
	if !conflictSeen {
		t.Error("the model was never shown the conflicting fact")
	}

	facts := book.List("")
	if len(facts) != 1 {
		t.Fatalf("supersede accumulated: %+v", facts)
	}
	if facts[0].Content != "the staging server is called helios" ||
		len(facts[0].Previous) != 1 ||
		facts[0].Previous[0].Content != "the staging server is called atlas" {
		t.Errorf("fact after supersede = %+v, want helios with the atlas trail", facts[0])
	}
}
