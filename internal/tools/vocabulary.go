package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/rpickz/jarvix/internal/vocabulary"
)

// This file is the model's hands on the taught vocabulary (issue #129): two
// verbs over one vocabulary.Store. Teach stores or supersedes — the natural-
// phrasing companion to the router's deterministic "when I say X I mean Y" —
// and forget deletes from disk. The store owns the file, the caps, and the
// supersede semantics (the phrase is the entry's identity, so re-teaching
// can never accumulate a silent second entry); this file owns what the
// *model* is told, which is where the explicit-teach trust boundary is
// stated: nothing enters the vocabulary unless the user taught it in so many
// words.

// Vocabulary tool names, exported so the policy's built-in tiers and the
// status surfaces can name them without guessing.
const (
	VocabularyTeachToolName  = "vocabulary.teach"
	VocabularyForgetToolName = "vocabulary.forget"
)

// VocabularyOptions configure the vocabulary tools.
type VocabularyOptions struct {
	// Store is the vocabulary both verbs act on.
	Store *vocabulary.Store
	// Source names the turn a taught entry came from (a session id). A hook
	// rather than a value because the tools outlive any one session; nil
	// records no source.
	Source func() string
	// Log records operations — ids and sizes only, never phrases or
	// meanings. Nil uses slog.Default().
	Log *slog.Logger
}

// Vocabulary bundles the two verbs, mirroring tools.Memory: one Store, one
// source hook, registered together or not at all.
type Vocabulary struct {
	store  *vocabulary.Store
	source func() string
	log    *slog.Logger
}

// NewVocabulary builds the vocabulary tool family over one Store.
func NewVocabulary(opts VocabularyOptions) *Vocabulary {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Vocabulary{store: opts.Store, source: opts.Source, log: log}
}

// Tools returns the family in registration order.
func (v *Vocabulary) Tools() []Tool {
	return []Tool{
		&vocabularyTeach{v},
		&vocabularyForget{v},
	}
}

// Names lists the family's tool names, for the startup log.
func (v *Vocabulary) Names() []string {
	return []string{VocabularyTeachToolName, VocabularyForgetToolName}
}

// sourceTurn resolves the current turn reference, "" when unknown.
func (v *Vocabulary) sourceTurn() string {
	if v.source == nil {
		return ""
	}
	return v.source()
}

// describeEntry renders one entry for a tool result: id, dates, phrase,
// meaning, note, flag, and the supersede trail — everything the model needs
// to answer "what have I taught you" and "what did quid used to mean".
func describeEntry(e vocabulary.Entry) string {
	verb := "taught"
	if e.Updated.After(e.Taught) {
		verb = "re-taught"
	}
	line := fmt.Sprintf("[%s, %s %s] %q means: %s", e.ID, verb,
		e.Updated.Format("2006-01-02"), e.Phrase, e.Meaning)
	if e.Note != "" {
		line += " (" + e.Note + ")"
	}
	if e.HardToHear {
		line += " [listened for]"
	}
	for _, p := range e.Previous {
		line += fmt.Sprintf("\n  (previously meant %q, %s to %s)",
			p.Meaning, p.Taught.Format("2006-01-02"), p.Superseded.Format("2006-01-02"))
	}
	return line
}

// ---------------------------------------------------------- vocabulary.teach

// vocabularyTeach stores one phrase → meaning. Built-in allow, on
// memory.remember's exact argument: the blast radius is bounded by
// construction (one write into the user's own 0600 vocabulary file, spoken
// confirmation, undone with one forget), and the user just SAID the teaching
// out loud — asking "should I learn the word you told me to learn?" would be
// confirming their own sentence.
type vocabularyTeach struct{ v *Vocabulary }

// Name implements Tool.
func (t *vocabularyTeach) Name() string { return VocabularyTeachToolName }

// Description implements Tool. The trust boundary is stated here — teaching
// is the user's explicit word, never the model's inference — because with
// zero entries no vocabulary block rides the prompt (the byte-identity
// contract of #129), so this description is the only place the rule can
// live.
func (t *vocabularyTeach) Description() string {
	return "Save a word or phrase the user explicitly teaches you — \"when I say quid I mean " +
		"pounds\", \"quid means pounds, remember that\" — as phrase and meaning, with an optional " +
		"short note for context. Only when the user explicitly teaches: never record a phrase " +
		"because you inferred what they meant. Teaching an already-taught phrase updates its " +
		"meaning and keeps the old one on record, so correct freely. Set hard_to_hear true when " +
		"the user says the word keeps being misheard (\"listen for the word quid\") — it then " +
		"biases speech recognition too. After teaching, confirm in one short sentence what the " +
		"phrase now means; do not start using the phrase yourself."
}

