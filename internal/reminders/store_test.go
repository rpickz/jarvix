package reminders

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The storage discipline (ADR 0025 over reminders): hand-edits land on the
// next operation, corruption degrades and is never overwritten, ids ratchet,
// and the history stays capped.

func testLogger(*testing.T) *slog.Logger { return slog.New(slog.DiscardHandler) }

// fixedClock is a hand-set clock safe to read from any goroutine.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFixedClock() *fixedClock {
	// A Wednesday, 13:00 UTC — afternoon, so "at three" resolves same-day.
	return &fixedClock{t: time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)}
}

func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fixedClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestService(t *testing.T, path string) (*Service, *fixedClock) {
	t.Helper()
	clock := newFixedClock()
	s := NewService(path, Options{Now: clock.now}, testLogger(t))
	return s, clock
}

func TestStoreRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	s, _ := newTestService(t, path)
	if _, err := s.Create("at three", "call the pharmacy"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("in twenty minutes", "stretch"); err != nil {
		t.Fatal(err)
	}

	// A second service over the same file sees both, soonest first.
	s2, _ := newTestService(t, path)
	v := s2.Snapshot()
	if len(v.Pending) != 2 || v.Pending[0].Text != "stretch" || v.Pending[1].Text != "call the pharmacy" {
		t.Fatalf("snapshot = %+v", v.Pending)
	}
	if v.Pending[1].DueSpoken != "at three this afternoon" {
		t.Errorf("due_spoken = %q", v.Pending[1].DueSpoken)
	}

	// The file is private and self-documenting.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("store mode = %v, want 0600", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "# Jarvix's one-shot reminders") {
		t.Errorf("store header missing:\n%s", string(data))
	}
}

func TestHandEditLandsOnTheNextOperation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	s, _ := newTestService(t, path)
	if _, err := s.Create("at three", "call the pharmacy"); err != nil {
		t.Fatal(err)
	}
	// Edit by hand: change the text. mtime granularity can be coarse, so
	// the size-change half of the detector is what this rides.
	data, _ := os.ReadFile(path)
	edited := strings.Replace(string(data), "call the pharmacy", "call the pharmacy about the meds", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.ListSpoken(); !strings.Contains(got, "about the meds") {
		t.Errorf("hand edit not picked up: %q", got)
	}
}

func TestCorruptStoreDegradesAndIsMovedAsideNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	if err := os.WriteFile(path, []byte("version = 1\nthis is not toml ["), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := newTestService(t, path)
	if got := s.ListSpoken(); got != "No reminders set." {
		t.Fatalf("corrupt store spoke %q", got)
	}
	// The first write moves the unparseable file aside.
	if _, err := s.Create("at three", "call the pharmacy"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Errorf("unparseable store was not moved aside: %v", err)
	}
	if got := s.ListSpoken(); !strings.Contains(got, "call the pharmacy") {
		t.Errorf("post-repair listing = %q", got)
	}
}

func TestUnknownKeyIsTreatedAsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	content := "version = 1\nnext_id = 2\n\n[[reminder]]\nid = \"r1\"\ntext = \"x\"\n" +
		"due = 2026-08-26T15:00:00Z\ncreated = 2026-08-26T13:00:00Z\nremnd = 5\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := newTestService(t, path)
	if got := s.ListSpoken(); got != "No reminders set." {
		t.Errorf("a typo'd key was silently dropped: %q", got)
	}
}

func TestNormalizeRepairsAHandEditedStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	// Two entries share an id, one has no text, one has no created time.
	content := `version = 1
next_id = 1

[[reminder]]
id = "r1"
text = "call the pharmacy"
due = 2026-08-26T15:00:00Z
created = 2026-08-26T13:00:00Z

[[reminder]]
id = "r1"
text = "stretch"
due = 2026-08-26T16:00:00Z

[[reminder]]
id = "r9"
text = ""
due = 2026-08-26T17:00:00Z
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := newTestService(t, path)
	v := s.Snapshot()
	if len(v.Pending) != 2 {
		t.Fatalf("pending = %+v", v.Pending)
	}
	if v.Pending[0].ID == v.Pending[1].ID {
		t.Errorf("duplicate id survived repair: %+v", v.Pending)
	}
	for _, p := range v.Pending {
		if p.Created.IsZero() {
			t.Errorf("created not repaired: %+v", p)
		}
	}
}

func TestIdsRatchetAcrossFirings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	s, clock := newTestService(t, path)
	if _, err := s.Create("in five minutes", "stretch"); err != nil {
		t.Fatal(err)
	}
	clock.advance(6 * time.Minute)
	if spoken, n := s.ClaimDue(); n != 1 || !strings.Contains(spoken, "stretch") {
		t.Fatalf("claim = %q, %d", spoken, n)
	}
	if _, err := s.Create("in five minutes", "hydrate"); err != nil {
		t.Fatal(err)
	}
	v := s.Snapshot()
	if len(v.Pending) != 1 || v.Pending[0].ID != "r2" {
		t.Errorf("id was reused after a firing: %+v", v.Pending)
	}
}

func TestHistoryStaysCapped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	s, clock := newTestService(t, path)
	for range maxHistory + 5 {
		if _, err := s.Create("in one minute", "blink"); err != nil {
			t.Fatal(err)
		}
		clock.advance(2 * time.Minute)
		if _, n := s.ClaimDue(); n != 1 {
			t.Fatal("claim missed")
		}
	}
	v := s.Snapshot()
	if len(v.History) != maxHistory {
		t.Errorf("history = %d entries, want the %d cap", len(v.History), maxHistory)
	}
}

func TestFailedWriteCostsNothingInMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.toml")
	s, _ := newTestService(t, path)
	if _, err := s.Create("at three", "call the pharmacy"); err != nil {
		t.Fatal(err)
	}
	s.write = func(string, persisted) error { return os.ErrPermission }
	if _, err := s.Create("at four", "second"); err == nil {
		t.Fatal("a failed write claimed success")
	}
	s.write = writeStore
	if got := s.ListSpoken(); strings.Contains(got, "second") {
		t.Errorf("a failed write left state in memory: %q", got)
	}
}
