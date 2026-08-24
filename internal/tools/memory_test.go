package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/memory"
)

// testMemory builds the tool family over a real file-backed Book in a temp
// dir — the store is hermetic and synchronous, so no fake is needed — with a
// controllable clock and a fixed source turn.
func testMemory(t *testing.T) (*Memory, *memory.Book, func(time.Duration)) {
	t.Helper()
	now := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	book := memory.NewBook(filepath.Join(t.TempDir(), "memory.toml"),
		memory.BookOptions{Now: func() time.Time { return now }}, nil)
	m := NewMemory(MemoryOptions{Book: book, Source: func() string { return "s7" }})
	return m, book, func(d time.Duration) { now = now.Add(d) }
}

func execute(t *testing.T, tool Tool, args string) string {
	t.Helper()
	result, err := tool.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", tool.Name(), err)
	}
	return result
}

func memoryTool(t *testing.T, m *Memory, name string) Tool {
	t.Helper()
	for _, tool := range m.Tools() {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("no tool named %s", name)
	return nil
}

// TestRememberStoresWithTimestampAndSource is the first acceptance
// criterion: the fact lands on disk with a timestamp and the turn that said
// it, and the result tells the model to confirm in one sentence.
func TestRememberStoresWithTimestampAndSource(t *testing.T) {
	m, book, _ := testMemory(t)
	remember := memoryTool(t, m, MemoryRememberToolName)

	result := execute(t, remember, `{"content":"the staging server is called atlas"}`)
	if !strings.Contains(result, "Remembered") || !strings.Contains(result, "one sentence") {
		t.Errorf("result = %q, want a stored confirmation with the one-sentence instruction", result)
	}

	facts := book.List("")
	if len(facts) != 1 {
		t.Fatalf("facts = %+v, want one", facts)
	}
	f := facts[0]
	if f.Content != "the staging server is called atlas" || f.Source != "s7" || f.Stored.IsZero() {
		t.Errorf("fact = %+v, want content, source turn s7, and a timestamp", f)
	}
}

// TestRememberSimilarFactHandsBackCandidates is the supersede steering: the
// conflicting remember stores nothing and returns the matching facts with
// their ids, so the model — not a heuristic — decides update-versus-new.
func TestRememberSimilarFactHandsBackCandidates(t *testing.T) {
	m, book, _ := testMemory(t)
	remember := memoryTool(t, m, MemoryRememberToolName)
	execute(t, remember, `{"content":"the staging server is called atlas"}`)

	result := execute(t, remember, `{"content":"the staging server is called helios"}`)
	for _, want := range []string{"Not stored yet", "m1", "atlas", "update_id", "force_new"} {
		if !strings.Contains(result, want) {
			t.Errorf("conflict result missing %q:\n%s", want, result)
		}
	}
	// Nothing was written: still one fact, still the old value.
	facts := book.List("")
	if len(facts) != 1 || facts[0].Content != "the staging server is called atlas" {
		t.Errorf("store changed by an undecided remember: %+v", facts)
	}
}

// TestRememberWithUpdateIDSupersedes completes the flow the previous test
// starts: update replaces the value, keeps the id, and keeps the trail —
// "when did that change" is answerable from the result alone.
func TestRememberWithUpdateIDSupersedes(t *testing.T) {
	m, book, advance := testMemory(t)
	remember := memoryTool(t, m, MemoryRememberToolName)
	execute(t, remember, `{"content":"the staging server is called atlas"}`)
	advance(24 * time.Hour)

	result := execute(t, remember, `{"content":"the staging server is called helios","update_id":"m1"}`)
	for _, want := range []string{"Updated", "helios", `previously "the staging server is called atlas"`,
		"2026-08-01", "2026-08-02"} {
		if !strings.Contains(result, want) {
			t.Errorf("update result missing %q:\n%s", want, result)
		}
	}
	facts := book.List("")
	if len(facts) != 1 {
		t.Fatalf("update accumulated: %+v", facts)
	}
	if facts[0].Content != "the staging server is called helios" || len(facts[0].Previous) != 1 {
		t.Errorf("fact after update = %+v", facts[0])
	}
}

func TestRememberForceNewStoresBesideSimilar(t *testing.T) {
	m, book, _ := testMemory(t)
	remember := memoryTool(t, m, MemoryRememberToolName)
	execute(t, remember, `{"content":"the staging server is called atlas"}`)
	execute(t, remember, `{"content":"the staging database server is called argon","force_new":true}`)
	if facts := book.List(""); len(facts) != 2 {
		t.Errorf("force_new stored %d facts, want 2", len(facts))
	}
}

func TestRememberUnknownUpdateIDPointsAtSearch(t *testing.T) {
	m, book, _ := testMemory(t)
	remember := memoryTool(t, m, MemoryRememberToolName)
	result := execute(t, remember, `{"content":"something","update_id":"m9"}`)
	if !strings.Contains(result, "error") || !strings.Contains(result, "memory.search") {
		t.Errorf("result = %q, want an error naming memory.search", result)
	}
	if facts := book.List(""); len(facts) != 0 {
		t.Errorf("a failed update wrote: %+v", facts)
	}
}

func TestRememberNearCapWarns(t *testing.T) {
	now := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	book := memory.NewBook(filepath.Join(t.TempDir(), "memory.toml"),
		memory.BookOptions{MaxFacts: 10, Now: func() time.Time { return now }}, nil)
	m := NewMemory(MemoryOptions{Book: book})
	remember := memoryTool(t, m, MemoryRememberToolName)

	// force_new keeps the similarity detour out of a test that is about the
	// cap; the contents share words on purpose to prove it.
	for i, s := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"} {
		result := execute(t, remember, `{"content":"subject `+s+` entirely unrelated","force_new":true}`)
		if strings.Contains(result, "nearly full") {
			t.Errorf("warned at fact %d, below the nine-tenths mark", i+1)
		}
	}
	result := execute(t, remember, `{"content":"subject india entirely unrelated","force_new":true}`)
	if !strings.Contains(result, "nearly full") || !strings.Contains(result, "9 of 10") {
		t.Errorf("ninth fact result = %q, want the near-cap warning", result)
	}
	execute(t, remember, `{"content":"subject juliet entirely unrelated","force_new":true}`)
	result = execute(t, remember, `{"content":"subject kilo entirely unrelated","force_new":true}`)
	if !strings.Contains(result, "not stored") || !strings.Contains(result, "full") {
		t.Errorf("over-cap result = %q, want a refusal that says so", result)
	}
}

