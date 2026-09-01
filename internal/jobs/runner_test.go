package jobs

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The runner's tests. Every one of them is hermetic: a temp directory, an
// injected clock, a scripted planner and a recording actor. Nothing here calls
// a model, sleeps, or touches a real machine — which is what lets the
// enforcement claims be assertions rather than hopes.

// scriptedPlanner hands back a fixed sequence of steps and records what it was
// shown. It never blocks unless a test asks it to.
type scriptedPlanner struct {
	mu    sync.Mutex
	steps []Step
	err   error
	views []View
	calls int
	hold  chan struct{}
}

func (p *scriptedPlanner) Next(ctx context.Context, v View) (Step, error) {
	p.mu.Lock()
	p.views = append(p.views, v)
	p.calls++
	i := p.calls - 1
	steps, err, hold := p.steps, p.err, p.hold
	p.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return Step{}, ctx.Err()
		}
	}
	if err != nil {
		return Step{}, err
	}
	if i >= len(steps) {
		return Step{Finished: true, Intent: "there was nothing left to do"}, nil
	}
	return steps[i], nil
}

func (p *scriptedPlanner) seen() []View {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]View(nil), p.views...)
}

// recordingActor answers Subject and Judge from a script and records every Do.
type recordingActor struct {
	mu       sync.Mutex
	subject  func(Step) (Attempt, error)
	verdict  func(Step) Verdict
	result   func(Step) (Result, error)
	dispatch []Step
	// entered is closed on the first Do, and blocking waits on release, so a
	// test can stop a job with a step genuinely in flight.
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *recordingActor) Subject(_ context.Context, s Step) (Attempt, error) {
	if a.subject != nil {
		return a.subject(s)
	}
	return Attempt{Tool: s.Tool}, nil
}

func (a *recordingActor) Judge(_ context.Context, s Step) Verdict {
	if a.verdict != nil {
		return a.verdict(s)
	}
	return Verdict{Decision: Allow}
}

func (a *recordingActor) Do(ctx context.Context, _ string, s Step) (Result, error) {
	a.mu.Lock()
	a.dispatch = append(a.dispatch, s)
	a.mu.Unlock()
	if a.entered != nil {
		a.once.Do(func() { close(a.entered) })
	}
	if a.release != nil {
		select {
		case <-a.release:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	if a.result != nil {
		return a.result(s)
	}
	return Result{Said: "done"}, nil
}

func (a *recordingActor) did() []Step {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Step(nil), a.dispatch...)
}

// rig is one runner over one store, wired to a scripted planner and a
// recording actor.
type rig struct {
	store   *Store
	runner  *Runner
	planner *scriptedPlanner
	actor   *recordingActor
	path    string
	scope   Scope
}

func newRig(t *testing.T, steps ...Step) *rig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.toml")
	store := NewStore(path, StoreOptions{Now: newClock().now}, nil)
	planner := &scriptedPlanner{steps: steps}
	actor := &recordingActor{}
	runner := NewRunner(RunnerOptions{
		Store: store, Planner: planner, Actor: actor,
		Now:   newClock().now,
		Timer: func(time.Duration) (<-chan time.Time, func()) { return nil, func() {} },
	}, nil)
	return &rig{store: store, runner: runner, planner: planner, actor: actor, path: path,
		scope: Scope{Tools: []string{"memory.search", "memory.remember"}, Roots: []string{t.TempDir()}}}
}

