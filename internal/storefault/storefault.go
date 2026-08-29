// Package storefault is the fault-injection suite the durable stores share
// (issue #173).
//
// Every store under internal/ that keeps something the user would miss —
// the conversation archive, the memory book, the taught vocabulary, the
// focus threads, the reminders, the approval ledger, the monitor nicknames —
// makes the same five promises about its file, and has made them one store
// at a time. The promises are written down in each package's doc comment
// (ADR 0011, ADR 0025): atomic fsync-and-rename writes, stat-based hand-edit
// pickup, an unreadable document set aside rather than overwritten, ids that
// are never reused, and readers that never see half a write.
//
// Written down is not the same as proven. Before this package the promises
// were asserted in some stores, implied in others, and — in the approval
// ledger — quietly broken. So they are stated once here, as executable
// assertions, and every store runs them: a store joins by filling in a
// Subject and implementing Store over its own real type, which is twenty
// lines of adapter and no new test logic. When a new store lands it inherits
// the whole matrix by registration, and when a promise changes it changes in
// one place.
//
// Hermetic by construction. Nothing here fills a disk, drops privileges, or
// chmods a directory to provoke a failure: the faults arrive through the
// store's own write seam (see Faults), which is the field each store already
// carries so that a write can be made to fail on command. A real filesystem
// cannot be made to fail hermetically — every trick a test could play on a
// temp directory is either privileged or repaired by the store's own chmod
// on the next write — and a fault that needs a privileged runner is a fault
// nobody runs.
//
// There are no sleeps and no timing anywhere in this package. Each fault
// mode is its own named subtest, so a failure reads as the promise it broke
// ("AFullDiskIsSurfacedAndNoSuccessIsRecorded/approvals") rather than as
// "the fault suite failed".
package storefault

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// Record is one stored thing as the suite sees it. Three fields, because
// three is what the promises need: an identity to prove ids are never
// reused, the content the caller stored, and one more observable so a hand
// edit can be proven to have arrived even in a store whose membership is
// decided elsewhere.
type Record struct {
	// ID is the identity the store gave the record — "m3", a conversation
	// id, or the user's own word for a store that mints none.
	ID string
	// Content is exactly the string that was handed to Add.
	Content string
	// Detail is whatever else the store keeps about the record that a reader
	// can see and a hand edit can change: a fact's source, an approval's use
	// count. The suite never invents one and never requires one — it only
	// checks that what HandEdit says the store must report is what the store
	// reports.
	Detail string
}

// Store is one durable store, adapted to the suite. Every method is a
// promise's minimum: something to store, something to read back, something
// to remove, a restart, and the two ways a file goes wrong underneath a
// running daemon.
//
// Implementations live in their own package's test files, because the write
// seam and the on-disk shape are both unexported — which is the right place
// for them anyway: the adapter is the one part of this that has to know what
// a focus thread or an approval row actually is.
type Store interface {
	// Add stores one record with the given content and returns the identity
	// the store gave it. A non-nil error is the store refusing, which is
	// what every fault case reads as "no success was recorded".
	//
	// A store whose own API reports a write failure only by logging (the
	// approval ledger does; a lost use count must never cost the user the
	// command they were running) returns that warning as the error here. The
	// promise is that the failure is surfaced honestly, not that it is
	// surfaced through a particular return value.
	Add(content string) (id string, err error)

	// Forget removes the record with id. It exists for the id-reuse case:
	// an id must not come back after the record that held it is gone.
	Forget(id string) error

	// Records returns everything the store holds, read through the store's
	// own readers rather than off the disk, in the store's own order.
	Records() []Record

	// Reload reopens the store over the same files with the same write seam,
	// as a daemon restart does. The returned Store replaces the receiver.
	Reload(t *testing.T) Store

	// HandEdit rewrites the store's document as a user with a text editor
	// would, and returns what the store must report once it notices. The
	// edit itself is the adapter's business — every store's file has a
	// different shape, and the promise under test is that the change is
	// picked up without a restart, not that this package can write TOML.
	HandEdit(t *testing.T) []Record

	// Damage replaces the document the store keeps its records in with a
	// truncated, unparseable one — what a torn write or an editor crash
	// leaves behind — and returns the path it wrote and the bytes it wrote
	// there, so the suite can check what became of them.
	Damage(t *testing.T) (path string, raw []byte)

	// Disclosure returns everything the store has said about damage it
	// found: the warnings it logged, the entries it reported unreadable.
	// It is how "and says so" becomes an assertion rather than a hope.
	Disclosure() []string
}