// Schema implements Tool.
func (t *vocabularyTeach) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"phrase": {
				"type": "string",
				"description": "The word or phrase, exactly as the user says it"
			},
			"meaning": {
				"type": "string",
				"description": "What the phrase means when the user says it"
			},
			"note": {
				"type": "string",
				"description": "Optional short context, e.g. \"UK money slang\""
			},
			"hard_to_hear": {
				"type": "boolean",
				"description": "Also bias speech recognition toward the phrase (only when the user says it is misheard)"
			}
		},
		"required": ["phrase", "meaning"]
	}`)
}

// Execute implements Tool. Refusals come back as results, not errors, so the
// model can relay the real problem — and a teach that landed while the
// hard-to-hear flag was refused reports both halves honestly: the entry is
// taught, the flag is not, and pretending otherwise in either direction
// would be the silent-cap failure ADR 0037 forbids.
func (t *vocabularyTeach) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Phrase     string `json:"phrase"`
		Meaning    string `json:"meaning"`
		Note       string `json:"note"`
		HardToHear bool   `json:"hard_to_hear"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid vocabulary.teach arguments: %w", err)
	}

	entry, warning, err := t.v.store.Teach(args.Phrase, args.Meaning, args.Note, t.v.sourceTurn())
	if err != nil {
		return fmt.Sprintf("error: the phrase was not saved: %v", err), nil
	}
	result := "Taught:\n" + describeEntry(entry)
	if args.HardToHear && !entry.HardToHear {
		flagged, biasWarning, flagErr := t.v.store.SetHardToHear(entry.ID, true)
		switch {
		case flagErr != nil:
			result += fmt.Sprintf("\nThe phrase is taught, but it will NOT be listened for: %v. "+
				"Tell the user plainly.", flagErr)
		default:
			entry = flagged
			result = "Taught:\n" + describeEntry(entry)
			if biasWarning != "" {
				result += "\nNote: " + biasWarning + "."
			}
		}
	}
	result += "\nConfirm to the user in one short sentence what the phrase now means."
	if warning != "" {
		result += "\nNote: " + warning + "."
	}
	return result, nil
}

// --------------------------------------------------------- vocabulary.forget

// vocabularyForget deletes entries. Unlike teach it is not built-in allow:
// deleting an entry destroys its taught history — the supersede trail and
// its dates cannot be reconstructed by re-teaching — so it follows ADR
// 0025's reversibility split exactly as memory.forget does: the policy
// default (ask) applies, and Confirmable names the exact phrase about to go.
// The call is recorded in the ADR: consistency with memory's forget beats
// the "the user can just re-teach it" shortcut, because two forget verbs
// with two stances would make the gate's rule unguessable.
type vocabularyForget struct{ v *Vocabulary }

// Name implements Tool.
func (t *vocabularyForget) Name() string { return VocabularyForgetToolName }

// Description implements Tool.
func (t *vocabularyForget) Description() string {
	return "Permanently delete a taught word or phrase from your vocabulary, when the user asks " +
		"you to forget it. Give the entry's id when you know it (the taught-vocabulary block " +
		"carries ids); otherwise give the phrase and the tool resolves it. Deletion is permanent " +
		"— the phrase's history goes with it. Confirm to the user in one sentence what was " +
		"forgotten."
}

// Schema implements Tool.
func (t *vocabularyForget) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {
				"type": "string",
				"description": "Id of the entry to forget (from the taught-vocabulary block or a teach result)"
			},
			"phrase": {
				"type": "string",
				"description": "The taught phrase, when the id is not known"
			}
		}
	}`)
}

// resolve finds the single entry a forget call is about. ok is false when
// the arguments do not pin down exactly one entry; the reason is a
// ready-made tool result explaining what to do instead.
func (t *vocabularyForget) resolve(input json.RawMessage) (entry vocabulary.Entry, reason string, ok bool) {
	var args struct {
		ID     string `json:"id"`
		Phrase string `json:"phrase"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return vocabulary.Entry{}, fmt.Sprintf("error: invalid vocabulary.forget arguments: %v", err), false
		}
	}
	if args.ID != "" {
		for _, e := range t.v.store.List("") {
			if e.ID == args.ID {
				return e, "", true
			}
		}
		return vocabulary.Entry{}, fmt.Sprintf("error: no taught entry has id %q — "+
			"the taught-vocabulary block lists what is stored", args.ID), false
	}
	if strings.TrimSpace(args.Phrase) == "" {
		return vocabulary.Entry{}, "error: vocabulary.forget needs an id or a phrase", false
	}
	if e, found := t.v.store.ByPhrase(args.Phrase); found {
		return e, "", true
	}
	return vocabulary.Entry{}, fmt.Sprintf("No taught phrase matches %q; nothing was forgotten.",
		strings.TrimSpace(args.Phrase)), false
}

// Confirmation implements Confirmable: the question names the entry actually
// about to be deleted — resolved daemon-side from the store — so the model
// cannot describe forgetting one phrase while deleting another (the
// memory.forget property, held here too).
func (t *vocabularyForget) Confirmation(input json.RawMessage) (command, summary string, ok bool) {
	entry, _, ok := t.resolve(input)
	if !ok {
		return "", "", false
	}
	return fmt.Sprintf("forget %s: %q means %s", entry.ID, entry.Phrase, entry.Meaning),
		fmt.Sprintf("I want to permanently forget that %s means %s. Should I go ahead?",
			entry.Phrase, entry.Meaning), true
}

// Execute implements Tool.
func (t *vocabularyForget) Execute(_ context.Context, input json.RawMessage) (string, error) {
	entry, reason, ok := t.resolve(input)
	if !ok {
		return reason, nil
	}
	forgotten, err := t.v.store.Forget(entry.ID)
	if err != nil {
		if errors.Is(err, vocabulary.ErrUnknownID) {
			// Deleted between resolve and here (a hand-edit, a racing forget):
			// the outcome the user asked for already holds.
			return fmt.Sprintf("The phrase %q was already gone. Confirm to the user in one sentence.",
				entry.Phrase), nil
		}
		return fmt.Sprintf("error: %v", err), nil
	}
	return fmt.Sprintf("Forgotten and deleted from disk: %q. "+
		"Confirm to the user in one sentence.", forgotten.Phrase), nil
}
