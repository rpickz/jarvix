package memory

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// memory.search's storage half (ADR 0037): the ranking is deterministic pure
// code, the retrieval stats ride the book's own write discipline in one
// batched write per search, and a failed stats write can degrade nothing but
// the stats themselves. These tests pin each property, plus the benchmark
// the issue's sub-50ms budget is checked against.

func TestSearchRanksExactWordsOverPrefixes(t *testing.T) {
	b, clock, _ := newTestBook(t, BookOptions{})
	// Exact match oldest, so recency cannot be what puts it on top.
	exact := b.mustAdd(t, "the deploy user is called ops")
	clock.advance(time.Minute)
	prefix := b.mustAdd(t, "the deployment pipeline runs at noon")
	clock.advance(time.Minute)
	b.mustAdd(t, "the user's editor is neovim")

	got := b.Search("deploy")
	if len(got) != 2 {
		t.Fatalf("Search = %+v, want the two matching facts", got)
	}
	if got[0].ID != exact.ID || got[1].ID != prefix.ID {
		t.Errorf("order = [%s %s], want the exact-word match before the prefix match",
			got[0].ID, got[1].ID)
	}
}

// TestSearchPhraseBeatsSharedVocabulary: the whole query quoted inside a
// fact outranks a fact that merely shares its words — asserted with the
// phrase match stored *older*, so recency cannot be doing the work.
func TestSearchPhraseBeatsSharedVocabulary(t *testing.T) {
	b, clock, _ := newTestBook(t, BookOptions{})
	quoted := b.mustAdd(t, "ssh to the staging server as deploy")
	clock.advance(time.Minute)
	scattered := b.mustAdd(t, "the atlas of servers lives on the staging shelf")
	got := b.Search("staging server")
	if len(got) != 2 || got[0].ID != quoted.ID || got[1].ID != scattered.ID {
		t.Errorf("older phrase match lost to newer vocabulary match: %+v", got)
	}
}

func TestSearchTiesBreakOnRecency(t *testing.T) {
	b, clock, _ := newTestBook(t, BookOptions{})
	older := b.mustAdd(t, "project alpha uses postgres")
	clock.advance(time.Minute)
	newer := b.mustAdd(t, "project beta uses postgres")
	got := b.Search("postgres")
	if len(got) != 2 || got[0].ID != newer.ID || got[1].ID != older.ID {
		t.Errorf("equal-score order = %+v, want most recently confirmed first", got)
	}
}

// TestSearchIsDeterministic: same query, same book, same answer — ranked
// search is code, not judgement, and nothing about map iteration or the
// stats it writes may leak into the order.
func TestSearchIsDeterministic(t *testing.T) {
	b, clock, _ := newTestBook(t, BookOptions{})
	for i := 0; i < 20; i++ {
		clock.advance(time.Minute)
		b.mustAdd(t, fmt.Sprintf("service number %d runs on host alpha-%d in the rack", i, i))
	}
	first := b.Search("service host alpha rack")
	ids := func(facts []Fact) []string {
		out := make([]string, len(facts))
		for i, f := range facts {
			out[i] = f.ID
		}
		return out
	}
	for i := 0; i < 5; i++ {
		again := b.Search("service host alpha rack")
		if !reflect.DeepEqual(ids(first), ids(again)) {
			t.Fatalf("run %d returned %v, first run returned %v", i, ids(again), ids(first))
		}
	}
}

func TestSearchCapsItsResults(t *testing.T) {
	b, clock, _ := newTestBook(t, BookOptions{})
	for i := 0; i < maxSearchResults+5; i++ {
		clock.advance(time.Minute)
		b.mustAdd(t, fmt.Sprintf("machine %d lives in the basement", i))
	}
	if got := b.Search("basement"); len(got) != maxSearchResults {
		t.Errorf("Search returned %d facts, want the %d cap", len(got), maxSearchResults)
	}
}

func TestSearchNoMatchIsEmptyNotPadded(t *testing.T) {
	b, _, _ := newTestBook(t, BookOptions{})
	b.mustAdd(t, "the staging server is called atlas")
	if got := b.Search("kubernetes"); len(got) != 0 {
		t.Errorf("Search(kubernetes) = %+v, want nothing", got)
	}
	if got := b.Search("   "); len(got) != 0 {
		t.Errorf("blank search = %+v, want nothing", got)
	}
}

// TestSearchRecordsRetrievalStats is the observability contract (#104): a
// returned fact's times_retrieved increments and last_retrieved is the
// injected clock's now, persisted through the book (reopen sees it), in one
// write, with facts the search did not return untouched.
func TestSearchRecordsRetrievalStats(t *testing.T) {
	b, clock, path := newTestBook(t, BookOptions{})
	hit := b.mustAdd(t, "the staging server is called atlas")
	miss := b.mustAdd(t, "the user's editor is neovim")
	clock.advance(time.Hour)
	searchedAt := clock.now()

	got := b.Search("staging")
	if len(got) != 1 || got[0].ID != hit.ID {
		t.Fatalf("Search = %+v", got)
	}
	if got[0].TimesRetrieved != 1 || !got[0].LastRetrieved.Equal(searchedAt) {
		t.Errorf("returned fact stats = {%d, %v}, want {1, %v}",
			got[0].TimesRetrieved, got[0].LastRetrieved, searchedAt)
	}

	facts := NewBook(path, BookOptions{}, nil).List("")
	for _, f := range facts {
		switch f.ID {
		case hit.ID:
			if f.TimesRetrieved != 1 || !f.LastRetrieved.Equal(searchedAt) {
				t.Errorf("persisted stats = {%d, %v}, want {1, %v}",
					f.TimesRetrieved, f.LastRetrieved, searchedAt)
			}
		case miss.ID:
			if f.TimesRetrieved != 0 || !f.LastRetrieved.IsZero() {
				t.Errorf("unreturned fact grew stats: %+v", f)
			}
		}
	}

	// A second search keeps counting.
	clock.advance(time.Hour)
	b.Search("staging")
	if again := b.List("staging"); again[0].TimesRetrieved != 2 ||
		!again[0].LastRetrieved.Equal(clock.now()) {
		t.Errorf("second retrieval = {%d, %v}, want {2, %v}",
			again[0].TimesRetrieved, again[0].LastRetrieved, clock.now())
	}
}

