package vocabulary

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The storage half of issue #129, pinned against the memory-book discipline
// it inherits: supersede-on-reteach (never a silent second entry), the
// hand-edit pickup, the corrupt-file move-aside, the never-reused ids, and
// the two loud caps — the store's and the hard-to-hear list's.

// testClock is an injected, hand-advanced clock: no test in this package
// sleeps, and every timestamp is chosen.
type testClock struct{ now time.Time }

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time { return c.now }

func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestStore(t *testing.T, opts StoreOptions) (*Store, *testClock) {
	t.Helper()
	clock := newTestClock()
	if opts.Now == nil {
		opts.Now = clock.Now
	}
	return NewStore(filepath.Join(t.TempDir(), "vocabulary.toml"), opts, nil), clock
}

func TestTeachStoresAndListsAnEntry(t *testing.T) {
	s, _ := newTestStore(t, StoreOptions{})
	entry, warning, err := s.Teach("  quid  ", "  pounds ", " UK money slang ", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if warning != "" {
		t.Errorf("warning = %q, want none with an empty store", warning)
	}
	if entry.ID != "w1" || entry.Phrase != "quid" || entry.Meaning != "pounds" ||
		entry.Note != "UK money slang" || entry.Source != "s1" {
		t.Errorf("entry = %+v, want the trimmed teach with its first id", entry)
	}
	entries := s.List("")
	if len(entries) != 1 || entries[0].ID != "w1" {
		t.Fatalf("List = %+v, want the one taught entry", entries)
	}
}

func TestTeachRefusesEmptyHalves(t *testing.T) {
	s, _ := newTestStore(t, StoreOptions{})
	if _, _, err := s.Teach("  ", "pounds", "", ""); !errors.Is(err, ErrNoPhrase) {
		t.Errorf("empty phrase err = %v, want ErrNoPhrase", err)
	}
	if _, _, err := s.Teach("quid", "   ", "", ""); !errors.Is(err, ErrNoMeaning) {
		t.Errorf("empty meaning err = %v, want ErrNoMeaning", err)
	}
	if n, _ := s.Count(); n != 0 {
		t.Errorf("count = %d after refused teaches, want 0", n)
	}
}

// TestReteachSupersedesNeverDuplicates is the duplicate-phrase acceptance
// criterion: teaching an existing phrase — case and punctuation folded, the
// spoken identity — updates the meaning and keeps the old one on the trail,
// never a second entry.
func TestReteachSupersedesNeverDuplicates(t *testing.T) {
	s, clock := newTestStore(t, StoreOptions{})
	if _, _, err := s.Teach("quid", "pounds", "", "s1"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	entry, _, err := s.Teach("Quid,", "euros", "", "s2")
	if err != nil {
		t.Fatal(err)
	}
	entries := s.List("")
	if len(entries) != 1 {
		t.Fatalf("re-teach accumulated: %+v", entries)
	}
	if entry.ID != "w1" || entry.Meaning != "euros" {
		t.Errorf("entry = %+v, want the same id with the new meaning", entry)
	}
	if len(entry.Previous) != 1 || entry.Previous[0].Meaning != "pounds" {
		t.Errorf("trail = %+v, want the old meaning kept", entry.Previous)
	}
	if !entry.Updated.After(entry.Taught) {
		t.Errorf("Updated %v not after Taught %v", entry.Updated, entry.Taught)
	}
}

// TestIdenticalReteachWritesNothing: repeating yourself must not manufacture
// a revision of nothing, nor touch the disk.
func TestIdenticalReteachWritesNothing(t *testing.T) {
	s, clock := newTestStore(t, StoreOptions{})
	if _, _, err := s.Teach("quid", "pounds", "", ""); err != nil {
		t.Fatal(err)
	}
	writes := 0
	inner := s.write
	s.write = func(path string, entries []Entry, next int) error {
		writes++
		return inner(path, entries, next)
	}
	clock.Advance(time.Hour)
	entry, _, err := s.Teach("quid", "pounds", "", "s9")
	if err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Errorf("identical re-teach wrote %d times, want 0", writes)
	}
	if len(entry.Previous) != 0 || entry.Updated.Equal(clock.Now()) {
		t.Errorf("entry = %+v, want untouched", entry)
	}
}

func TestStoreCapRefusesLoudlyAndWarnsBefore(t *testing.T) {
	s, _ := newTestStore(t, StoreOptions{MaxEntries: 10})
	var lastWarning string
	for i := 0; i < 10; i++ {
		_, warning, err := s.Teach(fmt.Sprintf("word%d", i), "meaning", "", "")
		if err != nil {
			t.Fatal(err)
		}
		lastWarning = warning
	}
	if !strings.Contains(lastWarning, "nearly full") {
		t.Errorf("teach at 10/10 warned %q, want the near-cap warning before the refusal", lastWarning)
	}
	_, _, err := s.Teach("one more", "meaning", "", "")
	if !errors.Is(err, ErrStoreFull) {
		t.Fatalf("teach past the cap err = %v, want ErrStoreFull", err)
	}
	if !strings.Contains(err.Error(), "vocabulary.max_entries") {
		t.Errorf("refusal %q does not name the fix", err)
	}
	// A re-teach of an existing phrase is not an addition: it must still
	// supersede at the cap, or a full store would freeze its own corrections.
	if _, _, err := s.Teach("word3", "new meaning", "", ""); err != nil {
		t.Errorf("supersede at the cap err = %v, want success", err)
	}
}

func TestUpdateRenameCollisionRefused(t *testing.T) {
	s, _ := newTestStore(t, StoreOptions{})
	if _, _, err := s.Teach("quid", "pounds", "", ""); err != nil {
		t.Fatal(err)
	}
	second, _, err := s.Teach("telly", "television", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(second.ID, "QUID", "television", "", ""); !errors.Is(err, ErrDuplicatePhrase) {
		t.Fatalf("rename onto a taught phrase err = %v, want ErrDuplicatePhrase", err)
	}
	if _, err := s.Update("w9", "x", "y", "", ""); !errors.Is(err, ErrUnknownID) {
		t.Errorf("unknown id err = %v, want ErrUnknownID", err)
	}
}

// TestHardToHearCapRefusesAndWarns pins the bias budget: the flag refuses at
// MaxHardToHear with an actionable message, warns as the list fills, and an
// unflag makes room again.
func TestHardToHearCapRefusesAndWarns(t *testing.T) {
	s, _ := newTestStore(t, StoreOptions{})
	ids := make([]string, 0, MaxHardToHear+1)
	for i := 0; i <= MaxHardToHear; i++ {
		e, _, err := s.Teach(fmt.Sprintf("word%d", i), "meaning", "", "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.ID)
	}
	var lastWarning string
	for i := 0; i < MaxHardToHear; i++ {
		_, warning, err := s.SetHardToHear(ids[i], true)
		if err != nil {
			t.Fatalf("flag %d: %v", i, err)
		}
		lastWarning = warning
	}
	if !strings.Contains(lastWarning, "nearly full") {
		t.Errorf("flagging the last slot warned %q, want the near-cap warning", lastWarning)
	}
	if _, _, err := s.SetHardToHear(ids[MaxHardToHear], true); !errors.Is(err, ErrBiasFull) {
		t.Fatalf("flag past the cap err = %v, want ErrBiasFull", err)
	}
	if n, max := s.BiasCount(); n != MaxHardToHear || max != MaxHardToHear {
		t.Errorf("BiasCount = %d/%d, want %d/%d", n, max, MaxHardToHear, MaxHardToHear)
	}
	if _, _, err := s.SetHardToHear(ids[0], false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetHardToHear(ids[MaxHardToHear], true); err != nil {
		t.Errorf("flag after unflagging err = %v, want room again", err)
	}
	if got := s.HardToHear(); len(got) != MaxHardToHear {
		t.Errorf("HardToHear = %d phrases, want %d", len(got), MaxHardToHear)
	}
}

// TestFlagIsNotAContentChange: toggling hard-to-hear touches neither the
// timestamps nor the trail, and re-setting the same value skips the write.
func TestFlagIsNotAContentChange(t *testing.T) {
	s, clock := newTestStore(t, StoreOptions{})
	taught, _, err := s.Teach("quid", "pounds", "", "")
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	flagged, _, err := s.SetHardToHear(taught.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !flagged.Updated.Equal(taught.Updated) || len(flagged.Previous) != 0 {
		t.Errorf("flag changed content state: %+v", flagged)
	}
	writes := 0
	inner := s.write
	s.write = func(path string, entries []Entry, next int) error {
		writes++
		return inner(path, entries, next)
	}
	if _, _, err := s.SetHardToHear(taught.ID, true); err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Errorf("re-setting the same flag wrote %d times, want 0", writes)
	}
}

func TestForgetDeletesTrailAndAll(t *testing.T) {
	s, clock := newTestStore(t, StoreOptions{})
	if _, _, err := s.Teach("quid", "pounds", "", ""); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	if _, _, err := s.Teach("quid", "euros", "", ""); err != nil {
		t.Fatal(err)
	}
	forgotten, err := s.Forget("w1")
	if err != nil {
		t.Fatal(err)
	}
	if forgotten.Meaning != "euros" {
		t.Errorf("forgotten = %+v", forgotten)
	}
	if entries := s.List(""); len(entries) != 0 {
		t.Fatalf("entries after forget = %+v", entries)
	}
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "quid") || strings.Contains(string(raw), "euros") {
		t.Errorf("the file still carries the forgotten phrase:\n%s", raw)
	}
	if _, err := s.Forget("w1"); !errors.Is(err, ErrUnknownID) {
		t.Errorf("double forget err = %v, want ErrUnknownID", err)
	}
}

// TestIdsAreNeverReused: forgetting w1 must not let a later teach mint a
// second w1 — a supersede trail or an old conversation naming it would come
// to describe a different phrase. Pinned across a reopen, because the mark
// is persisted.
func TestIdsAreNeverReused(t *testing.T) {
	s, _ := newTestStore(t, StoreOptions{})
	if _, _, err := s.Teach("quid", "pounds", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Forget("w1"); err != nil {
		t.Fatal(err)
	}
	reopened := NewStore(s.Path(), StoreOptions{Now: newTestClock().Now}, nil)
	entry, _, err := reopened.Teach("telly", "television", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if entry.ID == "w1" {
		t.Fatalf("id w1 was reused after a forget and reopen")
	}
}

// TestFailedWriteCommitsNothing is the mutation check: when the disk refuses,
// the in-memory store must still hold exactly what the file holds — no
// half-taught entry, no half-forgotten one.
func TestFailedWriteCommitsNothing(t *testing.T) {
	s, _ := newTestStore(t, StoreOptions{})
	if _, _, err := s.Teach("quid", "pounds", "", ""); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("disk full")
	s.write = func(string, []Entry, int) error { return boom }

	if _, _, err := s.Teach("telly", "television", "", ""); !errors.Is(err, boom) {
		t.Fatalf("teach err = %v, want the disk failure", err)
	}
	if _, _, err := s.Teach("quid", "euros", "", ""); !errors.Is(err, boom) {
		t.Fatalf("supersede err = %v, want the disk failure", err)
	}
	if _, err := s.Forget("w1"); !errors.Is(err, boom) {
		t.Fatalf("forget err = %v, want the disk failure", err)
	}
	if _, _, err := s.SetHardToHear("w1", true); !errors.Is(err, boom) {
		t.Fatalf("flag err = %v, want the disk failure", err)
	}

	entries := s.List("")
	if len(entries) != 1 || entries[0].Phrase != "quid" || entries[0].Meaning != "pounds" ||
		entries[0].HardToHear || len(entries[0].Previous) != 0 {
		t.Errorf("after failed writes the store claims %+v, want the original entry untouched", entries)
	}
}

// TestHandEditIsPickedUpWithoutRestart: the user edits the file, the very
// next operation sees it — the stat-per-operation contract.
func TestHandEditIsPickedUpWithoutRestart(t *testing.T) {
	s, _ := newTestStore(t, StoreOptions{})
	if _, _, err := s.Teach("quid", "pounds", "", ""); err != nil {
		t.Fatal(err)
	}
	doc := `version = 1
next_id = 5

[[entry]]
id = "w1"
phrase = "quid"
meaning = "pounds sterling"
taught = 2026-08-01T10:00:00Z
updated = 2026-08-01T10:00:00Z

[[entry]]
phrase = "telly"
meaning = "the television"
hard_to_hear = true
`
	writeFileWithNewMtime(t, s.Path(), doc)
	entries := s.List("")
	if len(entries) != 2 {
		t.Fatalf("after hand-edit List = %+v", entries)
	}
	byPhrase, ok := s.ByPhrase("telly")
	if !ok {
		t.Fatal("the hand-added entry was not picked up")
	}
	// normalize repaired the missing id (above the persisted mark) and
	// timestamps, and kept the flag.
	if byPhrase.ID != "w5" || byPhrase.Taught.IsZero() || !byPhrase.HardToHear {
		t.Errorf("hand-added entry = %+v, want id w5 with timestamps filled", byPhrase)
	}
}

// TestNormalizeDropsHandEditedDuplicates: two [[entry]] tables for one
// spoken phrase collapse to the first — a duplicate is exactly the state
// teach exists to prevent, hand-edits included.
func TestNormalizeDropsHandEditedDuplicates(t *testing.T) {
	s, _ := newTestStore(t, StoreOptions{})
	doc := `version = 1
next_id = 1

[[entry]]
phrase = "quid"
meaning = "pounds"

[[entry]]
phrase = "Quid"
meaning = "euros"
`
	writeFileWithNewMtime(t, s.Path(), doc)
	entries := s.List("")
	if len(entries) != 1 || entries[0].Meaning != "pounds" {
		t.Fatalf("duplicate hand-edit kept %+v, want the first entry only", entries)
	}
}

// TestNormalizeClearsFlagsPastTheCap: the bias cap holds against hand-edits
// — flags past MaxHardToHear are ignored (with a warning in the log), so
// the bias set is always exactly what HardToHear reports.
func TestNormalizeClearsFlagsPastTheCap(t *testing.T) {
	s, _ := newTestStore(t, StoreOptions{})
	var doc strings.Builder
	doc.WriteString("version = 1\nnext_id = 1\n")
	for i := 0; i < MaxHardToHear+3; i++ {
		fmt.Fprintf(&doc, "\n[[entry]]\nphrase = \"word%d\"\nmeaning = \"m\"\nhard_to_hear = true\n", i)
	}
	writeFileWithNewMtime(t, s.Path(), doc.String())
	if n, _ := s.BiasCount(); n != MaxHardToHear {
		t.Errorf("BiasCount after over-flagged hand-edit = %d, want the cap %d", n, MaxHardToHear)
	}
	if got := len(s.HardToHear()); got != MaxHardToHear {
		t.Errorf("HardToHear = %d phrases, want %d", got, MaxHardToHear)
	}
}

// TestCorruptFileDegradesAndIsMovedAside: an unparseable file serves an
// empty vocabulary, and the first write moves it aside rather than
// overwriting the user's mid-repair edit.
func TestCorruptFileDegradesAndIsMovedAside(t *testing.T) {
	s, _ := newTestStore(t, StoreOptions{})
	writeFileWithNewMtime(t, s.Path(), "this is not toml [[[")
	if entries := s.List(""); len(entries) != 0 {
		t.Fatalf("corrupt file served %+v, want an empty vocabulary", entries)
	}
	if _, _, err := s.Teach("quid", "pounds", "", ""); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(s.Path() + ".corrupt")
	if err != nil {
		t.Fatalf("the unparseable file was not moved aside: %v", err)
	}
	if !strings.Contains(string(backup), "not toml") {
		t.Errorf("backup content = %q", backup)
	}
	if entries := s.List(""); len(entries) != 1 {
		t.Errorf("after the recovery write List = %+v", entries)
	}
}

// TestUnknownKeyIsCorruption: a hand-edit typo ("meening") must degrade
// loudly rather than silently dropping the value — the memory book's rule.
func TestUnknownKeyIsCorruption(t *testing.T) {
	s, _ := newTestStore(t, StoreOptions{})
	writeFileWithNewMtime(t, s.Path(),
		"version = 1\n\n[[entry]]\nphrase = \"quid\"\nmeening = \"pounds\"\n")
	if entries := s.List(""); len(entries) != 0 {
		t.Fatalf("a typo'd key was silently accepted: %+v", entries)
	}
}

func TestStoreSurvivesReopen(t *testing.T) {
	s, clock := newTestStore(t, StoreOptions{})
	if _, _, err := s.Teach("quid", "pounds", "", "s1"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	if _, _, err := s.Teach("quid", "euros", "note", "s2"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetHardToHear("w1", true); err != nil {
		t.Fatal(err)
	}

	reopened := NewStore(s.Path(), StoreOptions{}, nil)
	entries := reopened.List("")
	if len(entries) != 1 {
		t.Fatalf("reopened List = %+v", entries)
	}
	e := entries[0]
	if e.ID != "w1" || e.Meaning != "euros" || e.Note != "note" || !e.HardToHear ||
		len(e.Previous) != 1 || e.Previous[0].Meaning != "pounds" {
		t.Errorf("reopened entry = %+v, want everything persisted, trail included", e)
	}
}

func TestListFiltersAcrossFields(t *testing.T) {
	s, _ := newTestStore(t, StoreOptions{})
	mustTeach(t, s, "quid", "pounds", "UK money slang")
	mustTeach(t, s, "telly", "television", "")
	for query, wantPhrase := range map[string]string{
		"quid":  "quid",  // phrase
		"visio": "telly", // meaning substring
		"slang": "quid",  // note
	} {
		got := s.List(query)
		if len(got) != 1 || got[0].Phrase != wantPhrase {
			t.Errorf("List(%q) = %+v, want %q", query, got, wantPhrase)
		}
	}
	if got := s.List(""); len(got) != 2 {
		t.Errorf("List(\"\") = %d entries, want both", len(got))
	}
}

func mustTeach(t *testing.T, s *Store, phrase, meaning, note string) Entry {
	t.Helper()
	e, _, err := s.Teach(phrase, meaning, note, "")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// writeFileWithNewMtime writes content and guarantees the stat-based change
// detector sees it: same-second writes with equal sizes are the one case
// mtime granularity could hide, so the mtime is pushed forward explicitly —
// no test sleeps.
func writeFileWithNewMtime(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}
