package conversations

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/provenance"
)

// update regenerates the golden files: go test ./internal/conversations -update
var update = flag.Bool("update", false, "rewrite golden files")

// fixedStore returns a FileStore with a frozen clock and deterministic ids,
// so every byte it writes is reproducible.
func fixedStore(t *testing.T) *FileStore {
	t.Helper()
	base := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	n := 0
	return &FileStore{
		Dir: t.TempDir(),
		Now: func() time.Time { return base },
		NewID: func(time.Time) string {
			n++
			return fmt.Sprintf("20260821-1000%02d-test", n)
		},
	}
}

// turnsAt builds a user+assistant exchange stamped at ts.
func turnsAt(ts time.Time, question, answer string) []Turn {
	return []Turn{
		{Role: "user", Text: question, Time: ts},
		{Role: "assistant", Text: answer, Time: ts},
	}
}

func TestRoundTrip(t *testing.T) {
	s := fixedStore(t)
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)

	id, err := s.Append("", turnsAt(ts, "why is my build failing?", "Your linker is out of date."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(id, turnsAt(ts.Add(time.Minute), "how do I fix it?", "Update binutils.")); err != nil {
		t.Fatal(err)
	}

	conv, err := s.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	if conv.Meta.ID != id || conv.Meta.TurnCount != 4 {
		t.Errorf("meta = %+v, want id %q with 4 turns", conv.Meta, id)
	}
	if !conv.Meta.Started.Equal(ts) || !conv.Meta.LastActive.Equal(ts.Add(time.Minute)) {
		t.Errorf("timestamps = %v / %v", conv.Meta.Started, conv.Meta.LastActive)
	}
	if conv.Meta.Preview != "why is my build failing?" {
		t.Errorf("preview = %q", conv.Meta.Preview)
	}
	want := []Turn{
		{Role: "user", Text: "why is my build failing?", Time: ts},
		{Role: "assistant", Text: "Your linker is out of date.", Time: ts},
		{Role: "user", Text: "how do I fix it?", Time: ts.Add(time.Minute)},
		{Role: "assistant", Text: "Update binutils.", Time: ts.Add(time.Minute)},
	}
	if len(conv.Turns) != len(want) {
		t.Fatalf("read %d turns, want %d", len(conv.Turns), len(want))
	}
	for i, turn := range conv.Turns {
		if turn.Role != want[i].Role || turn.Text != want[i].Text || !turn.Time.Equal(want[i].Time) {
			t.Errorf("turn %d = %+v, want %+v", i, turn, want[i])
		}
	}
}

// The interrupted flag (issue #117) is additive: it round-trips when set and
// leaves every completed turn's line byte-identical when not. The golden test
// below already proves the second half — these files carry no interrupted
// turn and must not change — so this test covers the flag's own trip and the
// omitempty pin.
func TestInterruptedFlagRoundTrips(t *testing.T) {
	s := fixedStore(t)
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	id, err := s.Append("", []Turn{
		{Role: "user", Text: "what's on my calendar tomorrow?", Time: ts, Interrupted: true},
		{Role: "assistant", Text: "Do you mean tomorrow, or the whole week?", Time: ts, Interrupted: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(id, turnsAt(ts.Add(time.Minute), "tomorrow", "One meeting, at ten.")); err != nil {
		t.Fatal(err)
	}
	conv, err := s.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.Turns) != 4 {
		t.Fatalf("read %d turns, want 4", len(conv.Turns))
	}
	if !conv.Turns[0].Interrupted || !conv.Turns[1].Interrupted {
		t.Errorf("interrupted flags lost on the cut exchange: %+v", conv.Turns[:2])
	}
	if conv.Turns[2].Interrupted || conv.Turns[3].Interrupted {
		t.Errorf("interrupted flags leaked onto the completed exchange: %+v", conv.Turns[2:])
	}
	// omitempty is the compatibility mechanism: a completed turn's line must
	// not so much as mention the key, so archives written after #117 read
	// identically to ones written before it wherever nothing was interrupted.
	raw, err := os.ReadFile(s.turnsPath(id))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, line := range lines[3:] { // header + the two flagged turns precede
		if strings.Contains(line, "interrupted") {
			t.Errorf("completed turn's line carries the key: %s", line)
		}
	}
}

// An archive written before the interrupted flag existed loads clean: the
// missing key reads as false, the schema version still matches, and nothing
// is reported unreadable. This is the promise that let the flag ship without
// a schema bump.
func TestOldArchiveWithoutInterruptedKeyLoadsClean(t *testing.T) {
	s := fixedStore(t)
	id := "20260820-090000-old"
	// Byte-for-byte the pre-#117 format, straight from the golden files.
	transcript := `{"schema":1,"id":"` + id + `"}
{"role":"user","text":"an old question","ts":"2026-08-20T09:00:00Z"}
{"role":"assistant","text":"An old answer.","ts":"2026-08-20T09:00:00Z"}
`
	meta := `{"schema":1,"id":"` + id + `","started":"2026-08-20T09:00:00Z","last_active":"2026-08-20T09:00:00Z","turns":2,"preview":"an old question"}`
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.turnsPath(id), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.metaPath(id), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}

	conv, err := s.Read(id)
	if err != nil {
		t.Fatalf("old archive did not load: %v", err)
	}
	if len(conv.Turns) != 2 {
		t.Fatalf("read %d turns, want 2", len(conv.Turns))
	}
	for i, turn := range conv.Turns {
		if turn.Interrupted {
			t.Errorf("turn %d read as interrupted from a file without the key", i)
		}
	}
	if _, unreadable, err := s.List(); err != nil || len(unreadable) != 0 {
		t.Errorf("old archive listed as unreadable: %v %v", unreadable, err)
	}
}

// A confirmation record (issue #118) is a turn like any other to the store:
// it round-trips whole — role, question, payload, timestamp — at its position
// between the halves of its exchange, and its keys never leak onto an
// utterance's line (the omitempty pin, exactly as for interrupted).
func TestConfirmationRecordRoundTrips(t *testing.T) {
	s := fixedStore(t)
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	record := &Confirmation{
		Tool:       "shell.run",
		Command:    "rm -rf ./build",
		Rule:       "shell fallback",
		Outcome:    ConfirmationApproved,
		Source:     "cli",
		TimeoutSec: 30,
	}
	id, err := s.Append("", []Turn{
		{Role: "user", Text: "clean the build dir", Time: ts},
		{Role: RoleConfirmation, Text: "Should I run rm -rf ./build?", Time: ts, Confirmation: record},
		{Role: "assistant", Text: "Build directory removed.", Time: ts},
	})
	if err != nil {
		t.Fatal(err)
	}
	conv, err := s.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.Turns) != 3 {
		t.Fatalf("read %d turns, want 3", len(conv.Turns))
	}
	rec := conv.Turns[1]
	if rec.Role != RoleConfirmation || rec.Text != "Should I run rm -rf ./build?" {
		t.Errorf("record turn = %+v, want the confirmation between the exchange's halves", rec)
	}
	if rec.Confirmation == nil {
		t.Fatal("confirmation payload lost on the round trip")
	}
	if *rec.Confirmation != *record {
		t.Errorf("payload = %+v, want %+v", *rec.Confirmation, *record)
	}
	if conv.Turns[0].Confirmation != nil || conv.Turns[2].Confirmation != nil {
		t.Error("confirmation payload leaked onto an utterance")
	}
	// The utterances' lines must not so much as mention the key — the same
	// compatibility mechanism the interrupted flag pinned: archives written
	// after #118 read identically to ones written before it wherever no
	// confirmation happened.
	raw, err := os.ReadFile(s.turnsPath(id))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, line := range []string{lines[1], lines[3]} { // the user and assistant turns
		if strings.Contains(line, "confirmation") {
			t.Errorf("utterance's line carries the key: %s", line)
		}
	}
	if conv.Meta.Preview != "clean the build dir" {
		t.Errorf("preview = %q; a record must never become the preview", conv.Meta.Preview)
	}
}

// An archive written before confirmation records existed loads clean: no key,
// no payload, nothing unreadable, and the schema version still matches — the
// promise that lets the record ship without a schema bump (the #117
// precedent, and this issue's old-archive compatibility criterion).
func TestOldArchiveWithoutConfirmationRecordsLoadsClean(t *testing.T) {
	s := fixedStore(t)
	id := "20260820-091500-old"
	// Byte-for-byte the pre-#118 format, straight from the golden files.
	transcript := `{"schema":1,"id":"` + id + `"}
{"role":"user","text":"an old question","ts":"2026-08-20T09:15:00Z"}
{"role":"assistant","text":"An old answer.","ts":"2026-08-20T09:15:00Z"}
`
	meta := `{"schema":1,"id":"` + id + `","started":"2026-08-20T09:15:00Z","last_active":"2026-08-20T09:15:00Z","turns":2,"preview":"an old question"}`
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.turnsPath(id), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.metaPath(id), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}

	conv, err := s.Read(id)
	if err != nil {
		t.Fatalf("old archive did not load: %v", err)
	}
	if len(conv.Turns) != 2 {
		t.Fatalf("read %d turns, want 2", len(conv.Turns))
	}
	for i, turn := range conv.Turns {
		if turn.Confirmation != nil {
			t.Errorf("turn %d read a confirmation from a file without the key", i)
		}
	}
	if _, unreadable, err := s.List(); err != nil || len(unreadable) != 0 {
		t.Errorf("old archive listed as unreadable: %v %v", unreadable, err)
	}
}

