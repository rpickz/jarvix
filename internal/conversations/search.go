package conversations

// This file is full-text search over the archive (issue #59): query in,
// ranked passages with conversation id and turn references out.
//
// That sentence is the design. The Searcher interface is deliberately the
// exact seam a future embedding index would implement — same query in, same
// ranked passages out — so RAG can arrive later as an implementation upgrade
// invisible to every caller (window, CLI, and the model's tool). The ADR for
// this file records why the first implementation is a streaming scan and what
// would justify replacing it.
//
// The scan streams transcripts line by line rather than loading files
// wholesale: one bufio pass per conversation, one decoded turn in memory at a
// time. It inherits conversation.list's unreadable contract — a file that
// cannot be read is skipped and reported, never fatal — and tolerates files
// vanishing mid-scan, because deletion removes files outright and ids are
// never reused.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Search bounds. The passage caps exist so no search — however broad the
// query — can flood a caller: the model's context window is the scarce
// resource the tool surface protects, and the window/CLI inherit the same
// discipline for free.
const (
	// DefaultSearchLimit is how many passages a search returns when the
	// caller does not say.
	DefaultSearchLimit = 10
	// MaxSearchLimit is the hard ceiling on passages per search.
	MaxSearchLimit = 20
	// DefaultPassageRunes bounds one passage when the caller does not say.
	DefaultPassageRunes = 240
	// MaxPassageRunes is the hard ceiling on one passage.
	MaxPassageRunes = 600
)

// Query is one search request.
type Query struct {
	// Text is what to look for. Matching is case-insensitive; every word
	// must appear in a turn for it to match, and the words appearing as one
	// contiguous phrase outranks them scattered.
	Text string
	// Limit caps how many passages come back. Zero means
	// DefaultSearchLimit; values above MaxSearchLimit are clamped.
	Limit int
	// PassageRunes caps the size of each passage. Zero means
	// DefaultPassageRunes; values above MaxPassageRunes are clamped.
	PassageRunes int
}

// Match is one ranked passage: where it was said, when, and the clipped text
// around the hit. ConversationID plus Turn is the reference a caller needs to
// open the conversation and land on the spot.
type Match struct {
	// ConversationID names the conversation the passage came from.
	ConversationID string
	// Turn is the 1-based position of the matched turn in the transcript.
	Turn int
	// Role is who said it: "user" or "assistant".
	Role string
	// Time is when the turn was archived.
	Time time.Time
	// Passage is the matched turn's text, clipped around the hit and bounded
	// by Query.PassageRunes.
	Passage string
	// Phrase reports whether the query matched as one contiguous phrase
	// (the higher ranking tier) rather than as scattered words.
	Phrase bool
}

// SearchStats describes what a search actually covered, so callers can tell
// "no matches" from "nothing to search" — the difference between the
// assistant saying it found nothing and saying there is no archive yet.
type SearchStats struct {
	// Conversations is how many readable conversations were scanned.
	Conversations int
	// Matched is how many passages matched in total, before Query.Limit cut
	// the list down to what came back.
	//
	// It is here because the limit is the one cap in this package that used
	// to truncate in silence. A clipped passage says so with an ellipsis and
	// a skipped conversation is named in Skipped, but a search that found
	// two hundred hits and returned twenty looked exactly like a search that
	// found twenty — so "is that all of them?" had no answer, and the
	// honest one ("showing 20 of 200") could not be given. Equal to the
	// number of matches returned whenever nothing was cut.
	Matched int
	// Skipped lists conversations that could not be (fully) searched, with
	// why. The unreadable contract from List: skipped and reported, never a
	// failed search.
	Skipped []Unreadable
}

// Searcher is the search seam — and, deliberately, the RAG seam. A future
// embedding index implements this same interface (query in, ranked passages
// with conversation and turn references out) and slots in behind every
// caller unchanged; ranking would improve, the contract would not move.
type Searcher interface {
	Search(q Query) ([]Match, SearchStats, error)
}

