package memory

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

// Book caps and defaults.
const (
	// DefaultMaxFacts caps the store. Two hundred short facts is far more
	// than a curated memory should hold; the cap exists so a store nobody
	// prunes cannot grow without bound, and it warns well before it refuses.
	DefaultMaxFacts = 200
	// DefaultMaxInjectedTokens caps what the memory block may cost a turn.
	// ~500 tokens is a page of short facts — enough for a well-kept store,
	// small enough that memory can never crowd out the conversation.
	DefaultMaxInjectedTokens = 500
	// MinInjectedTokens is the smallest configurable injection budget: below
	// this not even the preamble and one fact fit, and the feature would be
	// silently useless while looking enabled.
	MinInjectedTokens = 100
)

// nearCapFraction is where the store starts warning that it is filling up:
// at nine tenths full every successful remember carries an actionable
// warning, so the refusal at the cap is never the first anyone hears of it.
const nearCapFraction = 0.9

// The Book's refusals, as matchable sentinels. The window's memory form
// (issue #100) must place each refusal — empty content under its text field,
// a full store in the form's general area, an unknown id as a crisp
// parameter error — and matching wrapped sentinels with errors.Is keeps that
// placement decision from becoming a second copy of the rule's wording
// (ADR 0013: the rule lives here; callers only place its message). The
// messages themselves are unchanged: each sentinel is the sentence's own
// opening words, and the dynamic detail is wrapped around it.
var (
	// ErrNoContent refuses a fact with nothing in it.
	ErrNoContent = errors.New("a fact needs content")
	// ErrStoreFull refuses a store past memory.max_facts.
	ErrStoreFull = errors.New("the memory store is full")
	// ErrUnknownID refuses an id no stored fact carries.
	ErrUnknownID = errors.New("no remembered fact has id")
)

// BookOptions configure a Book. Zero values take the defaults.
type BookOptions struct {
	// MaxFacts caps how many facts the store holds.
	MaxFacts int
	// MaxInjectedTokens caps the estimated token cost of one injection.
	MaxInjectedTokens int
	// Now is the clock, injectable so tests control every timestamp.
	Now func() time.Time
	// Gate is the backup write barrier (ADR 0045); nil — the CLI, tests —
	// means writes are never held. Only the daemon threads one through.
	Gate *statehold.Gate
}

// Book is the in-memory view of the fact store, backed by one TOML file. All
// operations are safe for concurrent use, and every one of them begins by
// checking whether the file changed on disk — a hand-edit is picked up on the
// very next turn, no restart, no watcher. The check is one stat(2) of a file
// already in the page cache, so consultation adds no measurable latency.
type Book struct {
	path              string
	maxFacts          int
	maxInjectedTokens int
	now               func() time.Time
	// gate is the backup write barrier (ADR 0045); nil never blocks.
	gate *statehold.Gate
	log  *slog.Logger
	// write persists a fact list; always writeStore outside tests. It is a
	// field for one reason: the write-failure contracts (a failed stats
	// write must cost exactly the stats, never the book) need a disk that
	// fails on command, and the real filesystem cannot be made to do that
	// hermetically — writeStore itself repairs the permission tricks a test
	// could play.
	write func(path string, facts []Fact, nextID int) error

	mu    sync.Mutex
	facts []Fact
	// next is the id high-water mark, persisted with the store (next_id) and
	// only ever ratcheted up: an id, once used, is never handed out again —
	// even after the fact is forgotten — so a supersede trail or an old
	// conversation naming "m2" can never come to describe a different fact.
	next int
	// loaded, mod and size are the change detector: the file is re-read when
	// its mtime or size no longer matches what was last loaded or written.
	loaded bool
	mod    time.Time
	size   int64
	// corrupt latches when the on-disk file could not be parsed. While set,
	// the Book serves an empty memory (the documented degradation), and the
	// first write moves the unparseable file aside instead of overwriting it
	// — a hand-edit typo must never cost the user their facts.
	corrupt bool
}