// Subject registers one store with the suite. Everything a store has to
// decide for itself is here; everything else is the same for all of them.
type Subject struct {
	// Name is the store's name in every subtest and every failure message.
	Name string

	// Open opens the store over dir — a directory the suite owns — with
	// faults installed as its write seam.
	Open func(t *testing.T, dir string, faults *Faults) Store

	// MovedAsideSuffix is what the store appends to a document it could not
	// parse when it sets it aside before writing: ".corrupt" for the stores
	// that keep everything in one file.
	//
	// Empty means the store leaves the unreadable document exactly where it
	// is — which is the conversation archive's answer, and a considered one:
	// a damaged conversation is one record among many, it is reported in the
	// listing so the user can see that it exists, and moving it out of the
	// archive directory would hide it. What no store may do is overwrite it,
	// and the suite checks that either way.
	MovedAsideSuffix string

	// NoIDsBecause explains why the store mints no ids, for the stores whose
	// records are identified by the user's own word. Empty means the store
	// mints ids and the never-reused promise applies to it.
	NoIDsBecause string

	// SingleWord asks the suite for one-word record contents. It is here for
	// the monitor nicknames, whose record IS a name the user says out loud
	// and which the store therefore refuses to let be a sentence. Everything
	// else gets readable phrases, because a failure message quoting "the
	// record the disk refused" says more than one quoting "refused".
	SingleWord bool
}

// text picks between the readable phrase and the one-word form, so a store
// that stores names rather than sentences still runs the same assertions.
func (s Subject) text(phrase, word string) string {
	if s.SingleWord {
		return word
	}
	return phrase
}

// Run runs the whole suite against one store. Each fault mode is its own
// named subtest on purpose (the ticket's NFR): a red build should name the
// promise that broke and the store that broke it, without anybody opening
// the file.
func Run(t *testing.T, s Subject) {
	t.Helper()
	for _, f := range faultModes {
		t.Run(f.name, func(t *testing.T) { f.run(t, s) })
	}
}

// faultModes is the matrix. Every entry is one sentence from the ticket's
// acceptance criteria, and nothing is here that is not.
var faultModes = []struct {
	name string
	run  func(*testing.T, Subject)
}{
	{"AFailedWriteLeavesThePreviousFileAndTheMemoryIntact", failedWriteCostsNothing},
	{"AFullDiskIsSurfacedAndNoSuccessIsRecorded", fullDiskIsSurfaced},
	{"ACorruptFileIsNotOverwrittenAndTheStoreSaysSo", corruptFileIsSetAside},
	{"AHandEditBetweenOperationsIsPickedUp", handEditIsPickedUp},
	{"IDsAreNeverReusedAcrossAReload", idsAreNeverReused},
	{"AReadConcurrentWithAWriteNeverSeesAPartialRecord", concurrentReadSeesWholeRecords},
}

// ---------------------------------------------------------------------------
// The write seam
// ---------------------------------------------------------------------------

// Faults is the injectable write seam. A store's adapter calls Before at the
// top of the store's own write function, so one suite drives every store's
// disk without knowing any store's write signature — which differ, and
// should: a focus write carries threads and a memory write carries facts.
//
// Safe for concurrent use, because the concurrency case has readers and a
// writer running against a store whose seam the test goroutine may heal.
type Faults struct {
	mu   sync.Mutex
	mode mode
}

type mode int

const (
	healthy mode = iota
	midFlight
	fullDisk
)

// Before is what a store's write function calls before it puts a byte on
// disk. A nil return means write normally.
func (f *Faults) Before(path string) error {
	f.mu.Lock()
	m := f.mode
	f.mu.Unlock()
	switch m {
	case midFlight:
		// A mid-flight failure leaves behind what an interrupted atomic
		// write leaves behind: a partial temp file in the store's own
		// directory, next to the document it was going to replace. It is
		// there so the suite can prove the store neither adopts the debris
		// nor trips over it — the previous document is what a reload must
		// still find.
		leaveDebris(path)
		return &fs.PathError{Op: "write", Path: path, Err: errors.New("input/output error")}
	case fullDisk:
		return &fs.PathError{Op: "write", Path: path, Err: syscall.ENOSPC}
	}
	return nil
}

