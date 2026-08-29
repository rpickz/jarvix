// Package approvals is the ledger behind remembered command approvals (issue
// #162, ADR 0053): when each `[tools.policy] shell_allow` pattern was added,
// who added it, and how many times it has since let a command run without
// asking.
//
// The ledger is deliberately NOT the source of truth for which patterns
// exist. config.toml is, and stays: the classifier compiles its allow list
// from the configuration file and nothing else, so a user who opens
// config.toml sees the whole of what they have granted, and deleting this
// file changes what Jarvix may do by exactly nothing. What lives here is the
// history a configuration file cannot hold without becoming a log — the added
// timestamp and the firing count that make the Approvals view answer "when
// did I agree to this, and is it still earning its place?".
//
// That split is why Reconcile exists and why it is called on every read: the
// file is folded onto the configured list, entries the configuration no
// longer names are dropped, and configured patterns the ledger has never seen
// appear as hand-added with no date. A hand edit therefore always wins, and
// the ledger can never resurrect a rule the user deleted with an editor.
package approvals

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Source records how a pattern arrived. Two values, because the Approvals
// view has to be able to say "you agreed to this on the card at 14:02" rather
// than implying Jarvix chose it.
const (
	// SourceCard is a pattern the user added by answering a confirmation
	// card with "don't ask again".
	SourceCard = "card"
	// SourceHand is a pattern found in config.toml that this ledger has no
	// record of — written with an editor, or predating the ledger.
	SourceHand = "hand"
)

// Entry is one remembered pattern as the Approvals view, the CLI and the
// spoken listing all see it.
type Entry struct {
	// Pattern is the word-prefix rule, verbatim as it appears in
	// `[tools.policy] shell_allow`.
	Pattern string
	// Source is SourceCard or SourceHand.
	Source string
	// Added is when the card wrote it; zero for a hand-added pattern, which
	// surfaces honestly as "added by hand" rather than as a guessed date.
	Added time.Time
	// Uses is how many commands this rule has let through unprompted, and
	// LastUsed when the most recent one was (zero when it has never fired).
	// A rule with a use count of zero after weeks is a rule to revoke, which
	// is the whole reason the count is kept.
	Uses     int
	LastUsed time.Time
}

// Store is the ledger. Safe for concurrent use: the daemon bumps use counts
// from the bus goroutine while the window reads the listing over IPC.
type Store struct {
	mu   sync.Mutex
	path string
	log  *slog.Logger
	// loaded, mod and size are the change detector: the file is re-read when
	// its mtime or size no longer matches what was last loaded or written.
	loaded bool
	mod    time.Time
	size   int64
	// corrupt latches when the on-disk ledger could not be parsed. While
	// set, the store serves an empty history, and the first write moves the
	// unreadable file aside instead of overwriting it.
	corrupt bool
	// write persists the ledger; always writeLedger outside tests. It is a
	// field for the reason the memory book's identical field is one: the
	// write-failure contracts — a failed write must cost exactly the write,
	// never the in-memory ledger — need a disk that fails on command, and
	// the real filesystem cannot be made to do that hermetically.
	write   func(path string, records map[string]*record) error
	records map[string]*record
}

// record is one ledger row. A pointer in the map so a use bump does not
// reinsert.
type record struct {
	source   string
	added    time.Time
	uses     int
	lastUsed time.Time
}

// NewStore opens (lazily) the ledger at path. logger may be nil.
func NewStore(path string, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{path: path, log: logger, write: writeLedger, records: map[string]*record{}}
}

// List folds the ledger onto patterns — the configured `shell_allow` list —
// and returns one entry per configured pattern, in configuration order.
//
// Configuration order rather than newest-first because that is the order the
// file has, and the Approvals view exists to make the file legible, not to
// impose a second ordering on it that a user comparing the two would have to
// reconcile in their head.
func (s *Store) List(patterns []string) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	s.reconcileLocked(patterns)
	out := make([]Entry, 0, len(patterns))
	for _, p := range normalisePatterns(patterns) {
		r := s.records[p]
		if r == nil { // reconcile guarantees this cannot happen; belt and braces
			r = &record{source: SourceHand}
		}
		out = append(out, Entry{
			Pattern: p, Source: r.source, Added: r.added,
			Uses: r.uses, LastUsed: r.lastUsed,
		})
	}
	return out
}

// Added records that the card wrote pattern at now. Called after the config
// write succeeds, never before: a ledger row for a rule that failed to land
// would claim a grant the classifier does not have. The row is committed to
// memory only once the disk has taken it, for the same reason.
func (s *Store) Added(pattern string, now time.Time) {
	pattern = normalise(pattern)
	if pattern == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	next := cloneRecords(s.records)
	next[pattern] = &record{source: SourceCard, added: now}
	s.commitLocked(next)
}

// Used bumps the firing count for pattern. It is called from the audit path,
// so a failure to persist is a warning and nothing more: the count is a
// convenience for deciding what to revoke, never a control, and losing one
// must not cost the user the command they were running. What a lost write
// costs is the count, not the truth of the ledger — the bump is committed
// only once the disk has taken it, so nothing here can report a use that no
// file holds.
func (s *Store) Used(pattern string, now time.Time) {
	pattern = normalise(pattern)
	if pattern == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	next := cloneRecords(s.records)
	r := next[pattern]
	if r == nil {
		// A conversation-scoped grant, or a pattern added to config.toml by
		// hand since the last read. Either way it fired, and the honest
		// record of that is a row saying so, sourced as hand-added.
		r = &record{source: SourceHand}
		next[pattern] = r
	}
	r.uses++
	r.lastUsed = now
	s.commitLocked(next)
}