// NewBook opens the fact store at path. Nothing is read until the first
// operation, so construction is free and a daemon that is never asked to
// remember anything never touches the file.
func NewBook(path string, opts BookOptions, log *slog.Logger) *Book {
	if log == nil {
		log = slog.Default()
	}
	b := &Book{
		path:              path,
		maxFacts:          opts.MaxFacts,
		maxInjectedTokens: opts.MaxInjectedTokens,
		now:               opts.Now,
		gate:              opts.Gate,
		log:               log,
	}
	if b.maxFacts <= 0 {
		b.maxFacts = DefaultMaxFacts
	}
	if b.maxInjectedTokens <= 0 {
		b.maxInjectedTokens = DefaultMaxInjectedTokens
	}
	if b.now == nil {
		b.now = time.Now
	}
	b.write = writeStore
	// Ids are 1-based; the mark only ever moves up from here (refreshLocked).
	b.next = 1
	return b
}

// Path returns the store file, for the CLI and doctor to name.
func (b *Book) Path() string { return b.path }

// refreshLocked brings the in-memory facts up to date with the file. Callers
// hold b.mu. Every failure degrades: a missing file is an empty memory, an
// unreadable or unparseable one is a warning plus an empty memory — never an
// error to the caller, never a crash (the history precedent, ADR 0011).
func (b *Book) refreshLocked() {
	info, err := os.Stat(b.path)
	if errors.Is(err, fs.ErrNotExist) {
		// Deleting the file is a legitimate hand-edit: deletion is deletion.
		b.facts, b.corrupt = nil, false
		b.loaded, b.mod, b.size = true, time.Time{}, 0
		return
	}
	if err != nil {
		if !b.corrupt {
			b.log.Warn("memory store could not be read; continuing with an empty memory",
				"component", "memory", "error", err.Error())
		}
		b.facts, b.corrupt = nil, true
		b.loaded = true
		return
	}
	if b.loaded && info.ModTime().Equal(b.mod) && info.Size() == b.size {
		return // unchanged since last load or write — the common case
	}
	facts, persistedNext, err := readStore(b.path)
	b.loaded, b.mod, b.size = true, info.ModTime(), info.Size()
	if err != nil {
		// Warned per corruption event, not per turn: the mtime/size check
		// above keeps this branch from re-running until the file changes
		// again, and content never appears in the message.
		b.log.Warn("memory store could not be parsed; continuing with an empty memory "+
			"(fix the file by hand — it will not be overwritten)",
			"component", "memory", "path", b.path, "error", err.Error())
		b.facts, b.corrupt = nil, true
		return
	}
	// The high-water mark ratchets: the persisted value, the highest id in
	// use, and whatever this Book already promised all hold it up, so a
	// hand-edit that drops next_id cannot cause an id to be reissued.
	if persistedNext > b.next {
		b.next = persistedNext
	}
	if inUse := nextID(facts); inUse > b.next {
		b.next = inUse
	}
	b.facts, b.corrupt = b.normalize(facts), false
	b.log.Debug("memory store loaded", "component", "memory", "facts", len(b.facts))
}

// normalize repairs what a hand-edit may have left out: missing or duplicate
// ids get fresh ones, and missing timestamps become now — so a fact the user
// just typed in is treated as freshly confirmed rather than sorting to the
// bottom of the injection order and being trimmed first.
func (b *Book) normalize(facts []Fact) []Fact {
	seen := make(map[string]bool, len(facts))
	now := b.now()
	out := facts[:0]
	for _, f := range facts {
		if strings.TrimSpace(f.Content) == "" {
			continue // an empty [[fact]] carries nothing worth injecting
		}
		if f.ID == "" || seen[f.ID] {
			f.ID = fmt.Sprintf("m%d", b.next)
			b.next++
		}
		seen[f.ID] = true
		if f.Stored.IsZero() {
			f.Stored = now
		}
		if f.Updated.IsZero() {
			f.Updated = f.Stored
		}
		// Retrieval stats can only arrive broken by hand-edit. A negative
		// count is nonsense, repaired to never-retrieved; a last_retrieved
		// with no count is evidence of one retrieval, so the count follows
		// the timestamp rather than the timestamp being erased — the repair
		// must never fabricate, but it must not discard the user's line
		// either.
		if f.TimesRetrieved < 0 {
			f.TimesRetrieved = 0
		}
		if f.TimesRetrieved == 0 && !f.LastRetrieved.IsZero() {
			f.TimesRetrieved = 1
		}
		out = append(out, f)
	}
	return out
}

// nextID returns the smallest integer above every numeric "m<n>" id in use,
// so ids never repeat within a store's lifetime even after forgets.
func nextID(facts []Fact) int {
	next := 1
	for _, f := range facts {
		if n, err := strconv.Atoi(strings.TrimPrefix(f.ID, "m")); err == nil && n >= next {
			next = n + 1
		}
	}
	return next
}

