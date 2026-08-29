package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
)

// scriptedTier is a tier provider under test control: it answers, refuses
// before streaming, or errors mid-stream. Its recording field is unexported
// behind an accessor for the reason every fake in this repo now is — a tool
// executes on a session goroutine while the test reads from its own.
type scriptedTier struct {
	answer   string
	chatErr  error
	streamer error

	mu   sync.Mutex
	last ai.ChatRequest
}

func (p *scriptedTier) Name() string { return "scripted" }

func (p *scriptedTier) Chat(_ context.Context, req ai.ChatRequest) (<-chan ai.Event, error) {
	p.mu.Lock()
	p.last = req
	p.mu.Unlock()
	if p.chatErr != nil {
		return nil, p.chatErr
	}
	ch := make(chan ai.Event, 3)
	if p.streamer != nil {
		ch <- ai.Event{Type: ai.EventError, Err: p.streamer}
	} else {
		ch <- ai.Event{Type: ai.EventDelta, Content: p.answer}
		ch <- ai.Event{Type: ai.EventDone}
	}
	close(ch)
	return ch, nil
}

func (p *scriptedTier) Last() ai.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

func deepArgs(question string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"question": question})
	return raw
}

func TestDeepThinkPassesTheQuestionAndFramesTheAnswer(t *testing.T) {
	tier := &scriptedTier{answer: "Sell the car."}
	d := &DeepThink{Provider: tier, Model: "strong", MaxTokens: 512, Temperature: 0.3}

	out, err := d.Execute(context.Background(), deepArgs("should i sell the car"))
	if err != nil {
		t.Fatal(err)
	}
	req := tier.Last()
	if req.Model != "strong" || req.MaxTokens != 512 {
		t.Errorf("request = %+v, want the tier's own model and the [ai] limits", req)
	}
	if len(req.Tools) != 0 {
		t.Error("the deep tier was handed tools; exactly one model in a turn holds them")
	}
	if !strings.Contains(out, "Sell the car.") {
		t.Errorf("result = %q, want the answer in it", out)
	}
	if !strings.Contains(out, "Give the user this answer") {
		t.Errorf("result = %q, want the model told what to do with it", out)
	}
	// The preamble must forbid claiming action: the deep model cannot act,
	// and #71 is what happens when a model that cannot act says it has.
	told := false
	for _, m := range req.Messages {
		if m.Role == ai.RoleSystem && strings.Contains(m.Content, "never say that you have") {
			told = true
		}
	}
	if !told {
		t.Error("the deep model was not told it cannot act on this computer")
	}
}

// Every way the tier can fail comes back as text for the model, on
// advisor.ask's terms: the session ends with one spoken sentence rather than
// an error, and nothing technical is returned, because anything returned here
// may be read aloud.
func TestDeepThinkFailuresAreSentencesRatherThanErrors(t *testing.T) {
	cases := map[string]*DeepThink{
		"unreachable": {Provider: &scriptedTier{chatErr: errors.New("dial tcp 10.0.0.1:443: i/o timeout")}},
		"broken":      {Provider: &scriptedTier{streamer: errors.New("HTTP 500: upstream exploded")}},
		"silent":      {Provider: &scriptedTier{answer: "   "}},
		"missing":     {},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := d.Execute(context.Background(), deepArgs("what should i do"))
			if err != nil {
				t.Fatalf("err = %v, want a sentence instead", err)
			}
			if !strings.Contains(out, "do not retry") && !strings.Contains(out, "as best you can") {
				t.Errorf("result = %q, want it to tell the model not to retry", out)
			}
			for _, leak := range []string{"dial tcp", "HTTP 500", "10.0.0.1"} {
				if strings.Contains(out, leak) {
					t.Errorf("result leaks %q, which may be read aloud: %q", leak, out)
				}
			}
		})
	}
}

func TestDeepThinkRefusesAnEmptyQuestion(t *testing.T) {
	d := &DeepThink{Provider: &scriptedTier{answer: "x"}}
	if _, err := d.Execute(context.Background(), deepArgs("   ")); err == nil {
		t.Error("an empty question was accepted")
	}
	if _, err := d.Execute(context.Background(), json.RawMessage("not json")); err == nil {
		t.Error("malformed arguments were accepted")
	}
}

// It runs silently by default, for advisor.ask's exact argument: it reads and
// replies and nothing else, which is no more authority than the model turn
// Jarvix was already making.
func TestDeepThinkRunsWithoutAsking(t *testing.T) {
	if got := builtinToolDefaults[DeepToolName]; got != PolicyAllow {
		t.Errorf("default policy = %q, want allow", got)
	}
}

// The advisor bridge as a provider: the conversation goes to the CLI as one
// question, and every non-answer is an error from Chat — the engine's signal
// that the tier produced nothing and another may still answer.
func TestAdvisorProviderRendersTheConversationAsOneQuestion(t *testing.T) {
	got := advisorQuestion([]ai.Message{
		{Role: ai.RoleSystem, Content: "you are jarvix"},
		{Role: ai.RoleUser, Content: "what is the rota"},
		{Role: ai.RoleAssistant, Content: "it is on the fridge"},
		{Role: ai.RoleUser, Content: "who is on it tomorrow"},
	})
	for _, want := range []string{"you are jarvix", "Earlier in this conversation:",
		"User: what is the rota", "Assistant: it is on the fridge"} {
		if !strings.Contains(got, want) {
			t.Errorf("question is missing %q:\n%s", want, got)
		}
	}
	// The question the user actually asked is last, so a CLI reading this can
	// find it without inferring it from position.
	if !strings.HasSuffix(got, "who is on it tomorrow") {
		t.Errorf("the question is not last:\n%s", got)
	}
}

func TestAdvisorProviderReportsAMissingCliAsUnreachable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	p := &AdvisorProvider{
		Advisor:     &Advisor{Advisors: []AdvisorSpec{{Name: "claude", Binary: "claude"}}},
		AdvisorName: "claude",
	}
	if got := p.Name(); got != "advisor:claude" {
		t.Errorf("name = %q, want it to say which bridge it is", got)
	}
	_, err := p.Chat(context.Background(), ai.ChatRequest{
		Messages: []ai.Message{{Role: ai.RoleUser, Content: "hello"}},
	})
	if err == nil {
		t.Fatal("a missing advisor CLI answered")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error = %q, want it to name the advisor", err)
	}
}
