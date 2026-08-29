package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/jobs"
)

// fakeWorking scripts the job service and records what it was asked for.
type fakeWorking struct {
	mu sync.Mutex

	job    jobs.Job
	err    error
	status string

	started  []jobs.Scope
	stopped  []string
	answered []bool
}

func (f *fakeWorking) Start(name, goal string, scope jobs.Scope) (jobs.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, scope)
	if f.err != nil {
		return jobs.Job{}, f.err
	}
	f.job.Name, f.job.Goal, f.job.Scope = name, goal, scope
	return f.job, nil
}

func (f *fakeWorking) Status(string) string { return f.status }

func (f *fakeWorking) Stop(ref, _ string) (jobs.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, ref)
	return f.job, f.err
}

func (f *fakeWorking) Answer(_ string, approved bool, _ string) (jobs.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answered = append(f.answered, approved)
	return f.job, f.err
}

func (f *fakeWorking) Path() string { return "" }

// jobTools builds the four verbs over one fake, keyed by name.
func jobTools(t *testing.T, svc Working) map[string]Tool {
	t.Helper()
	out := map[string]Tool{}
	for _, tool := range NewJobs(JobsOptions{Service: svc}) {
		out[tool.Name()] = tool
	}
	return out
}

// TestTheForbiddenNamesAreTheRealToolNames is the drift guard for #109's wall.
//
// internal/jobs cannot import internal/tools — this package imports it back for
// the verbs above — so its Forbidden list is string literals. This test is the
// only thing standing between a tool rename and a wall that quietly stops
// covering the capability it was built for.
func TestTheForbiddenNamesAreTheRealToolNames(t *testing.T) {
	want := map[string]bool{
		ConfigWriteEntryToolName:   true,
		ConfigDeleteEntryToolName:  true,
		ConfigWriteSettingToolName: true,
		ScriptToolName:             true,
		IntentToolName:             true,
		AdvisorToolName:            true,
		ManageWindowToolName:       true,
	}
	got := map[string]bool{}
	for _, name := range jobs.Forbidden {
		got[name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("%q is not in jobs.Forbidden; a job could be given a tool that governs what "+
				"I am allowed to do", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("jobs.Forbidden names %q, which no tool constant here claims; either the tool "+
				"was renamed or the entry is dead", name)
		}
	}
}

// TestAScopeReachingTheGovernanceToolsIsRefusedStructurally is the wall in the
// gate's own path: Refusing is consulted before any policy, including the
// no-policy case, so nothing a user writes in configuration softens it.
func TestAScopeReachingTheGovernanceToolsIsRefusedStructurally(t *testing.T) {
	registry := NewRegistry(nil)
	for _, tool := range NewJobs(JobsOptions{Service: &fakeWorking{}}) {
		registry.Register(tool)
	}
	// Deliberately no policy installed at all — the reading under which
	// everything else is allowed.
	verdict := registry.Check(ai.ToolCall{Name: JobsStartToolName, Arguments: `{
		"name":"sneaky","goal":"widen my own scope",
		"tools":["memory.search","config.write_entry"],"directories":["/tmp"]}`})
	if verdict.Decision != PolicyDeny {
		t.Fatalf("decision = %q, want %q: a job must never be a privilege escalation",
			verdict.Decision, PolicyDeny)
	}
	if !strings.Contains(verdict.Rule, ConfigWriteEntryToolName) {
		t.Errorf("rule = %q, want it to name the tool that is off limits", verdict.Rule)
	}

	// And an ordinary scope is not refused by it.
	ok := registry.Check(ai.ToolCall{Name: JobsStartToolName, Arguments: `{
		"name":"tidy","goal":"tidy up","tools":["memory.search"],"directories":["/tmp"]}`})
	if ok.Decision == PolicyDeny {
		t.Errorf("an ordinary scope was refused by the wall: %s", ok.Rule)
	}
}

// TestTheScopeIsStatedBackOnTheCard is the acceptance criterion: the boundary
// is what the user judges, before the job begins, not after.
func TestTheScopeIsStatedBackOnTheCard(t *testing.T) {
	tool := jobTools(t, &fakeWorking{})[JobsStartToolName].(*JobsStart)
	command, summary, ok := tool.Confirmation(json.RawMessage(`{
		"name":"Tidy Downloads","goal":"tidy up my downloads",
		"tools":["memory.search"],"directories":["/tmp"],"apps":["Alacritty"]}`))
	if !ok {
		t.Fatal("the card offered no words of its own, so the user would be asked about a tool name")
	}
	for _, want := range []string{"tidy up my downloads", "/tmp", "alacritty", "memory.search"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary = %q, want it to state %q back", summary, want)
		}
	}
	if !strings.Contains(summary, "can't undo") {
		t.Errorf("summary = %q, want it to promise the gate's floor still holds", summary)
	}
	// The command is what a remembered approval would key on, so two jobs with
	// the same words and different boundaries are two different grants.
	if !strings.Contains(command, "/tmp") {
		t.Errorf("command = %q, want the boundary in the ground-truth string", command)
	}
}

