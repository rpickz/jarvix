package vocabulary

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/statehold"
)

// Store caps and defaults.
const (
	// DefaultMaxEntries caps the store. Two hundred taught phrases is far
	// more than a personal vocabulary needs; the cap exists so a store
	// nobody prunes cannot grow without bound, and it warns well before it
	// refuses (the memory book's stance, ADR 0025).
	DefaultMaxEntries = 200
	// DefaultMaxInjectedTokens caps what the vocabulary block may cost a
	// turn. Entries are one short line each (~15 tokens), so 300 tokens
	// carries a well-kept vocabulary whole while never crowding out the
	// conversation — deliberately smaller than memory's 500, because a
	// glossary is terser than a fact list.
	DefaultMaxInjectedTokens = 300
	// MinInjectedTokens is the smallest configurable injection budget: below
	// this not even the preamble, the stance sentence, one entry, and a trim
	// disclosure fit together, and the feature would be silently useless
	// while looking enabled (the preamble alone is ~100 tokens — it carries
	// the use-without-echo instruction, which cannot be shortened away).
	MinInjectedTokens = 150
	// MaxHardToHear caps how many phrases may carry the hard-to-hear flag.
	// The flag feeds whisper's initial prompt, and that prompt is *finite*:
	// whisper.cpp conditions its decoder on roughly 224 tokens, which the
	// assistant's name sentence and the [stt] vocabulary already spend from
	// (issue #83/#107). Twenty short phrases is comfortably inside what
	// remains — past that, terms start crowding each other out of the bias
	// rather than adding to it. The cap refuses loudly at the limit and
	// warns before it (never silent, the ADR 0037 stance), and it is a
	// constant rather than a setting because no user can be asked to reason
	// about a decoder's conditioning window.
	MaxHardToHear = 20
)

// nearCapFraction is where the store starts warning that a cap is filling:
// at nine tenths full every successful write carries an actionable warning,
// so the refusal at the cap is never the first anyone hears of it.
const nearCapFraction = 0.9

// The Store's refusals, as matchable sentinels — the window's vocabulary
// form places each one (empty phrase under its field, a full store in the
// general area) by matching with errors.Is, so the rule's wording lives here
// once (the memory book's ADR 0013 arrangement, reused).
var (
	// ErrNoPhrase refuses an entry with no phrase.
	ErrNoPhrase = errors.New("an entry needs a phrase")
	// ErrNoMeaning refuses an entry with no meaning.
	ErrNoMeaning = errors.New("an entry needs a meaning")
	// ErrStoreFull refuses a store past vocabulary.max_entries.
	ErrStoreFull = errors.New("the vocabulary store is full")
	// ErrUnknownID refuses an id no taught entry carries.
	ErrUnknownID = errors.New("no taught entry has id")
	// ErrBiasFull refuses a hard-to-hear flag past MaxHardToHear.
	ErrBiasFull = errors.New("the hard-to-hear list is full")
	// ErrDuplicatePhrase refuses a rename onto a phrase another entry owns.
	ErrDuplicatePhrase = errors.New("that phrase is already taught")
)

// StoreOptions configure a Store. Zero values take the defaults.
type StoreOptions struct {
	// MaxEntries caps how many entries the store holds.
	MaxEntries int
	// MaxInjectedTokens caps the estimated token cost of one injection.
	MaxInjectedTokens int
	// Now is the clock, injectable so tests control every timestamp.
	Now func() time.Time
	// Gate is the backup write barrier (ADR 0045); nil — the CLI, tests —
	// means writes are never held. Only the daemon threads one through.
	Gate *statehold.Gate
}

