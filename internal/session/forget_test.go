package session

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/memory"
	"github.com/rpickz/jarvix/internal/tools"
)

// The engine half of the window's per-fact Forget (issue #92): the button is
// the gated memory.forget path — the policy decides the tier, the ask tier's
// question names the exact fact from the store (never from any caller's
// description), and only an approval deletes. The provider fake proves no
// model is consulted anywhere in it.

// newForgetHarness wires an engine whose registry carries the real memory
// tools over a real (temp-file) book, behind a real policy.
func newForgetHarness(t *testing.T, policyCfg tools.PolicyConfig) (*harness, *memory.Book) {
	t.Helper()
	book := memory.NewBook(filepath.Join(t.TempDir(), "memory.toml"),
		memory.BookOptions{MaxFacts: 50, MaxInjectedTokens: 500},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	policy, err := tools.NewPolicy(policyCfg)
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry(nil)
	registry.SetPolicy(policy)
	for _, tool := range tools.NewMemory(tools.MemoryOptions{Book: book}).Tools() {
		registry.Register(tool)
	}

	h := newHarness(t, Options{})
	h.tools = registry
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	t.Cleanup(h.cancel)
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, registry, nil, bus, nil,
		Options{Model: "m", SpeakResponses: true, HistoryTurns: 8,
			ConfirmTimeout: 5 * time.Second})
	return h, book
}

// TestForgetFactAsksNamingTheExactFact: the confirmation question and the
// verbatim command both carry the fact resolved from the store, and approval
// deletes it — with zero provider calls.
func TestForgetFactAsksNamingTheExactFact(t *testing.T) {
	h, book := newForgetHarness(t, tools.PolicyConfig{})
	fact, _, err := book.Add("the staging server is at 10.0.0.7", "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := h.engine.ForgetFact(fact.ID, fact.Content); err != nil {
		t.Fatal(err)
	}
	required := h.waitFor(t, "tool.confirmation_required")
	if required.Data["tool"] != tools.MemoryForgetToolName {
		t.Errorf("tool = %v, want the memory.forget identity", required.Data["tool"])
	}
	command, _ := required.Data["command"].(string)
	if !strings.Contains(command, fact.ID) || !strings.Contains(command, fact.Content) {
		t.Errorf("command = %q, want the exact fact named", command)
	}
	summary, _ := required.Data["summary"].(string)
	if !strings.Contains(summary, fact.Content) {
		t.Errorf("summary = %q, want the fact's content in the question", summary)
	}

	if err := h.engine.Confirm(true); err != nil {
		t.Fatal(err)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	if facts := book.List(""); len(facts) != 0 {
		t.Errorf("facts after approved forget = %v, want none", facts)
	}
	if len(h.provider.Requests) != 0 {
		t.Errorf("the provider was called %d times; forgetting is not a model turn", len(h.provider.Requests))
	}
	if h.tts.Last().Text != "Forgotten." {
		t.Errorf("spoken ack = %q, want Forgotten.", h.tts.Last().Text)
	}
}

// TestForgetFactDeclineKeepsTheFact: a decline deletes nothing and says
// Cancelled — the gate's contract, from the button as from a voice turn.
func TestForgetFactDeclineKeepsTheFact(t *testing.T) {
	h, book := newForgetHarness(t, tools.PolicyConfig{})
	fact, _, err := book.Add("the editor is neovim", "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := h.engine.ForgetFact(fact.ID, fact.Content); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "tool.confirmation_required")
	if err := h.engine.Confirm(false); err != nil {
		t.Fatal(err)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	if facts := book.List(""); len(facts) != 1 {
		t.Fatalf("facts after decline = %v, want the one kept", facts)
	}
	if h.tts.Last().Text != "Cancelled." {
		t.Errorf("spoken ack = %q, want Cancelled.", h.tts.Last().Text)
	}
}

// TestForgetFactHonoursAnAllowPolicy: a user who configured memory.forget to
// allow gets no question — the same policy override the model path honours.
func TestForgetFactHonoursAnAllowPolicy(t *testing.T) {
	h, book := newForgetHarness(t, tools.PolicyConfig{
		Tools: map[string]tools.PolicyDecision{
			tools.MemoryForgetToolName: tools.PolicyAllow,
		}})
	fact, _, err := book.Add("the editor is neovim", "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := h.engine.ForgetFact(fact.ID, fact.Content); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if _, asked := seen["tool.confirmation_required"]; asked {
		t.Error("an allow-tier forget still asked")
	}
	if _, ran := seen["tool.finished"]; !ran {
		t.Error("no tool.finished event; the deletion must be audited as an execution")
	}
	if facts := book.List(""); len(facts) != 0 {
		t.Errorf("facts = %v, want none", facts)
	}
}

// TestForgetFactUnknownIdSpeaksAnHonestFailure: a fact that vanished between
// the click and the execution ends with one honest line, never a stuck
// session.
func TestForgetFactUnknownIdSpeaksAnHonestFailure(t *testing.T) {
	h, _ := newForgetHarness(t, tools.PolicyConfig{
		Tools: map[string]tools.PolicyDecision{
			tools.MemoryForgetToolName: tools.PolicyAllow,
		}})
	if _, err := h.engine.ForgetFact("m99", "something already gone"); err != nil {
		t.Fatal(err)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if !strings.Contains(h.tts.Last().Text, "could not be forgotten") {
		t.Errorf("spoken ack = %q, want the honest failure", h.tts.Last().Text)
	}
}
