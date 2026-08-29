package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/quiesce"
)

// The clockwork, and the one place a job actually acts.
//
// The shape is the automation scheduler's discipline (ADR 0032) over a queue
// rather than a clock, and it obeys ADR 0049 to the letter: **the supervisor
// loop is always armed.** There is no branch in which it stops selecting on its
// timer. With nothing ready it waits out a bounded idle sweep, still
// interruptible by a wake signal and still cancelled by the generation context.
//
// The sweep earns its keep here for the same reason it does in internal/focus
// and internal/reminders, and one more that is specific to jobs. The jobs file
// is hand-editable and its header says so: setting a parked job back to
// `state = "ready"` is how a person says "carry on" with a text editor. The
// supervisor is the only reader of that change with no caller to bring it back,
// so without a sweep the edit would arm nothing until some unrelated verb
// happened to wake the loop — the exact defect #152 found in the focus loop.
//
// A wake signal carries no claim about what changed (ADR 0049's first
// supporting rule). Callers signal after dropping the store's lock, so the loop
// routinely consumes a token for a change its last pass already saw. That
// asymmetry stays this way round: a spurious wake costs one re-read of a small
// file, and a suppressed wake costs a job that never starts.
//
// The #136 lesson — a boundary reschedule must not double-fire — applies to the
// one transition that could: Ready → Running. It happens exactly once per step,
// inside Store.Update under the store's lock, and the claim is what decides it.
// Two racing wakes therefore cannot put two runners on one job: the second one's
// Update sees a job that is no longer Ready and declines.

// Decision is the permission gate's answer for one step, mirrored here so this
// package does not import internal/tools (which imports it back, for the job
// verbs).
type Decision int

const (
	// Allow is the step running without a question.
	Allow Decision = iota
	// Ask is the gate requiring confirmation. A job parks on it.
	Ask
	// Deny is the gate refusing outright. A job parks with the reason and
	// cannot be resumed by answering: a denied tool is denied by standing
	// configuration, and a job is not a way to ask again.
	Deny
)

// Verdict is the gate's answer plus the words to park with.
type Verdict struct {
	Decision Decision
	// Question is the sentence the user is asked, for Ask. It is the gate's
	// own generated question — the same one a session would have shown — so a
	// person answering a parked job is answering exactly what they would have
	// been asked at the time.
	Question string
	// Reason is why, for Deny.
	Reason string
}

// Result is what one executed step actually did, as the runner observed it.
type Result struct {
	// Said is the tool's own result text. It is the gathered fact and the only
	// thing a report is allowed to be built from.
	Said string
	// Failed is whether the tool reported that the work did not happen.
	Failed bool
	// Undo is the id of the account record the step produced, empty when it
	// changed nothing recordable.
	Undo string
}

// Planner decides what a job does next. It is the one seam a model sits behind,
// and it is deliberately narrow: it proposes, it never dispatches, and
// everything it proposes is checked against the scope and the gate before
// anything happens.
type Planner interface {
	// Next proposes the next step, given the goal, the scope and everything
	// that has already happened. It must be bounded by ctx.
	Next(ctx context.Context, v View) (Step, error)
}

// Actor is the machine a job acts on. Three verbs, in the order they are always
// called, because the order is the safety argument: read what the step would
// touch, judge it, and only then do it.
type Actor interface {
	// Subject reports what a proposed step would ACTUALLY touch, read
	// daemon-side out of the call's parsed arguments. An error means the
	// daemon cannot say — which parks the job rather than guessing, because a
	// subject nobody can name cannot be checked against a boundary.
	Subject(ctx context.Context, s Step) (Attempt, error)
	// Judge classifies the step under the permission gate, exactly as a
	// session would.
	Judge(ctx context.Context, s Step) Verdict
	// Do runs the step. The job id is handed over so the account records which
	// piece of work each change belonged to (#201, ADR 0064).
	Do(ctx context.Context, job string, s Step) (Result, error)
}

// View is what a planner is shown. It is a copy: nothing a planner does can
// reach the store.
type View struct {
	// Goal is the user's own words, verbatim.
	Goal string
	// Scope is the boundary, so the plan can be made inside it rather than
	// discovering it by being refused. The scope is still ENFORCED afterwards
	// — telling the planner the boundary is a courtesy, never the control.
	Scope Scope
	// Ledger is everything that has happened, oldest first.
	Ledger []Entry
	// Left is how many steps the job may still take.
	Left int
	// Answer is what the user said when they last unparked the job, empty for
	// an ordinary step. It is how a decision they made reaches the plan.
	Answer string
}