// TestGoldenFiles pins the on-disk schema byte for byte: two files per
// conversation, a schema version in both, and role/text/timestamp per turn.
// If this test breaks, the search ticket's index and every future reader
// break with it — change the schema only with a version bump and a decision.
func TestGoldenFiles(t *testing.T) {
	s := fixedStore(t)
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	id, err := s.Append("", turnsAt(ts, "what is on my calendar?", "Two meetings, both this morning."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(id, turnsAt(ts.Add(time.Minute), "move the second one", "Done — it is at three now.")); err != nil {
		t.Fatal(err)
	}

	for _, file := range []struct{ got, golden string }{
		{s.turnsPath(id), filepath.Join("testdata", "conversation.jsonl")},
		{s.metaPath(id), filepath.Join("testdata", "conversation.meta.json")},
	} {
		got, err := os.ReadFile(file.got)
		if err != nil {
			t.Fatal(err)
		}
		if *update {
			if err := os.WriteFile(file.golden, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(file.golden)
		if err != nil {
			t.Fatalf("missing golden file (run with -update): %v", err)
		}
		if string(got) != string(want) {
			t.Errorf("%s does not match its golden file:\ngot:  %s\nwant: %s",
				file.got, got, want)
		}
	}
}

func TestListNewestFirstWithoutReadingTranscripts(t *testing.T) {
	s := fixedStore(t)
	base := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)

	// A corpus big enough that accidentally parsing every transcript would be
	// a design change, not noise.
	const corpus = 150
	ids := make([]string, 0, corpus)
	for i := 0; i < corpus; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		id, err := s.Append("", turnsAt(ts, fmt.Sprintf("question %d", i), "an answer"))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// The proof that listing reads only metadata: destroy every transcript.
	// If List ever opens one — to count turns, say — this corpus makes it
	// fail loudly instead of quietly costing a read per conversation.
	for _, id := range ids {
		if err := os.WriteFile(s.turnsPath(id), []byte("not json at all"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	metas, unreadable, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(unreadable) != 0 {
		t.Fatalf("listing consulted transcripts: %v", unreadable)
	}
	if len(metas) != corpus {
		t.Fatalf("listed %d conversations, want %d", len(metas), corpus)
	}
	for i := 1; i < len(metas); i++ {
		if metas[i].LastActive.After(metas[i-1].LastActive) {
			t.Fatalf("listing is not newest-first at %d: %v after %v",
				i, metas[i].LastActive, metas[i-1].LastActive)
		}
	}
	if metas[0].Preview != fmt.Sprintf("question %d", corpus-1) {
		t.Errorf("newest preview = %q", metas[0].Preview)
	}
}

func TestCorruptMetadataIsReportedAndTheRestStillList(t *testing.T) {
	s := fixedStore(t)
	ts := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	good, err := s.Append("", turnsAt(ts, "a fine conversation", "yes"))
	if err != nil {
		t.Fatal(err)
	}
	bad, err := s.Append("", turnsAt(ts.Add(time.Minute), "a doomed conversation", "alas"))
	if err != nil {
		t.Fatal(err)
	}
	// A torn or hand-mangled metadata document.
	if err := os.WriteFile(s.metaPath(bad), []byte(`{"schema":1,"id":`), 0o600); err != nil {
		t.Fatal(err)
	}

	metas, unreadable, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].ID != good {
		t.Fatalf("readable listing = %+v, want just %q", metas, good)
	}
	if len(unreadable) != 1 || unreadable[0].ID != bad {
		t.Fatalf("unreadable = %+v, want %q reported", unreadable, bad)
	}
	if unreadable[0].Err == "" {
		t.Error("unreadable conversation carries no reason")
	}
	// Contents never leak into the error.
	if strings.Contains(unreadable[0].Err, "doomed") {
		t.Errorf("error leaks conversation content: %q", unreadable[0].Err)
	}
}

func TestUnsupportedSchemaVersionIsUnreadable(t *testing.T) {
	s := fixedStore(t)
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := `{"schema":99,"id":"future","started":"2026-08-21T10:00:00Z","last_active":"2026-08-21T10:00:00Z","turns":2,"preview":"hi"}`
	if err := os.WriteFile(filepath.Join(s.Dir, "future.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	_, unreadable, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(unreadable) != 1 || !strings.Contains(unreadable[0].Err, "version 99") {
		t.Fatalf("unreadable = %+v, want a version complaint", unreadable)
	}
}

func TestOrphanTranscriptIsReported(t *testing.T) {
	s := fixedStore(t)
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "lonely.jsonl"), []byte(`{"schema":1,"id":"lonely"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, unreadable, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(unreadable) != 1 || unreadable[0].ID != "lonely" {
		t.Fatalf("unreadable = %+v, want the orphan transcript reported", unreadable)
	}
}

func TestTornFinalLineIsTolerated(t *testing.T) {
	s := fixedStore(t)
	ts := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	id, err := s.Append("", turnsAt(ts, "the question", "the answer"))
	if err != nil {
		t.Fatal(err)
	}
	// Power died mid-append: half a JSON line at the end of the transcript.
	f, err := os.OpenFile(s.turnsPath(id), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"role":"user","tex`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	conv, err := s.Read(id)
	if err != nil {
		t.Fatalf("a torn final line cost the whole conversation: %v", err)
	}
	if conv.Meta.TurnCount != 2 || len(conv.Turns) != 2 {
		t.Errorf("read %d turns (meta says %d), want the 2 intact ones",
			len(conv.Turns), conv.Meta.TurnCount)
	}
}

func TestCorruptionMidFileFailsTheRead(t *testing.T) {
	s := fixedStore(t)
	ts := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	id, err := s.Append("", turnsAt(ts, "first", "answer one"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.turnsPath(id))
	if err != nil {
		t.Fatal(err)
	}
	mangled := strings.Replace(string(data), `{"role":"user"`, `{"role":`, 1)
	if err := os.WriteFile(s.turnsPath(id), []byte(mangled), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(id); err == nil {
		t.Fatal("mid-file corruption read back as a healthy conversation")
	}
}

func TestDeleteActuallyDeletes(t *testing.T) {
	s := fixedStore(t)
	ts := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	id, err := s.Append("", turnsAt(ts, "delete me", "as you wish"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(id); err != nil {
		t.Fatal(err)
	}
	// The proof is the state directory, not the API: nothing of the
	// conversation may remain on disk — no tombstone, no orphan, no pointer.
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), id) {
			t.Errorf("deletion left %s behind", entry.Name())
		}
	}
	if got := s.Active(); got != "" {
		t.Errorf("active pointer survived the deletion: %q", got)
	}
	if err := s.Delete(id); err == nil {
		t.Error("deleting an absent conversation reported success")
	}
}

func TestDeleteAll(t *testing.T) {
	s := fixedStore(t)
	ts := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if _, err := s.Append("", turnsAt(ts.Add(time.Duration(i)*time.Minute), "q", "a")); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.DeleteAll()
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("deleted %d conversations, want 3", n)
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("delete --all left %d entries in the state directory", len(entries))
	}
}

func TestActivePointerFollowsAppendsAndSetActive(t *testing.T) {
	s := fixedStore(t)
	ts := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	first, err := s.Append("", turnsAt(ts, "one", "1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Active(); got != first {
		t.Fatalf("active = %q after first append, want %q", got, first)
	}
	second, err := s.Append("", turnsAt(ts.Add(time.Minute), "two", "2"))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Active(); got != second {
		t.Fatalf("active = %q after second conversation, want %q", got, second)
	}
	if err := s.SetActive(first); err != nil {
		t.Fatal(err)
	}
	if got := s.Active(); got != first {
		t.Fatalf("active = %q after SetActive, want %q", got, first)
	}
}

func TestAppendToDeletedIDStartsFresh(t *testing.T) {
	s := fixedStore(t)
	ts := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	id, err := s.Append("", turnsAt(ts, "short-lived", "indeed"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(id); err != nil {
		t.Fatal(err)
	}
	// The daemon may still hold the old id in memory; its next append must
	// not resurrect the deleted record under the deleted name.
	landed, err := s.Append(id, turnsAt(ts.Add(time.Minute), "still here?", "in a new conversation"))
	if err != nil {
		t.Fatal(err)
	}
	if landed == id {
		t.Fatal("append recreated a deleted conversation id")
	}
	if _, err := s.Read(id); err == nil {
		t.Error("the deleted conversation came back")
	}
}

func TestIDsThatEscapeTheDirectoryAreRefused(t *testing.T) {
	s := fixedStore(t)
	for _, id := range []string{"", "../history", "a/b", `a\b`, ".hidden"} {
		if _, err := s.Read(id); err == nil {
			t.Errorf("Read(%q) accepted a hostile id", id)
		}
		if err := s.Delete(id); err == nil {
			t.Errorf("Delete(%q) accepted a hostile id", id)
		}
	}
}

func TestFilesArePrivate(t *testing.T) {
	s := fixedStore(t)
	ts := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	id, err := s.Append("", turnsAt(ts, "a private thing", "kept private"))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(s.Dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("conversations dir mode = %v (%v), want 0700", info.Mode().Perm(), err)
	}
	for _, path := range []string{s.turnsPath(id), s.metaPath(id), filepath.Join(s.Dir, activeFile)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", path, info.Mode().Perm())
		}
	}
}

func TestPreviewIsFirstLineAndCapped(t *testing.T) {
	s := fixedStore(t)
	ts := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	long := strings.Repeat("wordy ", 40) + "\nsecond line"
	id, err := s.Append("", turnsAt(ts, long, "short answer"))
	if err != nil {
		t.Fatal(err)
	}
	conv, err := s.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(conv.Meta.Preview, "second line") {
		t.Errorf("preview crossed a line break: %q", conv.Meta.Preview)
	}
	if len(conv.Meta.Preview) > PreviewChars {
		t.Errorf("preview is %d chars, cap is %d", len(conv.Meta.Preview), PreviewChars)
	}
}

func TestEmptyDirectoryListsEmpty(t *testing.T) {
	s := &FileStore{Dir: filepath.Join(t.TempDir(), "never-created")}
	metas, unreadable, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 0 || len(unreadable) != 0 {
		t.Errorf("fresh install listed %d/%d conversations", len(metas), len(unreadable))
	}
	if got := s.Active(); got != "" {
		t.Errorf("fresh install has active conversation %q", got)
	}
}

// TestProvenanceRoundTripsAndStaysOffEveryOtherTurn is #168's additive
// criterion: what went into an answer survives the archive whole, and the key
// never appears on a turn that has none — which is what keeps every line
// already on disk byte-identical and the golden files untouched.
func TestProvenanceRoundTripsAndStaysOffEveryOtherTurn(t *testing.T) {
	s := fixedStore(t)
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	record := &provenance.Record{
		Sources: []provenance.Reference{
			{Kind: provenance.KindFact, Strength: provenance.Available, Ref: "m3"},
			{Kind: provenance.KindTool, Strength: provenance.Returned,
				Tool: "shell.run", Subject: "git status"},
		},
		Truncated: 2,
	}
	turns := []Turn{
		{Role: "user", Text: "what is the deploy host?", Time: ts},
		{Role: "assistant", Text: "It is atlas.", Time: ts, Provenance: record},
		{Role: "user", Text: "thanks", Time: ts},
		{Role: "assistant", Text: "Any time.", Time: ts},
	}
	id, err := s.Append("", turns)
	if err != nil {
		t.Fatal(err)
	}

	conv, err := s.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	got := conv.Turns[1].Provenance
	if got == nil || len(got.Sources) != 2 || got.Truncated != 2 {
		t.Fatalf("provenance did not round-trip: %+v", got)
	}
	if got.Sources[0].Ref != "m3" || got.Sources[0].Strength != provenance.Available {
		t.Errorf("first source = %+v", got.Sources[0])
	}
	if got.Sources[1].Tool != "shell.run" || got.Sources[1].Subject != "git status" {
		t.Errorf("second source = %+v", got.Sources[1])
	}
	if conv.Turns[3].Provenance != nil {
		t.Errorf("a turn that consumed nothing carried provenance: %+v", conv.Turns[3].Provenance)
	}

	// omitempty is the compatibility mechanism, exactly as it is for
	// interrupted and confirmation: a turn without provenance must not so
	// much as mention the key.
	raw, err := os.ReadFile(s.turnsPath(id))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for i, line := range lines {
		if i == 2 { // header + user turn precede the answer that has provenance
			continue
		}
		if strings.Contains(line, "provenance") {
			t.Errorf("line %d carries the key without the fact: %s", i, line)
		}
	}
	// References, never content: nothing a fact said is in the file.
	if strings.Contains(string(raw), "atlas.example") {
		t.Error("provenance carried content into the archive")
	}
}

// TestOldArchiveWithoutProvenanceLoadsClean: an archive written before #168
// reads with no provenance, nothing unreadable, and the same schema version —
// the promise that lets the field ship without a bump.
func TestOldArchiveWithoutProvenanceLoadsClean(t *testing.T) {
	s := fixedStore(t)
	id := "20260820-091500-old"
	// Byte-for-byte the pre-#168 format, straight from the golden files.
	transcript := `{"schema":1,"id":"` + id + `"}
{"role":"user","text":"an old question","ts":"2026-08-20T09:15:00Z"}
{"role":"assistant","text":"An old answer.","ts":"2026-08-20T09:15:00Z"}
`
	meta := `{"schema":1,"id":"` + id + `","started":"2026-08-20T09:15:00Z","last_active":"2026-08-20T09:15:00Z","turns":2,"preview":"an old question"}`
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.turnsPath(id), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.metaPath(id), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}

	conv, err := s.Read(id)
	if err != nil {
		t.Fatalf("old archive did not load: %v", err)
	}
	if len(conv.Turns) != 2 {
		t.Fatalf("read %d turns, want 2", len(conv.Turns))
	}
	for i, turn := range conv.Turns {
		if turn.Provenance != nil {
			t.Errorf("turn %d read provenance from a file without the key", i)
		}
	}
	if _, unreadable, err := s.List(); err != nil || len(unreadable) != 0 {
		t.Errorf("old archive listed as unreadable: %v %v", unreadable, err)
	}
}
