package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/rpickz/jarvix/internal/memory"
)

// This file is the model's hands on the knowledge base (ADR 0025): three
// verbs over one memory.Book. Remember stores or supersedes, search finds
// (ADR 0037 renamed it from memory.recall, and gave it the deterministic
// ranking and the retrieval stats), forget deletes from disk. The store
// itself — the file, the caps, the hand-edit pickup — lives in
// internal/memory; this file owns what the *model* is told, which is where
// the supersede behaviour is actually made: when a remember looks like an
// existing fact, the tool refuses to guess and hands the candidates back, so
// the model decides update-versus-new deliberately and contradictions never
// accumulate by default.

// Memory tool names, exported so the policy's built-in tiers and the status
// surfaces can name them without guessing.
const (
	MemoryRememberToolName = "memory.remember"
	MemorySearchToolName   = "memory.search"
	MemoryForgetToolName   = "memory.forget"
)

// MemoryOptions configure the memory tools.
type MemoryOptions struct {
	// Book is the fact store all three verbs act on.
	Book *memory.Book
	// Source names the turn a stored fact came from (a session id). It is a
	// hook rather than a value because the tools outlive any one session;
	// nil records no source.
	Source func() string
	// Log records operations — ids and sizes only, never fact content. Nil
	// uses slog.Default().
	Log *slog.Logger
}

// Memory bundles the three verbs, mirroring how the window tools share one
// Desktop: one Book, one source hook, registered together or not at all.
type Memory struct {
	book   *memory.Book
	source func() string
	log    *slog.Logger
}

// NewMemory builds the memory tool family over one Book.
func NewMemory(opts MemoryOptions) *Memory {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Memory{book: opts.Book, source: opts.Source, log: log}
}

// Tools returns the family in registration order.
func (m *Memory) Tools() []Tool {
	return []Tool{
		&memoryRemember{m},
		&memorySearch{m},
		&memoryForget{m},
	}
}

// Names lists the family's tool names, for the startup log.
func (m *Memory) Names() []string {
	return []string{MemoryRememberToolName, MemorySearchToolName, MemoryForgetToolName}
}

// sourceTurn resolves the current turn reference, "" when unknown.
func (m *Memory) sourceTurn() string {
	if m.source == nil {
		return ""
	}
	return m.source()
}

// describeFact renders one fact for a tool result: id, dates, content, and
// the supersede trail — everything the model needs to answer "what do you
// know" and "when did that change" in words.
func describeFact(f memory.Fact) string {
	verb := "stored"
	if f.Updated.After(f.Stored) {
		verb = "updated"
	}
	line := fmt.Sprintf("[%s, %s %s] %s", f.ID, verb, f.Updated.Format("2006-01-02"), f.Content)
	for _, p := range f.Previous {
		line += fmt.Sprintf("\n  (previously %q, %s to %s)",
			p.Content, p.Stored.Format("2006-01-02"), p.Superseded.Format("2006-01-02"))
	}
	return line
}

// ---------------------------------------------------------- memory.remember

type memoryRemember struct{ m *Memory }

// Name implements Tool.
func (t *memoryRemember) Name() string { return MemoryRememberToolName }

// Description implements Tool. The supersede steering starts here: the model
// is told up front that a conflicting fact is an update, not an addition,
// and that the tool will hand back candidates when it spots one.
func (t *memoryRemember) Description() string {
	return "Store a fact in your long-term memory, only when the user explicitly asks you to " +
		"remember something. Phrase the fact as one short, self-contained statement that will " +
		"make sense on its own months from now. If new information corrects or replaces a fact " +
		"you already have (\"actually it's helios\"), you must update the existing fact rather " +
		"than add a contradicting one: the tool will show you similar stored facts — call it " +
		"again with update_id to replace one, or force_new only when it is genuinely a separate " +
		"fact. After storing, confirm to the user in one sentence what you stored."
}