// saveLocked writes facts to disk and commits them to memory only on
// success, so a failed write can never leave the Book claiming a fact is
// stored when it is not. Callers hold b.mu.
func (b *Book) saveLocked(facts []Fact) error {
	// Entered before the first byte moves, released once the store is
	// settled: `jarvix backup` holds this gate for its coherent cut.
	defer b.gate.Enter()()
	if b.corrupt {
		// The file on disk is one the user may be mid-way through fixing.
		// Move it aside rather than overwrite it: the write proceeds, and
		// the unparseable content survives next to it.
		backup := b.path + ".corrupt"
		if err := os.Rename(b.path, backup); err == nil {
			b.log.Warn("unparseable memory store moved aside before writing",
				"component", "memory", "backup", backup)
		}
		b.corrupt = false
	}
	if err := b.write(b.path, facts, b.next); err != nil {
		return err
	}
	b.facts = facts
	// Record the write's own stat so it is not mistaken for a hand-edit and
	// pointlessly re-read on the next turn.
	if info, err := os.Stat(b.path); err == nil {
		b.loaded, b.mod, b.size = true, info.ModTime(), info.Size()
	}
	return nil
}

// Add stores a new fact. It refuses at the store cap with an actionable
// error; near the cap the returned warning is non-empty and callers surface
// it — the refusal must never be the first anyone hears of the limit.
func (b *Book) Add(content, source string) (Fact, string, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return Fact{}, "", ErrNoContent
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshLocked()
	if len(b.facts) >= b.maxFacts {
		return Fact{}, "", fmt.Errorf(
			"%w (%d facts); forget something stale, or raise memory.max_facts",
			ErrStoreFull, b.maxFacts)
	}
	now := b.now()
	fact := Fact{
		ID:      fmt.Sprintf("m%d", b.next),
		Content: content,
		Stored:  now,
		Updated: now,
		Source:  source,
	}
	// Bumped before the save on purpose: a failed write may skip an id, but
	// no path can ever reuse one.
	b.next++
	next := append(append([]Fact(nil), b.facts...), fact)
	if err := b.saveLocked(next); err != nil {
		return Fact{}, "", err
	}
	b.log.Info("fact remembered", "component", "memory", "id", fact.ID,
		"chars", len(fact.Content), "facts", len(next))
	return fact, b.capWarningLocked(), nil
}

// Update supersedes a fact's content, keeping the old value on the trail
// with both of its timestamps — "when did that change" stays answerable.
func (b *Book) Update(id, content, source string) (Fact, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return Fact{}, ErrNoContent
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshLocked()
	i := b.indexLocked(id)
	if i < 0 {
		return Fact{}, fmt.Errorf("%w %q", ErrUnknownID, id)
	}
	next := append([]Fact(nil), b.facts...)
	f := next[i]
	now := b.now()
	f.Previous = append(append([]Revision(nil), f.Previous...), Revision{
		Content:    f.Content,
		Stored:     f.Updated,
		Superseded: now,
	})
	f.Content, f.Updated, f.Source = content, now, source
	next[i] = f
	if err := b.saveLocked(next); err != nil {
		return Fact{}, err
	}
	b.log.Info("fact superseded", "component", "memory", "id", f.ID,
		"chars", len(f.Content), "revisions", len(f.Previous))
	return f, nil
}

// SetPinned marks or unmarks a fact as ambient (issue #104). A pin is not a
// content change: Stored, Updated, and the supersede trail are untouched, so
// pinning never reorders the injection and never manufactures a revision.
// Setting the value it already has is a no-op that skips the disk write.
func (b *Book) SetPinned(id string, pinned bool) (Fact, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshLocked()
	i := b.indexLocked(id)
	if i < 0 {
		return Fact{}, fmt.Errorf("%w %q", ErrUnknownID, id)
	}
	if b.facts[i].Pinned == pinned {
		return copyFact(b.facts[i]), nil
	}
	next := append([]Fact(nil), b.facts...)
	next[i].Pinned = pinned
	if err := b.saveLocked(next); err != nil {
		return Fact{}, err
	}
	b.log.Info("fact pin toggled", "component", "memory", "id", id, "pinned", pinned)
	return copyFact(next[i]), nil
}