// compiledQuery is a Query normalised once: folded words, the contiguous
// phrase, and the resolved bounds.
type compiledQuery struct {
	words        []string
	phrase       string
	limit        int
	passageRunes int
}

// compileQuery normalises a query, rejecting one with nothing to look for.
func compileQuery(q Query) (compiledQuery, error) {
	words := strings.Fields(strings.ToLower(q.Text))
	if len(words) == 0 {
		return compiledQuery{}, errors.New("a search needs at least one word")
	}
	c := compiledQuery{
		words: words,
		// The phrase is the words re-joined, so "deploy   approach" and
		// "deploy approach" are the same phrase — whitespace is how the query
		// was typed, not part of what was said.
		phrase:       strings.Join(words, " "),
		limit:        q.Limit,
		passageRunes: q.PassageRunes,
	}
	if c.limit <= 0 {
		c.limit = DefaultSearchLimit
	}
	if c.limit > MaxSearchLimit {
		c.limit = MaxSearchLimit
	}
	if c.passageRunes <= 0 {
		c.passageRunes = DefaultPassageRunes
	}
	if c.passageRunes > MaxPassageRunes {
		c.passageRunes = MaxPassageRunes
	}
	return c, nil
}

// matchTurn classifies one turn against the query: no hit, scattered words,
// or contiguous phrase. hitAt is the byte offset of the phrase (or the first
// word) in the folded text, for passage clipping.
func (c compiledQuery) matchTurn(text string) (hit, phrase bool, hitAt int) {
	folded := strings.ToLower(text)
	if i := strings.Index(folded, c.phrase); i >= 0 {
		return true, true, i
	}
	first := -1
	for _, w := range c.words {
		i := strings.Index(folded, w)
		if i < 0 {
			return false, false, 0
		}
		if first < 0 || i < first {
			first = i
		}
	}
	return true, false, first
}

// consider offers turn number `turn` of conversation id to the ranking, and
// records it when it matches. Shared by the file store's streaming scan and
// the test fake, so there is exactly one definition of what matches and how
// passages are clipped.
func (c compiledQuery) consider(r *ranked, id string, turn int, t Turn) {
	hit, phrase, at := c.matchTurn(t.Text)
	if !hit {
		return
	}
	r.add(Match{
		ConversationID: id,
		Turn:           turn,
		Role:           t.Role,
		Time:           t.Time,
		Passage:        clipPassage(t.Text, at, c.passageRunes),
		Phrase:         phrase,
	})
}

// collectMatches offers every turn of one in-memory conversation.
func (c compiledQuery) collectMatches(r *ranked, id string, turns []Turn) {
	for i, t := range turns {
		c.consider(r, id, i+1, t)
	}
}

// better is the ranking: exact phrase beats scattered words, then recency,
// with id and turn breaking ties so the order is TOTAL — which is what makes
// a search run twice return byte-identical results, and what lets the best
// results be kept as the scan runs rather than sorted at the end.
func (c compiledQuery) better(a, b Match) bool {
	if a.Phrase != b.Phrase {
		return a.Phrase
	}
	if !a.Time.Equal(b.Time) {
		return a.Time.After(b.Time)
	}
	if a.ConversationID != b.ConversationID {
		return a.ConversationID > b.ConversationID
	}
	return a.Turn > b.Turn
}

// ranked keeps the best `limit` matches a scan has seen and counts the rest.
//
// Bounded on purpose, and this is the whole of what makes the streaming scan
// stream all the way to the caller. The archive is unbounded and a broad
// query matches a large fraction of it, so collecting every hit and sorting
// at the end held one clipped passage per matching turn — three quarters of
// a megabyte over four thousand turns, growing with the library for ever,
// to then throw all but twenty of them away. Since the order is total, a
// match that has already lost to `limit` others can never win, so it is
// dropped the moment it loses. What is kept is at most twenty passages,
// whatever the archive's size, and the results are identical to sorting the
// lot (issue #173).
type ranked struct {
	c     compiledQuery
	best  []Match
	total int
}