// RunnerOptions configure a Runner.
type RunnerOptions struct {
	// Store is where jobs live.
	Store *Store
	// Planner proposes steps; nil makes every job park as stuck with an
	// honest sentence, which is the disabled-means-absent rule.
	Planner Planner
	// Actor does them; nil is the same.
	Actor Actor
	// Now is the clock.
	Now func() time.Time
	// Timer creates one shot of it, so tests drive the loop without sleeping.
	Timer func(d time.Duration) (<-chan time.Time, func())
	// StepTimeout bounds one step — the plan and the action together. Zero
	// uses DefaultStepTimeout.
	StepTimeout time.Duration
	// IdleSweep overrides the sweep interval. Zero uses idleSweep.
	IdleSweep time.Duration
}

// DefaultStepTimeout bounds one step of one job: the planner's call and the
// action it proposes.
//
// Two minutes is chosen against what a step IS rather than against a model's
// latency. A step is one tool call — a file written, a command run, a window
// placed — and the planning turn that chose it. Anything that has not finished
// in two minutes is not slow, it is stuck, and a job that hangs on it is a job
// whose situation report says "working" forever. The timeout does not fail the
// job: the step is recorded as unverified and the job carries on, which is the
// honest reading of an action whose end nobody saw.
const DefaultStepTimeout = 2 * time.Minute

// idleSweep is the longest the supervisor goes without looking at the jobs
// file when nothing is ready. It is not a poll for work — every mutation wakes
// the loop directly — it is the file's hand-edit promise applied to the one
// reader with no other way of hearing about a change (ADR 0049).
const idleSweep = time.Minute

// Runner supervises every job on the machine.
type Runner struct {
	store   *Store
	planner Planner
	actor   Actor
	now     func() time.Time
	timer   func(d time.Duration) (<-chan time.Time, func())
	step    time.Duration
	sweep   time.Duration
	log     *slog.Logger

	rearm chan struct{}
	group quiesce.Group

	mu sync.Mutex
	// running is the set of job ids a goroutine currently holds, so the
	// supervisor does not start a second runner for a job whose first is still
	// between its store write and its next read.
	running map[string]bool
	// stops carries a cancel for each running job, so "stop" reaches the step
	// in flight rather than waiting for it.
	stops     map[string]context.CancelFunc
	base      context.Context
	cancelGen context.CancelFunc
	closed    bool
}

// NewRunner builds the supervisor. It starts nothing; Start does.
func NewRunner(opts RunnerOptions, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &Runner{
		store: opts.Store, planner: opts.Planner, actor: opts.Actor,
		now: opts.Now, timer: opts.Timer, step: opts.StepTimeout,
		sweep: opts.IdleSweep, log: log,
		rearm:   make(chan struct{}, 1),
		running: make(map[string]bool),
		stops:   make(map[string]context.CancelFunc),
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.timer == nil {
		r.timer = realTimer
	}
	if r.step <= 0 {
		r.step = DefaultStepTimeout
	}
	if r.sweep <= 0 {
		r.sweep = idleSweep
	}
	return r
}

// realTimer is the production clock seam.
func realTimer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// Bind late-binds the planner and the actor, for the daemon's construction
// order: both need services that only exist once the daemon does, and the
// runner has to exist before them. Called once during construction,
// single-threaded, exactly like the situation service's BindSources.
func (r *Runner) Bind(planner Planner, actor Actor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.planner, r.actor = planner, actor
}

// Start begins the supervisor. ctx is its lifetime: cancelling it reaches the
// loop and every step in flight, which is what makes Drain's deadline
// effective.
func (r *Runner) Start(ctx context.Context) {
	r.mu.Lock()
	if r.closed || r.base != nil {
		r.mu.Unlock()
		return
	}
	r.base = ctx
	loopCtx, cancel := context.WithCancel(ctx)
	r.cancelGen = cancel
	// Add before go, never inside the goroutine: a drain that started between
	// the two would otherwise return while the loop was starting.
	r.group.Go(func() { r.run(loopCtx) })
	r.mu.Unlock()
}

// Drain stops the supervisor and waits — bounded by ctx — for the loop and
// every step in flight, so daemon shutdown treats jobs as one more drained
// stage.
func (r *Runner) Drain(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	if r.cancelGen != nil {
		r.cancelGen()
		r.cancelGen = nil
	}
	r.mu.Unlock()
	return r.group.Wait(ctx)
}

// InFlight reports how many runner goroutines are still going, for the
// shutdown log when a drain gives up.
func (r *Runner) InFlight() int { return r.group.InFlight() }

// Rearm wakes the supervisor to look for work. Safe from any goroutine; a wake
// already pending is enough, so this never blocks.
func (r *Runner) Rearm() {
	select {
	case r.rearm <- struct{}{}:
	default:
	}
}

// run is the supervisor: dispatch whatever is ready, wait for a wake or the
// sweep, repeat. It is always armed — there is no branch in which it stops
// selecting on the timer (ADR 0049).
func (r *Runner) run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		r.dispatchReady(ctx)
		fire, stop := r.timer(r.sweep)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-r.rearm:
			stop()
		case <-fire:
		}
	}
}