// Schema implements Tool.
func (t *memoryRemember) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"content": {
				"type": "string",
				"description": "The fact, as one short self-contained statement"
			},
			"update_id": {
				"type": "string",
				"description": "Id of an existing fact this content supersedes (from a previous result or memory.search)"
			},
			"force_new": {
				"type": "boolean",
				"description": "Store as a new fact even though similar facts exist"
			}
		},
		"required": ["content"]
	}`)
}

// Execute implements Tool. A conflict is not an error: the candidates come
// back as the result so the model can decide, which is the whole supersede
// design — the daemon detects, the model chooses, the store keeps the trail.
func (t *memoryRemember) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Content  string `json:"content"`
		UpdateID string `json:"update_id"`
		ForceNew bool   `json:"force_new"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid memory.remember arguments: %w", err)
	}
	if strings.TrimSpace(args.Content) == "" {
		return "", fmt.Errorf("memory.remember: empty content")
	}

	if args.UpdateID != "" {
		fact, err := t.m.book.Update(args.UpdateID, args.Content, t.m.sourceTurn())
		if err != nil {
			return fmt.Sprintf("error: %v — use memory.search to see what is stored", err), nil
		}
		return fmt.Sprintf("Updated the remembered fact. It now reads:\n%s\n"+
			"Confirm to the user in one sentence what you now remember.", describeFact(fact)), nil
	}

	if !args.ForceNew {
		if similar := t.m.book.Similar(args.Content); len(similar) > 0 {
			lines := make([]string, 0, len(similar))
			for _, f := range similar {
				lines = append(lines, describeFact(f))
			}
			return fmt.Sprintf("Not stored yet: similar facts are already remembered.\n%s\n"+
				"If the new information replaces one of these, call memory.remember again with "+
				"update_id set to that fact's id. Only if it is genuinely a separate fact, call "+
				"again with force_new true.", strings.Join(lines, "\n")), nil
		}
	}

	fact, warning, err := t.m.book.Add(args.Content, t.m.sourceTurn())
	if err != nil {
		return fmt.Sprintf("error: the fact was not stored: %v", err), nil
	}
	result := fmt.Sprintf("Remembered:\n%s\nConfirm to the user in one sentence what you stored.",
		describeFact(fact))
	if warning != "" {
		result += "\nNote: " + warning + "."
	}
	return result, nil
}

// ------------------------------------------------------------ memory.search

// memorySearch is the retrieval half of the pin/search split (ADR 0037,
// formerly memory.recall). A query goes through the book's deterministic
// ranking and *records the retrieval* — each returned fact's stats move,
// which is what makes the Memory tab's "retrieved N times" line true. An
// omitted query is enumeration, not retrieval: it lists everything (the
// forget flow needs that) and moves no stats, so browsing can never inflate
// the usefulness signal.
type memorySearch struct{ m *Memory }

// Name implements Tool.
func (t *memorySearch) Name() string { return MemorySearchToolName }

// Description implements Tool. The honesty framing lives here as much as in
// the system prompt: the facts already injected are in front of the model,
// and everything else is only knowable by searching.
func (t *memorySearch) Description() string {
	return "Search your long-term memory of facts the user asked you to remember. Use it when " +
		"the remembered-facts block in your context says facts are not shown and the user's " +
		"question might touch one, when the user asks what you know or remember about something, " +
		"or before forgetting or updating a fact you need the id of. Do not search for facts " +
		"already shown in your context, and never claim to remember something you have neither " +
		"been shown nor found here. Answer the user in plain words — never read ids or " +
		"timestamps aloud unless asked when something changed."
}

// Schema implements Tool.
func (t *memorySearch) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {
				"type": "string",
				"description": "What to look for; omit to list every remembered fact"
			}
		}
	}`)
}

// Execute implements Tool.
func (t *memorySearch) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Query string `json:"query"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return "", fmt.Errorf("invalid memory.search arguments: %w", err)
		}
	}
	var facts []memory.Fact
	if strings.TrimSpace(args.Query) == "" {
		facts = t.m.book.List("")
		if len(facts) == 0 {
			return "Nothing is stored in memory yet.", nil
		}
	} else {
		facts = t.m.book.Search(args.Query)
		if len(facts) == 0 {
			return fmt.Sprintf("No remembered fact matches %q.", args.Query), nil
		}
	}
	lines := make([]string, 0, len(facts))
	for _, f := range facts {
		lines = append(lines, describeFact(f))
	}
	return strings.Join(lines, "\n"), nil
}

