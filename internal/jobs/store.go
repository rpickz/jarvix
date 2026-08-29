package jobs

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/statehold"
)

// ErrUnknownJob is returned when nothing in the store answers to a name or id.
var ErrUnknownJob = errors.New("I have no job by that name")

// ErrTooMany is returned when the store already holds as many live jobs as it
// will run at once.
var ErrTooMany = errors.New("too many jobs are already running")

// MaxLive bounds how many jobs may be unfinished at once.
//
// It is small on purpose, and the reason is the reliability stance rather than
// a resource one. Every live job is a plan a model is executing without anyone
// watching, and the honest question is not "how many can the machine run" but
// "how many can a person actually hold in their head when the situation report
// reads them out". Four is about the length of a sentence. Past that the report
// stops being an answer and becomes a list, which is the failure #196 was built
// against.
const MaxLive = 4

// MaxJobs bounds the whole file, live and finished together. Finished jobs are
// kept because "what did that job do" is a question asked after it ended, and
// evicted oldest-first past the bound so a machine that runs jobs every day does
// not grow a file forever.
const MaxJobs = 60

// StoreOptions configure a Store.
type StoreOptions struct {
	// MaxLive and MaxJobs override the constants. Zero uses them.
	MaxLive int
	MaxJobs int
	// Now is the clock, injected so tests are deterministic.
	Now func() time.Time
	// Publish emits one bus event; nil publishes nothing.
	Publish func(event string, data map[string]any)
	// Gate is the backup write barrier (ADR 0045); nil is never held.
	Gate *statehold.Gate
}

// Store is the jobs file: read through a stat so a hand-edit is live on the
// next question, written atomically, never overwritten when it cannot be read.
//
// It is the whole of a job's durability. There is no in-memory registry of
// running jobs beside it and deliberately so: two records of the same fact
// disagree the first time one of them is written and the other is not, and the
// fact here is "what is this job waiting for", which a restart must not lose.
type Store struct {
	path     string
	maxLive  int
	maxJobs  int
	now      func() time.Time
	publish  func(string, map[string]any)
	gate     *statehold.Gate
	log      *slog.Logger
	adoption func(Job) Job
	// write is the disk seam: writeStore in production, a failing stub in
	// tests, so the write-failure contract is provable hermetically.
	write func(path string, p persisted) error

	mu      sync.Mutex
	st      persisted
	loaded  bool
	mod     time.Time
	size    int64
	corrupt bool
}

// NewStore opens the jobs file at path. Nothing is read until the first
// operation, so construction is free and a daemon nobody gives a direction to
// never touches the disk.
func NewStore(path string, opts StoreOptions, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{
		path: path, maxLive: opts.MaxLive, maxJobs: opts.MaxJobs, now: opts.Now,
		publish: opts.Publish, gate: opts.Gate, log: log, write: writeStore,
	}
	if s.maxLive <= 0 {
		s.maxLive = MaxLive
	}
	if s.maxJobs <= 0 {
		s.maxJobs = MaxJobs
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.st.nextID = 1
	return s
}

// Path returns the file jobs live in, so every surface can tell the user where
// to read it by hand.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Start creates a job. The scope is validated here and never again widened:
// a job may not begin without a boundary the daemon can enforce, so a scope
// that cannot be checked is a refusal to create rather than a job created
// leniently.
func (s *Store) Start(name, goal string, scope Scope) (Job, error) {
	if s == nil {
		return Job{}, fmt.Errorf("jobs are not available on this daemon")
	}
	clean, err := CleanName(name)
	if err != nil {
		return Job{}, err
	}
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return Job{}, fmt.Errorf("a job needs a goal in your own words, or there is nothing to work on")
	}
	checked, err := scope.Validate()
	if err != nil {
		return Job{}, err
	}
	s.mu.Lock()
	s.refreshLocked()
	live := 0
	for _, j := range s.st.jobs {
		if j.Name == clean && j.State.Live() {
			s.mu.Unlock()
			return Job{}, fmt.Errorf("there is already a job called %q; stop it first, or pick another name", clean)
		}
		if j.State.Live() {
			live++
		}
	}
	if live >= s.maxLive {
		s.mu.Unlock()
		return Job{}, fmt.Errorf("%w: I already have %d on the go, which is as many as I will keep track of at once",
			ErrTooMany, live)
	}
	next := clone(s.st)
	job := Job{
		ID: mintID(&next), Name: clean, Goal: goal, Scope: checked,
		State: Ready, Started: s.now().UTC(),
	}
	next.jobs = append(next.jobs, job)
	evict(&next, s.maxJobs)
	if err := s.saveLocked(next); err != nil {
		s.mu.Unlock()
		return Job{}, err
	}
	s.mu.Unlock()
	s.emit("started", job)
	return job, nil
}

