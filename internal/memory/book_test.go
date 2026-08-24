package memory

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testClock is a controllable clock: every timestamp in these tests is
// chosen, never sampled, so ordering assertions can be exact.
type testClock struct{ t time.Time }

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time { return c.t }

func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestBook(t *testing.T, opts BookOptions) (*Book, *testClock, string) {
	t.Helper()
	clock := newTestClock()
	if opts.Now == nil {
		opts.Now = clock.now
	}
	path := filepath.Join(t.TempDir(), "state", "memory.toml")
	return NewBook(path, opts, slog.New(slog.NewTextHandler(os.Stderr, nil))), clock, path
}

func TestAddPersistsAcrossReopen(t *testing.T) {
	b, _, path := newTestBook(t, BookOptions{})
	fact, warning, err := b.Add("the staging server is called atlas", "s3")
	if err != nil {
		t.Fatal(err)
	}
	if warning != "" {
		t.Errorf("warning = %q on a nearly empty store", warning)
	}
	if fact.ID != "m1" || fact.Source != "s3" || fact.Stored.IsZero() {
		t.Errorf("fact = %+v, want id m1, source s3, a timestamp", fact)
	}

	// A fresh Book over the same path is a daemon restart: the fact must
	// come back from disk, not from anything held in memory.
	reopened := NewBook(path, BookOptions{}, nil)
	facts := reopened.List("")
	if len(facts) != 1 || facts[0].Content != "the staging server is called atlas" {
		t.Fatalf("after reopen facts = %+v, want the stored fact", facts)
	}
	if facts[0].ID != "m1" || facts[0].Source != "s3" {
		t.Errorf("after reopen fact = %+v, want id and source preserved", facts[0])
	}
}

func TestStoreFileIsPrivate(t *testing.T) {
	b, _, path := newTestBook(t, BookOptions{})
	if _, _, err := b.Add("a fact", ""); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("store file mode = %o, want 600", perm)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("state dir mode = %o, want 700", perm)
	}
}

// TestUpdateKeepsTheTimestampTrail is the supersede contract: "actually it's
// helios" replaces the value, and "when did that change" stays answerable
// because the old value survives with both of its timestamps.
func TestUpdateKeepsTheTimestampTrail(t *testing.T) {
	b, clock, _ := newTestBook(t, BookOptions{})
	original, _, err := b.Add("the staging server is called atlas", "s1")
	if err != nil {
		t.Fatal(err)
	}
	storedAt := clock.now()
	clock.advance(48 * time.Hour)

	updated, err := b.Update(original.ID, "the staging server is called helios", "s9")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Content != "the staging server is called helios" {
		t.Errorf("content = %q, want the new value", updated.Content)
	}
	if !updated.Stored.Equal(storedAt) {
		t.Errorf("Stored = %v, want the original %v — an update is not a new fact", updated.Stored, storedAt)
	}
	if !updated.Updated.Equal(clock.now()) {
		t.Errorf("Updated = %v, want now", updated.Updated)
	}
	if updated.Source != "s9" {
		t.Errorf("Source = %q, want the updating turn", updated.Source)
	}
	if len(updated.Previous) != 1 {
		t.Fatalf("trail = %+v, want exactly the superseded value", updated.Previous)
	}
	rev := updated.Previous[0]
	if rev.Content != "the staging server is called atlas" ||
		!rev.Stored.Equal(storedAt) || !rev.Superseded.Equal(clock.now()) {
		t.Errorf("revision = %+v, want the old value bracketed by its timestamps", rev)
	}

	// The update replaced, not accumulated: still one fact.
	if facts := b.List(""); len(facts) != 1 {
		t.Errorf("facts = %d, want 1 — an update must not add", len(facts))
	}
}

func TestUpdateTrailSurvivesReopen(t *testing.T) {
	b, clock, path := newTestBook(t, BookOptions{})
	f, _, _ := b.Add("the staging server is called atlas", "")
	clock.advance(time.Hour)
	if _, err := b.Update(f.ID, "the staging server is called helios", ""); err != nil {
		t.Fatal(err)
	}

	facts := NewBook(path, BookOptions{}, nil).List("")
	if len(facts) != 1 || len(facts[0].Previous) != 1 {
		t.Fatalf("after reopen facts = %+v, want one fact with one revision", facts)
	}
	if facts[0].Previous[0].Content != "the staging server is called atlas" {
		t.Errorf("revision content = %q", facts[0].Previous[0].Content)
	}
}

