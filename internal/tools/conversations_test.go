package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/conversations"
)

// conversations.search hands the model bounded, grounded passages from the
// archive. These tests pin the anti-confabulation contract: the no-match and
// nothing-to-search wordings verbatim, the passage caps, the "earlier in
// this conversation" distinction, and the spoken-date phrasing that keeps
// raw timestamps out of the answer.

// searchNow is the frozen clock the spoken dates are phrased against:
// a Friday, so weekday phrasing is easy to eyeball.
var searchNow = time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC)

// seededSearchTool builds the tool over a Fake archive.
func seededSearchTool(t *testing.T, retention bool, activeID string, seed func(*conversations.Fake)) *ConversationSearch {
	t.Helper()
	fake := conversations.NewFake()
	if seed != nil {
		seed(fake)
	}
	return NewConversationSearch(ConversationSearchOptions{
		Searcher:  fake,
		ActiveID:  func() string { return activeID },
		Retention: func() bool { return retention },
		Now:       func() time.Time { return searchNow },
	})
}

// execSearch runs one search and fails the test on infrastructure errors.
func execSearch(t *testing.T, tool *ConversationSearch, query string) string {
	t.Helper()
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"query":`+quoteJSON(query)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// exchange seeds one two-turn conversation ending at ts.
func exchange(f *conversations.Fake, id string, ts time.Time, question, answer string) {
	f.Seed(conversations.Meta{ID: id, Started: ts, LastActive: ts}, []conversations.Turn{
		{Role: "user", Text: question, Time: ts},
		{Role: "assistant", Text: answer, Time: ts},
	})
}

func TestConversationSearchNoMatchWordingSteersAgainstConfabulation(t *testing.T) {
	tool := seededSearchTool(t, true, "", func(f *conversations.Fake) {
		exchange(f, "conv1", searchNow.AddDate(0, 0, -3), "what about the build?", "It is green.")
	})
	got := execSearch(t, tool, "quantum sharks")
	// Pinned verbatim: this wording is the only thing standing between "no
	// results" and an invented recollection.
	want := `No past conversation mentions "quantum sharks". You searched the whole archive and ` +
		`found nothing about it, so tell the user plainly that it does not appear in your past ` +
		`conversations. Do not guess, and do not invent a recollection.`
	if got != want {
		t.Errorf("no-match result =\n%q\nwant\n%q", got, want)
	}
}

func TestConversationSearchEmptyArchiveAndRetentionOff(t *testing.T) {
	empty := seededSearchTool(t, true, "", nil)
	got := execSearch(t, empty, "anything")
	want := "There are no archived conversations yet, so there is nothing to search. Tell the " +
		"user plainly that you have no past conversations to look through."
	if got != want {
		t.Errorf("empty-archive result =\n%q\nwant\n%q", got, want)
	}

	off := seededSearchTool(t, false, "", nil)
	got = execSearch(t, off, "anything")
	want = "Conversation retention is switched off, so conversations are not being kept. Tell " +
		"the user plainly that you cannot search past conversations while retention is off; " +
		"they can turn it on in settings (conversation.retention)."
	if got != want {
		t.Errorf("retention-off result =\n%q\nwant\n%q", got, want)
	}

	// Retention off with an old archive still present: the search runs, and
	// a no-match answer says the recent gap out loud.
	offWithHistory := seededSearchTool(t, false, "", func(f *conversations.Fake) {
		exchange(f, "conv1", searchNow.AddDate(0, 0, -30), "old question", "Old answer.")
	})
	got = execSearch(t, offWithHistory, "quantum sharks")
	if !strings.Contains(got, "Do not guess, and do not invent a recollection.") ||
		!strings.Contains(got, "retention is currently off") {
		t.Errorf("retention-off no-match must keep the steering and state the gap:\n%q", got)
	}
}

func TestConversationSearchQuotesWithSpokenDates(t *testing.T) {
	tool := seededSearchTool(t, true, "conv-live", func(f *conversations.Fake) {
		exchange(f, "conv-past", searchNow.AddDate(0, 0, -10), // a Tuesday, last week
			"what did we decide about the deployment approach?", "Blue-green, remember.")
		exchange(f, "conv-live", searchNow.Add(-2*time.Hour),
			"more about the deployment approach please", "Working on it.")
	})
	got := execSearch(t, tool, "deployment approach")

	// The live head is part of the corpus, and its passages are named as
	// this conversation — "you told me earlier" and "we discussed last week"
	// are different answers.
	if !strings.Contains(got, "earlier in this conversation, turn 1 — earlier today") {
		t.Errorf("result must mark the active conversation:\n%s", got)
	}
	if !strings.Contains(got, "in past conversation conv-past, turn 1 — last Tuesday") {
		t.Errorf("result must cite the past conversation with a spoken date:\n%s", got)
	}
	if !strings.Contains(got, `the user said: "what did we decide about the deployment approach?"`) {
		t.Errorf("result must quote the passage:\n%s", got)
	}
	// Spoken citations only: no RFC 3339 timestamp anywhere in the result.
	if strings.Contains(got, "2026-08-") {
		t.Errorf("raw timestamps must not reach the model:\n%s", got)
	}
	if !strings.Contains(got, "never read conversation ids, turn numbers, or raw timestamps aloud") {
		t.Errorf("result must steer ids and timestamps out of speech:\n%s", got)
	}
}

func TestConversationSearchBoundsPassages(t *testing.T) {
	long := strings.Repeat("filler words about nothing much at all ", 30) +
		"the needle hides here" + strings.Repeat(" and yet more filler after it", 30)
	tool := seededSearchTool(t, true, "", func(f *conversations.Fake) {
		for i := 0; i < 4; i++ {
			ts := searchNow.AddDate(0, 0, -i-1)
			exchange(f, fmt.Sprintf("conv%d", i), ts, long, long)
		}
	})
	got := execSearch(t, tool, "needle hides")

	// 8 matching turns exist; the tool caps what reaches the context window.
	if n := strings.Count(got, "the needle hides here"); n != maxSearchPassages {
		t.Errorf("passages = %d, want the %d cap:\n%s", n, maxSearchPassages, got)
	}
	for _, line := range strings.Split(got, "\n") {
		if len([]rune(line)) > maxSearchPassageRunes+120 { // passage cap plus the citation prefix
			t.Errorf("a result line escaped the passage cap (%d runes): %q", len([]rune(line)), line)
		}
	}
}

func TestConversationSearchAuditLogsCountsNeverContent(t *testing.T) {
	var buf bytes.Buffer
	fake := conversations.NewFake()
	exchange(fake, "conv1", searchNow.AddDate(0, 0, -2), "the secret launch plan", "Noted.")
	tool := NewConversationSearch(ConversationSearchOptions{
		Searcher: fake,
		Now:      func() time.Time { return searchNow },
		Log:      slog.New(slog.NewTextHandler(&buf, nil)),
	})
	if _, err := tool.Execute(context.Background(),
		json.RawMessage(`{"query":"secret launch plan"}`)); err != nil {
		t.Fatal(err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "conversation search") || !strings.Contains(logged, "results=1") {
		t.Errorf("the audit trail must record that a search happened and its count:\n%s", logged)
	}
	// Neither the query nor the passage may reach the journal.
	for _, secret := range []string{"secret", "launch plan"} {
		if strings.Contains(logged, secret) {
			t.Errorf("search content leaked into the log: %q in\n%s", secret, logged)
		}
	}
}

func TestConversationSearchArgumentErrors(t *testing.T) {
	tool := seededSearchTool(t, true, "", nil)
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"  "}`)); err == nil {
		t.Error("an empty query must be an argument error, not a search")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{bad`)); err == nil {
		t.Error("malformed arguments must be an error")
	}
}

func TestConversationSearchIsAllowTier(t *testing.T) {
	// The gate treats the search like the other reads (desktop.list_windows,
	// memory.recall): silent under the default policy, with per-tool config
	// still able to override it.
	p, err := NewPolicy(PolicyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if d := p.ToolDecision(ConversationsSearchToolName); d != PolicyAllow {
		t.Errorf("default tier = %v, want allow", d)
	}
	v := p.Decide(ai.ToolCall{Name: ConversationsSearchToolName, Arguments: `{"query":"x"}`})
	if v.Decision != PolicyAllow {
		t.Errorf("Decide = %+v, want allow", v)
	}
	stricter, err := NewPolicy(PolicyConfig{Tools: map[string]PolicyDecision{
		ConversationsSearchToolName: PolicyDeny,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if d := stricter.ToolDecision(ConversationsSearchToolName); d != PolicyDeny {
		t.Errorf("per-tool override = %v, want deny", d)
	}
}

func TestSpokenWhen(t *testing.T) {
	// searchNow is Friday 21 August 2026.
	cases := []struct {
		name string
		when time.Time
		want string
	}{
		{"same day", searchNow.Add(-3 * time.Hour), "earlier today"},
		{"future skew degrades to today", searchNow.Add(2 * time.Hour), "earlier today"},
		{"yesterday", searchNow.AddDate(0, 0, -1), "yesterday"},
		{"this week", searchNow.AddDate(0, 0, -3), "on Tuesday"},
		{"six days back", searchNow.AddDate(0, 0, -6), "on Saturday"},
		{"last week", searchNow.AddDate(0, 0, -10), "last Tuesday"},
		{"thirteen days back", searchNow.AddDate(0, 0, -13), "last Saturday"},
		{"two weeks", searchNow.AddDate(0, 0, -14), "two weeks ago"},
		{"three weeks", searchNow.AddDate(0, 0, -23), "three weeks ago"},
		{"eight weeks", searchNow.AddDate(0, 0, -60), "eight weeks ago"},
		{"months this year", searchNow.AddDate(0, 0, -80), "in June"},
		{"late last year", searchNow.AddDate(0, 0, -280), "in November last year"},
		{"years back", searchNow.AddDate(-2, 0, 0), "in August 2024"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := spokenWhen(searchNow, tc.when); got != tc.want {
				t.Errorf("spokenWhen(%v) = %q, want %q", tc.when, got, tc.want)
			}
		})
	}
}