// ------------------------------------------------------------ memory.forget

// memoryForget deletes facts. Unlike its siblings it is not silently allowed
// by default (see builtinToolDefaults): deletion is the one memory operation
// that cannot be undone, so it takes the policy default and — via
// Confirmable — asks about the exact fact that is about to go.
type memoryForget struct{ m *Memory }

// Name implements Tool.
func (t *memoryForget) Name() string { return MemoryForgetToolName }

// Description implements Tool.
func (t *memoryForget) Description() string {
	return "Permanently delete a fact from your long-term memory, when the user asks you to " +
		"forget something. Give the fact's id when you know it; otherwise give a query and the " +
		"tool will resolve it — if several facts match, it lists them so you can call again " +
		"with the right id. Deletion is permanent. Confirm to the user in one sentence what " +
		"was forgotten."
}

// Schema implements Tool.
func (t *memoryForget) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {
				"type": "string",
				"description": "Id of the fact to forget (from memory.search or a remember result)"
			},
			"query": {
				"type": "string",
				"description": "Words identifying the fact, when the id is not known"
			}
		}
	}`)
}

// resolve finds the single fact a forget call is about. ok is false when the
// arguments do not pin down exactly one fact; the reason is a ready-made
// tool result explaining what to do instead.
func (t *memoryForget) resolve(input json.RawMessage) (fact memory.Fact, reason string, ok bool) {
	var args struct {
		ID    string `json:"id"`
		Query string `json:"query"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return memory.Fact{}, fmt.Sprintf("error: invalid memory.forget arguments: %v", err), false
		}
	}
	if args.ID != "" {
		for _, f := range t.m.book.List("") {
			if f.ID == args.ID {
				return f, "", true
			}
		}
		return memory.Fact{}, fmt.Sprintf("error: no remembered fact has id %q — "+
			"use memory.search to see what is stored", args.ID), false
	}
	if strings.TrimSpace(args.Query) == "" {
		return memory.Fact{}, "error: memory.forget needs an id or a query", false
	}
	matches := t.m.book.List(args.Query)
	switch len(matches) {
	case 0:
		return memory.Fact{}, fmt.Sprintf("No remembered fact matches %q; nothing was forgotten.", args.Query), false
	case 1:
		return matches[0], "", true
	}
	lines := make([]string, 0, len(matches))
	for _, f := range matches {
		lines = append(lines, describeFact(f))
	}
	return memory.Fact{}, fmt.Sprintf("Several remembered facts match %q:\n%s\n"+
		"Call memory.forget again with the id of the one to delete.",
		args.Query, strings.Join(lines, "\n")), false
}

// Confirmation implements Confirmable: the question names the fact actually
// about to be deleted — resolved daemon-side from the store — so the model
// cannot describe forgetting one thing while deleting another.
func (t *memoryForget) Confirmation(input json.RawMessage) (command, summary string, ok bool) {
	fact, _, ok := t.resolve(input)
	if !ok {
		return "", "", false
	}
	return fmt.Sprintf("forget %s: %s", fact.ID, fact.Content),
		fmt.Sprintf("I want to permanently forget that %s. Should I go ahead?", fact.Content), true
}

// Execute implements Tool.
func (t *memoryForget) Execute(_ context.Context, input json.RawMessage) (string, error) {
	fact, reason, ok := t.resolve(input)
	if !ok {
		return reason, nil
	}
	forgotten, err := t.m.book.Forget(fact.ID)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	return fmt.Sprintf("Forgotten and deleted from disk: %q. "+
		"Confirm to the user in one sentence.", forgotten.Content), nil
}
