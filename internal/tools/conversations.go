package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/conversations"
)

// This file is the model's access to the conversation archive (issue #59):
// one read-only search verb over the same Searcher the window and CLI use.
// The tool's whole job is grounded recall — the model quotes what the
// archive actually says instead of reconstructing it from vibes — so its
// result text is engineered against confabulation twice over: passages are
// bounded (count and size) so a search can never flood the context window,
// and every no-result shape says in so many words that nothing was found
// and that inventing a recollection is not an option.
//
// Timestamps come back as spoken phrases ("last Tuesday"), never raw dates:
// the answer is read aloud, and #30's speech normalisation handles numbers,
// not calendars — the words have to be right before they reach it.

// ConversationsSearchToolName is exported so the policy's built-in tiers and
// the daemon's startup log can name it without guessing.
const ConversationsSearchToolName = "conversations.search"

// Search tool bounds, deliberately tighter than the archive's own ceilings:
// a tool result lands in the model's context, where five good passages beat
// twenty mediocre ones.
const (
	// maxSearchPassages caps how many passages one search hands the model.
	maxSearchPassages = 5
	// maxSearchPassageRunes caps each passage.
	maxSearchPassageRunes = 280
)

// ConversationSearchOptions configure the search tool.
type ConversationSearchOptions struct {
	// Searcher is the archive search seam. Required.
	Searcher conversations.Searcher
	// ActiveID reports the conversation the live head belongs to, "" when
	// none — how results distinguish "earlier in this conversation" from a
	// past one. Nil means no active conversation.
	ActiveID func() string
	// Retention reports whether conversations are being archived, so an
	// empty archive is explained truthfully. Nil means retention is on.
	Retention func() bool
	// Now is the clock the spoken dates are phrased against, injectable for
	// tests. Nil means time.Now.
	Now func() time.Time
	// Log records that searches happened — result counts only, never the
	// query or any passage: transcript content stays out of the journal.
	// Nil uses slog.Default().
	Log *slog.Logger
}

// ConversationSearch is the conversations.search tool.
type ConversationSearch struct {
	searcher  conversations.Searcher
	activeID  func() string
	retention func() bool
	now       func() time.Time
	log       *slog.Logger
}

// NewConversationSearch builds the tool.
func NewConversationSearch(opts ConversationSearchOptions) *ConversationSearch {
	t := &ConversationSearch{
		searcher:  opts.Searcher,
		activeID:  opts.ActiveID,
		retention: opts.Retention,
		now:       opts.Now,
		log:       opts.Log,
	}
	if t.now == nil {
		t.now = time.Now
	}
	if t.log == nil {
		t.log = slog.Default()
	}
	return t
}

// Name implements Tool.
func (t *ConversationSearch) Name() string { return ConversationsSearchToolName }

// Description implements Tool. The steering is local-first on purpose: the
// archive is for what *earlier* conversations said, and the current context
// already answers questions about the current one.
func (t *ConversationSearch) Description() string {
	return "Search the user's past conversations with you for something said earlier — \"what did " +
		"we decide about X?\", \"when did I mention Y?\". Use it only when the answer was said in " +
		"an earlier conversation: never search for things the current conversation already " +
		"contains, and use memory.recall, not this, for facts you were asked to remember. Quote " +
		"what it returns instead of answering from your own impression, and say when each thing " +
		"was said using the day wording the result gives you."
}