// TestSearchStatsDoNotDisturbTheTrail: the stats write rides saveLocked like
// every other write, and the supersede trail must come through it verbatim.
func TestSearchStatsDoNotDisturbTheTrail(t *testing.T) {
	b, clock, path := newTestBook(t, BookOptions{})
	f := b.mustAdd(t, "the staging server is called atlas")
	clock.advance(time.Hour)
	if _, err := b.Update(f.ID, "the staging server is called helios", ""); err != nil {
		t.Fatal(err)
	}
	b.Search("staging")
	facts := NewBook(path, BookOptions{}, nil).List("")
	if len(facts) != 1 || len(facts[0].Previous) != 1 ||
		facts[0].Previous[0].Content != "the staging server is called atlas" {
		t.Errorf("trail after a stats write = %+v, want the revision intact", facts)
	}
}

// TestListDoesNotRecordRetrieval: browsing — the Memory tab, the CLI, the
// tool's empty-query enumeration — is not retrieval. Only a ranked search
// moves the stats, or "retrieved N times" would measure curiosity, not
// usefulness.
func TestListDoesNotRecordRetrieval(t *testing.T) {
	b, _, _ := newTestBook(t, BookOptions{})
	b.mustAdd(t, "the staging server is called atlas")
	b.List("")
	b.List("staging")
	if f := b.List("")[0]; f.TimesRetrieved != 0 || !f.LastRetrieved.IsZero() {
		t.Errorf("listing recorded a retrieval: %+v", f)
	}
}

// TestInjectionDoesNotRecordRetrieval: ambient presence is not retrieval
// either — a pinned fact riding every prompt with a climbing counter would
// drown the signal the counter exists for.
func TestInjectionDoesNotRecordRetrieval(t *testing.T) {
	b, _, _ := newTestBook(t, BookOptions{})
	f := b.mustAdd(t, "the staging server is called atlas")
	if _, err := b.SetPinned(f.ID, true); err != nil {
		t.Fatal(err)
	}
	b.Inject()
	b.Inject()
	if got := b.List("")[0]; got.TimesRetrieved != 0 {
		t.Errorf("injection recorded a retrieval: %+v", got)
	}
}

// TestFailedStatsWriteNeverCorruptsTheBook is the mutation check the issue
// demands: with the disk failing (through the book's write seam — the real
// filesystem cannot be made to fail hermetically, writeStore repairs the
// permission tricks), a search still answers, the book still lists every
// fact, and the unbumped file is what a reopen sees — a stats failure costs
// exactly the stats.
func TestFailedStatsWriteNeverCorruptsTheBook(t *testing.T) {
	b, _, path := newTestBook(t, BookOptions{})
	b.mustAdd(t, "the staging server is called atlas")
	b.mustAdd(t, "the user's editor is neovim")
	before := mustRead(t, path)

	b.write = func(string, []Fact, int) error {
		return errors.New("disk full")
	}
	got := b.Search("staging")
	if len(got) != 1 || !strings.Contains(got[0].Content, "atlas") {
		t.Fatalf("search failed with the stats write: %+v", got)
	}

	// The book still serves everything, uncorrupted…
	if facts := b.List(""); len(facts) != 2 {
		t.Errorf("book lists %d facts after a failed stats write, want 2", len(facts))
	}
	// …the in-memory state matches the file (unbumped — saveLocked commits
	// only on success, so the book cannot hold stats the disk does not)…
	if f := b.List("staging")[0]; f.TimesRetrieved != 0 {
		t.Errorf("in-memory stats = %d after a failed write, want 0 — book and file must agree",
			f.TimesRetrieved)
	}
	// …the file has not been touched at all…
	if after := mustRead(t, path); after != before {
		t.Errorf("store file changed across a failed write:\n%s", after)
	}
	// …and a healed disk carries on counting from the persisted state.
	b.write = writeStore
	b.Search("staging")
	facts := NewBook(path, BookOptions{}, nil).List("staging")
	if len(facts) != 1 || facts[0].TimesRetrieved != 1 {
		t.Errorf("after recovery stats = %+v, want the one successful retrieval", facts)
	}
}

// BenchmarkSearchTwoHundredFactBook is the issue's latency budget: a search
// over a full book (memory.max_facts default) must come in far under 50ms —
// and this benchmark includes the batched stats write, fsync and all, so the
// figure it reports is the whole cost of one memory.search call.
func BenchmarkSearchTwoHundredFactBook(b *testing.B) {
	dir := b.TempDir()
	clock := newTestClock()
	book := NewBook(filepath.Join(dir, "memory.toml"),
		BookOptions{Now: clock.now}, nil)
	for i := 0; i < DefaultMaxFacts; i++ {
		clock.advance(time.Minute)
		if _, _, err := book.Add(fmt.Sprintf(
			"service number %d runs on host alpha-%d in rack %d of the basement datacenter",
			i, i, i%7), ""); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := book.Search("service host alpha rack basement"); len(got) == 0 {
			b.Fatal("search found nothing")
		}
	}
}