// Store is the in-memory view of the vocabulary, backed by one TOML file.
// All operations are safe for concurrent use, and every one begins by
// checking whether the file changed on disk — a hand-edit is picked up on
// the very next turn, no restart, no watcher. The check is one stat(2) of a
// file already in the page cache (the memory book's discipline, verbatim).
type Store struct {
	path              string
	maxEntries        int
	maxInjectedTokens int
	now               func() time.Time
	// gate is the backup write barrier (ADR 0045); nil never blocks.
	gate *statehold.Gate
	log  *slog.Logger
	// write persists an entry list; always writeStore outside tests. A field
	// for the memory book's reason: the write-failure contracts (a failed
	// write must never leave the Store claiming an entry it does not hold)
	// need a disk that fails on command, and the real filesystem cannot be
	// made to do that hermetically.
	write func(path string, entries []Entry, nextID int) error

	mu      sync.Mutex
	entries []Entry
	// next is the id high-water mark, persisted with the store (next_id) and
	// only ever ratcheted up: an id, once used, is never handed out again.
	next int
	// loaded, mod and size are the change detector: the file is re-read when
	// its mtime or size no longer matches what was last loaded or written.
	loaded bool
	mod    time.Time
	size   int64
	// corrupt latches when the on-disk file could not be parsed. While set,
	// the Store serves an empty vocabulary (the documented degradation), and
	// the first write moves the unparseable file aside instead of
	// overwriting it — a hand-edit typo must never cost the user their words.
	corrupt bool
}