// start creates a job on the rig's scope.
func (r *rig) start(t *testing.T) Job {
	t.Helper()
	job, err := r.store.Start("tidy", "tidy up my downloads", r.scope)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// work runs the job to a stop, on the caller's goroutine, so every assertion
// after it is a statement about work that has finished rather than a race.
func (r *rig) work(t *testing.T, id string) Job {
	t.Helper()
	r.runner.work(context.Background(), id)
	job, err := r.store.Find(id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func step(tool, intent string) Step {
	return Step{Tool: tool, Intent: intent, Args: `{}`}
}

// TestAJobWorksInsideItsScopeWithoutAsking is the ordinary case: everything in
// bounds and allow-tier runs, and the job finishes.
func TestAJobWorksInsideItsScopeWithoutAsking(t *testing.T) {
	r := newRig(t, step("memory.search", "looked for the folder"),
		step("memory.remember", "wrote down what I found"))
	job := r.work(t, r.start(t).ID)
	if job.State != Done {
		t.Fatalf("state = %q, want %q", job.State, Done)
	}
	if len(r.actor.did()) != 2 {
		t.Errorf("dispatched %d steps, want 2", len(r.actor.did()))
	}
	if len(job.Ledger) != 2 {
		t.Fatalf("ledger = %+v, want two entries", job.Ledger)
	}
	for _, e := range job.Ledger {
		if !e.Verified {
			t.Errorf("step %q came back unverified: %+v", e.Tool, e)
		}
	}
}

// TestAnAttemptOutsideTheScopeStopsTheJobAndNothingHappens is the acceptance
// criterion in one test: the out-of-scope attempt is driven, the job parks with
// the reason, and NOTHING was dispatched.
func TestAnAttemptOutsideTheScopeStopsTheJobAndNothingHappens(t *testing.T) {
	r := newRig(t, step("memory.search", "looked around"),
		step("memory.remember", "wrote to somebody else's file"))
	outside := "/etc/passwd"
	r.actor.subject = func(s Step) (Attempt, error) {
		if s.Tool == "memory.remember" {
			return Attempt{Tool: s.Tool, Paths: []string{outside}}, nil
		}
		return Attempt{Tool: s.Tool}, nil
	}

	job := r.work(t, r.start(t).ID)

	if job.State != Parked {
		t.Fatalf("state = %q, want %q: an attempt outside the scope stops the job", job.State, Parked)
	}
	if job.Question.Why != WhyOutOfScope {
		t.Errorf("why = %q, want %q", job.Question.Why, WhyOutOfScope)
	}
	if !strings.Contains(job.Question.Ask, outside) {
		t.Errorf("reason = %q, want it to name what it would have touched", job.Question.Ask)
	}
	// The whole point: nothing happened.
	for _, s := range r.actor.did() {
		if s.Tool == "memory.remember" {
			t.Fatal("the out-of-scope step was dispatched; the boundary is decoration")
		}
	}
	if len(job.Ledger) != 1 {
		t.Errorf("ledger = %+v, want only the one step that was inside the scope", job.Ledger)
	}
	// And it cannot be waved through by saying yes.
	if _, err := r.runner.Answer("tidy", true, ""); err == nil {
		t.Error("a boundary was answerable; saying yes to one is not a decision the user gets to make")
	}
}

// TestASubjectTheDaemonCannotReadStopsTheJob is refuse-rather-than-guess: a
// subject nobody can name cannot be checked against a boundary.
func TestASubjectTheDaemonCannotReadStopsTheJob(t *testing.T) {
	r := newRig(t, step("memory.remember", "did something"))
	r.actor.subject = func(Step) (Attempt, error) {
		return Attempt{}, fmt.Errorf("a command's files cannot be read out of its text")
	}
	job := r.work(t, r.start(t).ID)
	if job.State != Parked || job.Question.Why != WhyUnclear {
		t.Fatalf("state = %q why = %q, want parked and unclear", job.State, job.Question.Why)
	}
	if len(r.actor.did()) != 0 {
		t.Fatal("a step whose subject could not be read was dispatched anyway")
	}
}

// TestTheGateStillAsksInsideAJobAndTheJobParksOnTheQuestion is the floor: an
// irreversible action inside the scope still stops, and it parks rather than
// blocking, because nobody may be present.
func TestTheGateStillAsksInsideAJobAndTheJobParksOnTheQuestion(t *testing.T) {
	r := newRig(t, step("memory.remember", "delete the old files"))
	r.actor.verdict = func(s Step) Verdict {
		return Verdict{Decision: Ask, Question: "Shall I delete them? This can't be undone."}
	}
	job := r.work(t, r.start(t).ID)

	if job.State != Parked {
		t.Fatalf("state = %q, want %q: a job parks on the gate, it does not block a session", job.State, Parked)
	}
	if job.Question.Why != WhyApproval {
		t.Errorf("why = %q, want %q", job.Question.Why, WhyApproval)
	}
	if !strings.Contains(job.Question.Ask, "can't be undone") {
		t.Errorf("question = %q, want the gate's own words", job.Question.Ask)
	}
	if job.Question.Step.Tool != "memory.remember" {
		t.Errorf("pending step = %+v, want the step it stopped on kept whole", job.Question.Step)
	}
	if len(r.actor.did()) != 0 {
		t.Fatal("the gated step ran before it was approved")
	}
}

// TestApprovingLaterResumesTheStepItStoppedOn is the difference between a
// checkpoint and a restart: the approved action is the one the user was shown,
// not one the planner proposes afresh.
func TestApprovingLaterResumesTheStepItStoppedOn(t *testing.T) {
	r := newRig(t, step("memory.remember", "delete the old files"),
		step("memory.search", "checked it worked"))
	asked := 0
	r.actor.verdict = func(Step) Verdict {
		asked++
		if asked == 1 {
			return Verdict{Decision: Ask, Question: "Shall I?"}
		}
		return Verdict{Decision: Allow}
	}
	job := r.start(t)
	r.work(t, job.ID)

	if _, err := r.runner.Answer("tidy", true, "go on then"); err != nil {
		t.Fatal(err)
	}
	after := r.work(t, job.ID)

	did := r.actor.did()
	if len(did) == 0 || did[0].Intent != "delete the old files" {
		t.Fatalf("dispatched %+v, want the approved step first", did)
	}
	// Three plans, not four: one that proposed the gated step, one that
	// proposed what came after it, and one that declared the job finished. The
	// approved step itself consumed no plan at all, which is the difference
	// between resuming from a checkpoint and starting again — a planner asked
	// the same question twice may answer differently, and the user approved the
	// action they were shown, not a fresh one.
	if got := len(r.planner.seen()); got != 3 {
		t.Errorf("the planner was consulted %d times, want 3 — the approved step must not be re-planned", got)
	}
	if after.State != Done {
		t.Errorf("state = %q, want the job to have carried on to the end", after.State)
	}
}

// The three tier transitions across a park (#225).
//
// All three set the same job parked on the same approval and then change only
// one thing — what the gate says when the job resumes. The resumption itself is
// driven the way the jobs file documents it: setting a parked job's state back
// to "ready" by hand is how a person says "carry on" with a text editor, and it
// is also the shape of every resumption whose tier moved after the approval was
// given. It reaches Runner.once with the question still on disk, which is
// exactly the path the gate has to hold, and it keeps these three about the
// runner's floor rather than about the offer that guards the button (which
// TestApproveIsWithheldOnceTheJobsToolIsDenied covers).
//
// Nothing here sleeps, waits or races: work runs on the caller's goroutine and
// every assertion is about work that has already finished.

// parkedOnApproval runs a job until the gate parks it on a question, and
// returns it. It fails the test unless that is what happened, so the tests
// below start from an established fact rather than an assumption.
func (r *rig) parkedOnApproval(t *testing.T, id string) Job {
	t.Helper()
	job := r.work(t, id)
	if job.State != Parked || job.Question.Why != WhyApproval {
		t.Fatalf("state = %q why = %q, want the job parked on the gate's question",
			job.State, job.Question.Why)
	}
	if job.Question.Step.Tool == "" {
		t.Fatal("the job parked without keeping the step, so there is nothing to resume")
	}
	return job
}

// resume puts a parked job back to work without answering it, which is the
// jobs file's own hand-edit ("carry on") and the state a just-approved job is
// left in. The question and its kept step stay on disk, so the runner takes the
// resumption path.
func (r *rig) resume(t *testing.T, id string) {
	t.Helper()
	if _, err := r.store.Update(id, func(j *Job) bool {
		if j.State != Parked {
			return false
		}
		j.State = Ready
		return true
	}); err != nil {
		t.Fatal(err)
	}
}

// TestATierChangedToDenyWhileParkedIsHonouredOnResumption is the whole of #225.
// A tier is the user's standing instruction about a capability, and an approval
// given before a denial does not reach back past it.
func TestATierChangedToDenyWhileParkedIsHonouredOnResumption(t *testing.T) {
	r := newRig(t, step("memory.remember", "delete the old files"))
	r.actor.verdict = func(Step) Verdict {
		return Verdict{Decision: Ask, Question: "Shall I delete them?"}
	}
	job := r.start(t)
	parked := r.parkedOnApproval(t, job.ID)

	// The user turns the tool off while the job waits, and only then does the
	// job carry on.
	r.actor.verdict = func(Step) Verdict {
		return Verdict{Decision: Deny, Reason: "I'm not allowed to use memory.remember (you turned it off)"}
	}
	r.resume(t, job.ID)
	after := r.work(t, job.ID)

	// The claim is about the actor, not about the absence of an effect: the
	// recording actor's Do is the one verb that acts, and it was never reached.
	if did := r.actor.did(); len(did) != 0 {
		t.Fatalf("the actor was called with %+v; a tier set to deny while the job "+
			"was parked did not survive the resumption", did)
	}
	if after.State != Parked || after.Question.Why != WhyRefused {
		t.Fatalf("state = %q why = %q, want parked and refused", after.State, after.Question.Why)
	}
	// The reason a first-pass denial parks with, word for word, so the account
	// reads the same either way.
	want := "I stopped without doing it: I'm not allowed to use memory.remember (you turned it off)."
	if after.Question.Ask != want {
		t.Errorf("the park says %q, want the denial's own wording %q", after.Question.Ask, want)
	}
	// AC 5: nothing went into the ledger as done, and the report cannot claim it.
	if len(after.Ledger) != 0 {
		t.Errorf("ledger = %+v, want nothing: the step never ran", after.Ledger)
	}
	if report := after.Report(); strings.Contains(report, "delete the old files") {
		t.Errorf("report = %q, want it not to claim the denied step happened", report)
	}
	// And the job can still say where it stands, which is the reliability half.
	if spoken := after.Spoken(); !strings.Contains(spoken, "you turned it off") {
		t.Errorf("spoken = %q, want the refusal and its reason in the job's account", spoken)
	}
	// The pending step is kept, so the record of what was refused survives.
	if after.Question.Step.Intent != parked.Question.Step.Intent {
		t.Errorf("the refused step was not kept: %+v", after.Question.Step)
	}
}

// TestATierStillAskingIsNotAskedAgainOnResumption is the guarantee the fix must
// not break: the user has just answered this question, so the gate's Ask is
// spent. Re-asking would park the job on the question it was unparked from,
// forever.
func TestATierStillAskingIsNotAskedAgainOnResumption(t *testing.T) {
	r := newRig(t, step("memory.remember", "delete the old files"))
	r.actor.verdict = func(Step) Verdict {
		return Verdict{Decision: Ask, Question: "Shall I delete them?"}
	}
	job := r.start(t)
	parked := r.parkedOnApproval(t, job.ID)
	plansBefore := len(r.planner.seen())

	// The tier is unchanged — the gate would ask again if anybody asked it.
	r.resume(t, job.ID)
	after := r.work(t, job.ID)

	did := r.actor.did()
	if len(did) == 0 {
		t.Fatal("the approved step never ran; an answered question must not be asked again")
	}
	// Byte-identical to what the user was shown: not re-planned, not rebuilt.
	if did[0] != parked.Question.Step {
		t.Errorf("dispatched %+v, want the kept step %+v unchanged", did[0], parked.Question.Step)
	}
	if after.State == Parked && after.Question.Why == WhyApproval {
		t.Fatal("the job parked on the same question again; approving it could never finish")
	}
	// The approved step consumed no plan at all — the checkpoint, not a restart.
	// One further plan, and it is the one that declared the job finished; a
	// second would be the resumed step being proposed afresh, and a planner
	// asked the same question twice may answer differently.
	if plans := len(r.planner.seen()); plans != plansBefore+1 {
		t.Errorf("the planner was consulted %d times across the resumption, want %d — "+
			"the approved step must not be re-planned", plans-plansBefore, 1)
	}
	if len(after.Ledger) == 0 || after.Ledger[0].Intent != "delete the old files" {
		t.Errorf("ledger = %+v, want the approved step recorded as it was approved", after.Ledger)
	}
}

// TestATierWidenedToAllowRunsWithoutAFurtherQuestion. Widening is the easy
// direction and it still has to be checked: the resumption must not invent a
// question the gate no longer asks.
func TestATierWidenedToAllowRunsWithoutAFurtherQuestion(t *testing.T) {
	r := newRig(t, step("memory.remember", "delete the old files"))
	r.actor.verdict = func(Step) Verdict {
		return Verdict{Decision: Ask, Question: "Shall I delete them?"}
	}
	job := r.start(t)
	parked := r.parkedOnApproval(t, job.ID)

	r.actor.verdict = func(Step) Verdict { return Verdict{Decision: Allow} }
	r.resume(t, job.ID)
	after := r.work(t, job.ID)

	did := r.actor.did()
	if len(did) == 0 || did[0] != parked.Question.Step {
		t.Fatalf("dispatched %+v, want the kept step %+v", did, parked.Question.Step)
	}
	if after.State == Parked {
		t.Errorf("state = %q why = %q, want the job to have carried on without another question",
			after.State, after.Question.Why)
	}
}

// TestApproveIsWithheldOnceTheJobsToolIsDenied is the offer half (#210's rule,
// AC 4): the row stops offering a yes the runner would refuse, and the verb
// declines in that same sentence.
//
// It withholds rather than settles, so a user who turns the tool back on gets
// the control back. Spending the job on a tier that lasted an afternoon would
// be a worse answer than the one the button gives.
func TestApproveIsWithheldOnceTheJobsToolIsDenied(t *testing.T) {
	r := newRig(t, step("memory.remember", "delete the old files"))
	r.actor.verdict = func(Step) Verdict {
		return Verdict{Decision: Ask, Question: "Shall I delete them?"}
	}
	job := r.start(t)
	parked := r.parkedOnApproval(t, job.ID)

	// Still asking: the yes stands, and the plain answer offer never wavered.
	if ok, because := parked.ApproveOffer(r.runner.gate()); !ok {
		t.Fatalf("a job parked on a question the tier still asks was refused its yes: %q", because)
	}

	r.actor.verdict = func(Step) Verdict {
		return Verdict{Decision: Deny, Reason: "I'm not allowed to use memory.remember (you turned it off)"}
	}
	ok, because := parked.ApproveOffer(r.runner.gate())
	if ok {
		t.Fatal("Approve is still offered for a tool the user has turned off")
	}
	if !strings.Contains(because, "you turned it off") ||
		!strings.Contains(because, "saying yes can't carry it on") {
		t.Errorf("the refusal = %q, want it to name the tier and say why a yes will not do", because)
	}

	// The pairing itself: the verb refuses in the offer's own words.
	if _, err := r.runner.Answer("tidy", true, ""); err == nil {
		t.Fatal("the runner accepted an approval the listing had withheld")
	} else if err.Error() != because {
		t.Errorf("the runner refuses with %q and the listing says %q", err, because)
	}
	if did := r.actor.did(); len(did) != 0 {
		t.Fatalf("the actor was called with %+v after a withheld approval", did)
	}

	// Saying no is not a use of the denied tool, so it still works — and it is
	// the thing a user who has just turned the tool off most likely wants.
	after, err := r.runner.Answer("tidy", false, "")
	if err != nil {
		t.Fatalf("a job could not be stopped by saying no to it: %v", err)
	}
	if after.State != Stopped {
		t.Errorf("state = %q, want %q", after.State, Stopped)
	}
}

// TestDecliningStopsTheJobRatherThanLookingForAnotherWayRound: a job that
// carried on after a refusal would be inventing a plan the user has just
// refused.
func TestDecliningStopsTheJobRatherThanLookingForAnotherWayRound(t *testing.T) {
	r := newRig(t, step("memory.remember", "delete the old files"))
	r.actor.verdict = func(Step) Verdict { return Verdict{Decision: Ask, Question: "Shall I?"} }
	job := r.start(t)
	r.work(t, job.ID)

	after, err := r.runner.Answer("tidy", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if after.State != Stopped {
		t.Fatalf("state = %q, want %q", after.State, Stopped)
	}
	if !strings.Contains(after.Closing, "delete the old files") {
		t.Errorf("closing = %q, want it to name what was refused", after.Closing)
	}
	if len(r.actor.did()) != 0 {
		t.Fatal("something ran after the user said no")
	}
}

// TestADeniedToolStopsTheJobAndCannotBeAnsweredIntoRunning: a denied tool is
// denied by standing configuration, and a job is not a way to ask again.
func TestADeniedToolStopsTheJobAndCannotBeAnsweredIntoRunning(t *testing.T) {
	r := newRig(t, step("memory.remember", "wrote something"))
	r.actor.verdict = func(Step) Verdict {
		return Verdict{Decision: Deny, Reason: "your policy denies it"}
	}
	job := r.work(t, r.start(t).ID)
	if job.State != Parked || job.Question.Why != WhyRefused {
		t.Fatalf("state = %q why = %q, want parked and refused", job.State, job.Question.Why)
	}
	if len(r.actor.did()) != 0 {
		t.Fatal("a denied step ran")
	}
	if _, err := r.runner.Answer("tidy", true, ""); err == nil {
		t.Error("a denial was answerable")
	}
}

// TestAJobParksOnADecisionItCannotMake and resumes from where it stopped.
func TestAJobParksOnADecisionItCannotMake(t *testing.T) {
	r := newRig(t,
		Step{Question: "Which folder did you mean, Downloads or Desktop?", Intent: "I need to know which"},
		step("memory.search", "looked in the folder you named"))
	job := r.start(t)
	r.work(t, job.ID)

	parked, _ := r.store.Find(job.ID)
	if parked.State != Parked || parked.Question.Why != WhyDecision {
		t.Fatalf("state = %q why = %q, want parked on a decision", parked.State, parked.Question.Why)
	}
	if !strings.Contains(parked.Question.Ask, "Downloads or Desktop") {
		t.Errorf("question = %q, want the planner's own words", parked.Question.Ask)
	}
	if _, err := r.runner.Answer("tidy", true, "Downloads"); err != nil {
		t.Fatal(err)
	}
	after := r.work(t, job.ID)
	if after.State != Done {
		t.Errorf("state = %q, want the answer to have let it carry on", after.State)
	}
	views := r.planner.seen()
	found := false
	for _, v := range views {
		if v.Answer == "Downloads" {
			found = true
		}
	}
	if !found {
		t.Error("the user's answer never reached the plan, so the job resumed knowing nothing new")
	}
}

// TestStopIsPromptAndTheJobSaysWhatItCouldNotConfirm drives a stop with a step
// genuinely in flight. The handshake is the actor's own entered channel, so
// "the step had started" is a fact rather than a timing guess.
func TestStopIsPromptAndTheJobSaysWhatItCouldNotConfirm(t *testing.T) {
	r := newRig(t, step("memory.remember", "the long one"))
	r.actor.entered = make(chan struct{})
	r.actor.release = make(chan struct{})

	job := r.start(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The supervisor is what owns the per-job cancel, so the step runs under
	// its dispatch rather than a bare goroutine.
	r.runner.Start(ctx)
	t.Cleanup(func() {
		drainCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := r.runner.Drain(drainCtx); err != nil {
			t.Errorf("the runner did not drain: %v", err)
		}
	})
	r.runner.Rearm()
	<-r.actor.entered

	stopped, err := r.runner.Stop("tidy", "You asked me to stop.")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != Stopped {
		t.Fatalf("state = %q, want %q", stopped.State, Stopped)
	}

	// The runner's own write lands after Do returns; wait for the ledger line
	// rather than for a clock.
	final := waitForJob(t, r.store, job.ID, func(j Job) bool { return len(j.Ledger) == 1 })
	if final.Ledger[0].Verified {
		t.Error("a step cut off mid-flight was recorded as verified")
	}
	if !strings.Contains(final.Ledger[0].Said, "before I saw how this ended") {
		t.Errorf("ledger = %+v, want it to say the outcome is unknown", final.Ledger[0])
	}
	report := final.Report()
	if !strings.Contains(report, "never saw the end of") {
		t.Errorf("report = %q, want it to lead with what it could not confirm", report)
	}
}

// TestARunningStepIsOnDiskBeforeItHappens is the crash-honesty invariant: the
// dispatch is written down first, so a process that dies between the write and
// the result leaves evidence that something was started.
func TestARunningStepIsOnDiskBeforeItHappens(t *testing.T) {
	r := newRig(t, step("memory.remember", "the long one"))
	r.actor.entered = make(chan struct{})
	r.actor.release = make(chan struct{})
	job := r.start(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.runner.work(context.Background(), job.ID)
	}()
	<-r.actor.entered

	// A second store over the same file is a fresh daemon reading what the
	// first one had written before it acted.
	fresh := NewStore(r.path, StoreOptions{Now: newClock().now}, nil)
	adopted, err := fresh.Find(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.State != Ready {
		t.Errorf("state = %q, want %q — a job found mid-step is a job whose daemon went away",
			adopted.State, Ready)
	}
	if len(adopted.Ledger) != 1 || adopted.Ledger[0].Verified {
		t.Errorf("ledger = %+v, want one unverified entry for the interrupted step", adopted.Ledger)
	}
	close(r.actor.release)
	<-done
}

// TestOneJobParkingDoesNotStallAnother: their place is state, and there is no
// shared thread to stall.
func TestOneJobParkingDoesNotStallAnother(t *testing.T) {
	r := newRig(t)
	r.planner.steps = nil
	// A planner that parks the first job forever and finishes the second.
	r.planner.err = nil
	scope := r.scope
	stuck, err := r.store.Start("stuck", "wait for me", scope)
	if err != nil {
		t.Fatal(err)
	}
	going, err := r.store.Start("going", "get on with it", scope)
	if err != nil {
		t.Fatal(err)
	}
	// The first job's plan is a question; the second's is a finish.
	perJob := map[string]Step{
		stuck.ID: {Question: "Which one?"},
		going.ID: {Finished: true, Intent: "there was nothing to do"},
	}
	r.runner.Bind(plannerFunc(func(_ context.Context, v View) (Step, error) {
		for id, s := range perJob {
			job, _ := r.store.Find(id)
			if job.Goal == v.Goal {
				return s, nil
			}
		}
		return Step{Finished: true}, nil
	}), r.actor)

	r.runner.dispatchReady(context.Background())
	drainCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	if err := r.runner.group.Wait(drainCtx); err != nil {
		t.Fatalf("the runners did not finish: %v", err)
	}

	first, _ := r.store.Find(stuck.ID)
	second, _ := r.store.Find(going.ID)
	if first.State != Parked {
		t.Errorf("the first job = %q, want parked", first.State)
	}
	if second.State != Done {
		t.Errorf("the second job = %q, want done: one job's parking must not stall another", second.State)
	}
}

// plannerFunc adapts a function to Planner.
type plannerFunc func(context.Context, View) (Step, error)

func (f plannerFunc) Next(ctx context.Context, v View) (Step, error) { return f(ctx, v) }

// TestAJobThatWillNotFinishIsStoppedRatherThanRunForever pins the step bound:
// a plan that never says "finished" is stuck, and the honest response is to
// stop and say so with the ledger intact.
func TestAJobThatWillNotFinishIsStoppedRatherThanRunForever(t *testing.T) {
	r := newRig(t)
	r.runner.Bind(plannerFunc(func(context.Context, View) (Step, error) {
		return step("memory.search", "looked again"), nil
	}), r.actor)
	job := r.work(t, r.start(t).ID)
	if job.State != Parked || job.Question.Why != WhyStuck {
		t.Fatalf("state = %q why = %q, want parked and stuck", job.State, job.Question.Why)
	}
	if job.Steps != MaxSteps {
		t.Errorf("steps = %d, want the bound %d", job.Steps, MaxSteps)
	}
	if len(job.Ledger) != MaxSteps {
		t.Errorf("ledger has %d entries, want %d — the user must be able to see what it spent them on",
			len(job.Ledger), MaxSteps)
	}
}

// TestProseWithNoActionIsNotAnEnding: a job that ended because a model narrated
// an ending would be #71 with a longer runway.
func TestProseWithNoActionIsNotAnEnding(t *testing.T) {
	r := newRig(t, Step{Intent: "I have finished tidying everything up."})
	job := r.work(t, r.start(t).ID)
	if job.State == Done {
		t.Fatal("a job finished on prose alone; nothing it writes may be read as an ending")
	}
	if job.Question.Why != WhyStuck {
		t.Errorf("why = %q, want %q", job.Question.Why, WhyStuck)
	}
}

// TestAPlannerThatCannotBeReachedParksRatherThanFails.
func TestAPlannerThatCannotBeReachedParksRatherThanFails(t *testing.T) {
	r := newRig(t)
	r.planner.err = fmt.Errorf("the provider is not configured")
	job := r.work(t, r.start(t).ID)
	if job.State != Parked || job.Question.Why != WhyStuck {
		t.Fatalf("state = %q why = %q, want parked and stuck", job.State, job.Question.Why)
	}
	if !strings.Contains(job.Question.Ask, "provider is not configured") {
		t.Errorf("reason = %q, want it to say what went wrong", job.Question.Ask)
	}
}

// TestARunnerWithNoModelBehindItSaysSo is the disabled-means-absent rule: a
// daemon built without a planner makes an honest refusal, never a silent stall.
func TestARunnerWithNoModelBehindItSaysSo(t *testing.T) {
	r := newRig(t)
	r.runner.Bind(nil, nil)
	job := r.work(t, r.start(t).ID)
	if job.State != Parked || job.Question.Why != WhyStuck {
		t.Fatalf("state = %q why = %q, want parked and stuck", job.State, job.Question.Why)
	}
}

// TestTheSupervisorIsAlwaysArmed is ADR 0049 for this loop. With nothing to do
// the loop must still be selecting on its timer, so a tick delivered to it is
// received rather than never arriving — which is also what makes a hand edit to
// the jobs file get picked up without a restart.
func TestTheSupervisorIsAlwaysArmed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.toml")
	store := NewStore(path, StoreOptions{Now: newClock().now}, nil)
	ticks := make(chan chan time.Time, 4)
	runner := NewRunner(RunnerOptions{
		Store: store, Planner: &scriptedPlanner{}, Actor: &recordingActor{},
		Now: newClock().now,
		Timer: func(time.Duration) (<-chan time.Time, func()) {
			c := make(chan time.Time, 1)
			// A blocking send: the loop cannot arm without a waiter taking the
			// channel, so a test that receives here KNOWS the loop is armed.
			// ADR 0049's rendezvous rule — a side channel announcing an arm
			// cannot distinguish one that later unwinds.
			ticks <- c
			return c, func() {}
		},
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner.Start(ctx)
	t.Cleanup(func() {
		drainCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := runner.Drain(drainCtx); err != nil {
			t.Errorf("the supervisor did not drain: %v", err)
		}
	})

	// Nothing is scheduled at all, and the loop still arms — twice, so this is
	// a loop going round rather than one pass that happened to reach a timer.
	for i := 0; i < 2; i++ {
		select {
		case c := <-ticks:
			c <- time.Now()
		case <-time.After(5 * time.Second):
			t.Fatal("the supervisor parked with an empty schedule; a scheduler loop is always armed")
		}
	}
}

// TestTheSupervisorPicksUpAJobAddedWhileItWasIdle is the sweep's purpose: the
// jobs file's hand-edit promise, applied to the one reader with no caller.
func TestTheSupervisorPicksUpAJobAddedWhileItWasIdle(t *testing.T) {
	r := newRig(t)
	ticks := make(chan chan time.Time, 4)
	r.runner.timer = func(time.Duration) (<-chan time.Time, func()) {
		c := make(chan time.Time, 1)
		ticks <- c
		return c, func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.runner.Start(ctx)
	t.Cleanup(func() {
		drainCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := r.runner.Drain(drainCtx); err != nil {
			t.Errorf("the supervisor did not drain: %v", err)
		}
	})
	// Wait until the loop is armed with nothing to do, then add a job WITHOUT
	// waking it — as a hand edit would — and let the sweep fire.
	first := <-ticks
	job := r.start(t)
	first <- time.Now()

	final := waitForJob(t, r.store, job.ID, func(j Job) bool { return j.State == Done })
	if final.State != Done {
		t.Fatalf("state = %q, want the sweep to have found the job", final.State)
	}
	// Drain the arming rendezvous so the loop is never blocked on a send while
	// the cleanup drain is waiting for it to notice the cancelled context.
	go func() {
		for c := range ticks {
			_ = c
		}
	}()
}

// TestStoppingAJobThatHasAlreadyEndedSaysSo.
func TestStoppingAJobThatHasAlreadyEndedSaysSo(t *testing.T) {
	r := newRig(t)
	job := r.work(t, r.start(t).ID)
	if job.State != Done {
		t.Fatalf("state = %q, want %q", job.State, Done)
	}
	if _, err := r.runner.Stop("tidy", ""); err == nil {
		t.Error("stopping a finished job was accepted; it should say it has already finished")
	}
	if _, err := r.runner.Stop("nothing-by-that-name", ""); err == nil {
		t.Error("stopping a job that does not exist was accepted")
	}
	if _, err := r.runner.Answer("tidy", true, ""); err == nil {
		t.Error("answering a job that is not waiting was accepted")
	}
}

// waitForJob polls the store until the condition holds, waiting on the fact
// rather than on a clock. It fails the test rather than hanging.
func waitForJob(t *testing.T, store *Store, id string, want func(Job) bool) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		job, err := store.Find(id)
		if err == nil && want(job) {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job never reached the state this test is about: %+v", job)
		}
	}
}