func TestSearchListsInWordsWithTrail(t *testing.T) {
	m, _, advance := testMemory(t)
	remember := memoryTool(t, m, MemoryRememberToolName)
	search := memoryTool(t, m, MemorySearchToolName)
	execute(t, remember, `{"content":"the staging server is called atlas"}`)
	advance(time.Hour)
	execute(t, remember, `{"content":"the staging server is called helios","update_id":"m1"}`)
	execute(t, remember, `{"content":"the user's editor is neovim","force_new":true}`)

	result := execute(t, search, `{"query":"staging server"}`)
	for _, want := range []string{"m1", "helios", "previously", "atlas"} {
		if !strings.Contains(result, want) {
			t.Errorf("search missing %q:\n%s", want, result)
		}
	}
	if strings.Contains(result, "neovim") {
		t.Errorf("search leaked an unrelated fact:\n%s", result)
	}

	// Empty query lists everything; empty store says so plainly.
	if all := execute(t, search, `{}`); !strings.Contains(all, "neovim") {
		t.Errorf("search of everything missing a fact:\n%s", all)
	}
	if none := execute(t, search, `{"query":"kubernetes"}`); !strings.Contains(none, "No remembered fact") {
		t.Errorf("no-match search = %q", none)
	}
}

// TestSearchRecordsRetrievalOnlyForQueries pins the tool half of the stats
// contract (#104): a queried search moves each returned fact's counter, an
// empty-query enumeration (the forget flow's listing) moves nothing —
// browsing must never inflate the usefulness signal.
func TestSearchRecordsRetrievalOnlyForQueries(t *testing.T) {
	m, book, _ := testMemory(t)
	execute(t, memoryTool(t, m, MemoryRememberToolName), `{"content":"the staging server is called atlas"}`)
	search := memoryTool(t, m, MemorySearchToolName)

	execute(t, search, `{}`)
	if f := book.List("")[0]; f.TimesRetrieved != 0 {
		t.Errorf("enumeration recorded a retrieval: %+v", f)
	}
	execute(t, search, `{"query":"staging"}`)
	f := book.List("")[0]
	if f.TimesRetrieved != 1 || f.LastRetrieved.IsZero() {
		t.Errorf("queried search stats = {%d, %v}, want the retrieval recorded",
			f.TimesRetrieved, f.LastRetrieved)
	}
}

// TestSearchRanksBestMatchFirst: the tool answers through the book's ranked
// search, not the loose listing — the exact-word fact leads even though the
// vocabulary-sharing one was confirmed later.
func TestSearchRanksBestMatchFirst(t *testing.T) {
	m, _, advance := testMemory(t)
	remember := memoryTool(t, m, MemoryRememberToolName)
	execute(t, remember, `{"content":"ssh to the staging server as deploy"}`)
	advance(time.Hour)
	execute(t, remember, `{"content":"the atlas of servers lives on the staging shelf","force_new":true}`)

	result := execute(t, memoryTool(t, m, MemorySearchToolName), `{"query":"staging server"}`)
	first := strings.SplitN(result, "\n", 2)[0]
	if !strings.Contains(first, "ssh to the staging server") {
		t.Errorf("first result = %q, want the phrase match ranked on top:\n%s", first, result)
	}
}

func TestSearchOnEmptyStore(t *testing.T) {
	m, _, _ := testMemory(t)
	search := memoryTool(t, m, MemorySearchToolName)
	if result := execute(t, search, `{}`); !strings.Contains(result, "Nothing is stored") {
		t.Errorf("empty search = %q", result)
	}
}

