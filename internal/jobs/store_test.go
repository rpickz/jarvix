package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The store's tests. Hermetic: a temp directory, an injected clock, and no
// runner anywhere — everything here is about what survives being written down.

// fixedClock is a clock that moves only when the test says so.
type fixedClock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *fixedClock {
	return &fixedClock{at: time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)}
}

func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(time.Second)
	return c.at
}

// newStore opens a store over a fresh temp directory.
func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.toml")
	return NewStore(path, StoreOptions{Now: newClock().now}, nil), path
}

// aScope is an enforceable scope over one directory.
func aScope(t *testing.T) Scope {
	t.Helper()
	return Scope{Tools: []string{"memory.search"}, Roots: []string{t.TempDir()}}
}

func TestAJobIsWrittenDownTheMomentItStarts(t *testing.T) {
	store, path := newStore(t)
	job, err := store.Start("Tidy Downloads", "tidy up my downloads", aScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if job.Name != "tidy downloads" {
		t.Errorf("name = %q, want it normalised", job.Name)
	}
	if job.Goal != "tidy up my downloads" {
		t.Errorf("goal = %q, want the user's own words kept verbatim", job.Goal)
	}
	if job.State != Ready {
		t.Errorf("state = %q, want %q", job.State, Ready)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("a started job left nothing on disk: %v", err)
	}
	if !strings.Contains(string(raw), "tidy up my downloads") {
		t.Error("the goal is not in the file, so a restart would not know what was asked for")
	}
}

func TestAJobWithoutAnEnforceableScopeIsNeverCreated(t *testing.T) {
	store, path := newStore(t)
	if _, err := store.Start("tidy", "tidy up", Scope{Tools: []string{"memory.search"}}); err == nil {
		t.Fatal("a job started with a boundary nobody can check")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a refused job still wrote a file; nothing about it should exist")
	}
}

func TestAJobNeedsAGoalInTheUsersWords(t *testing.T) {
	store, _ := newStore(t)
	if _, err := store.Start("tidy", "   ", aScope(t)); err == nil {
		t.Fatal("a job with no goal was created; there would be nothing to work on")
	}
}

func TestTwoLiveJobsMayNotShareAName(t *testing.T) {
	store, _ := newStore(t)
	scope := aScope(t)
	if _, err := store.Start("tidy", "tidy up", scope); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start("tidy", "tidy up again", scope); err == nil {
		t.Fatal("two live jobs took the same name; \"stop the tidy job\" would be ambiguous")
	}
	// Once the first has finished the name is free again: a name is how a
	// person addresses work in progress, not a permanent reservation.
	first, _ := store.Find("tidy")
	if _, err := store.Update(first.ID, func(j *Job) bool { j.State = Done; return true }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start("tidy", "tidy up again", scope); err != nil {
		t.Fatalf("a finished job kept its name reserved: %v", err)
	}
}

func TestOnlySoManyJobsRunAtOnce(t *testing.T) {
	store, _ := newStore(t)
	store.maxLive = 2
	scope := aScope(t)
	for _, name := range []string{"one", "two"} {
		if _, err := store.Start(name, "do "+name, scope); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Start("three", "do three", scope); err == nil {
		t.Fatal("a third job started past the live bound")
	}
}

func TestALiveJobAnswersToItsNameBeforeAFinishedOne(t *testing.T) {
	store, _ := newStore(t)
	scope := aScope(t)
	old, err := store.Start("tidy", "yesterday's tidy", scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(old.ID, func(j *Job) bool { j.State = Done; return true }); err != nil {
		t.Fatal(err)
	}
	live, err := store.Start("tidy", "today's tidy", scope)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Find("tidy")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != live.ID {
		t.Errorf("Find resolved to %s, want the live job %s", got.ID, live.ID)
	}
	// An id still reaches the finished one, which is how a report about it can
	// be asked for after the name has moved on.
	if got, err := store.Find(old.ID); err != nil || got.ID != old.ID {
		t.Errorf("Find(%q) = %v, %v; an id must always reach its own job", old.ID, got.ID, err)
	}
}

// TestAJobSurvivesARestartAtItsCheckpoint is the restart criterion. Nothing is
// held in memory: a second store over the same file is a new daemon.
func TestAJobSurvivesARestartAtItsCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.toml")
	first := NewStore(path, StoreOptions{Now: newClock().now}, nil)
	scope := aScope(t)
	job, err := first.Start("tidy", "tidy up my downloads", scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Update(job.ID, func(j *Job) bool {
		j.Ledger = append(j.Ledger, Entry{At: time.Now().UTC(), Intent: "looked at the folder",
			Tool: "memory.search", Said: "seven files", Verified: true})
		j.Steps = 1
		j.State = Parked
		j.Question = Question{Why: WhyApproval, Ask: "Shall I delete them? This can't be undone.",
			Step: Step{Intent: "delete the old files", Tool: "shell.run", Args: `{"command":"rm x"}`}}
		return true
	}); err != nil {
		t.Fatal(err)
	}

	restarted := NewStore(path, StoreOptions{Now: newClock().now}, nil)
	got, err := restarted.Find("tidy")
	if err != nil {
		t.Fatalf("the job did not survive the restart: %v", err)
	}
	if got.State != Parked {
		t.Errorf("state = %q, want %q — a parked job is state, not a paused goroutine", got.State, Parked)
	}
	if got.Question.Why != WhyApproval || !strings.Contains(got.Question.Ask, "can't be undone") {
		t.Errorf("question = %+v, want the approval it parked on, intact", got.Question)
	}
	if got.Question.Step.Tool != "shell.run" {
		t.Errorf("pending step = %+v, want the step it stopped on, so approving resumes THAT action",
			got.Question.Step)
	}
	if len(got.Ledger) != 1 || got.Ledger[0].Said != "seven files" {
		t.Errorf("ledger = %+v, want the checkpoint it had reached", got.Ledger)
	}
	if len(got.Scope.Roots) != 1 || got.Scope.Roots[0] != scope.Roots[0] {
		t.Errorf("scope = %+v, want the boundary it was given", got.Scope)
	}
}

// TestAJobInterruptedMidStepComesBackUnverified is the honesty rule written by
// the daemon about itself. The action was dispatched and its end was never
// seen; recording it as done would be #71 in the store's own hand.
func TestAJobInterruptedMidStepComesBackUnverified(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.toml")
	first := NewStore(path, StoreOptions{Now: newClock().now}, nil)
	job, err := first.Start("tidy", "tidy up", aScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Update(job.ID, func(j *Job) bool {
		j.State = Running
		j.InFlight = Step{Intent: "delete the old files", Tool: "shell.run"}
		return true
	}); err != nil {
		t.Fatal(err)
	}

	restarted := NewStore(path, StoreOptions{Now: newClock().now}, nil)
	got, err := restarted.Find("tidy")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Ready {
		t.Errorf("state = %q, want %q: a job marked running in a file nobody is running "+
			"is a job whose daemon went away", got.State, Ready)
	}
	if len(got.Ledger) != 1 {
		t.Fatalf("ledger = %+v, want the interrupted step recorded", got.Ledger)
	}
	if got.Ledger[0].Verified {
		t.Error("the interrupted step was recorded as verified; nobody saw how it ended")
	}
	if got.Ledger[0].Tool != "shell.run" {
		t.Errorf("the interrupted step = %+v, want it to name what was dispatched", got.Ledger[0])
	}
	if got.Unverified() != 1 {
		t.Errorf("Unverified() = %d, want 1 — the report leads with this number", got.Unverified())
	}
}

// TestAHandEditedScopeThatStoppedBeingEnforceableParksTheJob is the refusing
// direction applied to the file the user owns: a boundary nobody can state is
// not a boundary to carry on inside.
func TestAHandEditedScopeThatStoppedBeingEnforceableParksTheJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.toml")
	doc := `version = 1
next_id = 2

[[job]]
id = "j1"
name = "tidy"
goal = "tidy up"
state = "ready"
started = 2026-08-29T09:00:00Z

[job.scope]
tools = ["config.write_entry"]
roots = ["/tmp"]
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path, StoreOptions{Now: newClock().now}, nil)
	got, err := store.Find("tidy")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Parked {
		t.Fatalf("state = %q, want %q: a job whose scope names a governing tool must not carry on",
			got.State, Parked)
	}
	if got.Question.Why != WhyOutOfScope {
		t.Errorf("why = %q, want %q", got.Question.Why, WhyOutOfScope)
	}
}

// TestAHandEditedStateNobodyRecognisesParksTheJob pins the other refusing
// direction: a typo must not be able to set a job running.
func TestAHandEditedStateNobodyRecognisesParksTheJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.toml")
	doc := `version = 1
next_id = 2

[[job]]
id = "j1"
name = "tidy"
goal = "tidy up"
state = "reddy"
started = 2026-08-29T09:00:00Z

[job.scope]
tools = ["memory.search"]
roots = ["/tmp"]
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path, StoreOptions{Now: newClock().now}, nil)
	got, err := store.Find("tidy")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Parked || got.Question.Why != WhyUnclear {
		t.Errorf("state = %q why = %q, want parked and unclear", got.State, got.Question.Why)
	}
}

// TestSettingAParkedJobReadyByHandIsHowYouSayCarryOn is the file header's
// promise, and the reason the supervisor sweeps.
func TestSettingAParkedJobReadyByHandIsHowYouSayCarryOn(t *testing.T) {
	store, path := newStore(t)
	job, err := store.Start("tidy", "tidy up", aScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(job.ID, func(j *Job) bool {
		j.State = Parked
		j.Question = Question{Why: WhyDecision, Ask: "Which folder?"}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), `state = "parked"`, `state = "ready"`, 1)
	if edited == string(raw) {
		t.Fatal("the file does not say the state in a form a person could edit")
	}
	// The mtime check is second-granular on some filesystems, so the size
	// changes too — which is what refreshLocked also compares. Here the edit
	// shortens the document, so both differ.
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := store.Find("tidy")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Ready {
		t.Errorf("state = %q, want %q: the hand edit was not picked up", got.State, Ready)
	}
}

func TestAJobThatMovedUnderneathTheCallerIsNotOverwritten(t *testing.T) {
	store, _ := newStore(t)
	job, err := store.Start("tidy", "tidy up", aScope(t))
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Update(job.ID, func(j *Job) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Ready {
		t.Errorf("state = %q, want the job untouched", got.State)
	}
}

func TestAFinishedJobGetsAnEndingMoment(t *testing.T) {
	store, _ := newStore(t)
	job, err := store.Start("tidy", "tidy up", aScope(t))
	if err != nil {
		t.Fatal(err)
	}
	done, err := store.Update(job.ID, func(j *Job) bool { j.State = Done; return true })
	if err != nil {
		t.Fatal(err)
	}
	if done.Ended.IsZero() {
		t.Error("a finished job has no ending moment, so the report could not say it was news")
	}
}

func TestForgettingAJobRemovesItAndKeepsItsID(t *testing.T) {
	store, _ := newStore(t)
	job, err := store.Start("tidy", "tidy up", aScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Forget(job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Find("tidy"); err == nil {
		t.Error("a forgotten job still answers to its name")
	}
	next, err := store.Start("tidy", "tidy up again", aScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == job.ID {
		t.Errorf("id %q was reissued; a report that once named one job would come to name another", next.ID)
	}
}

func TestTheFileNeverGrowsWithoutLimit(t *testing.T) {
	store, _ := newStore(t)
	store.maxJobs = 3
	scope := aScope(t)
	for _, name := range []string{"one", "two", "three", "four", "five"} {
		job, err := store.Start(name, "do "+name, scope)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Update(job.ID, func(j *Job) bool { j.State = Done; return true }); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(store.List()); got > 3 {
		t.Errorf("the file holds %d jobs, want at most 3", got)
	}
	// The newest survive: "what did that job do" is asked about recent work.
	if _, err := store.Find("five"); err != nil {
		t.Error("the newest job was evicted")
	}
}

// TestALiveJobIsNeverEvicted: dropping work in progress would abandon a
// direction the user gave without telling them.
func TestALiveJobIsNeverEvicted(t *testing.T) {
	store, _ := newStore(t)
	store.maxJobs = 2
	store.maxLive = 4
	scope := aScope(t)
	live, err := store.Start("keeper", "keep going", scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two", "three"} {
		job, err := store.Start(name, "do "+name, scope)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Update(job.ID, func(j *Job) bool { j.State = Done; return true }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Find(live.ID); err != nil {
		t.Error("a live job was evicted to make room for finished ones")
	}
}

func TestAnUnreadableFileIsNeverOverwritten(t *testing.T) {
	store, path := newStore(t)
	if err := os.WriteFile(path, []byte("version = 1\n\n[[job]]\nname = \"cut off"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := store.List(); len(got) != 0 {
		t.Errorf("a corrupt file produced %d jobs; it should degrade to none", len(got))
	}
	if _, err := store.Start("tidy", "tidy up", aScope(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Error("the unreadable file was overwritten rather than set aside; it was the user's")
	}
}
