package monitors

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/placement"
	"github.com/rpickz/jarvix/internal/storefault"
)

// The monitor-nickname store's registration with the shared fault-injection
// suite (issue #173).
//
// This one is worth reading as evidence rather than as a test. It is the
// store the suite was NOT built around — it landed last, it was not on the
// ticket's list, and it is the odd one out twice over: it mints no ids
// (a nickname's identity is the word the user chose) and its records are
// single words rather than sentences. Joining the suite was still a
// registration: the Subject below, and an adapter that translates Assign and
// Forget into the suite's vocabulary. Not one assertion was written for it,
// and the two ways it differs are declared rather than special-cased.

func TestMonitorStoreKeepsItsPromisesUnderFault(t *testing.T) {
	storefault.Run(t, storefault.Subject{
		Name:             "monitors",
		Open:             openFaultMonitors,
		MovedAsideSuffix: ".corrupt",
		SingleWord:       true,
		NoIDsBecause: "a nickname's identity IS its name — the user says \"top\", the routine writes " +
			"`top`, and nothing anywhere refers to a nickname by a handle (see the document type)",
	})
}

func openFaultMonitors(t *testing.T, dir string, faults *storefault.Faults) storefault.Store {
	t.Helper()
	log, disclosure := storefault.Log()
	path := filepath.Join(dir, "monitors.toml")
	now := func() time.Time { return time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC) }
	store := NewStore(path, StoreOptions{Now: now}, log)
	store.write = func(path string, names []Nickname) error {
		if err := faults.Before(path); err != nil {
			return err
		}
		return writeStore(path, names)
	}
	return &faultMonitors{store: store, dir: dir, path: path, faults: faults, disclosure: disclosure}
}

type faultMonitors struct {
	store      *Store
	dir        string
	path       string
	faults     *storefault.Faults
	disclosure func() []string
}

// Add names the screen that is plugged in. Every record points at the same
// connector on purpose: the suite is exercising the file, and which screen a
// name means is settled by the store's own tests.
func (m *faultMonitors) Add(content string) (string, error) {
	n, err := m.store.Assign(content, topMonitor().Name, bothMonitors())
	if err != nil {
		return "", err
	}
	return n.Name, nil
}

func (m *faultMonitors) Forget(id string) error {
	_, err := m.store.Forget(id)
	return err
}

func (m *faultMonitors) Records() []storefault.Record {
	names := m.store.List()
	out := make([]storefault.Record, 0, len(names))
	for _, n := range names {
		out = append(out, storefault.Record{ID: n.Name, Content: n.Name, Detail: n.Connector})
	}
	return out
}

func (m *faultMonitors) Reload(t *testing.T) storefault.Store {
	t.Helper()
	return openFaultMonitors(t, m.dir, m.faults)
}

// HandEdit is the cable-move the file's own header describes: a user opens
// the file and repoints a name. The store lists by name, so the expectation
// is in alphabetical order.
func (m *faultMonitors) HandEdit(t *testing.T) []storefault.Record {
	t.Helper()
	doc := `version = 1

[[nickname]]
name = "bottom"
connector = "DP-2"
named = 2026-08-28T09:00:00Z
updated = 2026-08-28T09:00:00Z

[[nickname]]
name = "top"
connector = "HDMI-A-1"
named = 2026-08-28T09:00:00Z
updated = 2026-08-28T09:05:00Z
`
	if err := os.WriteFile(m.path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return []storefault.Record{
		{ID: "bottom", Content: "bottom", Detail: "DP-2"},
		{ID: "top", Content: "top", Detail: "HDMI-A-1"},
	}
}

func (m *faultMonitors) Damage(t *testing.T) (string, []byte) {
	t.Helper()
	raw := []byte("version = 1\n\n[[nickname]]\nname = \"cut off mid-n")
	if err := os.WriteFile(m.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return m.path, raw
}

func (m *faultMonitors) Disclosure() []string { return m.disclosure() }

// A compile-time note rather than a test: the inventory these adapters judge
// against is the package's own fixture, so a change to what a present screen
// looks like reaches this file too.
var _ = []placement.Monitor(bothMonitors())
