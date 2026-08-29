package conversations

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What the archive does when the disk says no.
//
// The shared suite (faults_test.go) drives the store's own write seam, which
// is the right level for "a write failed, what did the store do about it".
// This file is a level below that: the failures the seam stands in front of,
// provoked on a real temp directory so the error paths inside the writers
// themselves are exercised rather than skipped over. Nothing here needs a
// privilege or a full disk — a directory where a file should be, and a name
// longer than a filesystem will take, are enough to make every one of these
// calls fail for the reason it says it failed.

// blockedStore returns a store over a temp directory, with a fixed id so a
// test can put an obstacle exactly where the store is about to write.
func blockedStore(t *testing.T, id string) *FileStore {
	t.Helper()
	return &FileStore{
		Dir:   t.TempDir(),
		NewID: func(time.Time) string { return id },
	}
}

func TestAnUnwritableTranscriptIsRefusedNotSwallowed(t *testing.T) {
	// A name no filesystem will take, so the transcript cannot be created.
	s := blockedStore(t, strings.Repeat("n", 400))
	id, err := s.Append("", []Turn{{Role: "user", Text: "a question that cannot be written"}})
	if err == nil {
		t.Fatal("an unwritable transcript reported success")
	}
	if !strings.Contains(err.Error(), "write conversation") {
		t.Errorf("error = %v, want it to name the write", err)
	}
	if id != "" {
		t.Errorf("a refused write handed back the id %q", id)
	}
	metas, _, listErr := s.List()
	if listErr != nil || len(metas) != 0 {
		t.Errorf("listing after the refused write = %+v (%v)", metas, listErr)
	}
}

func TestATranscriptThatCannotBeReadForRepairIsRefused(t *testing.T) {
	s := blockedStore(t, "20260821-100000-test")
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	if _, err := s.Append("", turnsAt(ts, "the first question", "the first answer")); err != nil {
		t.Fatal(err)
	}
	// The transcript becomes something that cannot be opened for the
	// torn-tail check: a directory where the file was.
	path := s.turnsPath("20260821-100000-test")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("20260821-100000-test", turnsAt(ts, "a follow-up", "its answer")); err == nil {
		t.Fatal("an unopenable transcript reported success")
	}
}

