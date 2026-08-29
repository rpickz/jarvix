package undo

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/statehold"
)

// ErrUnknownAction is returned when nothing in the account carries an id.
var ErrUnknownAction = errors.New("no action in the account has that id")

// StoreOptions configure a Store.
type StoreOptions struct {
	// MaxActions overrides MaxActions. Zero uses the constant.
	MaxActions int
	// Now is the clock, injected so tests are deterministic.
	Now func() time.Time
	// Publish emits one bus event; nil publishes nothing. The account is a
	// surface like any other, and a window watching it should not have to
	// poll.
	Publish func(event string, data map[string]any)
	// Gate is the backup write barrier (ADR 0045); nil is never held.
	Gate *statehold.Gate
}

// Store is the account: one TOML file, read through a stat so a hand-edit is
// live on the next question, written atomically.
//
// It implements Recorder, so a Store is what the daemon installs on the tool
// context and every tool records through.
type Store struct {
	path    string
	max     int
	now     func() time.Time
	publish func(string, map[string]any)
	gate    *statehold.Gate
	log     *slog.Logger
	// write is the disk seam: writeStore in production, a failing stub in
	// tests. A field for the memory book's reason — the write-failure
	// contract (a failed write must cost exactly nothing in memory) needs a
	// disk that fails on command, hermetically.
	write func(path string, p persisted) error

	mu      sync.Mutex
	st      persisted
	loaded  bool
	mod     time.Time
	size    int64
	corrupt bool
}

// NewStore opens the account at path. Nothing is read until the first
// operation, so construction is free and a daemon that never acts never
// touches the disk.
func NewStore(path string, opts StoreOptions, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{
		path: path, max: opts.MaxActions, now: opts.Now,
		publish: opts.Publish, gate: opts.Gate, log: log, write: writeStore,
	}
	if s.max <= 0 {
		s.max = MaxActions
	}
	if s.now == nil {
		s.now = time.Now
	}
	// Ids are 1-based; the mark only ever moves up from here.
	s.st.nextID = 1
	return s
}

// Path returns the file the account lives in, so every surface can tell the
// user where to read it by hand.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Bound returns the cap, for the disclosure. A surface that lists the account
// says this number rather than hard-coding one, so the sentence cannot drift
// from the file.
func (s *Store) Bound() int {
	if s == nil {
		return 0
	}
	return s.max
}

// Append implements Recorder: one action becomes one record.
//
// It is the only path that mints an id, and the only one that evicts. Unlike
// every other store here it cannot refuse at the cap: refusing to record
// would leave an action that happened with nothing in the account, which is
// the one outcome this feature exists to prevent. So the oldest record goes,
// the count of what has gone is kept, and the listing says so.
func (s *Store) Append(a Action) (Record, error) {
	if s == nil {
		return Record{}, fmt.Errorf("the action account is not available")
	}
	a.Summary = strings.TrimSpace(a.Summary)
	if a.Summary == "" {
		// A record nobody can read is not a record. Refused rather than
		// stored blank, because a row saying nothing in an account of what
		// was done is worse than a caller that has to say what it did.
		return Record{}, fmt.Errorf("an action needs a summary saying what changed")
	}
	s.mu.Lock()
	s.refreshLocked()
	next := clone(s.st)
	rec := Record{ID: mintID(&next), At: s.now().UTC(), Action: a}
	next.records = append(next.records, rec)
	next.forgotten += evict(&next, s.max)
	if err := s.saveLocked(next); err != nil {
		s.mu.Unlock()
		s.log.Warn("an action could not be written to the account",
			"component", "undo", "path", s.path, "tool", a.Tool, "error", err.Error())
		return Record{}, err
	}
	s.mu.Unlock()
	s.emit("recorded", rec)
	return rec, nil
}