// NewStore opens the vocabulary store at path. Nothing is read until the
// first operation, so construction is free and a daemon that is never taught
// anything never touches the file.
func NewStore(path string, opts StoreOptions, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{
		path:              path,
		maxEntries:        opts.MaxEntries,
		maxInjectedTokens: opts.MaxInjectedTokens,
		now:               opts.Now,
		gate:              opts.Gate,
		log:               log,
	}
	if s.maxEntries <= 0 {
		s.maxEntries = DefaultMaxEntries
	}
	if s.maxInjectedTokens <= 0 {
		s.maxInjectedTokens = DefaultMaxInjectedTokens
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.write = writeStore
	// Ids are 1-based; the mark only ever moves up from here (refreshLocked).
	s.next = 1
	return s
}

// Path returns the store file, for the window and doctor to name.
func (s *Store) Path() string { return s.path }

// phraseKey reduces a phrase to its spoken identity: lower case, apostrophes
// dropped, punctuation folded to spaces. "Quid", "quid," and "quid" are one
// phrase — STT punctuates inconsistently and teaching must not care — and
// the same folding the intent router's normalize applies, so a phrase
// matches however it was heard. No stemming, no synonyms: identity only.
func phraseKey(phrase string) string {
	var b strings.Builder
	b.Grow(len(phrase))
	for _, r := range strings.ToLower(phrase) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '\'', r == '’':
			// Apostrophes vanish rather than split: "tellin'" and "tellin"
			// are the same spoken word.
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// refreshLocked brings the in-memory entries up to date with the file.
// Callers hold s.mu. Every failure degrades: a missing file is an empty
// vocabulary, an unreadable or unparseable one is a warning plus an empty
// vocabulary — never an error to the caller, never a crash (ADR 0011's
// precedent, via the memory book).
func (s *Store) refreshLocked() {
	info, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		// Deleting the file is a legitimate hand-edit: deletion is deletion.
		s.entries, s.corrupt = nil, false
		s.loaded, s.mod, s.size = true, time.Time{}, 0
		return
	}
	if err != nil {
		if !s.corrupt {
			s.log.Warn("vocabulary store could not be read; continuing with an empty vocabulary",
				"component", "vocabulary", "error", err.Error())
		}
		s.entries, s.corrupt = nil, true
		s.loaded = true
		return
	}
	if s.loaded && info.ModTime().Equal(s.mod) && info.Size() == s.size {
		return // unchanged since last load or write — the common case
	}
	entries, persistedNext, err := readStore(s.path)
	s.loaded, s.mod, s.size = true, info.ModTime(), info.Size()
	if err != nil {
		// Warned per corruption event, not per turn: the mtime/size check
		// above keeps this branch quiet until the file changes again, and
		// content never appears in the message.
		s.log.Warn("vocabulary store could not be parsed; continuing with an empty vocabulary "+
			"(fix the file by hand — it will not be overwritten)",
			"component", "vocabulary", "path", s.path, "error", err.Error())
		s.entries, s.corrupt = nil, true
		return
	}
	// The high-water mark ratchets: the persisted value, the highest id in
	// use, and whatever this Store already promised all hold it up, so a
	// hand-edit that drops next_id cannot cause an id to be reissued.
	if persistedNext > s.next {
		s.next = persistedNext
	}
	if inUse := nextID(entries); inUse > s.next {
		s.next = inUse
	}
	s.entries, s.corrupt = s.normalize(entries), false
	s.log.Debug("vocabulary store loaded", "component", "vocabulary", "entries", len(s.entries))
}

// normalize repairs what a hand-edit may have left out: missing or duplicate
// ids get fresh ones, missing timestamps become now, and a later duplicate
// of an already-seen phrase is dropped — the earlier entry is the taught one,
// and two entries for one phrase is exactly the state teach exists to
// prevent. Repair never fabricates content: an entry with no phrase or no
// meaning carries nothing worth injecting and is dropped.
func (s *Store) normalize(entries []Entry) []Entry {
	seenID := make(map[string]bool, len(entries))
	seenPhrase := make(map[string]bool, len(entries))
	now := s.now()
	out := entries[:0]
	for _, e := range entries {
		e.Phrase = strings.TrimSpace(e.Phrase)
		e.Meaning = strings.TrimSpace(e.Meaning)
		e.Note = strings.TrimSpace(e.Note)
		key := phraseKey(e.Phrase)
		if e.Phrase == "" || e.Meaning == "" || key == "" || seenPhrase[key] {
			continue
		}
		seenPhrase[key] = true
		if e.ID == "" || seenID[e.ID] {
			e.ID = fmt.Sprintf("w%d", s.next)
			s.next++
		}
		seenID[e.ID] = true
		if e.Taught.IsZero() {
			e.Taught = now
		}
		if e.Updated.IsZero() {
			e.Updated = e.Taught
		}
		out = append(out, e)
	}
	// The bias cap holds against hand-edits too: flags past MaxHardToHear
	// (in listing order — the file's order) are cleared with a warning, so
	// the file always states exactly what the bias carries. Silently keeping
	// an inert flag would look enabled while doing nothing.
	flagged := 0
	cleared := 0
	for i := range out {
		if !out[i].HardToHear {
			continue
		}
		flagged++
		if flagged > MaxHardToHear {
			out[i].HardToHear = false
			cleared++
		}
	}
	if cleared > 0 {
		s.log.Warn("hard-to-hear flags past the cap were ignored",
			"component", "vocabulary", "cap", MaxHardToHear, "ignored", cleared)
	}
	return out
}

// nextID returns the smallest integer above every numeric "w<n>" id in use.
func nextID(entries []Entry) int {
	next := 1
	for _, e := range entries {
		if n, err := strconv.Atoi(strings.TrimPrefix(e.ID, "w")); err == nil && n >= next {
			next = n + 1
		}
	}
	return next
}

// saveLocked writes entries to disk and commits them to memory only on
// success, so a failed write can never leave the Store claiming an entry is
// taught when it is not. Callers hold s.mu.
func (s *Store) saveLocked(entries []Entry) error {
	// Entered before the first byte moves, released once the store is
	// settled: `jarvix backup` holds this gate for its coherent cut.
	defer s.gate.Enter()()
	if s.corrupt {
		// The file on disk is one the user may be mid-way through fixing.
		// Move it aside rather than overwrite it: the write proceeds, and
		// the unparseable content survives next to it.
		backup := s.path + ".corrupt"
		if err := os.Rename(s.path, backup); err == nil {
			s.log.Warn("unparseable vocabulary store moved aside before writing",
				"component", "vocabulary", "backup", backup)
		}
		s.corrupt = false
	}
	if err := s.write(s.path, entries, s.next); err != nil {
		return err
	}
	s.entries = entries
	// Record the write's own stat so it is not mistaken for a hand-edit and
	// pointlessly re-read on the next turn.
	if info, err := os.Stat(s.path); err == nil {
		s.loaded, s.mod, s.size = true, info.ModTime(), info.Size()
	}
	return nil
}

// Teach stores a phrase → meaning, or supersedes the meaning when the phrase
// is already taught — never a silent second entry: the phrase is the entry's
// identity, and a duplicate would leave the model holding a contradiction.
// The returned warning is non-empty near the store cap and callers surface
// it — the refusal at the cap must never be the first anyone hears of it.
func (s *Store) Teach(phrase, meaning, note, source string) (Entry, string, error) {
	phrase = strings.TrimSpace(phrase)
	meaning = strings.TrimSpace(meaning)
	note = strings.TrimSpace(note)
	if phrase == "" || phraseKey(phrase) == "" {
		return Entry{}, "", ErrNoPhrase
	}
	if meaning == "" {
		return Entry{}, "", ErrNoMeaning
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()

	if i := s.indexByPhraseLocked(phrase); i >= 0 {
		return s.supersedeLocked(i, phrase, meaning, note, source)
	}

	if len(s.entries) >= s.maxEntries {
		return Entry{}, "", fmt.Errorf(
			"%w (%d entries); forget something stale, or raise vocabulary.max_entries",
			ErrStoreFull, s.maxEntries)
	}
	now := s.now()
	entry := Entry{
		ID:      fmt.Sprintf("w%d", s.next),
		Phrase:  phrase,
		Meaning: meaning,
		Note:    note,
		Taught:  now,
		Updated: now,
		Source:  source,
	}
	// Bumped before the save on purpose: a failed write may skip an id, but
	// no path can ever reuse one.
	s.next++
	next := append(append([]Entry(nil), s.entries...), entry)
	if err := s.saveLocked(next); err != nil {
		return Entry{}, "", err
	}
	s.log.Info("phrase taught", "component", "vocabulary", "id", entry.ID,
		"chars", len(entry.Phrase)+len(entry.Meaning), "entries", len(next))
	return entry, s.capWarningLocked(), nil
}

// supersedeLocked replaces entry i's meaning (and note, and phrase spelling),
// keeping the old value on the trail with both of its timestamps. An
// identical re-teach is a no-op that skips the disk write — repeating
// yourself must not manufacture a revision of nothing. Callers hold s.mu.
func (s *Store) supersedeLocked(i int, phrase, meaning, note, source string) (Entry, string, error) {
	current := s.entries[i]
	if current.Phrase == phrase && current.Meaning == meaning && current.Note == note {
		return copyEntry(current), s.capWarningLocked(), nil
	}
	next := append([]Entry(nil), s.entries...)
	e := next[i]
	now := s.now()
	e.Previous = append(append([]Revision(nil), e.Previous...), Revision{
		Phrase:     e.Phrase,
		Meaning:    e.Meaning,
		Note:       e.Note,
		Taught:     e.Updated,
		Superseded: now,
	})
	e.Phrase, e.Meaning, e.Note, e.Updated, e.Source = phrase, meaning, note, now, source
	next[i] = e
	if err := s.saveLocked(next); err != nil {
		return Entry{}, "", err
	}
	s.log.Info("phrase re-taught", "component", "vocabulary", "id", e.ID,
		"revisions", len(e.Previous))
	return copyEntry(e), s.capWarningLocked(), nil
}

// Update edits one entry by id — the window form's path. A phrase rename is
// allowed but must not collide with another taught phrase: two entries for
// one phrase is the state teach exists to prevent, whatever the surface.
// Content changes supersede onto the trail; an untouched entry writes
// nothing.
func (s *Store) Update(id, phrase, meaning, note, source string) (Entry, error) {
	phrase = strings.TrimSpace(phrase)
	meaning = strings.TrimSpace(meaning)
	note = strings.TrimSpace(note)
	if phrase == "" || phraseKey(phrase) == "" {
		return Entry{}, ErrNoPhrase
	}
	if meaning == "" {
		return Entry{}, ErrNoMeaning
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	i := s.indexLocked(id)
	if i < 0 {
		return Entry{}, fmt.Errorf("%w %q", ErrUnknownID, id)
	}
	if j := s.indexByPhraseLocked(phrase); j >= 0 && j != i {
		return Entry{}, fmt.Errorf("%w: %q is entry %s", ErrDuplicatePhrase,
			s.entries[j].Phrase, s.entries[j].ID)
	}
	entry, _, err := s.supersedeLocked(i, phrase, meaning, note, source)
	return entry, err
}

// SetHardToHear marks or unmarks one phrase for the STT bias. Setting is
// refused at MaxHardToHear — the bias prompt is finite, and a stored flag
// that silently did nothing would be worse than an honest refusal. Not a
// content change: timestamps and the supersede trail are untouched, and
// setting the value an entry already has skips the disk write. The returned
// warning is non-empty as the flag list nears its cap.
func (s *Store) SetHardToHear(id string, hard bool) (Entry, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	i := s.indexLocked(id)
	if i < 0 {
		return Entry{}, "", fmt.Errorf("%w %q", ErrUnknownID, id)
	}
	if s.entries[i].HardToHear == hard {
		return copyEntry(s.entries[i]), s.biasWarningLocked(), nil
	}
	if hard && s.flaggedLocked() >= MaxHardToHear {
		return Entry{}, "", fmt.Errorf(
			"%w (%d phrases); speech recognition can only be biased toward a few — unflag a word first",
			ErrBiasFull, MaxHardToHear)
	}
	next := append([]Entry(nil), s.entries...)
	next[i].HardToHear = hard
	if err := s.saveLocked(next); err != nil {
		return Entry{}, "", err
	}
	s.log.Info("hard-to-hear flag toggled", "component", "vocabulary",
		"id", id, "hard_to_hear", hard, "flagged", s.flaggedLocked())
	return copyEntry(next[i]), s.biasWarningLocked(), nil
}

// Forget deletes an entry from disk — trail and all. Deletion is deletion:
// nothing of a forgotten phrase survives anywhere Jarvix can reach.
func (s *Store) Forget(id string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	i := s.indexLocked(id)
	if i < 0 {
		return Entry{}, fmt.Errorf("%w %q", ErrUnknownID, id)
	}
	forgotten := s.entries[i]
	next := append(append([]Entry(nil), s.entries[:i]...), s.entries[i+1:]...)
	if err := s.saveLocked(next); err != nil {
		return Entry{}, err
	}
	s.log.Info("phrase forgotten", "component", "vocabulary", "id", forgotten.ID,
		"entries", len(next))
	return copyEntry(forgotten), nil
}

// indexLocked finds an entry by id. Callers hold s.mu.
func (s *Store) indexLocked(id string) int {
	for i, e := range s.entries {
		if e.ID == id {
			return i
		}
	}
	return -1
}

// indexByPhraseLocked finds an entry by its phrase identity. Callers hold s.mu.
func (s *Store) indexByPhraseLocked(phrase string) int {
	key := phraseKey(phrase)
	for i, e := range s.entries {
		if phraseKey(e.Phrase) == key {
			return i
		}
	}
	return -1
}

// flaggedLocked counts hard-to-hear entries. Callers hold s.mu.
func (s *Store) flaggedLocked() int {
	n := 0
	for _, e := range s.entries {
		if e.HardToHear {
			n++
		}
	}
	return n
}

// ByPhrase resolves one entry by its spoken identity, for the voice paths
// ("listen for the word quid") and the model's forget tool.
func (s *Store) ByPhrase(phrase string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	if i := s.indexByPhraseLocked(phrase); i >= 0 {
		return copyEntry(s.entries[i]), true
	}
	return Entry{}, false
}

// List returns the entries matching query, most recently taught first — or
// every entry when query is empty. Matching is a forgiving case-insensitive
// substring over phrase, meaning, and note: it serves the window's
// filter-as-you-type box, nothing subtler.
func (s *Store) List(query string) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	q := strings.ToLower(strings.TrimSpace(query))
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		if q == "" ||
			strings.Contains(strings.ToLower(e.Phrase), q) ||
			strings.Contains(strings.ToLower(e.Meaning), q) ||
			strings.Contains(strings.ToLower(e.Note), q) {
			out = append(out, copyEntry(e))
		}
	}
	sortForInjection(out)
	return out
}

// Count reports how full the store is.
func (s *Store) Count() (n, max int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	return len(s.entries), s.maxEntries
}

// BiasCount reports how full the hard-to-hear list is — the doctor's bias
// budget line reads this.
func (s *Store) BiasCount() (n, max int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	return s.flaggedLocked(), MaxHardToHear
}

// HardToHear returns the flagged phrases in listing order, for the STT bias
// prompt (composed by config.STTBiasPromptWith — one copy of the sentence
// rule). Capped defensively even though flags past the cap cannot be stored.
func (s *Store) HardToHear() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	var out []string
	for _, e := range s.entries {
		if e.HardToHear && len(out) < MaxHardToHear {
			out = append(out, e.Phrase)
		}
	}
	return out
}