func TestForgetByIDDeletes(t *testing.T) {
	m, book, _ := testMemory(t)
	execute(t, memoryTool(t, m, MemoryRememberToolName), `{"content":"the staging server is called atlas"}`)
	result := execute(t, memoryTool(t, m, MemoryForgetToolName), `{"id":"m1"}`)
	if !strings.Contains(result, "Forgotten") || !strings.Contains(result, "deleted from disk") {
		t.Errorf("forget result = %q", result)
	}
	if facts := book.List(""); len(facts) != 0 {
		t.Errorf("fact survived forget: %+v", facts)
	}
}

func TestForgetByUniqueQueryDeletes(t *testing.T) {
	m, book, _ := testMemory(t)
	remember := memoryTool(t, m, MemoryRememberToolName)
	execute(t, remember, `{"content":"the staging server is called atlas"}`)
	execute(t, remember, `{"content":"the user's editor is neovim","force_new":true}`)
	execute(t, memoryTool(t, m, MemoryForgetToolName), `{"query":"editor"}`)
	facts := book.List("")
	if len(facts) != 1 || !strings.Contains(facts[0].Content, "atlas") {
		t.Errorf("facts after forget = %+v, want only the server fact", facts)
	}
}

// TestForgetAmbiguousQueryRefusesToGuess: two matches delete nothing — the
// candidates come back and the model must name an id.
func TestForgetAmbiguousQueryRefusesToGuess(t *testing.T) {
	m, book, _ := testMemory(t)
	remember := memoryTool(t, m, MemoryRememberToolName)
	execute(t, remember, `{"content":"the staging server is called atlas"}`)
	execute(t, remember, `{"content":"the staging server certificate renews in march","force_new":true}`)

	result := execute(t, memoryTool(t, m, MemoryForgetToolName), `{"query":"staging server"}`)
	for _, want := range []string{"Several", "m1", "m2", "id"} {
		if !strings.Contains(result, want) {
			t.Errorf("ambiguous forget missing %q:\n%s", want, result)
		}
	}
	if facts := book.List(""); len(facts) != 2 {
		t.Errorf("an ambiguous forget deleted: %+v", facts)
	}
}

func TestForgetNothingMatches(t *testing.T) {
	m, _, _ := testMemory(t)
	result := execute(t, memoryTool(t, m, MemoryForgetToolName), `{"query":"kubernetes"}`)
	if !strings.Contains(result, "nothing was forgotten") {
		t.Errorf("no-match forget = %q", result)
	}
}

// TestForgetConfirmationNamesTheFact: the ask-tier question is generated
// from the store's own view of the fact about to go, not from anything the
// model claimed (the Confirmable contract).
func TestForgetConfirmationNamesTheFact(t *testing.T) {
	m, _, _ := testMemory(t)
	execute(t, memoryTool(t, m, MemoryRememberToolName), `{"content":"the staging server is called atlas"}`)
	forget := memoryTool(t, m, MemoryForgetToolName)
	c, ok := forget.(Confirmable)
	if !ok {
		t.Fatal("memory.forget must be Confirmable — its confirmation has to name the fact")
	}
	command, summary, found := c.Confirmation(json.RawMessage(`{"id":"m1"}`))
	if !found {
		t.Fatal("no confirmation for a resolvable forget")
	}
	if !strings.Contains(command, "m1") || !strings.Contains(command, "atlas") {
		t.Errorf("command = %q, want the id and the content", command)
	}
	if !strings.Contains(summary, "forget that the staging server is called atlas") {
		t.Errorf("summary = %q, want the fact in the question", summary)
	}
	// Unresolvable calls fall back to the generic gate question.
	if _, _, found := c.Confirmation(json.RawMessage(`{"query":"kubernetes"}`)); found {
		t.Error("an unresolvable forget offered a confirmation")
	}
}

// TestMemoryPolicyTiers pins the gate: remember and search run silently
// (bounded blast radius, spoken confirmation, reversible), forget takes the
// policy default because deletion is the one irreversible verb.
func TestMemoryPolicyTiers(t *testing.T) {
	policy, err := NewPolicy(PolicyConfig{Default: PolicyAsk})
	if err != nil {
		t.Fatal(err)
	}
	if got := policy.ToolDecision(MemoryRememberToolName); got != PolicyAllow {
		t.Errorf("memory.remember tier = %s, want allow", got)
	}
	if got := policy.ToolDecision(MemorySearchToolName); got != PolicyAllow {
		t.Errorf("memory.search tier = %s, want allow", got)
	}
	if got := policy.ToolDecision(MemoryForgetToolName); got != PolicyAsk {
		t.Errorf("memory.forget tier = %s, want the ask default", got)
	}
	// A per-tool entry still overrides the built-ins, both directions.
	strict, err := NewPolicy(PolicyConfig{Default: PolicyAsk,
		Tools: map[string]PolicyDecision{
			MemoryRememberToolName: PolicyAsk,
			MemoryForgetToolName:   PolicyAllow,
		}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strict.ToolDecision(MemoryRememberToolName); got != PolicyAsk {
		t.Errorf("configured memory.remember tier = %s, want ask", got)
	}
	if got := strict.ToolDecision(MemoryForgetToolName); got != PolicyAllow {
		t.Errorf("configured memory.forget tier = %s, want allow", got)
	}
}