func TestUpdateUnknownIDDoesNotWrite(t *testing.T) {
	b, _, _ := newTestBook(t, BookOptions{})
	b.mustAdd(t, "the staging server is called atlas")
	if _, err := b.Update("m99", "something", ""); err == nil {
		t.Fatal("updating an unknown id succeeded")
	}
	if facts := b.List(""); len(facts) != 1 || facts[0].Content != "the staging server is called atlas" {
		t.Errorf("store changed by a failed update: %+v", facts)
	}
}

// mustAdd is the test shorthand for storing one fact.
func (b *Book) mustAdd(t *testing.T, content string) Fact {
	t.Helper()
	f, _, err := b.Add(content, "")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestForgetDeletesFromDisk: deletion is deletion — nothing of the fact,
// trail included, survives a reopen.
func TestForgetDeletesFromDisk(t *testing.T) {
	b, clock, path := newTestBook(t, BookOptions{})
	f := b.mustAdd(t, "the user's partner's birthday is June 3")
	clock.advance(time.Minute)
	if _, err := b.Update(f.ID, "the user's partner's birthday is June 4", ""); err != nil {
		t.Fatal(err)
	}
	keep := b.mustAdd(t, "the staging server is called atlas")

	forgotten, err := b.Forget(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if forgotten.Content != "the user's partner's birthday is June 4" {
		t.Errorf("forgot %q, want the birthday fact", forgotten.Content)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"birthday", "June"} {
		if strings.Contains(string(data), gone) {
			t.Errorf("forgotten content %q still on disk:\n%s", gone, data)
		}
	}
	facts := NewBook(path, BookOptions{}, nil).List("")
	if len(facts) != 1 || facts[0].ID != keep.ID {
		t.Errorf("after reopen facts = %+v, want only the kept fact", facts)
	}
}

func TestForgetUnknownID(t *testing.T) {
	b, _, _ := newTestBook(t, BookOptions{})
	if _, err := b.Forget("m1"); err == nil {
		t.Fatal("forgetting from an empty store succeeded")
	}
}

// TestIDsNeverRepeat: forgetting m2 must not let the next fact become a
// second m2 — a supersede trail naming a reused id would lie.
func TestIDsNeverRepeat(t *testing.T) {
	b, _, _ := newTestBook(t, BookOptions{})
	b.mustAdd(t, "fact one")
	two := b.mustAdd(t, "completely different subject")
	if _, err := b.Forget(two.ID); err != nil {
		t.Fatal(err)
	}
	three := b.mustAdd(t, "another topic entirely")
	if three.ID == two.ID {
		t.Errorf("id %q reused after a forget", three.ID)
	}
}

func TestStoreCapRefusesWithActionableError(t *testing.T) {
	b, _, _ := newTestBook(t, BookOptions{MaxFacts: 2})
	b.mustAdd(t, "first topic")
	_, warning, err := b.Add("second topic", "")
	if err != nil {
		t.Fatal(err)
	}
	// At two of two the store is past the nine-tenths mark: the refusal at
	// the cap must never be the first anyone hears of it.
	if !strings.Contains(warning, "nearly full") {
		t.Errorf("warning = %q, want a near-cap warning", warning)
	}
	_, _, err = b.Add("third topic", "")
	if err == nil {
		t.Fatal("add beyond the cap succeeded")
	}
	for _, want := range []string{"full", "forget", "memory.max_facts"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q — it must name the fix", err, want)
		}
	}
	// Updates must still work at the cap: correcting is not growing.
	facts := b.List("")
	if _, err := b.Update(facts[0].ID, "corrected first topic", ""); err != nil {
		t.Errorf("update at the cap failed: %v", err)
	}
}

// TestHandEditIsPickedUpWithoutRestart is the hand-editable contract: the
// user edits the file, and the very next consultation sees the change — no
// restart, no reload command. The mtime is moved explicitly because a test
// must not sleep its way past filesystem timestamp granularity.
func TestHandEditIsPickedUpWithoutRestart(t *testing.T) {
	b, _, path := newTestBook(t, BookOptions{})
	b.mustAdd(t, "the staging server is called atlas")

	edited := strings.Replace(mustRead(t, path), "atlas", "helios", 1)
	writeHandEdit(t, path, edited)

	facts := b.List("")
	if len(facts) != 1 || facts[0].Content != "the staging server is called helios" {
		t.Fatalf("after hand-edit facts = %+v, want the edited value", facts)
	}
}

// TestHandAddedFactIsRepairedOnLoad: a user appending a bare [[fact]] with
// just a content gets an id and fresh timestamps, and — because a hand-add
// is a deliberate act — it must sort as recently confirmed, not be the first
// thing the injection trim drops.
func TestHandAddedFactIsRepairedOnLoad(t *testing.T) {
	b, clock, path := newTestBook(t, BookOptions{})
	b.mustAdd(t, "the staging server is called atlas")
	clock.advance(time.Hour)

	writeHandEdit(t, path, mustRead(t, path)+"\n[[fact]]\ncontent = \"the user's editor is neovim\"\n")

	facts := b.List("")
	if len(facts) != 2 {
		t.Fatalf("facts = %+v, want both", facts)
	}
	var added Fact
	found := false
	for _, f := range facts {
		if f.Content == "the user's editor is neovim" {
			added, found = f, true
		}
	}
	if !found {
		t.Fatalf("hand-added fact not loaded: %+v", facts)
	}
	if added.ID == "" || added.ID == "m1" {
		t.Errorf("hand-added fact id = %q, want a fresh unique id", added.ID)
	}
	if !added.Updated.Equal(clock.now()) {
		t.Errorf("hand-added fact Updated = %v, want load time %v", added.Updated, clock.now())
	}
}

func TestDeletingTheFileEmptiesTheStore(t *testing.T) {
	b, _, path := newTestBook(t, BookOptions{})
	b.mustAdd(t, "a fact")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if facts := b.List(""); len(facts) != 0 {
		t.Errorf("facts = %+v after the user deleted the file, want none", facts)
	}
}

// TestCorruptFileDegradesToEmptyAndIsNeverOverwritten is the two-part
// corruption contract: Jarvix serves an empty memory with a warning instead
// of crashing, and the user's unparseable file — which may hold every fact
// they own, one typo away from valid — is moved aside, not destroyed, when
// the next write happens.
func TestCorruptFileDegradesToEmptyAndIsNeverOverwritten(t *testing.T) {
	b, _, path := newTestBook(t, BookOptions{})
	b.mustAdd(t, "the staging server is called atlas")

	writeHandEdit(t, path, "version = 1\n[[fact]\nthis is not toml")

	if facts := b.List(""); len(facts) != 0 {
		t.Fatalf("facts = %+v from a corrupt file, want empty", facts)
	}

	// The next remember writes a fresh store — and preserves the corrupt
	// file beside it.
	b.mustAdd(t, "a new fact")
	backup, err := os.ReadFile(path + ".corrupt")
	if err != nil {
		t.Fatalf("corrupt file was not preserved: %v", err)
	}
	if !strings.Contains(string(backup), "this is not toml") {
		t.Errorf("backup content = %q, want the unparseable original", backup)
	}
	facts := NewBook(path, BookOptions{}, nil).List("")
	if len(facts) != 1 || facts[0].Content != "a new fact" {
		t.Errorf("store after recovery = %+v", facts)
	}
}

// TestHandEditTypoInAKeyIsCorruption: a misspelled key would silently drop
// the value it holds, which looks exactly like Jarvix forgetting — so it is
// treated as corruption (warn, empty, preserve), not tolerated.
func TestHandEditTypoInAKeyIsCorruption(t *testing.T) {
	b, _, path := newTestBook(t, BookOptions{})
	b.mustAdd(t, "a fact")
	writeHandEdit(t, path, "version = 1\n\n[[fact]]\nid = \"m1\"\ncontnet = \"typo in the key\"\n")
	if facts := b.List(""); len(facts) != 0 {
		t.Errorf("facts = %+v from a file with an unknown key, want empty", facts)
	}
}

func TestUnsupportedVersionIsCorruption(t *testing.T) {
	b, _, path := newTestBook(t, BookOptions{})
	writeHandEdit(t, path, "version = 99\n")
	if facts := b.List(""); len(facts) != 0 {
		t.Errorf("facts = %+v from an unsupported version, want empty", facts)
	}
}

// TestCorruptionWithoutAWriteStaysPreserved: fixing the file by hand after a
// corruption warning must bring the facts back — the corrupt latch is not a
// death sentence.
func TestFixingACorruptFileByHandRestoresIt(t *testing.T) {
	b, _, path := newTestBook(t, BookOptions{})
	b.mustAdd(t, "the staging server is called atlas")
	good := mustRead(t, path)

	writeHandEdit(t, path, good+"\nnot toml at all [")
	if facts := b.List(""); len(facts) != 0 {
		t.Fatalf("facts = %+v, want empty while corrupt", facts)
	}

	writeHandEdit(t, path, good)
	facts := b.List("")
	if len(facts) != 1 || facts[0].Content != "the staging server is called atlas" {
		t.Errorf("facts = %+v after the fix, want the original", facts)
	}
}

func TestListMatchesByWordsAndSubstring(t *testing.T) {
	b, _, _ := newTestBook(t, BookOptions{})
	b.mustAdd(t, "the staging server is called atlas")
	b.mustAdd(t, "the user's terminal is Ghostty on workspace one")

	if got := b.List("staging"); len(got) != 1 || !strings.Contains(got[0].Content, "atlas") {
		t.Errorf("List(staging) = %+v", got)
	}
	if got := b.List("what do you know about my terminal"); len(got) != 1 ||
		!strings.Contains(got[0].Content, "Ghostty") {
		t.Errorf("List(terminal question) = %+v", got)
	}
	if got := b.List("kubernetes"); len(got) != 0 {
		t.Errorf("List(kubernetes) = %+v, want none", got)
	}
	if got := b.List(""); len(got) != 2 {
		t.Errorf("List(\"\") = %d facts, want all", len(got))
	}
}

// TestReturnedFactsAreCopies: a caller mutating a returned trail must not
// reach the Book's own state.
func TestReturnedFactsAreCopies(t *testing.T) {
	b, clock, _ := newTestBook(t, BookOptions{})
	f := b.mustAdd(t, "the staging server is called atlas")
	clock.advance(time.Minute)
	if _, err := b.Update(f.ID, "the staging server is called helios", ""); err != nil {
		t.Fatal(err)
	}
	got := b.List("")
	got[0].Previous[0].Content = "tampered"
	if again := b.List(""); again[0].Previous[0].Content != "the staging server is called atlas" {
		t.Error("mutating a returned fact reached the store")
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// writeHandEdit simulates the user editing the store file, moving the mtime
// forward explicitly so the change detector fires regardless of filesystem
// timestamp granularity — determinism without a sleep.
func writeHandEdit(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Time{}, info.ModTime().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
}

// TestRefusalsMatchTheirSentinels pins the contract the daemon's memory form
// surface places refusals by (issue #100): each refusal wraps its sentinel —
// matchable with errors.Is — while the message keeps its exact actionable
// wording, id and cap numbers included.
func TestRefusalsMatchTheirSentinels(t *testing.T) {
	book, _, _ := newTestBook(t, BookOptions{MaxFacts: 1})

	if _, _, err := book.Add("   ", ""); !errors.Is(err, ErrNoContent) {
		t.Errorf("empty add err = %v, want ErrNoContent", err)
	}
	if _, err := book.Update("m1", "", ""); !errors.Is(err, ErrNoContent) {
		t.Errorf("empty update err = %v, want ErrNoContent", err)
	}
	if _, _, err := book.Add("the only fact", ""); err != nil {
		t.Fatal(err)
	}
	_, _, err := book.Add("one too many", "")
	if !errors.Is(err, ErrStoreFull) {
		t.Errorf("cap err = %v, want ErrStoreFull", err)
	}
	if err == nil || !strings.Contains(err.Error(),
		"the memory store is full (1 facts); forget something stale, or raise memory.max_facts") {
		t.Errorf("cap err = %v, want the actionable sentence intact", err)
	}
	_, err = book.Update("m9", "x", "")
	if !errors.Is(err, ErrUnknownID) || !strings.Contains(err.Error(), `no remembered fact has id "m9"`) {
		t.Errorf("unknown id err = %v, want ErrUnknownID naming it", err)
	}
	if _, err := book.Forget("m9"); !errors.Is(err, ErrUnknownID) {
		t.Errorf("unknown forget err = %v, want ErrUnknownID", err)
	}
}