// dispatchReady starts a goroutine for every job that is ready and does not
// already have one.
//
// The claim is the store write, not this map. The map only stops the supervisor
// from dispatching twice between a goroutine's last write and its next read;
// the thing that makes "one runner per job" true is that a runner claims a job
// by moving it Ready → Running inside Store.Update, and Update runs under the
// store's lock. A second claimant sees a job that is no longer Ready and gives
// it back.
func (r *Runner) dispatchReady(ctx context.Context) {
	for _, j := range r.store.List() {
		if j.State != Ready {
			continue
		}
		r.mu.Lock()
		if r.running[j.ID] || r.closed {
			r.mu.Unlock()
			continue
		}
		stepCtx, cancel := context.WithCancel(ctx)
		r.running[j.ID] = true
		r.stops[j.ID] = cancel
		r.mu.Unlock()
		id := j.ID
		r.group.Go(func() {
			defer func() {
				r.mu.Lock()
				delete(r.running, id)
				delete(r.stops, id)
				r.mu.Unlock()
				cancel()
			}()
			r.work(stepCtx, id)
		})
	}
}

// Stop interrupts a job: the step in flight is cancelled, and the job records
// what it had done and what it had not.
//
// It is prompt by construction. The cancel reaches the step's context, so a
// planner call or a tool execution returns rather than being waited out, and
// the state is written by whichever side gets there first — the runner
// noticing its context is done, or this call. Both write the same closing
// sentence through the same path, so they cannot disagree.
func (r *Runner) Stop(ref, why string) (Job, error) {
	job, err := r.store.Find(ref)
	if err != nil {
		return Job{}, err
	}
	if !job.State.Live() {
		return job, fmt.Errorf("%s has already %s", job.Name, endedWord(job.State))
	}
	r.mu.Lock()
	cancel := r.stops[job.ID]
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	out, err := r.store.Update(job.ID, func(j *Job) bool {
		if !j.State.Live() {
			return false
		}
		j.State, j.Question = Stopped, Question{}
		j.Closing = strings.TrimSpace(why)
		if j.Closing == "" {
			j.Closing = "You stopped it."
		}
		return true
	})
	if err != nil {
		return Job{}, err
	}
	r.Rearm()
	return out, nil
}

// endedWord words a finished state for a refusal sentence.
func endedWord(s State) string {
	switch s {
	case Done:
		return "finished"
	case Stopped:
		return "been stopped"
	default:
		return "ended"
	}
}

// Answer settles what a parked job was waiting for and puts it back to work.
//
// Approving resumes from the checkpoint rather than restarting: the step the
// job parked on is kept whole on the question, and the resumed runner executes
// THAT step with the approval attached — not a step the planner proposes afresh
// now. The distinction is the whole of "resumes where it stopped": a planner
// asked the same question twice may answer differently, and the user approved
// the action they were shown, not a fresh one.
//
// Declining stops the job. It does not carry on and look for another way round,
// which would be a job inventing a plan the user has just refused — precisely
// the unbounded autonomy the scope exists to prevent.
func (r *Runner) Answer(ref string, approved bool, said string) (Job, error) {
	job, err := r.store.Find(ref)
	if err != nil {
		return Job{}, err
	}
	if job.State != Parked {
		return job, fmt.Errorf("%s is not waiting on anything", job.Name)
	}
	if !job.Question.Why.Answerable() {
		return job, fmt.Errorf("%s stopped because %s, which isn't something I can carry on from",
			job.Name, strings.TrimSuffix(job.Question.Ask, "."))
	}
	out, err := r.store.Update(job.ID, func(j *Job) bool {
		if j.State != Parked {
			return false
		}
		if !approved {
			j.State, j.Question = Stopped, Question{}
			j.Closing = "You said no to " + questionSubject(job.Question) + ", so I stopped there."
			return true
		}
		j.State = Ready
		j.Question.Ask = strings.TrimSpace(said)
		return true
	})
	if err != nil {
		return Job{}, err
	}
	r.Rearm()
	return out, nil
}