// Schema implements Tool.
func (t *ConversationSearch) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {
				"type": "string",
				"description": "What to look for, in a few words — the thing itself (\"deployment approach\"), not a full sentence."
			}
		},
		"required": ["query"]
	}`)
}

// The no-result shapes. Constants (not inline formats) because their wording
// is load-bearing: it is the only thing standing between "the tool found
// nothing" and the model inventing a recollection, so tests pin it verbatim.
const (
	// searchNoMatchResult is returned when the archive was searched and the
	// query matched nothing.
	searchNoMatchResult = "No past conversation mentions %q. You searched the whole archive and " +
		"found nothing about it, so tell the user plainly that it does not appear in your past " +
		"conversations. Do not guess, and do not invent a recollection."
	// searchEmptyArchiveResult is returned when there is nothing to search.
	searchEmptyArchiveResult = "There are no archived conversations yet, so there is nothing to " +
		"search. Tell the user plainly that you have no past conversations to look through."
	// searchRetentionOffResult is returned when retention is off and the
	// archive is empty — the honest reason there is nothing to search.
	searchRetentionOffResult = "Conversation retention is switched off, so conversations are not " +
		"being kept. Tell the user plainly that you cannot search past conversations while " +
		"retention is off; they can turn it on in settings (conversation.retention)."
	// searchRetentionOffNote is appended to results and no-match answers
	// while retention is off but older conversations still exist.
	searchRetentionOffNote = " Note: conversation retention is currently off, so recent " +
		"conversations were not archived and could not be searched."
)

// Execute implements Tool. Every way the archive can disappoint — nothing
// stored, nothing matching, retention off — comes back as a result the
// assistant can speak in one sentence; only malformed arguments are an err.
func (t *ConversationSearch) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid %s arguments: %w", ConversationsSearchToolName, err)
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return "", fmt.Errorf("%s: empty query", ConversationsSearchToolName)
	}

	matches, stats, err := t.searcher.Search(conversations.Query{
		Text:         query,
		Limit:        maxSearchPassages,
		PassageRunes: maxSearchPassageRunes,
	})
	if err != nil {
		return "", fmt.Errorf("%s: %w", ConversationsSearchToolName, err)
	}
	// The audit trail records that a search happened and how much it found —
	// never the query or a passage (the archive's contents stay off the
	// journal, which outlives the conversation).
	t.log.Info("conversation search", "component", "tools",
		"tool", ConversationsSearchToolName,
		"results", len(matches), "searched", stats.Conversations,
		"skipped", len(stats.Skipped))

	retentionOn := t.retention == nil || t.retention()
	if stats.Conversations == 0 && len(stats.Skipped) == 0 {
		if !retentionOn {
			return searchRetentionOffResult, nil
		}
		return searchEmptyArchiveResult, nil
	}
	if len(matches) == 0 {
		result := fmt.Sprintf(searchNoMatchResult, query)
		if !retentionOn {
			result += searchRetentionOffNote
		}
		return result, nil
	}
	return t.renderMatches(matches, retentionOn), nil
}

// renderMatches formats ranked passages for the model: where, when in spoken
// words, who, and the quote — with the current conversation's passages named
// as such, because "you told me earlier" and "we discussed this last week"
// are different answers.
func (t *ConversationSearch) renderMatches(matches []conversations.Match, retentionOn bool) string {
	activeID := ""
	if t.activeID != nil {
		activeID = t.activeID()
	}
	now := t.now()
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d matching passage(s), best first:\n", len(matches))
	for i, m := range matches {
		who := "the user said"
		if m.Role == "assistant" {
			who = "you said"
		}
		where := fmt.Sprintf("in past conversation %s, turn %d", m.ConversationID, m.Turn)
		if m.ConversationID == activeID && activeID != "" {
			where = fmt.Sprintf("earlier in this conversation, turn %d", m.Turn)
		}
		fmt.Fprintf(&b, "%d. [%s — %s] %s: %q\n", i+1, where, spokenWhen(now, m.Time), who, m.Passage)
	}
	b.WriteString("Quote these passages rather than paraphrasing from your own impression, and say " +
		"when each thing was said using the spoken wording given (\"last Tuesday\", \"earlier in " +
		"this conversation\") — never read conversation ids, turn numbers, or raw timestamps aloud.")
	if !retentionOn {
		b.WriteString(searchRetentionOffNote)
	}
	return b.String()
}

// weekNumberWords spell the small counts spokenWhen uses; anything beyond
// them has left "weeks ago" territory for months.
var weekNumberWords = [...]string{2: "two", 3: "three", 4: "four", 5: "five", 6: "six", 7: "seven", 8: "eight"}

// spokenWhen phrases a timestamp the way a person would say it, relative to
// now: "earlier today", "yesterday", "on Tuesday", "last Tuesday", "three
// weeks ago", "in June", "in June last year", "in June 2024". Distances are
// calendar days in now's location — what "yesterday" means to the person
// asking, not a count of elapsed hours.
func spokenWhen(now, when time.Time) string {
	local := when.In(now.Location())
	day := func(t time.Time) time.Time {
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	}
	days := int(day(now).Sub(day(local)).Hours() / 24)
	switch {
	case days <= 0:
		// A future timestamp only happens via clock skew; "earlier today" is
		// the least wrong thing to say about it.
		return "earlier today"
	case days == 1:
		return "yesterday"
	case days < 7:
		return "on " + local.Weekday().String()
	case days < 14:
		return "last " + local.Weekday().String()
	case days < 62:
		return weekNumberWords[days/7] + " weeks ago"
	case local.Year() == now.Year():
		return "in " + local.Month().String()
	case days < 366:
		return "in " + local.Month().String() + " last year"
	default:
		return fmt.Sprintf("in %s %d", local.Month(), local.Year())
	}
}
