package reminders

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/storefault"
)

// The reminder store's registration with the shared fault-injection suite
// (issue #173).

func TestReminderStoreKeepsItsPromisesUnderFault(t *testing.T) {
	storefault.Run(t, storefault.Subject{
		Name:             "reminders",
		Open:             openFaultReminders,
		MovedAsideSuffix: ".corrupt",
	})
}

// faultRemindersNow is a Wednesday at one in the afternoon, so "at three"
// resolves to a moment later the same day and every reminder the suite makes
// is still pending when it reads them back.
func faultRemindersNow() time.Time {
	return time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)
}

func openFaultReminders(t *testing.T, dir string, faults *storefault.Faults) storefault.Store {
	t.Helper()
	log, disclosure := storefault.Log()
	path := filepath.Join(dir, "reminders.toml")
	svc := NewService(path, Options{Now: faultRemindersNow}, log)
	svc.write = func(path string, p persisted) error {
		if err := faults.Before(path); err != nil {
			return err
		}
		return writeStore(path, p)
	}
	return &faultReminders{svc: svc, dir: dir, path: path, faults: faults, disclosure: disclosure}
}

type faultReminders struct {
	svc        *Service
	dir        string
	path       string
	faults     *storefault.Faults
	disclosure func() []string
}

// Add stores one reminder. Create answers with the spoken confirmation
// rather than the id, so the id is read back out of the store — a
// synchronous read of the same lock, on the same goroutine, in the same call
// the write happened in.
func (r *faultReminders) Add(content string) (string, error) {
	if _, err := r.svc.Create("at three", content); err != nil {
		return "", err
	}
	for _, p := range r.svc.Snapshot().Pending {
		if p.Text == content {
			return p.ID, nil
		}
	}
	return "", errors.New("the reminder was stored but is not in the store")
}

func (r *faultReminders) Forget(id string) error {
	_, err := r.svc.Cancel(id)
	return err
}

func (r *faultReminders) Records() []storefault.Record {
	v := r.svc.Snapshot()
	out := make([]storefault.Record, 0, len(v.Pending))
	for _, p := range v.Pending {
		out = append(out, storefault.Record{
			ID: p.ID, Content: p.Text, Detail: p.Due.UTC().Format(time.RFC3339)})
	}
	return out
}

func (r *faultReminders) Reload(t *testing.T) storefault.Store {
	t.Helper()
	return openFaultReminders(t, r.dir, r.faults)
}

// HandEdit is the file's own invitation taken up: two reminders typed in by
// hand, with the due times that decide the soonest-first order below.
func (r *faultReminders) HandEdit(t *testing.T) []storefault.Record {
	t.Helper()
	doc := `version = 1
next_id = 92

[[reminder]]
id = "r90"
text = "call the pharmacy"
due = 2026-08-26T15:00:00Z
created = 2026-08-26T13:00:00Z

[[reminder]]
id = "r91"
text = "take the bins out"
due = 2026-08-26T18:00:00Z
created = 2026-08-26T13:00:00Z
`
	if err := os.WriteFile(r.path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return []storefault.Record{
		{ID: "r90", Content: "call the pharmacy", Detail: "2026-08-26T15:00:00Z"},
		{ID: "r91", Content: "take the bins out", Detail: "2026-08-26T18:00:00Z"},
	}
}

func (r *faultReminders) Damage(t *testing.T) (string, []byte) {
	t.Helper()
	raw := []byte("version = 1\n\n[[reminder]]\ntext = \"cut off mid-r")
	if err := os.WriteFile(r.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return r.path, raw
}

func (r *faultReminders) Disclosure() []string { return r.disclosure() }