// FailMidFlight makes every subsequent write fail after leaving debris.
func (f *Faults) FailMidFlight() { f.set(midFlight) }

// FailFullDisk makes every subsequent write fail with ENOSPC — the honest
// simulation of a full disk, at the seam, with no real disk filled.
func (f *Faults) FailFullDisk() { f.set(fullDisk) }

// Heal returns the seam to a working disk.
func (f *Faults) Heal() { f.set(healthy) }

func (f *Faults) set(m mode) {
	f.mu.Lock()
	f.mode = m
	f.mu.Unlock()
}

// leaveDebris drops the half-written temp file an interrupted atomic write
// leaves in the directory. Best-effort: the fault is the error, and a
// directory that will not take a temp file is a fault of its own.
func leaveDebris(path string) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".interrupted-*.tmp")
	if err != nil {
		return
	}
	_, _ = tmp.WriteString("half a document, written when the disk gave out")
	_ = tmp.Close()
}

// ---------------------------------------------------------------------------
// The faults
// ---------------------------------------------------------------------------

// failedWriteCostsNothing: a write that fails mid-flight leaves the previous
// file intact and the in-memory state unchanged.
//
// This is the promise with the sharpest edge, because the tempting
// implementation — mutate memory, then persist — passes every happy-path
// test and loses the user's data exactly once, on the day the disk fills.
// The store must commit to memory only after the disk says yes.
func failedWriteCostsNothing(t *testing.T, s Subject) {
	dir := t.TempDir()
	faults := &Faults{}
	store := s.Open(t, dir, faults)

	if _, err := store.Add(s.text("the record that was already there", "already")); err != nil {
		t.Fatalf("the healthy write failed: %v", err)
	}
	before := snapshot(t, dir)
	want := contents(store.Records())

	faults.FailMidFlight()
	if _, err := store.Add(s.text("the record the disk refused", "refused")); err == nil {
		t.Fatal("a write that failed mid-flight reported success")
	}
	faults.Heal()

	if got := contents(store.Records()); !equal(got, want) {
		t.Errorf("a failed write mutated the store's memory:\n got %q\nwant %q", got, want)
	}
	assertPreexistingFilesUnchanged(t, dir, before)

	// And the disk agrees, which is the half a memory check cannot see: a
	// store that recovered its old value from its own cache would pass the
	// assertion above and still have lost the file.
	if got := contents(store.Reload(t).Records()); !equal(got, want) {
		t.Errorf("a failed write cost the file its contents:\n got %q\nwant %q", got, want)
	}
}

// fullDiskIsSurfaced: a full disk is surfaced honestly and no caller records
// success — and the store is not left latched into uselessness by it, which
// is the second half people forget. A disk that fills and is then emptied
// must leave a store that works.
func fullDiskIsSurfaced(t *testing.T, s Subject) {
	dir := t.TempDir()
	faults := &Faults{}
	store := s.Open(t, dir, faults)

	if _, err := store.Add(s.text("stored while there was room", "roomy")); err != nil {
		t.Fatalf("the healthy write failed: %v", err)
	}
	want := contents(store.Records())

	faults.FailFullDisk()
	id, err := store.Add(s.text("stored when the disk was full", "nospace"))
	if err == nil {
		t.Fatal("a full disk was reported as a successful write")
	}
	if strings.TrimSpace(err.Error()) == "" {
		t.Error("the failure was surfaced as an empty error, which tells the user nothing")
	}
	if id != "" {
		t.Errorf("a refused write still handed back an identity %q", id)
	}
	if got := contents(store.Records()); !equal(got, want) {
		t.Errorf("a full disk left the store claiming a record it never wrote:\n got %q\nwant %q", got, want)
	}

	faults.Heal()
	if _, err := store.Add(s.text("stored once there was room again", "roomagain")); err != nil {
		t.Fatalf("the store stayed broken after the disk recovered: %v", err)
	}
	if n := len(store.Reload(t).Records()); n != 2 {
		t.Errorf("records after the disk recovered = %d, want the two that were written", n)
	}
}