// questionSubject names what was asked about, for the declining sentence.
func questionSubject(q Question) string {
	if q.Step.Intent != "" {
		return q.Step.Intent
	}
	if q.Step.Tool != "" {
		return q.Step.Tool
	}
	return "that"
}

// work runs one job until it parks, finishes, or is stopped. One goroutine per
// job, so one job parking cannot stall another: there is no shared thread and
// no shared lock held across a step.
func (r *Runner) work(ctx context.Context, id string) {
	for {
		if ctx.Err() != nil {
			return
		}
		carryOn, err := r.once(ctx, id)
		if err != nil {
			r.log.Warn("a job could not be written and has been left alone",
				"component", "jobs", "job", id, "error", err.Error())
			return
		}
		if !carryOn {
			return
		}
	}
}

// once takes one job one step forward and reports whether there is more to do.
//
// The order below is the enforcement contract, and every line of it is
// load-bearing:
//
//  1. **Claim.** Ready → Running, under the store's lock. A job nobody claimed
//     is not acted on.
//  2. **Plan.** The planner proposes, bounded by the step timeout.
//  3. **Read the subject.** The daemon says what the call would touch. It
//     cannot say → park.
//  4. **Judge the scope.** Outside → the job STOPS and parks with the reason,
//     and nothing whatever has been dispatched.
//  5. **Judge the gate.** Deny → park. Ask → park ON THE QUESTION, keeping the
//     step, so approving later resumes exactly this action.
//  6. **Do it**, and only now.
//  7. **Checkpoint.** The ledger entry and the state go to disk before the
//     loop comes round, so the step that just happened survives whatever
//     happens next.
func (r *Runner) once(ctx context.Context, id string) (bool, error) {
	claimed, err := r.store.Update(id, func(j *Job) bool {
		if j.State != Ready {
			return false
		}
		if j.Bounded() {
			j.State = Parked
			j.Question = Question{Why: WhyStuck, At: r.now().UTC(),
				Ask: fmt.Sprintf("I have taken %d steps on this and it is not finished. "+
					"I have stopped rather than keep going; the steps I took are in the record.",
					j.Steps)}
			return true
		}
		j.State = Running
		return true
	})
	if err != nil {
		return false, err
	}
	if claimed.State != Running {
		// Somebody else claimed it, it was stopped, or it hit its bound.
		return false, nil
	}

	planner, actor := r.seams()
	if planner == nil || actor == nil {
		_, err := r.park(id, WhyStuck,
			"I can't work on this: the part of me that carries jobs out isn't running on this daemon.", Step{})
		return false, err
	}

	stepCtx, cancel := context.WithTimeout(ctx, r.step)
	defer cancel()

	answer := strings.TrimSpace(claimed.Question.Ask)
	pending := claimed.Question.Step
	resuming := pending.Tool != "" && claimed.Question.Why == WhyApproval

	var step Step
	if resuming {
		// Resumption: the approved step is run as it was shown, not re-planned.
		step = pending
	} else {
		step, err = planner.Next(stepCtx, View{
			Goal: claimed.Goal, Scope: claimed.Scope,
			Ledger: claimed.Ledger, Left: MaxSteps - claimed.Steps, Answer: answer,
		})
		if err != nil {
			if ctx.Err() != nil {
				return false, nil // stopped underneath us; Stop owns the state
			}
			_, perr := r.park(id, WhyStuck,
				"I couldn't work out what to do next: "+err.Error(), Step{})
			return false, perr
		}
	}

	switch {
	case step.Finished:
		_, ferr := r.store.Update(id, func(j *Job) bool {
			if j.State != Running {
				return false
			}
			j.State, j.Question = Done, Question{}
			j.Closing = strings.TrimSpace(step.Intent)
			return true
		})
		return false, ferr
	case strings.TrimSpace(step.Question) != "":
		_, perr := r.park(id, WhyDecision, strings.TrimSpace(step.Question), step)
		return false, perr
	case strings.TrimSpace(step.Tool) == "":
		_, perr := r.park(id, WhyStuck,
			"I couldn't work out what to do next: nothing was proposed.", Step{})
		return false, perr
	}

	// (3) What would this actually touch? Read by the daemon, from the parsed
	// call — never from the model's account of it.
	attempt, err := actor.Subject(stepCtx, step)
	if err != nil {
		_, perr := r.park(id, WhyUnclear,
			"I stopped because I couldn't tell what "+step.Tool+" would have touched, "+
				"and I will not act inside a boundary I cannot check: "+err.Error(), step)
		return false, perr
	}

	// (4) The boundary. This is the enforcement, and it is here — in Go, on a
	// subject the daemon read — rather than in a sentence asking the model to
	// behave.
	if ruling := claimed.Scope.Judge(attempt); !ruling.OK {
		_, perr := r.park(id, WhyOutOfScope,
			"I stopped without doing it: "+ruling.Because+".", step)
		return false, perr
	}

	// (5) The gate's floor, which a job does not lift however long it has run.
	if !resuming {
		switch verdict := actor.Judge(stepCtx, step); verdict.Decision {
		case Deny:
			_, perr := r.park(id, WhyRefused,
				"I stopped without doing it: "+verdict.Reason+".", step)
			return false, perr
		case Ask:
			ask := strings.TrimSpace(verdict.Question)
			if ask == "" {
				ask = "Shall I go ahead with " + step.Tool + "?"
			}
			_, perr := r.park(id, WhyApproval, ask, step)
			return false, perr
		}
	}

	// (6) Only now — and the dispatch is written down BEFORE it happens.
	//
	// That order is the whole of the daemon's honesty about its own death. A
	// process that goes away between this write and the next one leaves a job
	// with a step in flight, which the store adopts into the ledger as
	// unverified. Writing after the action instead would leave the same crash
	// looking like a step that never started.
	if _, err := r.store.Update(id, func(j *Job) bool {
		if j.State != Running {
			return false
		}
		j.InFlight = step
		return true
	}); err != nil {
		return false, err
	}
	result, err := actor.Do(stepCtx, id, step)
	entry := Entry{At: r.now().UTC(), Intent: step.Intent, Tool: step.Tool}
	switch {
	case err != nil && ctx.Err() != nil:
		// Stopped mid-flight. The action was dispatched and its end was never
		// seen, which is exactly what "unverified" means. Recorded as such
		// rather than as failed: "it did not happen" is a claim, and we have
		// no basis for it.
		entry.Said = "I was stopped before I saw how this ended."
	case err != nil:
		entry.Said = "I could not run this: " + err.Error()
		entry.Verified, entry.Failed = true, true
	default:
		entry.Said, entry.Verified, entry.Failed = trimSaid(result.Said), true, result.Failed
		entry.Undo = result.Undo
	}

	// (7) The checkpoint.
	saved, err := r.store.Update(id, func(j *Job) bool {
		j.Ledger = append(j.Ledger, entry)
		j.Steps++
		j.InFlight = Step{}
		if j.State != Running {
			// Stopped while the step was running. The ledger entry still goes
			// in — it happened — but the state is not ours to set.
			return true
		}
		j.State, j.Question = Ready, Question{}
		return true
	})
	if err != nil {
		return false, err
	}
	return saved.State == Ready && ctx.Err() == nil, nil
}

