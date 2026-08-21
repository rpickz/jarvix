package conversations

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Search over the archive (issue #59). What is pinned here is the *contract*
// the RAG seam promises every caller: deterministic ranking (exact phrase
// beats scattered words, recency breaks ties), bounded passages, the
// skip-and-report handling of unreadable files, and identical semantics from
// the file store and the Fake — because the daemon and tool tests trust the
// Fake to mean what the files mean.

// seedConversation writes one conversation of alternating user/assistant
// turns, one minute apart from start.
func seedConversation(t *testing.T, s *FileStore, id string, start time.Time, texts ...string) {
	t.Helper()
	turns := make([]Turn, 0, len(texts))
	for i, text := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		turns = append(turns, Turn{Role: role, Text: text, Time: start.Add(time.Duration(i) * time.Minute)})
	}
	s.NewID = func(time.Time) string { return id }
	if _, err := s.Append("", turns); err != nil {
		t.Fatal(err)
	}
}

func searchStore(t *testing.T) *FileStore {
	t.Helper()
	return &FileStore{Dir: t.TempDir()}
}

func TestSearchRanking(t *testing.T) {
	s := searchStore(t)
	base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	// Older conversation holds the exact phrase; newer ones hold the words
	// scattered. The phrase must outrank recency; among the scattered
	// matches, recency must decide.
	seedConversation(t, s, "conv-old", base,
		"what did we decide about the deployment approach?",
		"We decided on blue-green deployment.")
	seedConversation(t, s, "conv-mid", base.AddDate(0, 0, 3),
		"the deployment is stuck",
		"The approach here is to check the deployment logs first.")
	seedConversation(t, s, "conv-new", base.AddDate(0, 0, 5),
		"is the new approach shipping with the deployment tomorrow?",
		"Yes, it ships tomorrow.")

	matches, stats, err := s.Search(Query{Text: "deployment approach"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Conversations != 3 || len(stats.Skipped) != 0 {
		t.Fatalf("stats = %+v, want 3 searched, none skipped", stats)
	}
	got := make([]string, 0, len(matches))
	for _, m := range matches {
		got = append(got, fmt.Sprintf("%s#%d phrase=%v", m.ConversationID, m.Turn, m.Phrase))
	}
	// The exact phrase (in the oldest conversation) first; then the
	// scattered matches newest first, later turns before earlier ones.
	want := []string{
		"conv-old#1 phrase=true",
		"conv-new#1 phrase=false",
		"conv-mid#2 phrase=false",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("ranking =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Determinism: the same search must return byte-identical results.
	again, _, err := s.Search(Query{Text: "deployment approach"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%+v", again) != fmt.Sprintf("%+v", matches) {
		t.Error("two identical searches returned different results")
	}
}

func TestSearchMatchingSemantics(t *testing.T) {
	s := searchStore(t)
	base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	seedConversation(t, s, "conv-a", base,
		"Remind me about the Deploy tomorrow", "Will do.")

	cases := []struct {
		name  string
		query string
		hits  int
	}{
		{"case-insensitive", "deploy", 1},
		{"all words must appear", "deploy rollback", 0},
		{"words may scatter", "tomorrow remind", 1},
		{"extra whitespace is not part of the query", "  deploy   tomorrow  ", 1},
		{"no word matches nothing", "kubernetes", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matches, _, err := s.Search(Query{Text: tc.query})
			if err != nil {
				t.Fatal(err)
			}
			if len(matches) != tc.hits {
				t.Errorf("Search(%q) = %d matches, want %d", tc.query, len(matches), tc.hits)
			}
		})
	}

	if _, _, err := s.Search(Query{Text: "   "}); err == nil {
		t.Error("an empty query must be rejected, not match everything")
	}
}

func TestSearchTurnReferences(t *testing.T) {
	s := searchStore(t)
	base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	seedConversation(t, s, "conv-a", base,
		"hello there", "Hi.", "what about helios?", "Helios ships Friday.")

	matches, _, err := s.Search(Query{Text: "helios"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("matches = %d, want 2", len(matches))
	}
	// Turn references are 1-based positions in the transcript — the handle a
	// caller uses to land on the spot after opening the conversation.
	if matches[0].Turn != 4 || matches[0].Role != "assistant" {
		t.Errorf("first match = turn %d role %s, want turn 4 assistant", matches[0].Turn, matches[0].Role)
	}
	if matches[1].Turn != 3 || matches[1].Role != "user" {
		t.Errorf("second match = turn %d role %s, want turn 3 user", matches[1].Turn, matches[1].Role)
	}
	if !matches[1].Time.Equal(base.Add(2 * time.Minute)) {
		t.Errorf("match time = %v, want the turn's timestamp", matches[1].Time)
	}
}

func TestSearchBoundsResultsAndPassages(t *testing.T) {
	s := searchStore(t)
	base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	long := strings.Repeat("padding words before the important part ", 20) +
		"the needle sits here" + strings.Repeat(" and padding words after it", 20)
	texts := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		texts = append(texts, long)
	}
	seedConversation(t, s, "conv-long", base, texts...)

	matches, _, err := s.Search(Query{Text: "needle sits", Limit: 5, PassageRunes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 5 {
		t.Fatalf("limit ignored: %d matches", len(matches))
	}
	for _, m := range matches {
		runes := []rune(m.Passage)
		// The budget plus at most two ellipsis marks.
		if len(runes) > 102 {
			t.Fatalf("passage overruns its cap: %d runes: %q", len(runes), m.Passage)
		}
		if !strings.Contains(strings.ToLower(m.Passage), "needle sits") {
			t.Fatalf("passage clipped away the hit: %q", m.Passage)
		}
		if !strings.HasPrefix(m.Passage, "…") || !strings.HasSuffix(m.Passage, "…") {
			t.Fatalf("a mid-text clip must be marked on both ends: %q", m.Passage)
		}
	}

	// A caller cannot raise the caps past the hard ceilings.
	matches, _, err = s.Search(Query{Text: "needle", Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) > MaxSearchLimit {
		t.Errorf("limit ceiling ignored: %d matches", len(matches))
	}
}

func TestSearchPassageIsOneLineAndRuneSafe(t *testing.T) {
	s := searchStore(t)
	base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	seedConversation(t, s, "conv-a", base,
		"première ligne\ndeuxième ligne über café\ntroisième", "ok")

	matches, _, err := s.Search(Query{Text: "über café"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(matches))
	}
	if strings.ContainsAny(matches[0].Passage, "\n\r") {
		t.Errorf("passage is not one line: %q", matches[0].Passage)
	}
	if !strings.Contains(matches[0].Passage, "über café") {
		t.Errorf("passage lost the accented hit: %q", matches[0].Passage)
	}
}

func TestSearchSkipsAndReportsUnreadable(t *testing.T) {
	s := searchStore(t)
	base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	seedConversation(t, s, "conv-good", base, "the needle is here", "Noted.")

	// A transcript with a bad header, one with an unsupported schema, and one
	// whose corruption sits before the end.
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(s.Dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("conv-badheader.jsonl", "not json\n")
	write("conv-oldschema.jsonl", `{"schema":99,"id":"conv-oldschema"}`+"\n")
	write("conv-corrupt.jsonl", `{"schema":1,"id":"conv-corrupt"}`+"\n"+
		`{"role":"user","text":"the needle is early","ts":"2026-08-12T09:00:00Z"}`+"\n"+
		"garbage line\n"+
		`{"role":"user","text":"the needle is late","ts":"2026-08-12T09:05:00Z"}`+"\n")
	// A torn final line — the in-flight append — is tolerated silently.
	write("conv-torn.jsonl", `{"schema":1,"id":"conv-torn"}`+"\n"+
		`{"role":"user","text":"the needle is fine","ts":"2026-08-11T09:00:00Z"}`+"\n"+
		`{"role":"user","te`)

	matches, stats, err := s.Search(Query{Text: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, sk := range stats.Skipped {
		ids = append(ids, sk.ID)
	}
	if strings.Join(ids, ",") != "conv-badheader,conv-corrupt,conv-oldschema" {
		t.Errorf("skipped = %v, want the three damaged records reported", ids)
	}
	// The good conversation, the torn one, and the readable head of the
	// corrupt one all still match: one bad file never hides the library.
	var found []string
	for _, m := range matches {
		found = append(found, m.ConversationID)
	}
	for _, want := range []string{"conv-good", "conv-torn", "conv-corrupt"} {
		if !strings.Contains(strings.Join(found, ","), want) {
			t.Errorf("matches %v missing %s", found, want)
		}
	}
	for _, m := range matches {
		if m.ConversationID == "conv-corrupt" && m.Passage != "the needle is early" {
			t.Errorf("corruption must stop the scan of that file: matched %q", m.Passage)
		}
	}
	if stats.Conversations != 3 {
		t.Errorf("searched = %d, want 3 (good, torn, and the corrupt head)", stats.Conversations)
	}
}

func TestSearchEmptyArchive(t *testing.T) {
	// A directory that does not exist yet is an empty library, not an error —
	// and the stats say nothing was searched, which is how the tool tells
	// "no matches" from "nothing to search".
	s := &FileStore{Dir: filepath.Join(t.TempDir(), "never-created")}
	matches, stats, err := s.Search(Query{Text: "anything"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 || stats.Conversations != 0 || len(stats.Skipped) != 0 {
		t.Errorf("empty archive: matches=%d stats=%+v", len(matches), stats)
	}
}

func TestFakeSearchMatchesFileStoreSemantics(t *testing.T) {
	// The Fake must rank exactly as the files do: daemon and tool tests
	// trust it to mean what the file store means.
	base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	s := searchStore(t)
	seedConversation(t, s, "conv-old", base,
		"what did we decide about the deployment approach?", "Blue-green.")
	seedConversation(t, s, "conv-new", base.AddDate(0, 0, 5),
		"the deployment needs a new approach eventually", "Noted.")

	f := NewFake()
	for _, id := range []string{"conv-old", "conv-new"} {
		conv, err := s.Read(id)
		if err != nil {
			t.Fatal(err)
		}
		f.Seed(conv.Meta, conv.Turns)
	}

	fromFiles, _, err := s.Search(Query{Text: "deployment approach"})
	if err != nil {
		t.Fatal(err)
	}
	fromFake, _, err := f.Search(Query{Text: "deployment approach"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%+v", fromFake) != fmt.Sprintf("%+v", fromFiles) {
		t.Errorf("fake ranks differently from the files:\nfake  %+v\nfiles %+v", fromFake, fromFiles)
	}
}

// BenchmarkSearch200Conversations measures the issue #59 budget: a search
// over a 200-conversation archive must complete in ≤200ms. The measured
// figure is recorded in the ADR; this benchmark is how it is re-measured.
func BenchmarkSearch200Conversations(b *testing.B) {
	s := &FileStore{Dir: b.TempDir()}
	base := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	for conv := 0; conv < 200; conv++ {
		id := fmt.Sprintf("conv-%03d", conv)
		s.NewID = func(time.Time) string { return id }
		turns := make([]Turn, 0, 30)
		for i := 0; i < 30; i++ {
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			text := fmt.Sprintf("turn %d of conversation %d talking about builds, deployments, "+
				"calendars, reminders and the odd needle in a haystack of ordinary words", i, conv)
			turns = append(turns, Turn{Role: role, Text: text,
				Time: base.AddDate(0, 0, conv).Add(time.Duration(i) * time.Minute)})
		}
		if _, err := s.Append("", turns); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		matches, stats, err := s.Search(Query{Text: "needle in a haystack"})
		if err != nil {
			b.Fatal(err)
		}
		if len(matches) == 0 || stats.Conversations != 200 {
			b.Fatalf("matches=%d searched=%d", len(matches), stats.Conversations)
		}
	}
}