// Forget deletes a fact from disk — trail and all. Deletion is deletion:
// nothing of a forgotten fact survives anywhere Jarvix can reach.
func (b *Book) Forget(id string) (Fact, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshLocked()
	i := b.indexLocked(id)
	if i < 0 {
		return Fact{}, fmt.Errorf("%w %q", ErrUnknownID, id)
	}
	forgotten := b.facts[i]
	next := append(append([]Fact(nil), b.facts[:i]...), b.facts[i+1:]...)
	if err := b.saveLocked(next); err != nil {
		return Fact{}, err
	}
	b.log.Info("fact forgotten", "component", "memory", "id", forgotten.ID,
		"facts", len(next))
	return forgotten, nil
}

// indexLocked finds a fact by id. Callers hold b.mu.
func (b *Book) indexLocked(id string) int {
	for i, f := range b.facts {
		if f.ID == id {
			return i
		}
	}
	return -1
}

// List returns the facts matching query, most recently confirmed first — or
// every fact when query is empty. Matching is forgiving (case-insensitive
// substring, or any significant word shared), because "what do you know
// about my setup" should find "the user's terminal is Ghostty".
func (b *Book) List(query string) []Fact {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshLocked()
	out := make([]Fact, 0, len(b.facts))
	for _, f := range b.facts {
		if query == "" || matchesQuery(query, f.Content) {
			out = append(out, copyFact(f))
		}
	}
	sortForInjection(out)
	return out
}

// Search is the memory.search tool's storage half (ADR 0037): the ranked,
// deterministic lookup over the whole book — pinned facts included, so a
// search can never claim a fact does not exist. It returns at most
// maxSearchResults facts, best match first (see rankSearch), and records the
// retrieval on each returned fact: times_retrieved increments, last_retrieved
// becomes now, persisted in ONE write for the whole result set — the batch is
// the search call itself, so a search costs exactly one store write, never
// one per fact.
//
// The stats write is best-effort by design: the facts were retrieved whether
// or not the bookkeeping about it reaches disk, so a failed write logs a
// warning and the search still answers. saveLocked commits the in-memory
// facts only on success, which is what makes the failure safe — the book
// never holds stats the file does not, the supersede trail is never touched
// (only two scalar fields change), and the next successful search simply
// counts from the persisted state.
func (b *Book) Search(query string) []Fact {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshLocked()
	matched := rankSearch(query, b.facts)
	if len(matched) > maxSearchResults {
		matched = matched[:maxSearchResults]
	}
	if len(matched) == 0 {
		return nil
	}
	hit := make(map[string]bool, len(matched))
	for _, f := range matched {
		hit[f.ID] = true
	}
	now := b.now()
	next := append([]Fact(nil), b.facts...)
	for i := range next {
		if hit[next[i].ID] {
			next[i].TimesRetrieved++
			next[i].LastRetrieved = now
		}
	}
	if err := b.saveLocked(next); err != nil {
		b.log.Warn("retrieval stats were not persisted; the search still answered",
			"component", "memory", "error", err.Error())
	}
	// The returned copies carry the retrieval that just happened — matched
	// holds copies made by rankSearch, so bumping them mutates nothing the
	// Book owns even when the save failed and the Book kept its old state.
	for i := range matched {
		matched[i].TimesRetrieved++
		matched[i].LastRetrieved = now
	}
	return matched
}

// Similar returns the stored facts that look like statements about the same
// thing as content — the supersede candidates a remember must decide about
// before it may accumulate a contradiction.
func (b *Book) Similar(content string) []Fact {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshLocked()
	var out []Fact
	for _, f := range b.facts {
		if similar(content, f.Content) {
			out = append(out, copyFact(f))
		}
	}
	sortForInjection(out)
	return out
}

// Count reports how full the store is.
func (b *Book) Count() (n, max int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshLocked()
	return len(b.facts), b.maxFacts
}

// capWarningLocked returns the near-cap warning, or "" while the store has
// comfortable room. Callers hold b.mu.
func (b *Book) capWarningLocked() string {
	if float64(len(b.facts)) < nearCapFraction*float64(b.maxFacts) {
		return ""
	}
	return fmt.Sprintf("the memory store is nearly full (%d of %d facts); "+
		"suggest forgetting stale facts, or raising memory.max_facts",
		len(b.facts), b.maxFacts)
}