func TestMetadataThatCannotBeReplacedIsRefusedAfterTheTurnsLand(t *testing.T) {
	const id = "20260821-100000-test"
	s := blockedStore(t, id)
	// A directory where the metadata file goes: the temp file is written and
	// the rename onto it fails, which is the atomic write's own failure.
	if err := os.MkdirAll(s.metaPath(id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.metaPath(id), "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	got, err := s.Append("", turnsAt(ts, "a question", "an answer"))
	if err == nil {
		t.Fatal("an unwritable metadata file reported success")
	}
	if !strings.Contains(err.Error(), "conversation metadata") {
		t.Errorf("error = %v, want it to name the metadata write", err)
	}
	if got != "" {
		t.Errorf("a refused write handed back the id %q", got)
	}
	// The transcript did land — the append comes first, by design — and the
	// listing reports the record as one whose metadata is missing rather
	// than hiding it.
	_, unreadable, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(unreadable) != 1 || unreadable[0].ID != id {
		t.Errorf("unreadable = %+v, want the half-written record reported", unreadable)
	}
}

func TestAnArchiveDirectoryThatCannotBeMadeIsRefused(t *testing.T) {
	// A file where the archive directory should be, so MkdirAll cannot.
	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &FileStore{Dir: filepath.Join(blocker, "conversations")}

	if _, err := s.Append("", []Turn{{Role: "user", Text: "a question"}}); err == nil {
		t.Error("an unmakeable archive directory reported a successful append")
	}
	if err := s.SetActive("20260821-100000-test"); err == nil {
		t.Error("an unmakeable archive directory reported a successful SetActive")
	}
	// And every reader answers honestly rather than pretending the archive
	// is empty: a directory that is not a directory is an error, where a
	// directory that is merely absent is a fresh install.
	if _, _, err := s.List(); err == nil {
		t.Error("listing an unreadable archive directory succeeded")
	}
	if _, _, err := s.Search(Query{Text: "anything"}); err == nil {
		t.Error("searching an unreadable archive directory succeeded")
	}
	if _, err := s.DeleteAll(); err == nil {
		t.Error("deleting all from an unreadable archive directory succeeded")
	}
}

func TestAConversationWhoseFilesWillNotGoIsReported(t *testing.T) {
	const id = "20260821-100000-test"
	s := blockedStore(t, id)
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	if _, err := s.Append("", turnsAt(ts, "a question", "an answer")); err != nil {
		t.Fatal(err)
	}
	// The metadata becomes a directory with something in it, which os.Remove
	// refuses — the deletion must say so rather than report a success that
	// leaves the record on disk.
	if err := os.Remove(s.metaPath(id)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.metaPath(id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.metaPath(id), "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(id); err == nil {
		t.Error("a deletion that could not remove the record reported success")
	}
}

// A wipe that cannot remove a file says so rather than reporting a count.
// "Delete every conversation" answered with a number while a transcript
// stayed on disk would be the worst possible lie this store could tell.
func TestAWipeThatCannotRemoveAFileSaysSo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which a read-only directory does not refuse")
	}
	s := blockedStore(t, "20260821-100000-test")
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	if _, err := s.Append("", turnsAt(ts, "a question", "an answer")); err != nil {
		t.Fatal(err)
	}
	// Readable, so the wipe can list what is there; not writable, so it
	// cannot unlink any of it.
	if err := os.Chmod(s.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	// Restored before the test framework tries to clean the directory up.
	t.Cleanup(func() { _ = os.Chmod(s.Dir, 0o700) })

	if n, err := s.DeleteAll(); err == nil {
		t.Errorf("delete-all reported %d removed over files it could not remove", n)
	}
	if err := s.Delete("20260821-100000-test"); err == nil {
		t.Error("a deletion that could not unlink anything reported success")
	}
}

func TestAConversationWhoseFilesCannotBeReadIsAnError(t *testing.T) {
	const id = "20260821-100000-test"
	s := blockedStore(t, id)
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	if _, err := s.Append("", turnsAt(ts, "a question", "an answer")); err != nil {
		t.Fatal(err)
	}
	// The transcript becomes a directory: readable metadata, unreadable
	// turns. Read must not answer with the metadata alone.
	if err := os.Remove(s.turnsPath(id)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.turnsPath(id), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(id); err == nil {
		t.Error("reading a conversation with an unreadable transcript succeeded")
	}
	// And the metadata too: an error that is not "no such file" must not be
	// mistaken for a conversation that does not exist.
	if err := os.Remove(s.metaPath(id)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.metaPath(id), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(id); err == nil {
		t.Error("reading a conversation with unreadable metadata succeeded")
	}
}

// An append whose write fails on a conversation that ALREADY exists gives
// back the id it was handed. Nothing new is claimed by that — the caller
// already had it — and it is the counterpart of a failed create, which
// answers "" because the conversation it would have named is not there.
func TestAFailedAppendToAnExistingConversationKeepsItsID(t *testing.T) {
	const id = "20260821-100000-test"
	s := blockedStore(t, id)
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	if _, err := s.Append("", turnsAt(ts, "a question", "an answer")); err != nil {
		t.Fatal(err)
	}
	s.write = func(diskWrite) error { return os.ErrPermission }
	got, err := s.Append(id, turnsAt(ts.Add(time.Minute), "a follow-up", "its answer"))
	if err == nil {
		t.Fatal("a refused append reported success")
	}
	if got != id {
		t.Errorf("append to %q came back as %q; the caller's own id must survive a refusal", id, got)
	}
}

// A torn tail longer than the repair's scan block still gets cut back: the
// scan walks the file backwards a block at a time, which is what keeps a
// transcript of any size from being read into memory to repair its last
// line.
func TestATornTailLongerThanAScanBlockIsStillTrimmed(t *testing.T) {
	const id = "20260821-100000-test"
	s := blockedStore(t, id)
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	if _, err := s.Append("", turnsAt(ts, "the question that survived", "the answer that survived")); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.turnsPath(id), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Ninety kilobytes of a turn line, and no newline: three blocks' worth.
	if _, err := f.WriteString(`{"role":"user","text":"` + strings.Repeat("x", 90*1024)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Append(id, turnsAt(ts.Add(time.Minute), "a question afterwards",
		"an answer afterwards")); err != nil {
		t.Fatal(err)
	}
	conv, err := s.Read(id)
	if err != nil {
		t.Fatalf("the long torn tail cost the conversation: %v", err)
	}
	if len(conv.Turns) != 4 {
		t.Errorf("turns = %d, want 4", len(conv.Turns))
	}
}

// The active pointer is a convenience, never a record: every way it can be
// wrong degrades to "no active conversation" rather than to an error or, far
// worse, to a path that escapes the archive directory.
func TestAnActivePointerThatCannotBeTrustedReadsAsNone(t *testing.T) {
	s := blockedStore(t, "20260821-100000-test")
	pointer := filepath.Join(s.Dir, activeFile)
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"../../etc/passwd\n", ".hidden\n", "  \n"} {
		if err := os.WriteFile(pointer, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := s.Active(); got != "" {
			t.Errorf("active pointer %q resolved to %q, want none", strings.TrimSpace(content), got)
		}
	}
	// A well-formed id naming a conversation that is gone is stale, not
	// active.
	if err := os.WriteFile(pointer, []byte("20260821-100000-gone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.Active(); got != "" {
		t.Errorf("a stale pointer resolved to %q", got)
	}
}

// A store with no injected clock stamps turns with the real one, which is
// the shape the daemon actually constructs.
func TestAStoreWithoutAnInjectedClockStampsTurns(t *testing.T) {
	s := &FileStore{Dir: t.TempDir()}
	before := time.Now()
	id, err := s.Append("", []Turn{{Role: "user", Text: "a question with no timestamp"}})
	if err != nil {
		t.Fatal(err)
	}
	conv, err := s.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.Turns) != 1 || conv.Turns[0].Time.Before(before) {
		t.Errorf("turn = %+v, want one stamped at or after %v", conv.Turns, before)
	}
	if conv.Meta.Started.IsZero() || conv.Meta.LastActive.IsZero() {
		t.Errorf("metadata timestamps = %+v, want both filled in", conv.Meta)
	}
}

// DeleteAll ignores what is not a conversation. The archive directory is the
// state directory's, and a stray file in it — an editor's backup, a temp
// file an interrupted write left — is not the user's to lose to a wipe of
// their conversations.
func TestDeleteAllLeavesWhatIsNotAConversation(t *testing.T) {
	s := blockedStore(t, "20260821-100000-test")
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	if _, err := s.Append("", turnsAt(ts, "a question", "an answer")); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(s.Dir, "notes.txt")
	if err := os.WriteFile(stray, []byte("something else entirely"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(s.Dir, "a-directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteAll()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want the one conversation", n)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("delete-all removed a file that was not a conversation: %v", err)
	}
	// And a wipe of an already-empty archive is a quiet zero.
	if n, err := s.DeleteAll(); err != nil || n != 0 {
		t.Errorf("second delete-all = %d/%v, want 0 and no error", n, err)
	}
}

// A preview cut in the middle of a multi-byte character would corrupt the
// JSON the metadata is written as, so the cut lands on a rune boundary — and
// still says it happened.
func TestAPreviewOfNonASCIISpeechIsCutOnARuneBoundary(t *testing.T) {
	s := blockedStore(t, "20260821-100000-test")
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	// Three-byte runes, so the cap falls inside one.
	long := strings.Repeat("日", 200)
	id, err := s.Append("", turnsAt(ts, long, "an answer"))
	if err != nil {
		t.Fatal(err)
	}
	conv, err := s.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	preview := strings.TrimSuffix(conv.Meta.Preview, "…")
	if !strings.HasSuffix(conv.Meta.Preview, "…") {
		t.Errorf("the cut preview gives no sign it was cut: %q", conv.Meta.Preview)
	}
	if len(preview)%3 != 0 || !strings.HasPrefix(long, preview) {
		t.Errorf("preview %q is not a whole-rune prefix of what was said", preview)
	}
}

// clipPassage takes the hit's offset from the FOLDED text, and lowercasing a
// few rare runes changes their width — İ becomes two runes. An offset that
// no longer lands inside the original must clip from the start rather than
// slice out of range.
func TestAPassageClipsFromTheStartWhenFoldingMovedTheOffset(t *testing.T) {
	s := searchStore(t)
	base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	// Enough İ that the folded offset of a late hit overruns the original.
	text := strings.Repeat("İ", 400) + " the needle"
	seedConversation(t, s, "conv-turkish", base, text)

	matches, _, err := s.Search(Query{Text: "needle", PassageRunes: 60})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %+v, want the one hit", matches)
	}
	if runes := []rune(matches[0].Passage); len(runes) > 62 {
		t.Errorf("passage overran its cap: %d runes", len(runes))
	}
}

// The Fake's notification channel is optional and never blocks: a Fake with
// no channel works, and one whose channel nobody is draining works too. Both
// matter because the archive is driven from the session tail, which must not
// be held up by a test that stopped listening.
func TestTheFakeNeverBlocksOnItsNotifications(t *testing.T) {
	quiet := NewFake()
	quiet.Ops = nil
	if _, err := quiet.Append("", turnsAt(time.Now(), "a question", "an answer")); err != nil {
		t.Fatal(err)
	}

	full := NewFake()
	for range cap(full.Ops) {
		full.Ops <- "filler"
	}
	if _, err := full.Append("", turnsAt(time.Now(), "a question", "an answer")); err != nil {
		t.Fatal(err)
	}
	if got := full.Turns("no-such-conversation"); got != nil {
		t.Errorf("the Fake produced turns for an unknown id: %+v", got)
	}
}

// A preview is the first line of the first user turn that HAS one. A turn
// the recogniser heard as silence, or one the user opened with a blank line,
// must not make a conversation unrecognisable in the listing.
func TestAPreviewSkipsTurnsWithNothingInThem(t *testing.T) {
	s := blockedStore(t, "20260821-100000-test")
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	id, err := s.Append("", []Turn{
		{Role: "user", Text: "   \n  ", Time: ts},
		{Role: "assistant", Text: "an answer to nothing", Time: ts},
		{Role: "user", Text: "the question worth showing", Time: ts},
	})
	if err != nil {
		t.Fatal(err)
	}
	conv, err := s.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	if conv.Meta.Preview != "the question worth showing" {
		t.Errorf("preview = %q, want the first user turn that said something", conv.Meta.Preview)
	}

	// And a conversation with nothing sayable in it previews as nothing,
	// rather than as whitespace pretending to be a sentence.
	silent, err := s.Append("", []Turn{{Role: "assistant", Text: "an answer nobody asked for", Time: ts}})
	if err != nil {
		t.Fatal(err)
	}
	quiet, err := s.Read(silent)
	if err != nil {
		t.Fatal(err)
	}
	if quiet.Meta.Preview != "" {
		t.Errorf("preview = %q, want none", quiet.Meta.Preview)
	}
}

// Deleting a conversation the live head does NOT belong to leaves the active
// pointer where it is. The pointer is cleared only when it named the record
// that went — deleting an old conversation must not detach the one the user
// is in the middle of.
func TestDeletingAnotherConversationLeavesTheActivePointerAlone(t *testing.T) {
	dir := t.TempDir()
	n := 0
	s := &FileStore{Dir: dir, NewID: func(time.Time) string {
		n++
		return fmt.Sprintf("20260821-10000%d-test", n)
	}}
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	old, err := s.Append("", turnsAt(ts, "the old thread", "its answer"))
	if err != nil {
		t.Fatal(err)
	}
	live, err := s.Append("", turnsAt(ts.Add(time.Hour), "the live thread", "its answer"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(old); err != nil {
		t.Fatal(err)
	}
	if got := s.Active(); got != live {
		t.Errorf("active = %q after deleting another conversation, want %q", got, live)
	}

	// An id that could escape the archive directory is refused before it
	// reaches the filesystem, whichever verb it arrives at.
	for _, bad := range []string{"", ".hidden", "../escape", "a/b"} {
		if err := s.SetActive(bad); err == nil {
			t.Errorf("SetActive accepted %q", bad)
		}
		if _, err := s.Read(bad); err == nil {
			t.Errorf("Read accepted %q", bad)
		}
		if err := s.Delete(bad); err == nil {
			t.Errorf("Delete accepted %q", bad)
		}
	}
}
