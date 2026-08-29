package conversations

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The archive's own hazards, as opposed to the ones every durable store
// shares (those are in faults_test.go, driven by internal/storefault).
//
// Three things make this store different and put the risk somewhere else.
// It is append-mostly rather than rewritten whole, so its writes are not
// atomic and a torn line is a state the file can really be in. It is
// unbounded, so a reader that loads what it reads is a reader that stops
// working at some size nobody will notice until they have years of history.
// And its search runs without the store's lock on purpose, so its readers
// meet writes in flight by design rather than by accident.

// TestAnAppendNeverLandsBehindATornLine is the defect this ticket's fault
// injection found, pinned at the mechanism (issue #173).
//
// Both readers tolerate a torn LAST line — appends land whole lines, so the
// line being written when the machine died is the only one that can be half
// there — and both treat a bad line anywhere EARLIER as corruption. Read
// refuses the conversation outright; the search scanner keeps what it found
// before the damage and stops.
//
// Those two rules are right on their own and lethal together, because a
// failed append leaves exactly the state that turns one into the other: half
// a line at the end of the file. The next successful append writes behind
// it, the torn line is no longer last, and a conversation that had lost one
// turn has now lost all of it. The engine's archive latch hides this within
// a process — the first failure stops it appending again — but it does not
// survive a restart, which is precisely when a machine that just died comes
// back.
//
// The fix is in the writer: a transcript that does not end in a newline is
// cut back to its last complete line before anything is appended. That is
// the same judgement the readers already make, taken by the only party that
// can still act on it, and it discards nothing a reader would have parsed.
func TestAnAppendNeverLandsBehindATornLine(t *testing.T) {
	s := fixedStore(t)
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	id, err := s.Append("", turnsAt(ts, "the question that survived", "the answer that survived"))
	if err != nil {
		t.Fatal(err)
	}

	// The disk dies halfway through the next append: some bytes of a turn
	// line reach the file, the newline never does.
	f, err := os.OpenFile(s.turnsPath(id), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"role":"user","text":"the half-written`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// The machine comes back and the conversation continues.
	if _, err := s.Append(id, turnsAt(ts.Add(time.Minute), "a question after the crash",
		"an answer after the crash")); err != nil {
		t.Fatal(err)
	}

	conv, err := s.Read(id)
	if err != nil {
		t.Fatalf("the torn line was buried and cost the whole conversation: %v", err)
	}
	if len(conv.Turns) != 4 {
		t.Fatalf("turns = %d, want the two before the crash and the two after: %+v",
			len(conv.Turns), conv.Turns)
	}
	if conv.Turns[0].Text != "the question that survived" ||
		conv.Turns[3].Text != "an answer after the crash" {
		t.Errorf("the surviving turns are not the ones that were written: %+v", conv.Turns)
	}
	for _, turn := range conv.Turns {
		if strings.Contains(turn.Text, "half-written") {
			t.Errorf("the torn line was adopted as a turn: %+v", turn)
		}
	}

	// Search sees exactly the same file, and reports nothing skipped.
	matches, stats, err := s.Search(Query{Text: "after the crash"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Skipped) != 0 {
		t.Errorf("search reported damage in a repaired transcript: %+v", stats.Skipped)
	}
	if len(matches) != 2 {
		t.Errorf("search over the repaired transcript found %d matches, want 2", len(matches))
	}
}

// A transcript torn so badly that not even its header line is whole is not a
// transcript, and the next append starts one rather than writing turns under
// a header that will never parse.
func TestAnAppendRestartsATranscriptWhoseHeaderWasTorn(t *testing.T) {
	s := fixedStore(t)
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	id, err := s.Append("", turnsAt(ts, "the first question", "the first answer"))
	if err != nil {
		t.Fatal(err)
	}
	// Everything after the first few bytes of the header is gone.
	if err := os.WriteFile(s.turnsPath(id), []byte(`{"schema":1,"i`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Append(id, turnsAt(ts.Add(time.Minute), "a question afterwards",
		"an answer afterwards")); err != nil {
		t.Fatal(err)
	}
	conv, err := s.Read(id)
	if err != nil {
		t.Fatalf("the conversation did not come back: %v", err)
	}
	if len(conv.Turns) != 2 || conv.Turns[0].Text != "a question afterwards" {
		t.Errorf("turns after the header repair = %+v, want the two written afterwards", conv.Turns)
	}
}

// largeArchive writes conversations × turns turns, one planted phrase, and
// returns the store, the id holding the plant, and the archive's size on
// disk.
func largeArchive(t *testing.T, conversations, turns int) (*FileStore, string, int64) {
	t.Helper()
	s := &FileStore{Dir: t.TempDir()}
	base := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	planted := ""
	for c := range conversations {
		texts := make([]string, 0, turns)
		for i := range turns {
			// Every turn carries the common word, so the scan has to look at
			// all of them; one turn in one conversation carries the phrase.
			text := fmt.Sprintf("turn %d of conversation %d, ordinary archive filler about the work", i, c)
			if c == conversations/2 && i == turns/2 {
				text += " and the pelican crossing decision"
			}
			texts = append(texts, text)
		}
		id := fmt.Sprintf("20260801-0900%02d-c%03d", c%60, c)
		seedConversation(t, s, id, base.Add(time.Duration(c)*time.Hour), texts...)
		if c == conversations/2 {
			planted = id
		}
	}
	var size int64
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		size += info.Size()
	}
	return s, planted, size
}

// TestALargeArchiveIsSearchedCorrectlyAndInBoundedMemory is the growth case.
//
// Correctness first: a phrase said once, four thousand turns ago, in the
// middle of a library, comes back with the conversation and turn number that
// would let a client open it and land on the spot. A search that gets
// slower is a nuisance; a search that quietly stops finding old things is
// the archive failing at the only job it has.
//
// Then memory. The scan streams — one bufio pass per transcript, one decoded
// turn alive at a time — and what that buys is that the heap after a search
// is the size of the RESULTS, not the size of the library. So the assertion
// is on what the search retains, measured against the archive it just read:
// a reader that loaded transcripts would retain something on the order of
// the archive, and this one retains a few capped passages.
func TestALargeArchiveIsSearchedCorrectlyAndInBoundedMemory(t *testing.T) {
	const conversations, turnsEach = 40, 100 // four thousand turns
	s, planted, size := largeArchive(t, conversations, turnsEach)

	matches, stats, err := s.Search(Query{Text: "pelican crossing decision"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Conversations != conversations {
		t.Errorf("searched %d conversations, want %d", stats.Conversations, conversations)
	}
	if len(stats.Skipped) != 0 {
		t.Errorf("a healthy archive reported skipped conversations: %+v", stats.Skipped)
	}
	if len(matches) != 1 {
		t.Fatalf("the planted phrase came back %d times, want once", len(matches))
	}
	hit := matches[0]
	if hit.ConversationID != planted || hit.Turn != turnsEach/2+1 || !hit.Phrase {
		t.Errorf("hit = %+v, want conversation %s turn %d as a phrase match",
			hit, planted, turnsEach/2+1)
	}
	if !strings.Contains(hit.Passage, "pelican crossing decision") {
		t.Errorf("the passage lost the hit: %q", hit.Passage)
	}

	// Bounded memory. A warm-up search first, so one-off allocations behind
	// the first scan are not charged to the measurement; then the heap that
	// is still live once the result is in hand.
	if _, _, err := s.Search(Query{Text: "ordinary archive filler", Limit: MaxSearchLimit}); err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	broad, broadStats, err := s.Search(Query{Text: "ordinary archive filler", Limit: MaxSearchLimit})
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(broad)
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	// An eighth of the archive is a wide margin around the real figure (the
	// capped passages are a few kilobytes against roughly a megabyte on
	// disk) and still an order of magnitude below what loading transcripts
	// would cost. The point is the shape of the number, not its value.
	if limit := size / 8; retained > limit {
		t.Errorf("a search over %d bytes of archive retained %d bytes, more than the %d-byte bound; "+
			"the scan is holding the library rather than streaming it", size, retained, limit)
	}
	if len(broad) != MaxSearchLimit {
		t.Errorf("the broad search returned %d passages, want the ceiling of %d",
			len(broad), MaxSearchLimit)
	}
	// And it says how many it left out, which is the disclosure half of the
	// same bound: twenty of four thousand must not look like twenty of
	// twenty.
	if broadStats.Matched != conversations*turnsEach {
		t.Errorf("matched = %d, want every one of the %d turns",
			broadStats.Matched, conversations*turnsEach)
	}
}

// TestTheSearchLimitDisclosesWhatItTruncated: every cap in this package has
// to say when it fired.
//
// A clipped passage marks itself with an ellipsis, a clipped preview does
// the same (both pinned elsewhere), and a conversation that could not be
// read is named in Skipped. The result limit was the one that truncated in
// silence: two hundred hits and twenty hits came back looking identical, so
// "is that all of them?" had no answer. SearchStats.Matched is the answer
// (issue #173).
func TestTheSearchLimitDisclosesWhatItTruncated(t *testing.T) {
	s := searchStore(t)
	base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	texts := make([]string, 0, 60)
	for i := range 60 {
		texts = append(texts, fmt.Sprintf("mention number %d of the recurring subject", i))
	}
	seedConversation(t, s, "conv-many", base, texts...)

	matches, stats, err := s.Search(Query{Text: "recurring subject", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 5 {
		t.Fatalf("returned %d passages, want the requested 5", len(matches))
	}
	if stats.Matched != 60 {
		t.Errorf("matched = %d, want all 60 — a caller cannot say \"showing 5 of 60\" without it",
			stats.Matched)
	}

	// Nothing cut means the two numbers agree, so a client never has to
	// guess whether a difference is real.
	matches, stats, err = s.Search(Query{Text: "mention number 7 of"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Matched != len(matches) {
		t.Errorf("matched = %d with %d returned; an untruncated search must report the same number",
			stats.Matched, len(matches))
	}

	// The Fake answers the same way, because the daemon and tool tests trust
	// it to mean what the files mean.
	fake := NewFake()
	fake.Seed(Meta{ID: "conv-many"}, turnsFrom(base, texts...))
	fakeMatches, fakeStats, err := fake.Search(Query{Text: "recurring subject", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(fakeMatches) != 5 || fakeStats.Matched != 60 {
		t.Errorf("the Fake returned %d/%d, want 5 of 60", len(fakeMatches), fakeStats.Matched)
	}
}

// turnsFrom builds alternating user/assistant turns a minute apart.
func turnsFrom(start time.Time, texts ...string) []Turn {
	turns := make([]Turn, 0, len(texts))
	for i, text := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		turns = append(turns, Turn{Role: role, Text: text,
			Time: start.Add(time.Duration(i) * time.Minute)})
	}
	return turns
}

// TestAnOverlongTurnLineIsReportedRatherThanLoaded: the streaming scan is
// bounded by a buffer ceiling, and a line past it is disclosed as a skipped
// conversation rather than swallowed. The bound is the point — an archive is
// unbounded, so something has to refuse — and so is the disclosure: a search
// that silently ignored part of the library would answer "I found nothing"
// about a conversation it never looked at.
func TestAnOverlongTurnLineIsReportedRatherThanLoaded(t *testing.T) {
	s := searchStore(t)
	base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	seedConversation(t, s, "conv-normal", base, "an ordinary turn about the needle")

	// One line larger than the scanner's ceiling, in its own conversation.
	huge := strings.Repeat("x", 5*1024*1024)
	transcript := "{\"schema\":1,\"id\":\"conv-huge\"}\n" +
		"{\"role\":\"user\",\"text\":\"" + huge + "\",\"ts\":\"2026-08-10T09:00:00Z\"}\n"
	if err := os.WriteFile(filepath.Join(s.Dir, "conv-huge.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	matches, stats, err := s.Search(Query{Text: "needle"})
	if err != nil {
		t.Fatalf("one oversized line failed the whole search: %v", err)
	}
	if len(matches) != 1 || matches[0].ConversationID != "conv-normal" {
		t.Errorf("matches = %+v, want the readable conversation's hit", matches)
	}
	if len(stats.Skipped) != 1 || stats.Skipped[0].ID != "conv-huge" || stats.Skipped[0].Err == "" {
		t.Fatalf("the oversized transcript was not reported: %+v", stats.Skipped)
	}
}

// TestSearchDuringAnAppendNeverSeesAPartialTurn is the archive's version of
// the concurrency promise, and it is the one that has to be made here rather
// than in the shared suite: Search deliberately does not take the store's
// mutex — a scan over a large library would otherwise stall the engine's
// post-session write, and search must never block a session — so its readers
// meet writes in flight by construction.
//
// What makes that safe is that appends land whole lines. A scan racing an
// append sees the lines that landed, and at worst a torn final line, which
// it skips exactly as Read does. It must never see half a turn as a turn.
func TestSearchDuringAnAppendNeverSeesAPartialTurn(t *testing.T) {
	s := &FileStore{Dir: t.TempDir(), NewID: func(time.Time) string { return "live" }}
	base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	const appends = 40
	// A long text, so a turn line is several kilobytes and a write that
	// could be seen half-done has room to be seen half-done. Long enough to
	// matter, short enough that the readers below re-scan the transcript
	// hundreds of times inside the gate's time budget.
	body := strings.Repeat("the same recurring subject said again and again ", 30)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range appends {
			ts := base.Add(time.Duration(i) * time.Minute)
			if _, err := s.Append("live", []Turn{{Role: "user",
				Text: fmt.Sprintf("%s number %d", body, i), Time: ts}}); err != nil {
				t.Errorf("append %d failed: %v", i, err)
				return
			}
		}
	}()

	bad := make(chan string, 8)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range appends * 3 {
				matches, stats, err := s.Search(Query{Text: "recurring subject"})
				if err != nil {
					select {
					case bad <- "a search racing an append failed: " + err.Error():
					default:
					}
					return
				}
				if len(stats.Skipped) != 0 {
					select {
					case bad <- fmt.Sprintf("a search racing an append reported damage: %+v", stats.Skipped):
					default:
					}
					return
				}
				for _, m := range matches {
					// Every turn ends in "number <n>": a passage that came
					// from half a written line could not.
					if m.Role != "user" || !strings.Contains(m.Passage, "recurring subject") {
						select {
						case bad <- fmt.Sprintf("a search saw a partial turn: %+v", m):
						default:
						}
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(bad)
	for msg := range bad {
		t.Error(msg)
	}

	conv, err := s.Read("live")
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.Turns) != appends {
		t.Errorf("the transcript holds %d turns after %d appends", len(conv.Turns), appends)
	}
}

// TestConversationIDsAreUniqueWithinASecond: the default minter is a UTC
// timestamp plus a random suffix, and the timestamp alone is not unique — a
// busy second archives several conversations. The suffix is what carries the
// promise, so it is asserted rather than assumed.
func TestConversationIDsAreUniqueWithinASecond(t *testing.T) {
	at := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	const ids = 200
	collisions := 0
	for range ids {
		id := defaultID(at)
		if !strings.HasPrefix(id, "20260821-100000-") {
			t.Fatalf("id %q is not the sortable timestamp form", id)
		}
		if seen[id] {
			collisions++
		}
		seen[id] = true
	}
	// Two random bytes over two hundred draws: a handful of collisions is
	// arithmetic, a heap of them is a broken minter. The archive's real
	// guarantee is that the second moves on, which is what makes a reused id
	// impossible rather than merely unlikely — asserted below.
	if collisions > ids/20 {
		t.Errorf("%d of %d ids collided within one second; the suffix is not random", collisions, ids)
	}
	if defaultID(at) == defaultID(at.Add(time.Second)) {
		t.Error("ids minted a second apart are identical")
	}
}

// TestADeletedConversationsIDIsNeverHandedOutAgain: deletion removes the
// files outright, and the search scan relies on ids never coming back — a
// transcript that vanishes mid-scan is treated as a legitimate deletion
// rather than an error, which is only safe while a new conversation cannot
// take the dead id.
func TestADeletedConversationsIDIsNeverHandedOutAgain(t *testing.T) {
	dir := t.TempDir()
	clock := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	tick := func() time.Time { clock = clock.Add(time.Second); return clock }
	s := &FileStore{Dir: dir, Now: tick}
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)

	first, err := s.Append("", turnsAt(ts, "the conversation that gets deleted", "an answer"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(first); err != nil {
		t.Fatal(err)
	}
	// A restart over the same directory, which is where a store that
	// recomputed its ids from what it can see would go wrong.
	restarted := &FileStore{Dir: dir, Now: tick}
	second, err := restarted.Append("", []Turn{{Role: "user", Text: "a new conversation"}})
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Errorf("conversation id %q was handed out again after the record was deleted", first)
	}
}

// TestTheFakeAndTheFileStoreAgreeOnTheStoreContract. The daemon, the session
// engine and the model's tool are all tested against the Fake, and every one
// of those tests is only worth what the Fake's fidelity is worth. So the two
// implementations answer the same script here, and the answers are compared
// rather than described.
func TestTheFakeAndTheFileStoreAgreeOnTheStoreContract(t *testing.T) {
	ts := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	stores := map[string]Store{
		"file": func() Store {
			n := 0
			return &FileStore{Dir: t.TempDir(), NewID: func(time.Time) string {
				n++
				return fmt.Sprintf("conv%d", n)
			}}
		}(),
		"fake": NewFake(),
	}
	for name, s := range stores {
		t.Run(name, func(t *testing.T) {
			// An empty store has no active conversation and lists nothing.
			if got := s.Active(); got != "" {
				t.Errorf("a fresh store is active on %q", got)
			}
			metas, unreadable, err := s.List()
			if err != nil || len(metas) != 0 || len(unreadable) != 0 {
				t.Fatalf("fresh listing = %+v/%+v (%v)", metas, unreadable, err)
			}

			// Appending nothing is a no-op that mints nothing.
			if id, err := s.Append("", nil); err != nil || id != "" {
				t.Errorf("empty append = %q/%v, want no id and no error", id, err)
			}

			// A first exchange creates a conversation and adopts it.
			first, err := s.Append("", turnsAt(ts, "the first question", "the first answer"))
			if err != nil || first == "" {
				t.Fatalf("first append = %q/%v", first, err)
			}
			if got := s.Active(); got != first {
				t.Errorf("active = %q after the first append, want %q", got, first)
			}

			// A follow-up extends the same record rather than forking it.
			same, err := s.Append(first, turnsAt(ts.Add(time.Minute), "a follow-up", "and its answer"))
			if err != nil || same != first {
				t.Fatalf("follow-up landed in %q/%v, want %q", same, err, first)
			}
			conv, err := s.Read(first)
			if err != nil {
				t.Fatal(err)
			}
			if len(conv.Turns) != 4 || conv.Meta.TurnCount != 4 {
				t.Errorf("read %d turns (meta says %d), want 4", len(conv.Turns), conv.Meta.TurnCount)
			}
			if conv.Meta.Preview != "the first question" {
				t.Errorf("preview = %q, want the first user turn", conv.Meta.Preview)
			}

			// A second conversation, and the listing puts the newest first.
			second, err := s.Append("", turnsAt(ts.Add(time.Hour), "a separate thread", "its answer"))
			if err != nil {
				t.Fatal(err)
			}
			metas, _, err = s.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(metas) != 2 || metas[0].ID != second || metas[1].ID != first {
				t.Fatalf("listing = %+v, want %q then %q", metas, second, first)
			}

			// SetActive moves the live head without touching the records.
			if err := s.SetActive(first); err != nil {
				t.Fatal(err)
			}
			if got := s.Active(); got != first {
				t.Errorf("active = %q after SetActive(%q)", got, first)
			}

			// Reading an unknown id is an error, and so is deleting one.
			if _, err := s.Read("no-such-conversation"); err == nil {
				t.Error("reading an unknown conversation succeeded")
			}
			if err := s.Delete("no-such-conversation"); err == nil {
				t.Error("deleting an unknown conversation succeeded")
			}

			// Deleting the active conversation clears the pointer.
			if err := s.Delete(first); err != nil {
				t.Fatal(err)
			}
			if got := s.Active(); got != "" {
				t.Errorf("active = %q after its conversation was deleted", got)
			}
			metas, _, err = s.List()
			if err != nil || len(metas) != 1 || metas[0].ID != second {
				t.Fatalf("listing after the delete = %+v (%v)", metas, err)
			}

			// And DeleteAll reports how many went.
			n, err := s.DeleteAll()
			if err != nil || n != 1 {
				t.Fatalf("delete all = %d/%v, want 1", n, err)
			}
			metas, _, err = s.List()
			if err != nil || len(metas) != 0 {
				t.Fatalf("listing after delete all = %+v (%v)", metas, err)
			}
		})
	}
}

// TestTheFakesScriptedFailuresLookLikeTheRealOnes. The Fake's error hooks are
// what every daemon and engine test uses to drive the archive's failure
// paths, so they have to behave the way a refusing disk behaves: an error
// out, nothing recorded, and no id issued for turns that did not land.
func TestTheFakesScriptedFailuresLookLikeTheRealOnes(t *testing.T) {
	f := NewFake()
	f.AppendErr = errors.New("no space left on device")
	id, err := f.Append("", turnsAt(time.Now(), "a question", "an answer"))
	if err == nil {
		t.Fatal("a scripted append failure reported success")
	}
	if id != "" {
		t.Errorf("a refused append handed back the id %q", id)
	}
	if f.Appends() != 1 {
		t.Errorf("attempts = %d, want the refused one counted", f.Appends())
	}
	if got := f.Turns("conv1"); got != nil {
		t.Errorf("a refused append stored turns anyway: %+v", got)
	}

	f.AppendErr = nil
	landed, err := f.Append("", turnsAt(time.Now(), "a question", "an answer"))
	if err != nil {
		t.Fatal(err)
	}
	f.ReadErr = errors.New("the disk is unhappy")
	if _, err := f.Read(landed); err == nil {
		t.Error("a scripted read failure reported success")
	}
	f.ReadErr = nil
	f.ListErr = errors.New("the directory is unreadable")
	if _, _, err := f.List(); err == nil {
		t.Error("a scripted list failure reported success")
	}
	if _, _, err := f.Search(Query{Text: "anything"}); err == nil {
		t.Error("a scripted list failure did not reach search")
	}
	f.ListErr = nil
	f.DeleteErr = errors.New("the file will not go")
	if err := f.Delete(landed); err == nil {
		t.Error("a scripted delete failure reported success")
	}
	if _, err := f.DeleteAll(); err == nil {
		t.Error("a scripted delete failure did not reach delete all")
	}
	f.DeleteErr = nil
	if err := f.Delete(landed); err != nil {
		t.Errorf("the record was gone after a refused delete: %v", err)
	}
	// A deleted conversation is skipped by the search rather than crashing
	// it — the Fake keeps creation order for deterministic listings, and a
	// deleted id is still in that order.
	if _, stats, err := f.Search(Query{Text: "question"}); err != nil || stats.Conversations != 0 {
		t.Errorf("search after the delete = %+v (%v), want nothing searched", stats, err)
	}

	// Seeded state behaves like a previous daemon run's archive.
	f2 := NewFake()
	f2.Seed(Meta{ID: "seeded", Started: time.Now()}, turnsAt(time.Now(), "seeded question", "seeded answer"))
	f2.SeedActive("seeded")
	if got := f2.Active(); got != "seeded" {
		t.Errorf("active = %q after SeedActive", got)
	}
	if f2.Appends() != 0 {
		t.Errorf("Seed counted as an append: %d", f2.Appends())
	}
	if err := f2.SetActive("seeded"); err != nil {
		t.Fatal(err)
	}
	conv, err := f2.Read("seeded")
	if err != nil || conv.Meta.TurnCount != 2 {
		t.Errorf("seeded read = %+v (%v)", conv.Meta, err)
	}
}

// TestAnEmptyTranscriptIsNotReportedAsDamage is the second defect the
// concurrency case found (issue #173), pinned on its own so a regression
// names it.
//
// Creating a conversation is an open(O_CREAT) and then a write, and the
// search scan runs without the store's lock on purpose. A search landing
// between the two saw a zero-length file and reported the conversation as
// unreadable — a damage report about the conversation the user was in the
// middle of having, which would be right there in the window's search
// results a moment later, undamaged.
//
// An empty transcript is skipped in silence for the same reason a vanished
// one is: both are what a write in flight looks like from outside the lock.
// Damage that really is damage still surfaces, because the listing takes the
// lock and Read says so outright.
func TestAnEmptyTranscriptIsNotReportedAsDamage(t *testing.T) {
	s := searchStore(t)
	base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	seedConversation(t, s, "conv-good", base, "the needle is here", "Noted.")
	if err := os.WriteFile(filepath.Join(s.Dir, "conv-being-created.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	matches, stats, err := s.Search(Query{Text: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Skipped) != 0 {
		t.Errorf("a conversation caught mid-creation was reported as damaged: %+v", stats.Skipped)
	}
	if stats.Conversations != 1 {
		t.Errorf("searched = %d, want the one readable conversation", stats.Conversations)
	}
	if len(matches) != 1 || matches[0].ConversationID != "conv-good" {
		t.Errorf("matches = %+v, want the readable conversation's hit", matches)
	}
	// A conversation whose transcript really is empty is still visible where
	// it matters: the listing takes the store's lock, so it sees the file
	// whole and reports it.
	_, unreadable, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(unreadable) != 1 || unreadable[0].ID != "conv-being-created" {
		t.Errorf("listing = %+v, want the empty transcript reported", unreadable)
	}
}