// corruptFileIsSetAside: a truncated or corrupt document is never
// overwritten, the store starts clean rather than refusing to work, and it
// says what it found.
//
// Never overwritten is the load-bearing clause. The bytes are the user's own
// content — very often a hand edit halfway through being fixed — and a store
// that repairs itself by writing over them has destroyed the only copy.
func corruptFileIsSetAside(t *testing.T, s Subject) {
	dir := t.TempDir()
	faults := &Faults{}
	store := s.Open(t, dir, faults)

	if _, err := store.Add(s.text("the record that is about to become unreadable", "doomed")); err != nil {
		t.Fatalf("the healthy write failed: %v", err)
	}
	path, raw := store.Damage(t)

	store = store.Reload(t)
	if got := store.Records(); len(got) != 0 {
		t.Errorf("a store with an unreadable document served %d records; it must start clean: %+v",
			len(got), got)
	}
	if len(store.Disclosure()) == 0 {
		t.Error("the store found an unreadable document and said nothing about it")
	}

	// The next write must not silently take the file over.
	if _, err := store.Add(s.text("the record written after the damage", "afterwards")); err != nil {
		t.Fatalf("a damaged store refused to accept new records: %v", err)
	}
	if s.MovedAsideSuffix != "" {
		aside := path + s.MovedAsideSuffix
		got, err := os.ReadFile(aside)
		if err != nil {
			t.Fatalf("the unreadable document was not set aside at %s: %v", aside, err)
		}
		if !bytes.Equal(got, raw) {
			t.Errorf("the document set aside at %s is not the one that was damaged", aside)
		}
	} else if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, raw) {
		t.Errorf("the unreadable document was overwritten in place at %s (err %v)", path, err)
	}
}