func newRanked(c compiledQuery) *ranked {
	return &ranked{c: c, best: make([]Match, 0, c.limit)}
}

// add offers one match to the ranking.
func (r *ranked) add(m Match) {
	r.total++
	if len(r.best) < r.c.limit {
		r.best = append(r.best, m)
		r.siftLast()
		return
	}
	if !r.c.better(m, r.best[len(r.best)-1]) {
		return // it has already lost to `limit` others and cannot recover
	}
	r.best[len(r.best)-1] = m
	r.siftLast()
}

// siftLast moves the newly placed last element up to where it belongs. The
// slice is at most MaxSearchLimit long, so this is cheaper than any of the
// alternatives and keeps the ordering identical to a full sort.
func (r *ranked) siftLast() {
	for i := len(r.best) - 1; i > 0 && r.c.better(r.best[i], r.best[i-1]); i-- {
		r.best[i], r.best[i-1] = r.best[i-1], r.best[i]
	}
}

// matches returns the ranked results. newRanked allocates the slice, so an
// empty search encodes as [] rather than null.
func (r *ranked) matches() []Match { return r.best }

// clipPassage bounds one turn's text to maxRunes around the hit at byte
// offset foldedAt, cutting on rune boundaries and marking cuts with an
// ellipsis. Newlines collapse to spaces so a passage is always one line —
// what a result list renders and what the model quotes.
//
// foldedAt was found in strings.ToLower(text). Lowercasing preserves byte
// offsets for every common script; the rare rune whose lowercase form has a
// different width (İ and friends) can drift the offset, so an offset that no
// longer lands inside the text degrades to clipping from the start rather
// than slicing out of range.
func clipPassage(text string, foldedAt, maxRunes int) string {
	if foldedAt < 0 || foldedAt > len(text) {
		foldedAt = 0
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return flattenPassage(text)
	}
	hit := utf8.RuneCountInString(text[:foldedAt])
	// Lead in with a quarter of the budget so the hit has context on both
	// sides, then take the full budget — shifted back if it would overrun.
	start := hit - maxRunes/4
	if start < 0 {
		start = 0
	}
	end := start + maxRunes
	if end > len(runes) {
		end = len(runes)
		start = end - maxRunes
	}
	passage := flattenPassage(string(runes[start:end]))
	if start > 0 {
		passage = "…" + passage
	}
	if end < len(runes) {
		passage += "…"
	}
	return passage
}

// flattenPassage renders text as one display line.
func flattenPassage(text string) string {
	return strings.TrimSpace(strings.Join(strings.FieldsFunc(text, func(r rune) bool {
		return r == '\n' || r == '\r'
	}), " "))
}

// Search implements Searcher over the archive directory: one streaming pass
// per transcript, newest turns winning ties, every unreadable file reported
// beside the results.
//
// It deliberately does not take the store's mutex. A search may cost tens of
// milliseconds over a large library, and holding the lock would stall the
// engine's post-session archive write behind it — search must never block a
// session. The files tolerate the race by construction: appends land whole
// lines, so the worst a concurrent write can show the scanner is a torn
// final line, which is skipped exactly as Read skips it.
func (s *FileStore) Search(q Query) ([]Match, SearchStats, error) {
	c, err := compileQuery(q)
	if err != nil {
		return nil, SearchStats{}, err
	}
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return []Match{}, SearchStats{Skipped: []Unreadable{}}, nil // no archive yet
	}
	if err != nil {
		return nil, SearchStats{}, fmt.Errorf("search conversations: %w", err)
	}

	r := newRanked(c)
	stats := SearchStats{Skipped: []Unreadable{}}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		skip, ok := s.scanTranscript(id, c, r)
		if skip != nil {
			stats.Skipped = append(stats.Skipped, *skip)
		}
		if ok {
			stats.Conversations++
		}
	}
	sort.Slice(stats.Skipped, func(i, j int) bool { return stats.Skipped[i].ID < stats.Skipped[j].ID })
	stats.Matched = r.total
	return r.matches(), stats, nil
}

