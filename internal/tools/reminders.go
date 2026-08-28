package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/rpickz/jarvix/internal/reminders"
)

// This file is the model's hands on one-shot reminders (#141, ADR 0046):
// three verbs over one reminders.Service, for the natural phrasings the
// deterministic grammar cannot claim ("could you give me a nudge about the
// oven around six?"). The store, the parsing table, and every spoken
// sentence live in internal/reminders and internal/intent; this file owns
// only what the *model* is told — including the honest refusal it must
// relay when a time cannot be read, which is where the spoken hint for the
// unparseable comes from.

// Reminder tool names, exported so the policy's built-in tiers and the
// status surfaces can name them without guessing.
const (
	ReminderSetToolName    = "reminder.set"
	ReminderListToolName   = "reminder.list"
	ReminderCancelToolName = "reminder.cancel"
)

// RemindersOptions configure the reminder tools.
type RemindersOptions struct {
	// Service is the reminder store all three verbs act on.
	Service *reminders.Service
	// Log records operations — ids and counts only, never reminder text. Nil
	// uses slog.Default().
	Log *slog.Logger
}

// Reminders bundles the three verbs, mirroring how the memory tools share
// one Book: one Service, registered together or not at all.
type Reminders struct {
	svc *reminders.Service
	log *slog.Logger
}

// NewReminders builds the reminder tool family over one Service.
func NewReminders(opts RemindersOptions) *Reminders {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Reminders{svc: opts.Service, log: log}
}

// Tools returns the family in registration order.
func (r *Reminders) Tools() []Tool {
	return []Tool{
		&reminderSet{r},
		&reminderList{r},
		&reminderCancel{r},
	}
}

// Names lists the family's tool names, for the startup log.
func (r *Reminders) Names() []string {
	return []string{ReminderSetToolName, ReminderListToolName, ReminderCancelToolName}
}

// -------------------------------------------------------------- reminder.set

type reminderSet struct{ r *Reminders }

// Name implements Tool.
func (t *reminderSet) Name() string { return ReminderSetToolName }

// Description implements Tool.
func (t *reminderSet) Description() string {
	return "Set a one-shot spoken reminder, only when the user asks to be reminded of something. " +
		"Pass the time exactly as a short expression — \"at three\", \"at 15:00\", \"in twenty " +
		"minutes\", \"tomorrow at nine\" — and the reminder's words as text. The daemon parses " +
		"the time itself and resolves an ambiguous hour to its next occurrence; the result tells " +
		"you exactly when the reminder will fire, and you must confirm that to the user in one " +
		"sentence. If the time cannot be read, the result says so with a hint — relay it rather " +
		"than guessing a time the user did not say."
}

// Schema implements Tool.
func (t *reminderSet) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"when": {
				"type": "string",
				"description": "The moment, as a short time expression: \"at three\", \"at 15:00\", \"in twenty minutes\", \"tomorrow at nine\""
			},
			"text": {
				"type": "string",
				"description": "What to say when it fires, as the user put it"
			}
		},
		"required": ["when", "text"]
	}`)
}

// Execute implements Tool. A refusal is not an error: the reason comes back
// as the result so the model relays the hint — the daemon detects, the user
// hears exactly what could not be read.
func (t *reminderSet) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		When string `json:"when"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid reminder.set arguments: %w", err)
	}
	spoken, err := t.r.svc.Create(args.When, args.Text)
	if err != nil {
		return fmt.Sprintf("error: %v — relay this to the user in one sentence.", err), nil
	}
	t.r.log.Info("reminder set via tool", "component", "reminders")
	return spoken + " Confirm to the user in one sentence exactly when the reminder will fire.", nil
}

// ------------------------------------------------------------- reminder.list

type reminderList struct{ r *Reminders }

// Name implements Tool.
func (t *reminderList) Name() string { return ReminderListToolName }

// Description implements Tool.
func (t *reminderList) Description() string {
	return "List the user's pending one-shot reminders, soonest first, with ids — use it when " +
		"the user asks what reminders they have, or before cancelling one you need the id of. " +
		"Answer in plain words; never read ids aloud unless asked."
}

// Schema implements Tool.
func (t *reminderList) Schema() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}

// Execute implements Tool.
func (t *reminderList) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	v := t.r.svc.Snapshot()
	if len(v.Pending) == 0 {
		return "No reminders are set.", nil
	}
	lines := make([]string, 0, len(v.Pending))
	for _, p := range v.Pending {
		lines = append(lines, fmt.Sprintf("[%s] %s — %s", p.ID, p.Text, p.DueSpoken))
	}
	return strings.Join(lines, "\n"), nil
}

// ----------------------------------------------------------- reminder.cancel

type reminderCancel struct{ r *Reminders }

// Name implements Tool.
func (t *reminderCancel) Name() string { return ReminderCancelToolName }

// Description implements Tool.
func (t *reminderCancel) Description() string {
	return "Cancel a pending one-shot reminder, when the user asks. Give the reminder's id when " +
		"you know it; otherwise give enough of its words and the tool resolves them — if several " +
		"reminders match, it lists them so you can ask the user which. Confirm to the user in one " +
		"sentence what was cancelled."
}

// Schema implements Tool.
func (t *reminderCancel) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"reminder": {
				"type": "string",
				"description": "The reminder's id (from reminder.list or a set result), or words of its text"
			}
		},
		"required": ["reminder"]
	}`)
}

// Execute implements Tool. Ambiguity is not an error: the candidates come
// back as the result so the model can ask which — never guess.
func (t *reminderCancel) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Reminder string `json:"reminder"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid reminder.cancel arguments: %w", err)
	}
	spoken, err := t.r.svc.Cancel(args.Reminder)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	t.r.log.Info("reminder cancelled via tool", "component", "reminders")
	return spoken + " Confirm to the user in one sentence.", nil
}