// handEditIsPickedUp: an edit made between two operations lands on the next
// one — no restart, no watcher, no waiting.
//
// The store instance is deliberately not reloaded. Picking the edit up after
// a restart is not the promise; the promise is that the user can fix a fact
// in an editor and the next thing they say uses it.
func handEditIsPickedUp(t *testing.T, s Subject) {
	dir := t.TempDir()
	faults := &Faults{}
	store := s.Open(t, dir, faults)

	if _, err := store.Add(s.text("what the store held before the edit", "before")); err != nil {
		t.Fatalf("the healthy write failed: %v", err)
	}
	want := store.HandEdit(t)

	got := store.Records()
	if len(got) != len(want) {
		t.Fatalf("the hand edit was not picked up: %d records, want %d\n got %+v\nwant %+v",
			len(got), len(want), got, want)
	}
	for i := range want {
		if got[i].Content != want[i].Content || got[i].Detail != want[i].Detail {
			t.Errorf("record %d after the hand edit = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// idsAreNeverReused: an id is retired with the record that held it, across a
// reload.
//
// The cost of breaking this is not a wrong listing, it is a wrong answer: a
// conversation that once named "m2" would come to describe a different fact,
// and the supersede trail behind it would be a fabrication.
func idsAreNeverReused(t *testing.T, s Subject) {
	if s.NoIDsBecause != "" {
		t.Skipf("%s mints no ids: %s", s.Name, s.NoIDsBecause)
	}
	dir := t.TempDir()
	faults := &Faults{}
	store := s.Open(t, dir, faults)

	first, err := store.Add(s.text("the record whose id must be retired with it", "retired"))
	if err != nil {
		t.Fatalf("the healthy write failed: %v", err)
	}
	if first == "" {
		t.Fatal("the store handed back an empty id")
	}
	if err := store.Forget(first); err != nil {
		t.Fatalf("forgetting the record failed: %v", err)
	}

	store = store.Reload(t)
	second, err := store.Add(s.text("the record that must not inherit a dead id", "successor"))
	if err != nil {
		t.Fatalf("the write after the reload failed: %v", err)
	}
	if second == first {
		t.Errorf("id %q was handed out again after the record holding it was removed", first)
	}
}

// concurrentReadSeesWholeRecords: a read running alongside a write never
// observes a partial record.
//
// The archive is why this is a suite-wide case rather than one store's test.
// Its search deliberately does not take the store's mutex — a search over a
// large library would otherwise stall the engine's post-session write — so
// its readers meet writes in flight by design, and the atomic-rename and
// whole-line-append disciplines are the whole of what makes that safe.
func concurrentReadSeesWholeRecords(t *testing.T, s Subject) {
	dir := t.TempDir()
	faults := &Faults{}
	store := s.Open(t, dir, faults)

	// Sixteen, because the smallest cap any registered store carries is the
	// focus store's twenty threads, and a concurrency case that trips a
	// store's own limit is measuring the limit rather than the race.
	const writes = 16
	known := map[string]bool{}
	for i := range writes {
		known[s.text(fmt.Sprintf("record number %d", i), fmt.Sprintf("record%d", i))] = true
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range writes {
			if _, err := store.Add(s.text(fmt.Sprintf("record number %d", i), fmt.Sprintf("record%d", i))); err != nil {
				t.Errorf("concurrent write %d failed: %v", i, err)
				return
			}
		}
	}()

	// Three readers, each reading many more times than there are writes, so
	// the reads land all over the write sequence without anything having to
	// be timed.
	bad := make(chan string, 8)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range writes * 8 {
				for _, r := range store.Records() {
					if r.Content == "" || !known[r.Content] {
						select {
						case bad <- fmt.Sprintf("a reader saw a partial record %+v", r):
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

	if d := store.Disclosure(); len(d) != 0 {
		t.Errorf("reading during a write reported damage that never happened: %q", d)
	}
	if got := len(store.Reload(t).Records()); got != writes {
		t.Errorf("records after the concurrent run = %d, want %d", got, writes)
	}
}

// ---------------------------------------------------------------------------
// Helpers the adapters share
// ---------------------------------------------------------------------------

// Log returns a logger that keeps every warning it is given, and the reader
// for them. Adapters hand the logger to their store and return the reader
// from Disclosure, so "the store said what it did" is checkable without any
// store having to grow a test-only accessor.
//
// Only WARN and above is kept. Everything these stores say about damage they
// found is a warning — a store that could not read its file keeps working
// and warns once, which is the documented degradation — and keeping the
// debug chatter too would make an empty Disclosure impossible to assert.
func Log() (*slog.Logger, func() []string) {
	c := &capture{mu: &sync.Mutex{}, lines: new([]string)}
	return slog.New(c), c.read
}

// capture is a slog.Handler that keeps warning messages in a slice. The
// mutex and the slice are behind pointers because WithAttrs and WithGroup
// must hand back a handler writing to the same place — a store that
// decorates its logger has to land in the same Disclosure.
type capture struct {
	mu    *sync.Mutex
	lines *[]string
}

func (c *capture) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}

func (c *capture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	*c.lines = append(*c.lines, r.Message)
	return nil
}

func (c *capture) WithAttrs([]slog.Attr) slog.Handler { return c }

func (c *capture) WithGroup(string) slog.Handler { return c }

func (c *capture) read() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), *c.lines...)
}

// contents projects records down to what was stored, which is what most of
// the assertions above compare.
func contents(records []Record) []string {
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, r.Content)
	}
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// snapshot reads every file under dir. It is taken before a write is made to
// fail so the suite can prove, byte for byte, that the files that already
// existed were not touched.
func snapshot(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out[rel] = data
		return nil
	})
	if err != nil {
		t.Fatalf("reading the store directory: %v", err)
	}
	return out
}

// assertPreexistingFilesUnchanged checks that every file that existed before
// the failed write still holds exactly the bytes it held.
//
// It deliberately says nothing about files that appeared: an interrupted
// atomic write leaves a temp file behind, and leaving it is correct — the
// next successful write cleans it up, and a store that went hunting for
// debris to delete on a failure path would be doing filesystem repair in the
// middle of an error.
func assertPreexistingFilesUnchanged(t *testing.T, dir string, before map[string][]byte) {
	t.Helper()
	after := snapshot(t, dir)
	for name, want := range before {
		got, ok := after[name]
		if !ok {
			t.Errorf("a failed write removed %s", name)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("a failed write rewrote %s", name)
		}
	}
}
