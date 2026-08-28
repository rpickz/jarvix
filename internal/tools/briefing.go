package tools

import (
	"context"
	"encoding/json"
	"log/slog"
)

// This file is the model's one way to reach the return briefing (#150, ADR
// 0050), for the phrasings the deterministic grammar deliberately does not
// claim — "did anything happen overnight?", "anything from the sessions?".
// The account itself is composed entirely in internal/briefing, from sources
// Jarvix already participates in; this tool relays it and adds nothing.
//
// It is a read with no arguments, which is why it is allow-tier by built-in
// default: the widest thing it can do is tell the user about work they own,
// on the machine they are sitting at, in response to their own question.

// BriefingToolName is exported so the policy's built-in tiers and the status
// surfaces can name the tool without guessing.
const BriefingToolName = "briefing.get"

// Briefer is the tool's view of the briefing service — one verb, declared
// here so the tools package does not depend on the daemon and the test can
// answer it with a fixture.
type Briefer interface {
	Briefing(ctx context.Context) (string, error)
}

// BriefingOptions configure the briefing tool.
type BriefingOptions struct {
	// Service composes the account.
	Service Briefer
	// Log records that the tool ran — never what it said. Nil uses
	// slog.Default().
	Log *slog.Logger
}

// BriefingGet is the model's return-briefing verb.
type BriefingGet struct {
	svc Briefer
	log *slog.Logger
}

// NewBriefing builds the tool over one briefing service.
func NewBriefing(opts BriefingOptions) *BriefingGet {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &BriefingGet{svc: opts.Service, log: log}
}

// Name implements Tool.
func (t *BriefingGet) Name() string { return BriefingToolName }

// Description implements Tool.
//
// The last sentence is the whole anti-confabulation contract for this path.
// The account arrives already composed and already bounded; a model that
// "helpfully" expands it would be inventing exactly the kind of claim the
// briefing's own contract exists to refuse, and it would be doing it outside
// that contract's reach.
func (t *BriefingGet) Description() string {
	return "Report what happened while the user was away — AI sessions that finished or are " +
		"waiting on them, reminders that fired or are now due, their focus threads, and what " +
		"Jarvix ran on its own. Use it when the user asks what they missed, what happened " +
		"overnight, or for a briefing. Read the result back as it is written: it is already " +
		"a complete spoken answer, every claim in it came from a record, and you must not add " +
		"anything to it, explain it, or guess at anything it does not say."
}

// Schema implements Tool.
func (t *BriefingGet) Schema() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}

// Execute implements Tool. Every disappointment the briefing can produce —
// nothing to report, a source that could not be read, a provider that would
// not word the headline — is already a sentence the assistant can speak, so
// err is reserved for a service that is not there at all.
func (t *BriefingGet) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	if t.svc == nil {
		return "The return briefing is not available on this daemon.", nil
	}
	spoken, err := t.svc.Briefing(ctx)
	if err != nil {
		return "The briefing could not be put together just now: " + err.Error(), nil
	}
	t.log.Info("return briefing given via tool", "component", "briefing")
	return spoken, nil
}
