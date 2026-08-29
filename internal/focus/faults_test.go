package focus

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/storefault"
)

// The focus thread store's registration with the shared fault-injection
// suite (issue #173). store_test.go already asserted several of these
// promises by hand; they stay there, because they say things about threads
// specifically. What this adds is the rest of the matrix — a mid-flight
// failure's debris, a full disk, and a reader running alongside a writer —
// on the same terms as every other store.

func TestFocusStoreKeepsItsPromisesUnderFault(t *testing.T) {
	storefault.Run(t, storefault.Subject{
		Name:             "focus",
		Open:             openFaultFocus,
		MovedAsideSuffix: ".corrupt",
	})
}

func openFaultFocus(t *testing.T, dir string, faults *storefault.Faults) storefault.Store {
	t.Helper()
	log, disclosure := storefault.Log()
	path := filepath.Join(dir, "focus.toml")
	now := func() time.Time { return time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC) }
	svc := NewService(path, Options{Now: now}, log)
	svc.write = func(path string, p persisted) error {
		if err := faults.Before(path); err != nil {
			return err
		}
		return writeStore(path, p)
	}
	return &faultFocus{svc: svc, dir: dir, path: path, faults: faults, disclosure: disclosure}
}

type faultFocus struct {
	svc        *Service
	dir        string
	path       string
	faults     *storefault.Faults
	disclosure func() []string
}

// Add creates a thread with no anchors: the desktop is not what this suite
// is about, and a store that needed a compositor to be tested would not be
// hermetic.
func (f *faultFocus) Add(content string) (string, error) {
	th, _, err := f.svc.Create(context.Background(), content, 0)
	if err != nil {
		return "", err
	}
	return th.ID, nil
}

func (f *faultFocus) Forget(id string) error {
	_, err := f.svc.End(id)
	return err
}

func (f *faultFocus) Records() []storefault.Record {
	v := f.svc.Snapshot(context.Background())
	out := make([]storefault.Record, 0, len(v.Threads))
	for _, th := range v.Threads {
		out = append(out, storefault.Record{ID: th.ID, Content: th.Name, Detail: th.Recap})
	}
	return out
}

func (f *faultFocus) Reload(t *testing.T) storefault.Store {
	t.Helper()
	return openFaultFocus(t, f.dir, f.faults)
}

// HandEdit is what the file's header invites: two threads, one of them told
// to always recap. No active pointer, so the listing order is the
// last-activity order the times below fix.
func (f *faultFocus) HandEdit(t *testing.T) []storefault.Record {
	t.Helper()
	doc := `version = 1
next_thread_id = 92
next_parked_id = 1

[[thread]]
id = "t90"
name = "the ci refactor"
created = 2026-08-27T08:00:00Z
last_activity = 2026-08-27T08:30:00Z
recap = "always"

[[thread]]
id = "t91"
name = "the quarterly review"
created = 2026-08-27T08:00:00Z
last_activity = 2026-08-27T08:10:00Z
`
	if err := os.WriteFile(f.path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return []storefault.Record{
		{ID: "t90", Content: "the ci refactor", Detail: "always"},
		{ID: "t91", Content: "the quarterly review", Detail: ""},
	}
}

func (f *faultFocus) Damage(t *testing.T) (string, []byte) {
	t.Helper()
	raw := []byte("version = 1\n\n[[thread]]\nname = \"cut off mid-t")
	if err := os.WriteFile(f.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return f.path, raw
}

func (f *faultFocus) Disclosure() []string { return f.disclosure() }