// List returns the account newest-first, with the bound and what it has
// dropped. Both numbers travel with the rows on purpose: every surface that
// shows the account has to be able to say "and N older ones I no longer
// keep", and a caller that had to ask for the disclosure separately is a
// caller that will forget to.
func (s *Store) List() View {
	if s == nil {
		return View{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	out := append([]Record(nil), s.st.records...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return View{Records: out, Bound: s.max, Forgotten: s.st.forgotten, Path: s.path,
		Now: s.now().UTC()}
}

// View is the account as a reader sees it.
type View struct {
	// Records are newest-first.
	Records []Record
	// Bound is how many the store keeps.
	Bound int
	// Forgotten is how many the cap has dropped over the file's lifetime.
	Forgotten int
	// Path is where the file is, so a surface can point at it.
	Path string
	// Now is the store's own clock at the moment the view was taken (#210).
	//
	// It travels with the rows for the same reason the bound does: a surface
	// that says "4 minutes ago" has to measure that against the clock the
	// records were written with, not against its own. A window reads the
	// account over a socket and has no business subtracting one machine's idea
	// of the time from another's — and a test that injects a clock gets a
	// hermetic answer out of both halves rather than out of one.
	Now time.Time
}

// Disclosure is the bound as a sentence, for every surface that lists the
// account. It is one function rather than a format string in each caller
// because "I only keep the last N" is a promise, and a promise worded three
// different ways in three places is three promises.
func (v View) Disclosure() string {
	if v.Forgotten > 0 {
		return fmt.Sprintf("I keep the last %d actions; %d older ones have dropped off.",
			v.Bound, v.Forgotten)
	}
	return fmt.Sprintf("I keep the last %d actions.", v.Bound)
}

// Get returns one record by id.
func (s *Store) Get(id string) (Record, error) {
	if s == nil {
		return Record{}, ErrUnknownAction
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	for _, r := range s.st.records {
		if r.ID == id {
			return r, nil
		}
	}
	return Record{}, fmt.Errorf("%w: %s", ErrUnknownAction, id)
}

// Latest returns the most recent record that can still be reversed, and the
// most recent record overall.
//
// Both, because "undo that" has two honest answers and they are different
// sentences. When the last thing Jarvix did was a shell command, the answer
// is not "here is the config write from ten minutes ago" — it is "the last
// thing I did was run a command, and I can't take that back; the one before
// it I can". Handing back only the reversible one would quietly skip past the
// thing the user meant.
func (s *Store) Latest() (reversible Record, last Record, ok bool) {
	if s == nil {
		return Record{}, Record{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	records := append([]Record(nil), s.st.records...)
	sort.SliceStable(records, func(i, j int) bool { return records[i].At.After(records[j].At) })
	for i, r := range records {
		if i == 0 {
			last, ok = r, true
		}
		if reversible.ID == "" && r.Reversible() {
			reversible = r
		}
	}
	return reversible, last, ok
}

// Job returns one job's records, oldest first — which is the order they
// happened in, so a caller reversing them walks the slice backwards.
//
// The id is set by undo.Note from the job context a runner installs (#200,
// ADR 0065). A job id nothing carries returns an empty slice, which is the
// honest answer for a job that did nothing rather than an error — and it is
// what a job that only read things gets.
func (s *Store) Job(job string) []Record {
	if s == nil || strings.TrimSpace(job) == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	out := make([]Record, 0, 4)
	for _, r := range s.st.records {
		if r.Job == job {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// MarkUndone records that one action has been reversed by another. Both rows
// stay: the account is what happened, and a reversal happening is part of
// what happened.
func (s *Store) MarkUndone(id, by string) error {
	if s == nil {
		return ErrUnknownAction
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	next := clone(s.st)
	found := false
	for i := range next.records {
		if next.records[i].ID != id {
			continue
		}
		next.records[i].UndoneBy = by
		next.records[i].UndoneAt = s.now().UTC()
		found = true
		break
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrUnknownAction, id)
	}
	return s.saveLocked(next)
}

// Forget drops one row from the account.
//
// It is the account's only deletion, and it deletes rather than tombstones —
// the conversation archive's stance (ADR 0027), for the same reason. An
// approved shell command is recorded verbatim, and a user who dictated a
// secret at an interactive prompt must have a way to remove it that actually
// removes it. The id is not reissued afterwards: the high-water mark only
// ever moves up, so a sentence that once named an action can never come to
// name a different one.
func (s *Store) Forget(id string) error {
	if s == nil {
		return ErrUnknownAction
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	next := clone(s.st)
	for i, r := range next.records {
		if r.ID != id {
			continue
		}
		next.records = append(next.records[:i], next.records[i+1:]...)
		return s.saveLocked(next)
	}
	return fmt.Errorf("%w: %s", ErrUnknownAction, id)
}

// emit publishes one undo.changed event, if anyone is listening. Never called
// with s.mu held — Publish reaches the bus, and a report must never hold the
// store's lock while it waits (the focus and reminder rule).
func (s *Store) emit(action string, rec Record) {
	if s.publish == nil {
		return
	}
	// The summary travels; the restore payload never does. A bus event
	// carrying the previous contents of config.toml would put the user's api
	// keys on every connected socket, which is exactly the mistake the
	// typing audit was careful not to make (ADR 0023).
	s.publish("undo.changed", map[string]any{
		"action": action, "id": rec.ID, "tool": rec.Tool,
		"summary": rec.Summary, "reversible": rec.Reversible(),
	})
}

// refreshLocked brings the in-memory account up to date with the file.
// Callers hold s.mu. Every failure degrades: a missing file is an empty
// account, an unreadable or unparseable one is a warning plus an empty
// account — never an error to the caller, never a crash (ADR 0011's
// precedent, via the memory book).
//
// Degrading towards "I have no account" rather than "the account I loaded
// last time still stands" is the safe direction and is chosen deliberately:
// an account that kept answering from a stale copy would offer to undo an
// action whose restore payload the user had just edited by hand.
func (s *Store) refreshLocked() {
	info, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		// Deleting the file is a legitimate hand-edit, and it means exactly
		// what it looks like: no account. The high-water mark is kept in
		// memory so a daemon that has already minted ids does not reissue
		// them into a file the user emptied.
		s.st.records, s.st.forgotten = nil, 0
		s.corrupt = false
		s.loaded, s.mod, s.size = true, time.Time{}, 0
		return
	}
	if err != nil {
		if !s.corrupt {
			s.log.Warn("the action account could not be read; continuing with an empty account",
				"component", "undo", "path", s.path, "error", err.Error())
		}
		s.st.records = nil
		s.corrupt, s.loaded = true, true
		return
	}
	if s.loaded && info.ModTime().Equal(s.mod) && info.Size() == s.size {
		return // unchanged since the last load or write — the common case
	}
	p, err := readStore(s.path)
	s.loaded, s.mod, s.size = true, info.ModTime(), info.Size()
	if err != nil {
		// Warned per corruption event, not per question: the mtime/size check
		// above keeps this branch quiet until the file changes again. Content
		// never appears in the message.
		if !s.corrupt {
			s.log.Warn("the action account could not be parsed; continuing with an empty account "+
				"(the file is left alone until something is recorded)",
				"component", "undo", "path", s.path, "error", err.Error())
		}
		s.st.records = nil
		s.corrupt = true
		return
	}
	// The high-water mark ratchets: the persisted value, the highest id in
	// use, and whatever this Store already promised all hold it up, so a
	// hand-edit that drops it cannot cause an id to be reissued.
	if p.nextID < s.st.nextID {
		p.nextID = s.st.nextID
	}
	s.st = normalize(p)
	s.corrupt = false
}

// saveLocked writes the account and only then commits it to memory, so a
// failed write leaves the store describing what is actually on disk.
func (s *Store) saveLocked(p persisted) error {
	// Entered before the first byte moves, released once the store is
	// settled: `jarvix backup` holds this gate for its coherent cut.
	defer s.gate.Enter()()
	if s.corrupt {
		// Never overwrite a file we could not read: the user's hand-edit may
		// be one typo away from correct, and it is theirs.
		backup := s.path + ".corrupt"
		if err := os.Rename(s.path, backup); err == nil {
			s.log.Warn("unparseable action account moved aside before writing",
				"component", "undo", "path", s.path, "backup", backup)
		}
		s.corrupt = false
	}
	if err := s.write(s.path, p); err != nil {
		return err
	}
	s.st = p
	// Record the write's own stat so it is not mistaken for a hand-edit and
	// pointlessly re-read on the next operation.
	if info, err := os.Stat(s.path); err == nil {
		s.loaded, s.mod, s.size = true, info.ModTime(), info.Size()
	}
	return nil
}

// normalize makes a hand-edited file behave like a written one: blank and
// duplicate rows dropped, an unrecognised kind folded to "none" with a stated
// reason, a restore stanza that does not match its kind dropped, and the cap
// held. Nothing is written back — a file that is merely untidy still works,
// and only a real change rewrites it.
//
// The repair never fabricates: a row with no summary is dropped because there
// is nothing to repair it into, and a kind the reader does not recognise
// becomes an honest "I can't say how to undo that" rather than a guess at
// which of the two reversals was meant.
//
// It deliberately does NOT apply the cap. A file with more rows than the
// bound is one the user put them in, and Jarvix showing fewer than the file
// holds would be the silent forgetting this whole store is written against;
// the bound is what Jarvix keeps, enforced where Jarvix writes (Append).
func normalize(p persisted) persisted {
	inFile := make(map[string]bool, len(p.records))
	for _, r := range p.records {
		inFile[r.ID] = true
	}
	seen := make(map[string]bool, len(p.records))
	out := make([]Record, 0, len(p.records))
	for _, r := range p.records {
		r.Summary = strings.TrimSpace(r.Summary)
		if r.Summary == "" {
			continue
		}
		if r.ID == "" || seen[r.ID] {
			r.ID = freshID(&p, inFile, seen)
		}
		seen[r.ID] = true
		switch r.Restore.Kind {
		case KindFile:
			if r.Restore.File == nil {
				r.Restore = Restore{Kind: KindNone,
					Because: "the record says it changed a file but does not say which"}
			}
			r.Restore.Window = nil
		case KindWindow:
			if r.Restore.Window == nil {
				r.Restore = Restore{Kind: KindNone,
					Because: "the record says it moved a window but does not say which"}
			}
			r.Restore.File = nil
		default:
			r.Restore = Restore{Kind: KindNone, Because: r.Restore.Because}
		}
		out = append(out, r)
	}
	p.records = out
	return p
}

// evict drops the oldest records until the account fits, and reports how many
// went. Oldest by position rather than by timestamp: the file's order is the
// order things were appended in, and a hand-written row with no time must not
// be the first thing evicted merely for lacking one.
func evict(p *persisted, max int) int {
	if max <= 0 || len(p.records) <= max {
		return 0
	}
	dropped := len(p.records) - max
	p.records = append([]Record(nil), p.records[dropped:]...)
	return dropped
}

// freshID mints an id no row in the file already answers to.
func freshID(p *persisted, inFile, seen map[string]bool) string {
	for {
		id := "a" + itoa(p.nextID)
		p.nextID++
		if !inFile[id] && !seen[id] {
			return id
		}
	}
}

// mintID takes the next unused id off the high-water mark. Bumped before the
// save on purpose: a failed write may skip an id, but no path can ever reuse
// one.
func mintID(p *persisted) string {
	used := make(map[string]bool, len(p.records))
	for _, r := range p.records {
		used[r.ID] = true
	}
	for {
		id := "a" + itoa(p.nextID)
		p.nextID++
		if !used[id] {
			return id
		}
	}
}

// clone deep-copies the account so callers can never mutate the Store's
// slice through a returned value.
func clone(p persisted) persisted {
	out := p
	out.records = append([]Record(nil), p.records...)
	return out
}
