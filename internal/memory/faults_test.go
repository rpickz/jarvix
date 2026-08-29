package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/storefault"
)

// The memory book's registration with the shared fault-injection suite
// (issue #173). Everything asserted here is asserted identically of every
// other durable store; what lives in this file is only the translation
// between the book's own API and the suite's — an adapter, not a test.
//
// The book is the store the discipline was written for (ADR 0025), so if any
// registration should be short, it is this one.

func TestMemoryBookKeepsItsPromisesUnderFault(t *testing.T) {
	storefault.Run(t, storefault.Subject{
		Name:             "memory",
		Open:             openFaultBook,
		MovedAsideSuffix: ".corrupt",
	})
}

// faultBookClock is a fixed clock, so every timestamp the suite provokes is
// chosen rather than sampled.
func faultBookClock() time.Time {
	return time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
}

func openFaultBook(t *testing.T, dir string, faults *storefault.Faults) storefault.Store {
	t.Helper()
	log, disclosure := storefault.Log()
	path := filepath.Join(dir, "memory.toml")
	book := NewBook(path, BookOptions{Now: faultBookClock}, log)
	// The write seam. writeStore is reached only when the suite's faults say
	// the disk is healthy, which is the whole reason this field exists.
	book.write = func(path string, facts []Fact, nextID int) error {
		if err := faults.Before(path); err != nil {
			return err
		}
		return writeStore(path, facts, nextID)
	}
	return &faultBook{book: book, dir: dir, path: path, faults: faults, disclosure: disclosure}
}

type faultBook struct {
	book       *Book
	dir        string
	path       string
	faults     *storefault.Faults
	disclosure func() []string
}

func (b *faultBook) Add(content string) (string, error) {
	fact, _, err := b.book.Add(content, "test")
	if err != nil {
		return "", err
	}
	return fact.ID, nil
}

func (b *faultBook) Forget(id string) error {
	_, err := b.book.Forget(id)
	return err
}

func (b *faultBook) Records() []storefault.Record {
	facts := b.book.List("")
	out := make([]storefault.Record, 0, len(facts))
	for _, f := range facts {
		out = append(out, storefault.Record{ID: f.ID, Content: f.Content, Detail: f.Source})
	}
	return out
}

func (b *faultBook) Reload(t *testing.T) storefault.Store {
	t.Helper()
	return openFaultBook(t, b.dir, b.faults)
}

// HandEdit writes the file the way the header invites the user to: two facts
// with their own ids, timestamps and sources. The updated times decide the
// order the book lists them in, so the expectation below is exact rather
// than approximate.
func (b *faultBook) HandEdit(t *testing.T) []storefault.Record {
	t.Helper()
	doc := `version = 1
next_id = 92

[[fact]]
id = "m90"
content = "the user's terminal is Ghostty"
stored = 2026-08-27T08:00:00Z
updated = 2026-08-27T08:30:00Z
source = "hand"

[[fact]]
id = "m91"
content = "the standup is at quarter past nine"
stored = 2026-08-27T08:00:00Z
updated = 2026-08-27T08:10:00Z
source = "editor"
`
	if err := os.WriteFile(b.path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return []storefault.Record{
		{ID: "m90", Content: "the user's terminal is Ghostty", Detail: "hand"},
		{ID: "m91", Content: "the standup is at quarter past nine", Detail: "editor"},
	}
}

func (b *faultBook) Damage(t *testing.T) (string, []byte) {
	t.Helper()
	raw := []byte("version = 1\n\n[[fact]]\ncontent = \"cut off mid-w")
	if err := os.WriteFile(b.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return b.path, raw
}

func (b *faultBook) Disclosure() []string { return b.disclosure() }