// seams reads the late-bound planner and actor.
func (r *Runner) seams() (Planner, Actor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.planner, r.actor
}

// park puts a job into its waiting state with the question or the reason. It
// is one function so that every way a job stops writes the same shape, and so
// that the step it stopped on is always kept — which is what makes resumption
// a checkpoint rather than a restart.
func (r *Runner) park(id string, why Why, ask string, step Step) (Job, error) {
	return r.store.Update(id, func(j *Job) bool {
		if j.State != Running {
			return false
		}
		j.State = Parked
		j.Question = Question{Why: why, Ask: ask, At: r.now().UTC(), Step: step}
		return true
	})
}

// MaxSaid bounds one ledger line's record of what a tool said.
//
// A ledger is read back to a person, and a report built from four hundred lines
// of a directory listing is not a report. It is also a file: a job that ran a
// command with a large output would otherwise write that output to disk on
// every step. The trim is disclosed in the line itself, because a truncated
// fact that does not say it is truncated reads as a complete one.
const MaxSaid = 400

// trimSaid shortens a tool's result for the ledger, saying so when it does.
func trimSaid(said string) string {
	said = strings.TrimSpace(said)
	if len(said) <= MaxSaid {
		return said
	}
	return strings.TrimSpace(said[:MaxSaid]) + "… (I have only kept the first part of this)"
}

// ErrNoPlanner is what a runner with no model behind it reports, kept as a
// sentinel so a caller can tell "nothing is configured" from "the plan failed".
var ErrNoPlanner = errors.New("no planner is bound to this runner")