// Inject builds the memory block for one model turn under the retrieval
// policy of ADR 0037. The ambient set is decided here, in code:
//
//   - No fact pinned and the whole book fits the budget: every fact is
//     ambient — exactly the pre-#104 behaviour, so a user who never touches
//     pinning sees no change at all.
//   - Any fact pinned: exactly the pinned facts are ambient. The budget
//     applies to them alone — an over-budget pinned set trims its least
//     recently confirmed tail, disclosed to the model here and to the user
//     via AmbientWarning (never silently). Unpinned facts are not in the
//     prompt; the block says how many exist and that memory.search finds
//     them.
//   - No fact pinned and the book no longer fits: nothing is ambient. The
//     old behaviour here was a silent tail-drop; the honest replacement is
//     a block that says all N facts are searchable, plus the user-facing
//     warning telling them pinning is how facts get back into every prompt.
//
// Facts are only ever dropped from the block, never from storage, and
// injection never touches the retrieval stats — ambient presence is not a
// retrieval.
func (b *Book) Inject() Injection {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshLocked()
	facts := make([]Fact, 0, len(b.facts))
	pinned := make([]Fact, 0, len(b.facts))
	for _, f := range b.facts {
		facts = append(facts, copyFact(f))
		if f.Pinned {
			pinned = append(pinned, copyFact(f))
		}
	}
	sortForInjection(facts)
	sortForInjection(pinned)

	if len(pinned) == 0 {
		inj := buildInjection(facts, b.maxInjectedTokens, 0)
		if inj.Trimmed == 0 {
			return inj // the graceful default: everything fits, all ambient
		}
		return searchOnlyInjection(len(facts))
	}
	return buildInjection(pinned, b.maxInjectedTokens, len(facts)-len(pinned))
}

// AmbientWarning is the Memory tab's over-budget disclosure (issue #104):
// non-empty exactly when the current book would leave facts out of the
// prompt *without the user having chosen that* — a pinned set past the
// budget, or an unpinned book that outgrew it. The designed split (pins plus
// searchable rest, everything fitting) warns about nothing: that state is
// the feature working. The daemon serves this from memory.list, so the
// warning appears wherever the facts do — a trim is never silent again.
func (b *Book) AmbientWarning() string {
	inj := b.Inject()
	pinnedAny := false
	for _, f := range inj.Facts {
		if f.Pinned {
			pinnedAny = true
			break
		}
	}
	switch {
	case inj.Trimmed > 0 && pinnedAny:
		return fmt.Sprintf("the pinned facts do not fit memory.max_injected_tokens: the %d least "+
			"recently confirmed pinned %s left out of every prompt — unpin something, or raise the budget",
			inj.Trimmed, plural(inj.Trimmed, "fact is", "facts are"))
	case inj.Trimmed > 0:
		// Pinned facts exist in the store (Trimmed only arises for an
		// ambient set) but none survived into the block: the pathological
		// tiny-budget case. The same sentence applies — these are pins the
		// prompt is not carrying.
		return fmt.Sprintf("the pinned facts do not fit memory.max_injected_tokens: %d pinned %s "+
			"left out of every prompt — unpin something, or raise the budget",
			inj.Trimmed, plural(inj.Trimmed, "fact is", "facts are"))
	case inj.Searchable > 0 && len(inj.Facts) == 0 && inj.Trimmed == 0 && inj.Total == inj.Searchable:
		return fmt.Sprintf("the %d remembered facts no longer fit memory.max_injected_tokens and none "+
			"are pinned, so none ride the prompt; pin the facts that must shape every answer — "+
			"the rest stay reachable with memory.search", inj.Total)
	}
	return ""
}

// sortForInjection orders facts most recently confirmed first — the facts
// the user last touched are the ones a token-capped injection keeps, and the
// order every listing surface shows. Ties break on Stored then ID so the
// order is deterministic.
func sortForInjection(facts []Fact) {
	sort.SliceStable(facts, func(i, j int) bool {
		if !facts[i].Updated.Equal(facts[j].Updated) {
			return facts[i].Updated.After(facts[j].Updated)
		}
		if !facts[i].Stored.Equal(facts[j].Stored) {
			return facts[i].Stored.After(facts[j].Stored)
		}
		return facts[i].ID < facts[j].ID
	})
}

// copyFact deep-copies a fact so callers can never mutate the Book's slices
// through a returned value.
func copyFact(f Fact) Fact {
	f.Previous = append([]Revision(nil), f.Previous...)
	return f
}
