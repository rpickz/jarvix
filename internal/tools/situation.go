package tools

import (
	"context"
	"encoding/json"
	"log/slog"
)

// This file is the model's one way to reach the situation report (#196, ADR
// 0061), for the phrasings the deterministic grammar deliberately does not
// claim — "how's the machine looking?", "is anything blocking me?", "anything
// waiting on me?". The account itself is composed entirely in
// internal/situation, from sources Jarvix already holds; this tool relays it
// and adds nothing.
//
// It is a read with no arguments, which is why it is allow-tier by built-in
// default: the widest thing it can do is tell the user about the state of the
// machine they are sitting at, in response to their own question.

// SituationToolName is exported so the policy's built-in tiers and the status
// surfaces can name the tool without guessing.
const SituationToolName = "situation.get"

// Situating is the tool's view of the situation service — one verb, declared
// here so the tools package does not depend on the daemon and the test can
// answer it with a fixture.
type Situating interface {
	Situation(ctx context.Context) (string, error)
}

// SituationOptions configure the situation tool.
type SituationOptions struct {
	// Service composes the account.
	Service Situating
	// Log records that the tool ran — never what it said. Nil uses
	// slog.Default().
	Log *slog.Logger
}

// SituationGet is the model's situation-report verb.
type SituationGet struct {
	svc Situating
	log *slog.Logger
}

// NewSituation builds the tool over one situation service.
func NewSituation(opts SituationOptions) *SituationGet {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &SituationGet{svc: opts.Service, log: log}
}

// Name implements Tool.
func (t *SituationGet) Name() string { return SituationToolName }

// Description implements Tool.
//
// The last sentence is the whole anti-confabulation contract for this path. The
// account arrives already composed and already bounded; a model that
// "helpfully" expanded it would be inventing exactly the kind of claim about
// work in flight that the report's own contract exists to refuse — and it would
// be doing it outside that contract's reach.
func (t *SituationGet) Description() string {
	return "Report the state of the whole machine right now — AI sessions waiting on the user " +
		"or still working, focus threads and timeboxes, reminders that are due, schedules " +
		"running, anything failing, and what is open on screen. Use it when the user asks " +
		"where things are, what is going on, what needs them, or for a status report. Read " +
		"the result back as it is written: it is already a complete spoken answer, every " +
		"claim in it was read from the machine, and you must not add anything to it, explain " +
		"it, or guess at anything it does not say."
}

// Schema implements Tool.
func (t *SituationGet) Schema() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}

// Execute implements Tool. Every disappointment the report can produce —
// nothing needing the user, a source that could not be read, a provider that
// would not word the headline — is already a sentence the assistant can speak,
// so err is reserved for a service that is not there at all.
func (t *SituationGet) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	if t.svc == nil {
		return "The situation report is not available on this daemon.", nil
	}
	spoken, err := t.svc.Situation(ctx)
	if err != nil {
		return "The situation report could not be put together just now: " + err.Error(), nil
	}
	t.log.Info("situation report given via tool", "component", "situation")
	return spoken, nil
}
