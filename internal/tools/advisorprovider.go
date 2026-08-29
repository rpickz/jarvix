package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/rpickz/jarvix/internal/ai"
)

// AdvisorProvider presents one configured advisor as an ai.Provider, so a
// model tier can be a CLI as easily as an endpoint (issue #159, ADR 0063).
//
// This is the generalisation the tier ticket asks for. Delegation already
// existed as a *tool* the model may choose (ADR 0016); a tier is the same
// bridge reached from the other end — the user, or the router, deciding that
// this turn is answered by the strong assistant rather than deciding to
// consult it mid-answer. Both go through Advisor.Consult, so there is exactly
// one place that runs an assistant CLI, with one copy of the no-shell,
// own-process-group, scrubbed-environment discipline.
//
// It is deliberately not a streaming provider. A one-shot CLI hands its answer
// over whole, up to two minutes later; pretending otherwise by dribbling the
// finished text out word by word would fake a liveness that is not there.
type AdvisorProvider struct {
	// Advisor is the bridge. Required.
	Advisor *Advisor
	// AdvisorName is which configured advisor this provider is. Required.
	AdvisorName string
}

// Name implements ai.Provider. The prefix matters: it appears in
// assistant.started and in the daemon log, and "claude" alone would be
// indistinguishable from an endpoint that happened to be called that.
func (p *AdvisorProvider) Name() string { return "advisor:" + p.AdvisorName }

// Chat implements ai.Provider by putting the conversation to the advisor as
// one question and streaming its answer as a single delta.
//
// req.Tools is ignored, and the caller is expected to have sent none: an
// advisor cannot call anything of Jarvix's, and a tier that holds tools it
// cannot use is the shape of failure #71 was (see TierBinding.Tools in
// internal/session).
//
// Every outcome except an answer comes back as an error from Chat rather than
// as an error event, because that is the engine's signal that the tier
// produced *nothing* and another one may still answer honestly. It is the
// literal truth here: a consultation either returns its whole answer or none
// of it.
func (p *AdvisorProvider) Chat(ctx context.Context, req ai.ChatRequest) (<-chan ai.Event, error) {
	if p.Advisor == nil {
		return nil, fmt.Errorf("no advisor bridge is configured for this tier")
	}
	answer, err := p.Advisor.Consult(ctx, p.AdvisorName, advisorQuestion(req.Messages))
	if err != nil {
		return nil, err
	}
	switch answer.Outcome {
	case AdvisorMissing:
		return nil, fmt.Errorf("the %s assistant is not installed on this computer", answer.Advisor)
	case AdvisorInterrupted:
		return nil, context.Canceled
	case AdvisorTimedOut:
		return nil, fmt.Errorf("the %s assistant did not answer within %s", answer.Advisor, answer.Timeout)
	case AdvisorFailed:
		// No stderr, no exit code: this string can reach a speech engine.
		return nil, fmt.Errorf("the %s assistant could not answer", answer.Advisor)
	case AdvisorEmpty:
		return nil, fmt.Errorf("the %s assistant returned nothing", answer.Advisor)
	}
	text := answer.Text
	if answer.Truncated {
		text += "\n\n[answer truncated at " + fmt.Sprint(answer.MaxOutput) + " bytes]"
	}
	ch := make(chan ai.Event, 2)
	ch <- ai.Event{Type: ai.EventDelta, Content: text}
	ch <- ai.Event{Type: ai.EventDone}
	close(ch)
	return ch, nil
}

// advisorQuestion renders a turn's messages as the single question an advisor
// CLI takes.
//
// The shape is a transcript rather than a bare last line, because a tier
// serves a *conversation*: the same memory, the same taught vocabulary, the
// same desktop context every other tier gets (that is the shared-context
// promise of #159), and an advisor handed only the last sentence would answer
// a different question from the one the medium tier would have answered.
//
// The standing material leads, the exchange follows, and the question the user
// actually asked is last and labelled — a CLI reading this has to be able to
// find the question without inferring it from position.
func advisorQuestion(msgs []ai.Message) string {
	var standing, exchange []string
	question := ""
	for i, m := range msgs {
		switch m.Role {
		case ai.RoleSystem:
			if s := strings.TrimSpace(m.Content); s != "" {
				standing = append(standing, s)
			}
		case ai.RoleUser:
			if i == len(msgs)-1 {
				question = strings.TrimSpace(m.Content)
				continue
			}
			exchange = append(exchange, "User: "+strings.TrimSpace(m.Content))
		case ai.RoleAssistant:
			if s := strings.TrimSpace(m.Content); s != "" {
				exchange = append(exchange, "Assistant: "+s)
			}
		}
	}
	var b strings.Builder
	if len(standing) > 0 {
		b.WriteString(strings.Join(standing, "\n\n"))
		b.WriteString("\n\n")
	}
	if len(exchange) > 0 {
		b.WriteString("Earlier in this conversation:\n")
		b.WriteString(strings.Join(exchange, "\n"))
		b.WriteString("\n\n")
	}
	b.WriteString(question)
	return strings.TrimSpace(b.String())
}