// List returns every job, live ones first and newest first within each half.
// A caller that wants only the live ones filters on State.Live.
func (s *Store) List() []Job {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	out := append([]Job(nil), s.st.jobs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].State.Live() != out[j].State.Live() {
			return out[i].State.Live()
		}
		return out[i].Started.After(out[j].Started)
	})
	return out
}

// Find resolves a job by name or by id. Names are what a person says, so they
// win: an id is only tried when nothing answers to the word, which means a job
// somebody named "j2" still resolves to itself.
func (s *Store) Find(ref string) (Job, error) {
	if s == nil {
		return Job{}, ErrUnknownJob
	}
	want := strings.ToLower(strings.TrimSpace(ref))
	if want == "" {
		return Job{}, ErrUnknownJob
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	// Live jobs win over finished ones with the same name, so "stop the tidy
	// job" reaches the one that is running rather than yesterday's.
	var found Job
	ok := false
	for _, j := range s.st.jobs {
		if j.Name != want {
			continue
		}
		if !ok || (j.State.Live() && !found.State.Live()) || j.Started.After(found.Started) {
			found, ok = j, true
		}
	}
	if ok {
		return found, nil
	}
	for _, j := range s.st.jobs {
		if j.ID == want {
			return j, nil
		}
	}
	return Job{}, fmt.Errorf("%w: %s", ErrUnknownJob, ref)
}

// Update applies a change to one job under the store's lock and writes the
// result. It is the only mutation path apart from Start, which is what makes
// "every move a job makes is on disk before anything observes it" true by
// construction rather than by each caller remembering to save.
//
// change returns false to abandon the update, which is how a caller that
// discovers the job has moved underneath it — stopped while it was thinking —
// declines to write a stale state over a fresh one.
func (s *Store) Update(id string, change func(j *Job) bool) (Job, error) {
	if s == nil {
		return Job{}, ErrUnknownJob
	}
	s.mu.Lock()
	s.refreshLocked()
	next := clone(s.st)
	for i := range next.jobs {
		if next.jobs[i].ID != id {
			continue
		}
		before := next.jobs[i].State
		if !change(&next.jobs[i]) {
			job := next.jobs[i]
			s.mu.Unlock()
			return job, nil
		}
		job := next.jobs[i]
		if !job.State.Live() && job.Ended.IsZero() {
			job.Ended = s.now().UTC()
			next.jobs[i] = job
		}
		if err := s.saveLocked(next); err != nil {
			s.mu.Unlock()
			return Job{}, err
		}
		s.mu.Unlock()
		if before != job.State {
			s.emit(string(job.State), job)
		}
		return job, nil
	}
	s.mu.Unlock()
	return Job{}, fmt.Errorf("%w: %s", ErrUnknownJob, id)
}

// Forget drops one job from the file. It deletes rather than tombstones, the
// conversation archive's stance (ADR 0027): a goal is the user's own sentence
// and they must have a way to remove it that actually removes it. The id is not
// reissued.
func (s *Store) Forget(id string) error {
	if s == nil {
		return ErrUnknownJob
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	next := clone(s.st)
	for i, j := range next.jobs {
		if j.ID != id {
			continue
		}
		next.jobs = append(next.jobs[:i], next.jobs[i+1:]...)
		return s.saveLocked(next)
	}
	return fmt.Errorf("%w: %s", ErrUnknownJob, id)
}

// emit publishes one jobs.changed event, if anyone is listening. Never called
// with s.mu held — Publish reaches the bus, and a report must never hold the
// store's lock while it waits (the focus and reminder rule).
//
// The goal travels; the ledger never does. A ledger line can carry the output
// of a tool that read a file, and a bus event carrying that would put the
// contents of the user's work on every connected socket — the mistake the
// typing audit was careful not to make (ADR 0023).
func (s *Store) emit(action string, j Job) {
	if s.publish == nil {
		return
	}
	data := map[string]any{
		"action": action, "id": j.ID, "name": j.Name,
		"state": string(j.State), "steps": len(j.Ledger),
	}
	if j.Question.Why != "" {
		data["waiting_on"] = string(j.Question.Why)
	}
	s.publish("jobs.changed", data)
}

// refreshLocked brings the in-memory jobs up to date with the file. Callers
// hold s.mu. Every failure degrades: a missing file is no jobs, an unreadable
// or unparseable one is a warning plus no jobs — never an error to the caller,
// never a crash (ADR 0011's precedent, via the memory book).
//
// Degrading towards "I have no jobs" rather than "the jobs I loaded last time
// still stand" is the safe direction here for a sharper reason than elsewhere:
// a stale copy would be a runner acting on a scope the user has just edited.
func (s *Store) refreshLocked() {
	info, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		s.st.jobs = nil
		s.corrupt = false
		s.loaded, s.mod, s.size = true, time.Time{}, 0
		return
	}
	if err != nil {
		if !s.corrupt {
			s.log.Warn("the jobs file could not be read; continuing with no jobs",
				"component", "jobs", "path", s.path, "error", err.Error())
		}
		s.st.jobs = nil
		s.corrupt, s.loaded = true, true
		return
	}
	if s.loaded && info.ModTime().Equal(s.mod) && info.Size() == s.size {
		return // unchanged since the last load or write — the common case
	}
	p, err := readStore(s.path)
	s.loaded, s.mod, s.size = true, info.ModTime(), info.Size()
	if err != nil {
		if !s.corrupt {
			s.log.Warn("the jobs file could not be parsed; continuing with no jobs "+
				"(the file is left alone until a job moves)",
				"component", "jobs", "path", s.path, "error", err.Error())
		}
		s.st.jobs = nil
		s.corrupt = true
		return
	}
	if p.nextID < s.st.nextID {
		p.nextID = s.st.nextID
	}
	s.st = s.normalize(p)
	s.corrupt = false
}

// saveLocked writes the file and only then commits it to memory, so a failed
// write leaves the store describing what is actually on disk.
func (s *Store) saveLocked(p persisted) error {
	// Entered before the first byte moves, released once the store is settled:
	// `jarvix backup` holds this gate for its coherent cut.
	defer s.gate.Enter()()
	if s.corrupt {
		// Never overwrite a file we could not read: the user's hand-edit may be
		// one typo away from correct, and it is theirs.
		backup := s.path + ".corrupt"
		if err := os.Rename(s.path, backup); err == nil {
			s.log.Warn("unparseable jobs file moved aside before writing",
				"component", "jobs", "path", s.path, "backup", backup)
		}
		s.corrupt = false
	}
	if err := s.write(s.path, p); err != nil {
		return err
	}
	s.st = p
	if info, err := os.Stat(s.path); err == nil {
		s.loaded, s.mod, s.size = true, info.ModTime(), info.Size()
	}
	return nil
}

// normalize makes a file that arrived from disk behave like one this process
// wrote: blank and duplicate rows dropped, an unrecognised state read
// conservatively, a scope re-validated, and a job that was mid-step when the
// daemon went away adopted honestly.
//
// **The adoption rule** is the interesting half and it is the restart promise.
// A job marked `running` in a file nobody is running is not a job in progress —
// it is a job whose daemon went away. It comes back as Ready, so the supervisor
// picks it up, and if it was mid-step when that happened the step is written
// into the ledger as UNVERIFIED. That is the only honest reading available:
// the action was dispatched, and whether it completed is genuinely unknown.
// Recording it as done would be the #71 failure written by the daemon itself;
// dropping it would let a job repeat an action it may already have taken.
//
// Nothing is written back by normalize. A file that is merely untidy still
// works, and only a real change rewrites it.
func (s *Store) normalize(p persisted) persisted {
	seen := make(map[string]bool, len(p.jobs))
	out := make([]Job, 0, len(p.jobs))
	for _, j := range p.jobs {
		j.Goal = strings.TrimSpace(j.Goal)
		name, err := CleanName(j.Name)
		if j.Goal == "" || err != nil {
			// A row with no goal or no sayable name is not a job: there is
			// nothing to work on and no way to ask about it. Dropped rather
			// than repaired, because a repair here would be inventing the
			// user's instruction.
			continue
		}
		j.Name = name
		if j.ID == "" || seen[j.ID] {
			j.ID = freshID(&p, seen)
		}
		seen[j.ID] = true
		if !validState(j.State) {
			// An unreadable state is read as parked with a stated reason
			// rather than as ready: the refusing direction, because the
			// alternative is a hand-edit typo that sets a job running.
			j.State = Parked
			j.Question = Question{Why: WhyUnclear, At: j.Started,
				Ask: "the jobs file gives this job a state I do not recognise, so I have not carried on with it"}
		}
		if checked, err := j.Scope.Validate(); err != nil {
			// A scope that stopped being enforceable — hand-edited to name a
			// forbidden tool, or emptied — parks the job. It does not shrink
			// the scope silently and carry on, because a job continuing under
			// a boundary nobody can state is the exact thing this feature
			// exists to prevent.
			j.State = Parked
			j.Question = Question{Why: WhyOutOfScope, At: j.Started,
				Ask: "I stopped because " + err.Error()}
		} else {
			j.Scope = checked
		}
		if j.State == Running {
			j = s.adopt(j)
		}
		out = append(out, j)
	}
	p.jobs = out
	return p
}

// adopt brings a job that was mid-flight when the process ended back to Ready,
// recording the interrupted step as unverified. Split out so the honesty rule
// has a name and a test of its own.
func (s *Store) adopt(j Job) Job {
	if s.adoption != nil {
		return s.adoption(j)
	}
	j.State = Ready
	if step := j.InFlight; step.Tool != "" {
		j.Ledger = append(j.Ledger, Entry{
			At: s.now().UTC(), Intent: step.Intent, Tool: step.Tool,
			Said: "I was stopped before I saw how this ended.", Verified: false,
		})
		j.Steps++
		j.InFlight = Step{}
	}
	return j
}

// validState reports whether a state read off disk is one this package knows.
func validState(st State) bool {
	switch st {
	case Ready, Running, Parked, Done, Stopped, Failed:
		return true
	default:
		return false
	}
}

// evict drops the oldest finished jobs until the file fits. Only finished ones:
// a live job is work in progress and dropping it would abandon a direction the
// user gave without telling them. A file with more live jobs than the bound
// cannot happen, because Start refuses at MaxLive.
func evict(p *persisted, max int) {
	if max <= 0 || len(p.jobs) <= max {
		return
	}
	over := len(p.jobs) - max
	out := make([]Job, 0, len(p.jobs))
	for _, j := range p.jobs {
		if over > 0 && !j.State.Live() {
			over--
			continue
		}
		out = append(out, j)
	}
	p.jobs = out
}

// freshID mints an id no job in the file already answers to.
func freshID(p *persisted, seen map[string]bool) string {
	for {
		id := "j" + itoa(p.nextID)
		p.nextID++
		if !seen[id] {
			return id
		}
	}
}

// mintID takes the next unused id off the high-water mark. Bumped before the
// save on purpose: a failed write may skip an id, but no path can ever reuse
// one.
func mintID(p *persisted) string {
	used := make(map[string]bool, len(p.jobs))
	for _, j := range p.jobs {
		used[j.ID] = true
	}
	for {
		id := "j" + itoa(p.nextID)
		p.nextID++
		if !used[id] {
			return id
		}
	}
}

// clone deep-copies the store so callers can never mutate the Store's slices
// through a returned value.
func clone(p persisted) persisted {
	out := p
	out.jobs = make([]Job, 0, len(p.jobs))
	for _, j := range p.jobs {
		j.Ledger = append([]Entry(nil), j.Ledger...)
		j.Scope.Tools = append([]string(nil), j.Scope.Tools...)
		j.Scope.Roots = append([]string(nil), j.Scope.Roots...)
		j.Scope.Apps = append([]string(nil), j.Scope.Apps...)
		out.jobs = append(out.jobs, j)
	}
	return out
}

// itoa avoids pulling strconv into a file that needs one integer rendered.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
