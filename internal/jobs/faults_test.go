package jobs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/storefault"
)

// The job store's registration with the shared fault-injection suite (#173,
// #205).
//
// It is here on the first day, which is the whole point of the suite existing:
// a new store inherits the promises by construction rather than re-arguing
// them. Nothing below is a new assertion — the Subject and a short adapter are
// the entire registration.
//
// It matters more for this store than for its siblings. Every other one holds
// something the user can retype; this one holds the record of work being done
// in their name while they are not watching, and a torn write here is a job
// whose scope or whose parked question has quietly changed.

func TestJobStoreKeepsItsPromisesUnderFault(t *testing.T) {
	storefault.Run(t, storefault.Subject{
		Name:             "jobs",
		Open:             openFaultJobs,
		MovedAsideSuffix: ".corrupt",
		SingleWord:       true,
	})
}

// faultJobsNow is a moving fixed clock, so every job the suite writes has a
// deterministic and distinct start time — which is what makes the store's
// order stable and the suite's positional comparison mean something.
func faultJobsNow() func() time.Time {
	at := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	return func() time.Time {
		at = at.Add(time.Second)
		return at
	}
}

func openFaultJobs(t *testing.T, dir string, faults *storefault.Faults) storefault.Store {
	t.Helper()
	log, disclosure := storefault.Log()
	path := filepath.Join(dir, "jobs.toml")
	// The live bound is lifted for the suite, and only for it. MaxLive is a
	// claim about how many pieces of unsupervised work a person can hold in
	// their head; the suite is proving what the FILE promises, and a store that
	// refused its sixteenth concurrent write would be failing an assertion
	// about torn reads with an answer about product design.
	store := NewStore(path, StoreOptions{Now: faultJobsNow(),
		MaxLive: 1000, MaxJobs: 1000}, log)
	store.write = func(path string, p persisted) error {
		if err := faults.Before(path); err != nil {
			return err
		}
		return writeStore(path, p)
	}
	return &faultJobs{store: store, dir: dir, path: path, faults: faults, disclosure: disclosure}
}

type faultJobs struct {
	store      *Store
	dir        string
	path       string
	faults     *storefault.Faults
	disclosure func() []string
}

// Add starts one job. The suite hands single words (SingleWord), which is what
// a job's name is anyway — the handle a person says out loud — so the record's
// content is the name and its detail is the goal.
func (f *faultJobs) Add(content string) (string, error) {
	job, err := f.store.Start(content, "work on "+content,
		Scope{Tools: []string{"memory.search"}, Roots: []string{f.dir}})
	if err != nil {
		return "", err
	}
	return job.ID, nil
}

func (f *faultJobs) Forget(id string) error { return f.store.Forget(id) }

func (f *faultJobs) Records() []storefault.Record {
	jobs := f.store.List()
	out := make([]storefault.Record, 0, len(jobs))
	// List puts the newest first within the live half; the suite compares
	// positionally against what HandEdit declares, and reading back in the
	// file's own order is what makes that comparison mean "the file says this".
	for i := len(jobs) - 1; i >= 0; i-- {
		out = append(out, storefault.Record{ID: jobs[i].ID, Content: jobs[i].Name, Detail: jobs[i].Goal})
	}
	return out
}

func (f *faultJobs) Reload(t *testing.T) storefault.Store {
	t.Helper()
	return openFaultJobs(t, f.dir, f.faults)
}

// HandEdit is the file's own invitation taken up: two jobs typed in by hand,
// in the order they read.
func (f *faultJobs) HandEdit(t *testing.T) []storefault.Record {
	t.Helper()
	doc := `version = 1
next_id = 92

[[job]]
id = "j90"
name = "tidy"
goal = "tidy up my downloads"
state = "parked"
started = 2026-08-29T09:00:00Z

[job.scope]
tools = ["memory.search"]
roots = ["` + f.dir + `"]

[job.question]
why = "decision"
ask = "Which folder did you mean?"
at = 2026-08-29T09:01:00Z

[[job]]
id = "j91"
name = "notes"
goal = "file away my meeting notes"
state = "done"
started = 2026-08-29T09:05:00Z
ended = 2026-08-29T09:06:00Z

[job.scope]
tools = ["memory.search"]
roots = ["` + f.dir + `"]
`
	if err := os.WriteFile(f.path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	// Records() reads the store's own order back to front, and List puts the
	// live job first — so the finished one comes back first and the parked one
	// second, which is what the suite is told to expect.
	return []storefault.Record{
		{ID: "j91", Content: "notes", Detail: "file away my meeting notes"},
		{ID: "j90", Content: "tidy", Detail: "tidy up my downloads"},
	}
}

func (f *faultJobs) Damage(t *testing.T) (string, []byte) {
	t.Helper()
	raw := []byte("version = 1\n\n[[job]]\ngoal = \"cut off mid-g")
	if err := os.WriteFile(f.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return f.path, raw
}

func (f *faultJobs) Disclosure() []string { return f.disclosure() }
