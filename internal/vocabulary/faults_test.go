package vocabulary

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/storefault"
)

// The taught vocabulary's registration with the shared fault-injection suite
// (issue #173). The store already carried the memory book's discipline by
// construction; this is what makes that a fact rather than an intention.

func TestVocabularyStoreKeepsItsPromisesUnderFault(t *testing.T) {
	storefault.Run(t, storefault.Subject{
		Name:             "vocabulary",
		Open:             openFaultVocabulary,
		MovedAsideSuffix: ".corrupt",
	})
}

func openFaultVocabulary(t *testing.T, dir string, faults *storefault.Faults) storefault.Store {
	t.Helper()
	log, disclosure := storefault.Log()
	path := filepath.Join(dir, "vocabulary.toml")
	now := func() time.Time { return time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC) }
	store := NewStore(path, StoreOptions{Now: now}, log)
	store.write = func(path string, entries []Entry, nextID int) error {
		if err := faults.Before(path); err != nil {
			return err
		}
		return writeStore(path, entries, nextID)
	}
	return &faultVocabulary{store: store, dir: dir, path: path, faults: faults, disclosure: disclosure}
}

type faultVocabulary struct {
	store      *Store
	dir        string
	path       string
	faults     *storefault.Faults
	disclosure func() []string
}

// Add teaches the content as a phrase. The meaning is derived from it so the
// suite's unique contents stay unique on both halves of the entry — teaching
// the same phrase twice supersedes rather than adding, which would make the
// concurrency case count wrong for a reason that is not a fault.
func (v *faultVocabulary) Add(content string) (string, error) {
	entry, _, err := v.store.Teach(content, "what "+content+" means", "", "test")
	if err != nil {
		return "", err
	}
	return entry.ID, nil
}

func (v *faultVocabulary) Forget(id string) error {
	_, err := v.store.Forget(id)
	return err
}

func (v *faultVocabulary) Records() []storefault.Record {
	entries := v.store.List("")
	out := make([]storefault.Record, 0, len(entries))
	for _, e := range entries {
		out = append(out, storefault.Record{ID: e.ID, Content: e.Phrase, Detail: e.Meaning})
	}
	return out
}

func (v *faultVocabulary) Reload(t *testing.T) storefault.Store {
	t.Helper()
	return openFaultVocabulary(t, v.dir, v.faults)
}

func (v *faultVocabulary) HandEdit(t *testing.T) []storefault.Record {
	t.Helper()
	doc := `version = 1
next_id = 92

[[entry]]
id = "w90"
phrase = "quid"
meaning = "pounds"
taught = 2026-08-27T08:00:00Z
updated = 2026-08-27T08:30:00Z

[[entry]]
id = "w91"
phrase = "the runbook"
meaning = "the deploy checklist in the wiki"
taught = 2026-08-27T08:00:00Z
updated = 2026-08-27T08:10:00Z
`
	if err := os.WriteFile(v.path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return []storefault.Record{
		{ID: "w90", Content: "quid", Detail: "pounds"},
		{ID: "w91", Content: "the runbook", Detail: "the deploy checklist in the wiki"},
	}
}

func (v *faultVocabulary) Damage(t *testing.T) (string, []byte) {
	t.Helper()
	raw := []byte("version = 1\n\n[[entry]]\nphrase = \"cut off mid-e")
	if err := os.WriteFile(v.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return v.path, raw
}

func (v *faultVocabulary) Disclosure() []string { return v.disclosure() }
