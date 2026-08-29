package storefault_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rpickz/jarvix/internal/storefault"
)

// This file is the suite's own test, and it is a reference implementation as
// much as a test: refStore is the storage discipline written down once, in
// the smallest form that keeps all six promises. When a new store is being
// written, this is the shortest description of what it has to do.
//
// It also exists for a duller reason worth stating: a package with no test
// of its own reads as 0% covered and drags the repo's coverage floor down,
// so a shared suite has to be able to run against something.
//
// Two subjects are registered, and the difference between them is the two
// legitimate answers to "what do you do with a document you cannot parse?".
// The single-file stores set it aside with a suffix; the archive leaves it
// where the user can see it. Both are covered here so the suite's own
// branches are exercised by its own test rather than only by its callers.

func TestTheSuiteHoldsForAStoreThatKeepsTheDiscipline(t *testing.T) {
	storefault.Run(t, storefault.Subject{
		Name:             "reference",
		Open:             openRef(refOptions{}),
		MovedAsideSuffix: ".corrupt",
	})
}

func TestTheSuiteHoldsForAStoreThatLeavesDamageWhereItIs(t *testing.T) {
	storefault.Run(t, storefault.Subject{
		Name:         "reference-in-place",
		Open:         openRef(refOptions{leaveDamageInPlace: true, noIDs: true}),
		NoIDsBecause: "the reference variant identifies a record by its content, as the monitor nicknames do",
	})
}

type refOptions struct {
	leaveDamageInPlace bool
	noIDs              bool
}

func openRef(opts refOptions) func(*testing.T, string, *storefault.Faults) storefault.Store {
	return func(t *testing.T, dir string, faults *storefault.Faults) storefault.Store {
		t.Helper()
		log, disclosure := storefault.Log()
		return &refStore{
			path:       filepath.Join(dir, "reference.json"),
			opts:       opts,
			faults:     faults,
			log:        log,
			disclosure: disclosure,
			next:       1,
		}
	}
}

// refDoc is the on-disk shape: a version, an id high-water mark that only
// ratchets up, and the records.
type refDoc struct {
	Version int         `json:"version"`
	Next    int         `json:"next"`
	Records []refRecord `json:"records"`
}

type refRecord struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Detail  string `json:"detail,omitempty"`
}

const refVersion = 1

// refStore is the discipline: one document, rewritten whole, atomically;
// a stat before every operation so a hand edit lands on the next one; a
// corrupt latch that serves empty and never overwrites; an id mark that only
// goes up; and memory committed only after the disk says yes.
type refStore struct {
	path       string
	opts       refOptions
	faults     *storefault.Faults
	log        *slog.Logger
	disclosure func() []string

	mu      sync.Mutex
	records []refRecord
	next    int
	loaded  bool
	mod     int64
	size    int64
	corrupt bool
}

func (s *refStore) Add(content string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	id := content
	if !s.opts.noIDs {
		id = "r" + strconv.Itoa(s.next)
		// Bumped before the write: a failed write may skip an id, but no
		// path may ever reuse one.
		s.next++
	}
	next := append(append([]refRecord(nil), s.records...), refRecord{ID: id, Content: content})
	if err := s.saveLocked(next); err != nil {
		return "", err
	}
	return id, nil
}

func (s *refStore) Forget(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	next := make([]refRecord, 0, len(s.records))
	found := false
	for _, r := range s.records {
		if r.ID == id {
			found = true
			continue
		}
		next = append(next, r)
	}
	if !found {
		return fmt.Errorf("no record has id %q", id)
	}
	return s.saveLocked(next)
}

func (s *refStore) Records() []storefault.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	out := make([]storefault.Record, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, storefault.Record{ID: r.ID, Content: r.Content, Detail: r.Detail})
	}
	return out
}

func (s *refStore) Reload(*testing.T) storefault.Store {
	return &refStore{path: s.path, opts: s.opts, faults: s.faults,
		log: s.log, disclosure: s.disclosure, next: 1}
}

func (s *refStore) HandEdit(t *testing.T) []storefault.Record {
	t.Helper()
	want := []refRecord{
		{ID: "r90", Content: "a record the user typed in themselves", Detail: "by hand"},
		{ID: "r91", Content: "and a second one, on the line below"},
	}
	data, err := json.Marshal(refDoc{Version: refVersion, Next: 92, Records: want})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out := make([]storefault.Record, 0, len(want))
	for _, r := range want {
		out = append(out, storefault.Record{ID: r.ID, Content: r.Content, Detail: r.Detail})
	}
	return out
}

func (s *refStore) Damage(t *testing.T) (string, []byte) {
	t.Helper()
	raw := []byte(`{"version":1,"next":2,"records":[{"id":"r1","cont`)
	if err := os.WriteFile(s.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return s.path, raw
}

func (s *refStore) Disclosure() []string { return s.disclosure() }

// refreshLocked is the stat-based change detector: one stat of a file
// already in the page cache, so consulting the store costs nothing.
func (s *refStore) refreshLocked() {
	info, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		s.records, s.corrupt = nil, false
		s.loaded, s.mod, s.size = true, 0, 0
		return
	}
	if err != nil {
		s.records, s.corrupt, s.loaded = nil, true, true
		return
	}
	if s.loaded && info.ModTime().UnixNano() == s.mod && info.Size() == s.size {
		return
	}
	s.loaded, s.mod, s.size = true, info.ModTime().UnixNano(), info.Size()
	doc, err := readRef(s.path)
	if err != nil {
		s.log.Warn("reference store could not be parsed; continuing empty", "error", err.Error())
		s.records, s.corrupt = nil, true
		return
	}
	if doc.Next > s.next {
		s.next = doc.Next
	}
	for _, r := range doc.Records {
		if n, err := strconv.Atoi(strings.TrimPrefix(r.ID, "r")); err == nil && n >= s.next {
			s.next = n + 1
		}
	}
	s.records, s.corrupt = doc.Records, false
}

// saveLocked commits to memory only after the disk says yes.
func (s *refStore) saveLocked(records []refRecord) error {
	if s.corrupt && !s.opts.leaveDamageInPlace {
		if err := os.Rename(s.path, s.path+".corrupt"); err == nil {
			s.log.Warn("unparseable reference store moved aside before writing")
		}
		s.corrupt = false
	}
	if err := s.writeLocked(records); err != nil {
		return err
	}
	s.records = records
	if info, err := os.Stat(s.path); err == nil {
		s.loaded, s.mod, s.size = true, info.ModTime().UnixNano(), info.Size()
	}
	return nil
}

// writeLocked is the atomic write, through the suite's seam. The variant
// that leaves damage in place writes to a fresh name rather than over the
// damaged document, which is how a per-record store behaves.
func (s *refStore) writeLocked(records []refRecord) error {
	path := s.path
	if s.corrupt && s.opts.leaveDamageInPlace {
		path = s.path + ".2"
	}
	if err := s.faults.Before(path); err != nil {
		return err
	}
	data, err := json.Marshal(refDoc{Version: refVersion, Next: s.next, Records: records})
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".reference-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func readRef(path string) (refDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return refDoc{}, err
	}
	var doc refDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return refDoc{}, err
	}
	if doc.Version != refVersion {
		return refDoc{}, fmt.Errorf("reference store version %d is not supported", doc.Version)
	}
	return doc, nil
}
