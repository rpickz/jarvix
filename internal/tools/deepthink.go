package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rpickz/jarvix/internal/ai"
)

// This file is the model's own route up to the deep tier (issue #159, ADR
// 0062): `thinking.ask_deep`.
//
// It exists because the routing table deliberately cannot read minds. Which
// tier a turn deserves is decided from configuration, a conversation pin and
// an explicit phrase — never from a classifier's opinion of the sentence — so
// the one party that *can* tell "what time is it" from "plan my week around
// these constraints" is the model already holding the question. ADR 0016 made
// exactly that argument for advisor.ask, and this is that argument
// generalised: a tier may now be an API model rather than only a CLI, and the
// escalation is the same shape either way.
//
// The tool answers; it does not act. It carries no tools of its own, so the
// deep model cannot call anything, and its answer comes back as a tool result
// the conversation's own tier speaks. That is deliberate and is the #71
// discipline again: exactly one model in a turn holds the machine's
// capabilities, and it is the one whose tool round is open.

// DeepToolName is the registry name of the escalation tool; the permission
// gate keys off it.
const DeepToolName = "thinking.ask_deep"

// DeepThink asks the configured deep tier one question and hands its answer
// back for the assistant to use.
type DeepThink struct {
	// Provider is the deep tier's client. Required.
	Provider ai.Provider
	// Model is the model name to ask it for. Empty is correct for an
	// advisor-backed tier, which has no model name of its own.
	Model string
	// Served is what the tier actually is, for the log and for the answer's
	// framing: a model name, or "advisor claude".
	Served string
	// MaxTokens and Temperature come from the [ai] section, so a deep answer
	// is shaped by the same settings every other answer is.
	MaxTokens   int
	Temperature float64
}

// Name implements Tool.
func (d *DeepThink) Name() string { return DeepToolName }

// Description implements Tool. Written for a small model deciding whether to
// spend the wait, so the rule is stated first and the cost is stated plainly —
// the same shape as the advisor tool's description, and for the same reason.
func (d *DeepThink) Description() string {
	return "Ask the strongest model available on this computer one question and get its answer. " +
		"Use this ONLY for a request that is genuinely beyond you — deep reasoning, planning or " +
		"reviewing a lot of material, a judgement that needs to be right rather than fast. " +
		"Answer everything else yourself. It is slow: the user may wait a while in silence, so " +
		"it has to earn the wait. Pass the whole question, with enough context to answer it, " +
		"because the strong model cannot see this conversation. It cannot run tools or act on " +
		"this computer; it only answers."
}

// Schema implements Tool.
func (d *DeepThink) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "question": {
      "type": "string",
      "description": "The complete question, including any context needed to answer it."
    }
  },
  "required": ["question"]
}`)
}

// Activity implements Progressive. A deep answer is the other tool call that
// can outlast the user's patience, so it gets a label for the whole wait and a
// sentence to say once, exactly as a consultation does (ADR 0016).
func (d *DeepThink) Activity(json.RawMessage) (label, waiting string, ok bool) {
	return "Thinking deeply…",
		"I'm still working on that. This one is taking a moment.", true
}

// Execute implements Tool. It streams the deep tier's answer and returns it
// whole: the tool loop has no way to speak a tool's output as it arrives, and
// pretending otherwise is not something this seam can fix.
//
// Every way the tier can fail comes back as text for the model, on advisor.ask's
// terms: the session should end with one spoken sentence about it rather than
// an error, and nothing technical is returned, because anything returned here
// may be read aloud.
func (d *DeepThink) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Question string `json:"question"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid %s arguments: %w", DeepToolName, err)
	}
	question := strings.TrimSpace(args.Question)
	if question == "" {
		return "", fmt.Errorf("%s: empty question", DeepToolName)
	}
	if d.Provider == nil {
		return "No stronger model is configured on this computer. Tell the user that in one " +
			"short sentence, answer as best you can yourself, and do not retry.", nil
	}

	events, err := d.Provider.Chat(ctx, ai.ChatRequest{
		Model:       d.Model,
		MaxTokens:   d.MaxTokens,
		Temperature: d.Temperature,
		Messages: []ai.Message{
			{Role: ai.RoleSystem, Content: deepThinkPreamble},
			{Role: ai.RoleUser, Content: question},
		},
	})
	if err != nil {
		return deepUnavailable, nil
	}
	var b strings.Builder
	for ev := range events {
		switch ev.Type {
		case ai.EventDelta:
			b.WriteString(ev.Content)
		case ai.EventError:
			if ctx.Err() != nil {
				return "The deeper answer was interrupted.", nil
			}
			return deepUnavailable, nil
		}
	}
	if ctx.Err() != nil {
		return "The deeper answer was interrupted.", nil
	}
	answer := strings.TrimSpace(b.String())
	if answer == "" {
		return "The stronger model returned nothing. Tell the user that in one short sentence, " +
			"and do not retry.", nil
	}
	return fmt.Sprintf("The stronger model answered:\n\n%s\n\nGive the user this answer. Stay "+
		"faithful to it, shorten it if it is long, and say it as speech: no markdown, no lists, "+
		"and no file paths, URLs, or code read out verbatim.", answer), nil
}

// deepThinkPreamble frames the request for a model that is answering one
// question with no conversation behind it and no ability to act. It asks for
// the same spoken shape config.defaultSystemPrompt asks of the local model,
// for the same reason advisorPreamble does: the answer is going to be read
// aloud, and speech normalisation strips markdown after the fact where this
// stops it being written.
const deepThinkPreamble = "You are being consulted by a voice assistant. Your answer will be read " +
	"aloud, so reply in short plain prose: no markdown, no lists, no code blocks, no preamble, " +
	"and no file paths, URLs, or command lines spelled out verbatim. You cannot run tools or act " +
	"on this computer, so never say that you have. Be specific and get to the point."

// deepUnavailable is what the model is told when the deep tier could not
// answer. No status code, no URL, no key: this string can reach a speech
// engine, and a stack trace full of paths is the worst possible thing to hear.
const deepUnavailable = "The stronger model could not be reached, so nothing was asked. Tell the " +
	"user in one short sentence that you could not reach it, answer as best you can yourself, " +
	"and do not retry."