// Forget drops the ledger row for pattern. Revocation itself is a config
// write — this only clears the history that would otherwise be re-attached
// if the same pattern were granted again later, which would misreport a fresh
// grant's age.
func (s *Store) Forget(pattern string) {
	pattern = normalise(pattern)
	if pattern == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	next := cloneRecords(s.records)
	delete(next, pattern)
	s.commitLocked(next)
}

// reconcileLocked makes the ledger's membership match the configured list.
// Callers hold s.mu.
func (s *Store) reconcileLocked(patterns []string) {
	want := map[string]bool{}
	changed := false
	for _, p := range normalisePatterns(patterns) {
		want[p] = true
		if s.records[p] == nil {
			s.records[p] = &record{source: SourceHand}
			changed = true
		}
	}
	for p := range s.records {
		if !want[p] {
			delete(s.records, p)
			changed = true
		}
	}
	if changed {
		// Reconciliation is derived from the configuration rather than from
		// this file, so it is committed to memory whether or not the write
		// lands — the listing must be right even on a disk that is refusing
		// writes. commitLocked adopts the same map it was handed, so the
		// only thing at stake here is whether the file catches up.
		s.commitLocked(s.records)
	}
}

// refreshLocked brings the in-memory ledger up to date with the file, which
// it checks with one stat(2) of a file already in the page cache. Callers
// hold s.mu.
//
// It replaced a load that ran once per process, and the difference is a
// defect this store had and its siblings did not (issue #173). The daemon
// runs for days; a user who opens this file and fixes a count, or deletes a
// row, had the edit silently reverted by the next write from the cached
// copy. Every other durable store here re-reads on a changed mtime or size
// (the memory book's discipline, ADR 0025) and this one now does too.
//
// Every failure degrades: no file is an empty history, an unreadable or
// unparseable one is a warning plus an empty history. Never a refusal to
// serve, because the permission decisions do not depend on this file and
// must not become hostage to it.
func (s *Store) refreshLocked() {
	info, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		// No ledger yet, or the user deleted it. Deletion is deletion: this
		// file is a record, not a permission, and losing it costs the
		// history and nothing else (the header says so).
		s.records, s.corrupt = map[string]*record{}, false
		s.loaded, s.mod, s.size = true, time.Time{}, 0
		return
	}
	if err != nil {
		if !s.corrupt {
			s.log.Warn("approval ledger could not be read; history starts empty",
				"component", "approvals", "path", s.path, "error", err.Error())
		}
		s.records, s.corrupt, s.loaded = map[string]*record{}, true, true
		return
	}
	if s.loaded && info.ModTime().Equal(s.mod) && info.Size() == s.size {
		return // unchanged since the last read or write — the common case
	}
	s.loaded, s.mod, s.size = true, info.ModTime(), info.Size()
	records, err := readLedger(s.path)
	if err != nil {
		// Warned per corruption event rather than per read: the stat check
		// above keeps this branch from re-running until the file changes
		// again, and no pattern appears in the message.
		s.log.Warn("approval ledger could not be parsed; history starts empty "+
			"(fix the file by hand — it will not be overwritten)",
			"component", "approvals", "path", s.path, "error", err.Error())
		s.records, s.corrupt = map[string]*record{}, true
		return
	}
	s.records, s.corrupt = records, false
}

// commitLocked writes next and adopts it only once the disk has taken it, so
// a failed write can never leave the ledger claiming history the file does
// not hold. Callers hold s.mu.
//
// A failure is a warning and nothing more, which is this store's own rule
// and not an oversight: the count exists to help the user decide what to
// revoke, and a disk that will not take it must never cost them the command
// they were running.
func (s *Store) commitLocked(next map[string]*record) {
	if s.corrupt {
		// The file on disk is one the user may be mid-way through fixing —
		// or the only copy of an approval history a torn write damaged.
		// Move it aside rather than overwrite it: the write proceeds, and
		// the unreadable content survives next to it. Every sibling store
		// does this; this one did not, and silently destroyed the history
		// on the first write after any damage (issue #173).
		backup := s.path + ".corrupt"
		if err := os.Rename(s.path, backup); err == nil {
			s.log.Warn("unparseable approval ledger moved aside before writing",
				"component", "approvals", "backup", backup)
		}
		s.corrupt = false
	}
	if err := s.write(s.path, next); err != nil {
		s.log.Warn("approval ledger could not be written",
			"component", "approvals", "path", s.path, "error", err.Error())
		return
	}
	s.records = next
	// Record the write's own stat so it is not mistaken for a hand edit and
	// pointlessly re-read on the next call.
	if info, err := os.Stat(s.path); err == nil {
		s.loaded, s.mod, s.size = true, info.ModTime(), info.Size()
	}
}

// cloneRecords copies the ledger map and every row in it, so a mutation
// staged for a write that may fail cannot reach the live ledger through a
// shared pointer.
func cloneRecords(records map[string]*record) map[string]*record {
	out := make(map[string]*record, len(records))
	for k, v := range records {
		row := *v
		out[k] = &row
	}
	return out
}

// normalise collapses a pattern's whitespace so `docker  ps` and `docker ps`
// are one rule. It is the same collapsing the classifier performs when it
// compiles a pattern with strings.Fields, applied here so the ledger's keys
// and the classifier's patterns cannot disagree about identity.
func normalise(pattern string) string {
	return strings.Join(strings.Fields(pattern), " ")
}

// normalisePatterns normalises a list and drops empties and duplicates,
// preserving first-seen order.
func normalisePatterns(patterns []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		n := normalise(p)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// sortedKeys gives the ledger file a deterministic order so repeated writes
// with no change are byte-identical — the same property the settings rewrite
// insists on, for the same reason: a file that churns is a file nobody can
// diff.
func sortedKeys(m map[string]*record) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
