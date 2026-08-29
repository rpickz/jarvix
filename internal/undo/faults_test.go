package undo

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/storefault"
)

// The account's registration with the shared fault-injection suite (#173).
//
// It is here on the first day rather than the second, which is the whole
// point of #205 having built the suite: a new store inherits the promises by
// construction instead of re-arguing them. Nothing below is a new assertion —
// the Subject and a twenty-line adapter are the entire registration, and the
// two ways this store differs from its siblings (it evicts rather than
// refusing at the cap, and its records carry a payload rather than a
// sentence) are declared rather than special-cased.

func TestUndoStoreKeepsItsPromisesUnderFault(t *testing.T) {
	storefault.Run(t, storefault.Subject{
		Name:             "undo",
		Open:             openFaultUndo,
		MovedAsideSuffix: ".corrupt",
	})
}

// faultUndoNow is a fixed moment, so every record the suite writes has a
// deterministic timestamp and the newest-first order is the order they were
// added — which is what Records() has to report stably.
func faultUndoNow() func() time.Time {
	at := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	return func() time.Time {
		at = at.Add(time.Second)
		return at
	}
}

func openFaultUndo(t *testing.T, dir string, faults *storefault.Faults) storefault.Store {
	t.Helper()
	log, disclosure := storefault.Log()
	path := filepath.Join(dir, "undo.toml")
	store := NewStore(path, StoreOptions{Now: faultUndoNow()}, log)
	store.write = func(path string, p persisted) error {
		if err := faults.Before(path); err != nil {
			return err
		}
		return writeStore(path, p)
	}
	return &faultUndo{store: store, dir: dir, path: path, faults: faults, disclosure: disclosure}
}

type faultUndo struct {
	store      *Store
	dir        string
	path       string
	faults     *storefault.Faults
	disclosure func() []string
}

// Add records one action. Every record the suite makes is irreversible, and
// deliberately so: the suite is proving the FILE's promises, and a restore
// payload pointing at a temp directory the suite owns would be a second thing
// under test in the same assertions.
func (f *faultUndo) Add(content string) (string, error) {
	rec, err := f.store.Append(Action{Tool: "shell.run", Summary: content,
		Restore: OneWay("shell.run")})
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

func (f *faultUndo) Forget(id string) error { return f.store.Forget(id) }

func (f *faultUndo) Records() []storefault.Record {
	view := f.store.List()
	out := make([]storefault.Record, 0, len(view.Records))
	// List is newest-first; the suite compares positionally against what
	// HandEdit declares, and reading back in the file's own order is what
	// makes that comparison mean "the file says this".
	for i := len(view.Records) - 1; i >= 0; i-- {
		r := view.Records[i]
		out = append(out, storefault.Record{ID: r.ID, Content: r.Summary, Detail: r.Tool})
	}
	return out
}

func (f *faultUndo) Reload(t *testing.T) storefault.Store {
	t.Helper()
	return openFaultUndo(t, f.dir, f.faults)
}

// HandEdit is the file's own invitation taken up: two rows typed in by hand,
// in the order they read.
func (f *faultUndo) HandEdit(t *testing.T) []storefault.Record {
	t.Helper()
	doc := `version = 1
next_id = 92

[[action]]
id = "a90"
at = 2026-08-29T09:00:00Z
tool = "config.write_entry"
summary = "saved the routine \"morning\""
kind = "none"
because = "typed in by hand"

[[action]]
id = "a91"
at = 2026-08-29T09:05:00Z
tool = "shell.run"
summary = "ran df -h"
kind = "none"
because = "a command that has run has run"
`
	if err := os.WriteFile(f.path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return []storefault.Record{
		{ID: "a90", Content: `saved the routine "morning"`, Detail: "config.write_entry"},
		{ID: "a91", Content: "ran df -h", Detail: "shell.run"},
	}
}

func (f *faultUndo) Damage(t *testing.T) (string, []byte) {
	t.Helper()
	raw := []byte("version = 1\n\n[[action]]\nsummary = \"cut off mid-a")
	if err := os.WriteFile(f.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return f.path, raw
}

func (f *faultUndo) Disclosure() []string { return f.disclosure() }