// TestACardIsNotOfferedForAScopeThatWillBeRefused: a question about a job that
// cannot be created would be a question the answer to which changes nothing.
func TestACardIsNotOfferedForAScopeThatWillBeRefused(t *testing.T) {
	tool := jobTools(t, &fakeWorking{})[JobsStartToolName].(*JobsStart)
	if _, _, ok := tool.Confirmation(json.RawMessage(
		`{"name":"tidy","goal":"tidy up","tools":["memory.search"]}`)); ok {
		t.Error("a card was offered for a scope with no boundary in it")
	}
	if _, _, ok := tool.Confirmation(json.RawMessage(`not json`)); ok {
		t.Error("a card was offered for arguments that could not be read")
	}
}

func TestStartingAJobRelaysWhatWasAgreed(t *testing.T) {
	svc := &fakeWorking{}
	tool := jobTools(t, svc)[JobsStartToolName]
	got, err := tool.Execute(context.Background(), json.RawMessage(`{
		"name":"tidy","goal":"tidy up my downloads","tools":["memory.search"],"directories":["/tmp"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "tidy up my downloads") || !strings.Contains(got, "/tmp") {
		t.Errorf("reply = %q, want the goal and the boundary", got)
	}
	if len(svc.started) != 1 || len(svc.started[0].Roots) != 1 {
		t.Errorf("the scope reached the service as %+v, want the directories it was given", svc.started)
	}
}

// TestARefusedJobIsASentenceNotAnError: every disappointment here is something
// the assistant can say, so err is reserved for a service that is not there.
func TestARefusedJobIsASentenceNotAnError(t *testing.T) {
	svc := &fakeWorking{err: &jobs.ErrUnenforceable{Because: "it names no tools"}}
	tool := jobTools(t, svc)[JobsStartToolName]
	got, err := tool.Execute(context.Background(), json.RawMessage(
		`{"name":"tidy","goal":"tidy up","tools":[]}`))
	if err != nil {
		t.Fatalf("a refusal came back as an error: %v", err)
	}
	if !strings.Contains(got, "names no tools") {
		t.Errorf("reply = %q, want the reason the job was not started", got)
	}
}

func TestTheJobVerbsSayWhenJobsAreUnavailable(t *testing.T) {
	for _, tool := range NewJobs(JobsOptions{}) {
		got, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"tidy","approved":true}`))
		if err != nil {
			t.Fatalf("%s returned an error rather than a sentence: %v", tool.Name(), err)
		}
		if !strings.Contains(got, "not available") {
			t.Errorf("%s said %q, want an honest refusal", tool.Name(), got)
		}
	}
}

// TestStoppingAndAnsweringWordTheirOwnQuestions: "may I use the jobs.stop
// tool?" is not a question a user can answer.
func TestStoppingAndAnsweringWordTheirOwnQuestions(t *testing.T) {
	tools := jobTools(t, &fakeWorking{})
	stop := tools[JobsStopToolName].(*JobsStop)
	_, summary, ok := stop.Confirmation(json.RawMessage(`{"name":"tidy"}`))
	if !ok || !strings.Contains(summary, "tidy") {
		t.Errorf("stop card = %q (ok=%v), want it to name the job", summary, ok)
	}
	answer := tools[JobsAnswerToolName].(*JobsAnswer)
	_, yes, ok := answer.Confirmation(json.RawMessage(`{"name":"tidy","approved":true}`))
	if !ok || !strings.Contains(yes, "go ahead") {
		t.Errorf("approve card = %q (ok=%v), want it to say the job will act", yes, ok)
	}
	_, no, ok := answer.Confirmation(json.RawMessage(`{"name":"tidy","approved":false}`))
	if !ok || !strings.Contains(no, "stops it") {
		t.Errorf("decline card = %q (ok=%v), want it to say declining stops the job", no, ok)
	}
}

func TestAnsweringPassesTheAnswerThrough(t *testing.T) {
	svc := &fakeWorking{job: jobs.Job{Name: "tidy", State: jobs.Ready}}
	tool := jobTools(t, svc)[JobsAnswerToolName]
	if _, err := tool.Execute(context.Background(),
		json.RawMessage(`{"name":"tidy","approved":true,"answer":"go on"}`)); err != nil {
		t.Fatal(err)
	}
	if len(svc.answered) != 1 || !svc.answered[0] {
		t.Errorf("answered = %v, want one approval", svc.answered)
	}
}

func TestStatusRelaysTheServicesAccountUntouched(t *testing.T) {
	svc := &fakeWorking{status: "Tidy is running: 3 steps, 1 of which changed something."}
	tool := jobTools(t, svc)[JobsStatusToolName]
	got, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != svc.status {
		t.Errorf("status = %q, want the service's own account verbatim", got)
	}
}

// TestEveryJobVerbDeclaresItself keeps the four honest about their own shape:
// a name, a description that says when to use them, and a parseable schema.
func TestEveryJobVerbDeclaresItself(t *testing.T) {
	for _, tool := range NewJobs(JobsOptions{Service: &fakeWorking{}}) {
		if !strings.HasPrefix(tool.Name(), "jobs.") {
			t.Errorf("%q is not in the jobs family", tool.Name())
		}
		if len(tool.Description()) < 40 {
			t.Errorf("%s has no useful description", tool.Name())
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
			t.Errorf("%s has an unparseable schema: %v", tool.Name(), err)
		}
	}
}