// capWarningLocked returns the near-cap warning for the store, or "" while
// there is comfortable room. Callers hold s.mu.
func (s *Store) capWarningLocked() string {
	if float64(len(s.entries)) < nearCapFraction*float64(s.maxEntries) {
		return ""
	}
	return fmt.Sprintf("the vocabulary store is nearly full (%d of %d entries); "+
		"suggest forgetting stale words, or raising vocabulary.max_entries",
		len(s.entries), s.maxEntries)
}

// biasWarningLocked returns the near-cap warning for the hard-to-hear list,
// or "" while there is comfortable room. Callers hold s.mu.
func (s *Store) biasWarningLocked() string {
	flagged := s.flaggedLocked()
	if float64(flagged) < nearCapFraction*float64(MaxHardToHear) {
		return ""
	}
	return fmt.Sprintf("the hard-to-hear list is nearly full (%d of %d phrases); "+
		"speech recognition can only be biased toward a few — unflag words it now hears fine",
		flagged, MaxHardToHear)
}

// InjectionWarning is the window's over-budget disclosure: non-empty exactly
// when the current store would leave entries out of the prompt. Decided
// here, in the store, so the sentence and the trim always agree (the
// AmbientWarning arrangement of #104, without the pin split — vocabulary has
// no search tool, so a trim is a plain loss worth flagging).
func (s *Store) InjectionWarning(speakBack bool) string {
	inj := s.Inject(speakBack)
	if inj.Trimmed == 0 {
		return ""
	}
	return fmt.Sprintf("the taught words no longer fit vocabulary.max_injected_tokens: the %d least "+
		"recently taught %s left out of every prompt — forget stale words, or raise the budget",
		inj.Trimmed, plural(inj.Trimmed, "entry is", "entries are"))
}

// sortForInjection orders entries most recently taught first — the words the
// user last touched are the ones a token-capped injection keeps, and the
// order every listing shows. Ties break on Taught then ID so the order is
// deterministic.
func sortForInjection(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].Updated.Equal(entries[j].Updated) {
			return entries[i].Updated.After(entries[j].Updated)
		}
		if !entries[i].Taught.Equal(entries[j].Taught) {
			return entries[i].Taught.After(entries[j].Taught)
		}
		return entries[i].ID < entries[j].ID
	})
}

// copyEntry deep-copies an entry so callers can never mutate the Store's
// slices through a returned value.
func copyEntry(e Entry) Entry {
	e.Previous = append([]Revision(nil), e.Previous...)
	return e
}