// scanTranscript streams one transcript into the ranking. ok reports whether
// the conversation counted as searched; skip carries the report when any of
// it could not be. Both can be set at once: a transcript that goes bad
// midway keeps the matches found before the damage *and* reports it.
func (s *FileStore) scanTranscript(id string, c compiledQuery, r *ranked) (skip *Unreadable, ok bool) {
	f, err := os.Open(s.turnsPath(id))
	if errors.Is(err, os.ErrNotExist) {
		// Deleted between the directory listing and now. Ids are never
		// reused, so this is a legitimate vanishing act, not an error.
		return nil, false
	}
	if err != nil {
		// The error text names what went wrong, never what was said.
		return &Unreadable{ID: id, Err: err.Error()}, false
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	if !scanner.Scan() {
		// An empty transcript is not damage, and reporting it as damage was
		// a defect the concurrency case found (issue #173). Creating a
		// conversation is an open(O_CREAT) followed by a write, and this
		// scan deliberately runs without the store's lock, so a search that
		// lands between the two sees a zero-length file — of a conversation
		// that is perfectly well a moment later. The user would have been
		// told their live conversation could not be searched.
		//
		// It is skipped in silence for the same reason a transcript that
		// vanished mid-scan is: both are what a write in flight looks like
		// from outside the lock, and neither is something to report. Damage
		// that really is damage still surfaces — the listing takes the lock,
		// so it sees the file whole, and Read says so outright.
		return nil, false
	}
	var h header
	if err := json.Unmarshal(scanner.Bytes(), &h); err != nil {
		return &Unreadable{ID: id, Err: "bad header"}, false
	}
	if h.Schema != SchemaVersion {
		return &Unreadable{ID: id, Err: fmt.Sprintf("conversation schema version %d is not supported", h.Schema)}, false
	}

	// A line that fails to parse is only tolerable when it is the last one —
	// the torn tail of an interrupted (or in-flight) append, the same rule
	// Read applies. Bad-then-more-lines is corruption: keep what was found
	// before it, stop, and say so.
	line, badLine := 1, 0
	turn := 0
	var t Turn
	for scanner.Scan() {
		line++
		if badLine != 0 {
			return &Unreadable{ID: id, Err: fmt.Sprintf("bad turn at line %d", badLine)}, true
		}
		t = Turn{}
		if err := json.Unmarshal(scanner.Bytes(), &t); err != nil {
			badLine = line
			continue
		}
		turn++
		c.consider(r, id, turn, t)
	}
	if err := scanner.Err(); err != nil {
		// The scan broke partway (an over-long line, an I/O error): what was
		// found before the break stands, and the break is reported.
		return &Unreadable{ID: id, Err: err.Error()}, true
	}
	return nil, true
}

// Search implements Searcher on the Fake, over the same matching and ranking
// core as the file store, so daemon and tool tests exercise identical search
// semantics without a disk.
func (f *Fake) Search(q Query) ([]Match, SearchStats, error) {
	c, err := compileQuery(q)
	if err != nil {
		return nil, SearchStats{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ListErr != nil {
		return nil, SearchStats{}, f.ListErr
	}
	r := newRanked(c)
	stats := SearchStats{Skipped: []Unreadable{}}
	for _, id := range f.order {
		rec, ok := f.records[id]
		if !ok {
			continue
		}
		stats.Conversations++
		c.collectMatches(r, id, rec.Turns)
	}
	stats.Matched = r.total
	return r.matches(), stats, nil
}
