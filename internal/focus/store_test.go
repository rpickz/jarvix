package focus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
)

// The storage half: the memory book's contract, proven over threads —
// restart survival, hand-edit pickup and repair, the corrupt latch, and the
// mutation checks that make a failed write cost exactly nothing.

// testClock is a hand-advanced clock so no test ever sleeps.
type testClock struct{ t time.Time }

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)}
}
func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newStoreService builds a service over a temp store with the injected
// clock and no scheduler running — these tests drive operations directly.
func newStoreService(t *testing.T, clock *testClock) *Service {
	t.Helper()
	path := filepath.Join(t.TempDir(), "focus.toml")
	return NewService(path, Options{Now: clock.now}, testLogger(t))
}

func TestThreadsAnchorsAndParkedSurviveRestart(t *testing.T) {
	clock := newTestClock()
	dir := t.TempDir()
	path := filepath.Join(dir, "focus.toml")
	windows := func(context.Context) ([]desktop.Window, error) {
		return []desktop.Window{
			{Address: "0xa", Class: "Alacritty", Title: "make test", Focused: true},
			{Address: "0xb", Class: "firefox", Title: "GitHub"},
		}, nil
	}
	s := NewService(path, Options{Now: clock.now, Windows: windows}, testLogger(t))

	if _, _, err := s.Create(context.Background(), "the ci refactor", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Park("reply to dan"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remind(45); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartSession(context.Background(), "ci refactor", 25); err != nil {
		t.Fatal(err)
	}

	// A different Service over the same file is the restart.
	restarted := NewService(path, Options{Now: clock.now, Windows: windows}, testLogger(t))
	v := restarted.Snapshot(context.Background())
	if len(v.Threads) != 1 {
		t.Fatalf("threads after restart = %d", len(v.Threads))
	}
	th := v.Threads[0]
	if th.Name != "the ci refactor" || !th.Active || th.RemindEveryMin != 45 {
		t.Errorf("thread after restart = %+v", th)
	}
	if len(th.Anchors) != 2 || th.Anchors[0].App != "Alacritty" || th.AnchorsGone[0] {
		t.Errorf("anchors after restart = %+v gone %v", th.Anchors, th.AnchorsGone)
	}
	if len(th.Parked) != 1 || th.Parked[0].Text != "reply to dan" {
		t.Errorf("parked after restart = %+v", th.Parked)
	}
	if v.Session == nil || v.Session.Minutes != 25 || v.Session.Phase != "running" {
		t.Errorf("session after restart = %+v", v.Session)
	}
}

func TestHandEditIsRepairedNotPunished(t *testing.T) {
	clock := newTestClock()
	s := newStoreService(t, clock)
	// A hand-written file: no ids, no timestamps, a duplicate id, three
	// anchors, an empty parked thought, and an active pointer at nothing.
	edit := `version = 1
active = "t404"

[[thread]]
name = "reviews"

[[thread]]
id = "t9"
name = "deploy"
[[thread.parked]]
text = "check the runbook"
[[thread.parked]]
text = ""

[[thread]]
id = "t9"
name = "the audit"
[[thread.anchor]]
address = "0x1"
app = "a"
[[thread.anchor]]
address = "0x2"
app = "b"
[[thread.anchor]]
address = "0x3"
app = "c"
`
	if err := os.WriteFile(s.Path(), []byte(edit), 0o600); err != nil {
		t.Fatal(err)
	}
	v := s.Snapshot(context.Background())
	if len(v.Threads) != 3 {
		t.Fatalf("threads = %+v", v.Threads)
	}
	if v.Active != "" {
		t.Errorf("active pointer at a vanished thread survived: %q", v.Active)
	}
	seen := map[string]bool{}
	for _, th := range v.Threads {
		if th.ID == "" || seen[th.ID] {
			t.Errorf("id not repaired uniquely: %+v", th)
		}
		seen[th.ID] = true
		if th.Created.IsZero() || th.LastActivity.IsZero() {
			t.Errorf("timestamps not repaired: %+v", th)
		}
		if len(th.Anchors) > maxAnchors {
			t.Errorf("anchors not trimmed: %+v", th.Anchors)
		}
		for _, pk := range th.Parked {
			if pk.Text == "" || pk.ID == "" {
				t.Errorf("parked not repaired: %+v", pk)
			}
		}
	}
}

func TestCorruptStoreServesEmptyAndIsMovedAsideOnWrite(t *testing.T) {
	clock := newTestClock()
	s := newStoreService(t, clock)
	if err := os.WriteFile(s.Path(), []byte("version = 1\nthis is not toml ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if v := s.Snapshot(context.Background()); len(v.Threads) != 0 {
		t.Fatalf("a corrupt store served threads: %+v", v.Threads)
	}
	// The first write moves the broken file aside instead of overwriting the
	// user's half-fixed edit.
	if _, _, err := s.Create(context.Background(), "fresh", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path() + ".corrupt"); err != nil {
		t.Errorf("the unparseable file was not preserved: %v", err)
	}
}

func TestUnknownKeyIsRefusedAsCorruption(t *testing.T) {
	clock := newTestClock()
	s := newStoreService(t, clock)
	// A typo'd key must not silently drop the value it holds.
	edit := "version = 1\n\n[[thread]]\nname = \"x\"\nremnd_every_min = 45\n"
	if err := os.WriteFile(s.Path(), []byte(edit), 0o600); err != nil {
		t.Fatal(err)
	}
	if v := s.Snapshot(context.Background()); len(v.Threads) != 0 {
		t.Fatalf("a store with an unknown key was half-loaded: %+v", v.Threads)
	}
}

// TestFailedWriteCostsNothing is the mutation check: an operation whose disk
// write fails must leave both memory and disk exactly as they were — the
// Service can never claim a state the file does not hold.
func TestFailedWriteCostsNothing(t *testing.T) {
	clock := newTestClock()
	s := newStoreService(t, clock)
	if _, _, err := s.Create(context.Background(), "deploy", 0); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}

	s.write = func(string, persisted) error { return errors.New("disk on fire") }
	if _, err := s.Park("a thought"); err == nil {
		t.Fatal("a failed write reported success")
	}
	if _, _, err := s.Create(context.Background(), "reviews", 0); err == nil {
		t.Fatal("a failed write reported success")
	}
	s.write = writeStore

	v := s.Snapshot(context.Background())
	if len(v.Threads) != 1 || len(v.Threads[0].Parked) != 0 {
		t.Errorf("a failed write mutated memory: %+v", v.Threads)
	}
	after, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("a failed write mutated the file:\n%s", string(after))
	}
}

// TestIDsAreNeverReused: ending a thread and creating another must not hand
// the old id out again, even across a restart — a conversation that named
// "t1" must never come to describe different work.
func TestIDsAreNeverReused(t *testing.T) {
	clock := newTestClock()
	dir := t.TempDir()
	path := filepath.Join(dir, "focus.toml")
	s := NewService(path, Options{Now: clock.now}, testLogger(t))
	th1, _, err := s.Create(context.Background(), "deploy", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.End("deploy"); err != nil {
		t.Fatal(err)
	}
	restarted := NewService(path, Options{Now: clock.now}, testLogger(t))
	th2, _, err := restarted.Create(context.Background(), "reviews", 0)
	if err != nil {
		t.Fatal(err)
	}
	if th1.ID == th2.ID {
		t.Errorf("thread id %q was reused", th1.ID)
	}
}

// TestDuplicateNameIsRefused: two threads answering to the same name would
// make every later reference a coin toss, so the second is a refusal.
func TestDuplicateNameIsRefused(t *testing.T) {
	clock := newTestClock()
	s := newStoreService(t, clock)
	if _, _, err := s.Create(context.Background(), "the deploy", 0); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Create(context.Background(), "Deploy", 0)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate name err = %v", err)
	}
}
