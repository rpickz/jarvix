package approvals

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// storeAt builds a store over a temp file. Hermetic: nothing outside the
// temp dir is read or written.
func storeAt(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "approvals.toml")
	return NewStore(path, nil), path
}

// The ledger's whole job: remember when a rule was agreed to and how often it
// has fired, across a restart.
func TestLedgerSurvivesAReopen(t *testing.T) {
	s, path := storeAt(t)
	added := time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC)
	s.Added("docker stats", added)
	s.Used("docker stats", added.Add(time.Hour))
	s.Used("docker stats", added.Add(2*time.Hour))

	reopened := NewStore(path, nil)
	entries := reopened.List([]string{"docker stats"})
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Source != SourceCard {
		t.Errorf("source = %q, want %q", e.Source, SourceCard)
	}
	if !e.Added.Equal(added) {
		t.Errorf("added = %v, want %v", e.Added, added)
	}
	if e.Uses != 2 {
		t.Errorf("uses = %d, want 2", e.Uses)
	}
	if !e.LastUsed.Equal(added.Add(2 * time.Hour)) {
		t.Errorf("last used = %v", e.LastUsed)
	}
}

// The configuration file is the source of truth for membership: a pattern
// deleted with an editor must vanish from the listing, and one added with an
// editor must appear — as hand-added, with no invented date.
func TestConfigurationDecidesMembership(t *testing.T) {
	s, _ := storeAt(t)
	s.Added("docker stats", time.Now())

	entries := s.List([]string{"kubectl get pods"})
	if len(entries) != 1 || entries[0].Pattern != "kubectl get pods" {
		t.Fatalf("entries = %+v, want only the configured pattern", entries)
	}
	if entries[0].Source != SourceHand {
		t.Errorf("source = %q, want %q for a pattern the ledger never saw",
			entries[0].Source, SourceHand)
	}
	if !entries[0].Added.IsZero() {
		t.Errorf("added = %v, want no date at all for a hand-added rule", entries[0].Added)
	}
	// The dropped pattern is gone for good: re-listing it would resurrect a
	// history for a permission the user deleted.
	if got := s.List([]string{"docker stats"}); got[0].Source != SourceHand {
		t.Errorf("a pattern deleted by hand kept its card history: %+v", got[0])
	}
}

// Listing order follows the configuration, so the view and the file read the
// same way.
func TestListingFollowsConfigurationOrder(t *testing.T) {
	s, _ := storeAt(t)
	want := []string{"zzz thing", "docker stats", "kubectl get pods"}
	entries := s.List(want)
	for i, e := range entries {
		if e.Pattern != want[i] {
			t.Fatalf("entry %d = %q, want %q", i, e.Pattern, want[i])
		}
	}
}

// Whitespace is collapsed the way the classifier collapses it, so `docker
// ps` and `docker  ps` are one rule and a revocation always names what the
// gate is actually holding.
func TestPatternsAreNormalisedLikeTheClassifier(t *testing.T) {
	s, _ := storeAt(t)
	s.Added("docker  stats", time.Now())
	s.Used("docker\tstats", time.Now())
	entries := s.List([]string{" docker stats "})
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want one", entries)
	}
	if entries[0].Pattern != "docker stats" {
		t.Errorf("pattern = %q, want the collapsed form", entries[0].Pattern)
	}
	if entries[0].Uses != 1 || entries[0].Source != SourceCard {
		t.Errorf("entry = %+v, want the card history to have carried across", entries[0])
	}
}

// Forget clears the history, so a rule granted again later does not inherit
// the age of the one it replaced.
func TestForgetClearsTheHistory(t *testing.T) {
	s, _ := storeAt(t)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	s.Added("docker stats", old)
	s.Forget("docker stats")
	s.Added("docker stats", time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC))
	e := s.List([]string{"docker stats"})[0]
	if e.Added.Equal(old) {
		t.Errorf("a re-granted rule inherited the old grant's date")
	}
	if e.Uses != 0 {
		t.Errorf("uses = %d, want 0 after a forget", e.Uses)
	}
}

// The file is 0600 in a 0700 directory — the memory book's storage contract,
// and this file records which commands run unsupervised.
func TestLedgerFileIsPrivate(t *testing.T) {
	s, path := storeAt(t)
	s.Added("docker stats", time.Now())
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("ledger mode = %v, want 0600", perm)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("state dir mode = %v, want 0700", perm)
	}
}

// A corrupt ledger costs the history and nothing else: permissions do not
// live here, so this file must never become something the gate is hostage to.
func TestACorruptLedgerServesAnEmptyHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approvals.toml")
	if err := os.WriteFile(path, []byte("this is not toml ]["), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path, nil)
	entries := s.List([]string{"docker stats"})
	if len(entries) != 1 || entries[0].Source != SourceHand {
		t.Fatalf("entries = %+v, want the configured pattern with an empty history", entries)
	}
}

// Repeated writes with no change are byte-identical, so a file nobody edited
// never churns.
func TestRepeatedWritesAreStable(t *testing.T) {
	s, path := storeAt(t)
	now := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	s.Added("docker stats", now)
	s.Added("kubectl get pods", now)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s.List([]string{"docker stats", "kubectl get pods"})
	s.List([]string{"kubectl get pods", "docker stats"})
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("the ledger churned on a no-op read:\n%s\n---\n%s", first, second)
	}
}

// The store is read from the window's IPC goroutine while the bus subscriber
// bumps counts. The orderings here are explicit — every goroutine is joined
// before anything is asserted — so the test proves the same thing with and
// without -race (#156).
func TestConcurrentUseIsSafe(t *testing.T) {
	s, _ := storeAt(t)
	patterns := []string{"docker stats", "kubectl get pods"}
	now := time.Now()
	var wg sync.WaitGroup
	for _, p := range patterns {
		wg.Add(2)
		go func(pattern string) { defer wg.Done(); s.Added(pattern, now) }(p)
		go func() { defer wg.Done(); s.List(patterns) }()
	}
	wg.Wait()

	// Every write is done; the counts below are therefore deterministic.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.Used("docker stats", now) }()
	}
	wg.Wait()
	for _, e := range s.List(patterns) {
		if e.Pattern == "docker stats" && e.Uses != 10 {
			t.Errorf("uses = %d, want 10", e.Uses)
		}
	}
}
