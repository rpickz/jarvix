package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/rpickz/jarvix/internal/jobs"
	"github.com/rpickz/jarvix/internal/undo"
)

// This file is the model's way to reach jobs (#200, ADR 0065): starting one,
// asking where one is up to, stopping one, and answering what one is waiting
// for.
//
// Four verbs and no more, because four is what a manager does with a piece of
// work. Everything else a job needs from the user — approving an irreversible
// step, settling a decision — arrives through jobs.answer, and everything a
// user needs from a job arrives through jobs.status or through the situation
// report, which carries a job source of its own (#196). There is no second
// reporting mechanism, and that was an acceptance criterion rather than a
// preference.
//
// **Nothing here decides anything.** The scope is validated and enforced in
// internal/jobs, the gate is applied by the runner exactly as a session applies
// it, and the report is composed from the ledger. These tools carry arguments
// in and sentences out.

// The job tools' gate identities.
const (
	JobsStartToolName  = "jobs.start"
	JobsStatusToolName = "jobs.status"
	JobsStopToolName   = "jobs.stop"
	JobsAnswerToolName = "jobs.answer"
)

// Working is the tools' view of the job service — four verbs, declared here so
// the tools package does not depend on the daemon and the tests can answer it
// with a fixture.
type Working interface {
	// Start creates a job and returns it, or refuses with a sentence saying
	// why the scope will not do.
	Start(name, goal string, scope jobs.Scope) (jobs.Job, error)
	// Status is the spoken account of one job, or of all of them when ref is
	// empty.
	Status(ref string) string
	// Stop interrupts a job.
	Stop(ref, why string) (jobs.Job, error)
	// Answer settles what a parked job is waiting for.
	Answer(ref string, approved bool, said string) (jobs.Job, error)
	// Path is the file jobs live in, so the account can snapshot the real file
	// rather than a guess (the ConfigAdmin.Path precedent, ADR 0064).
	Path() string
}

// JobsOptions configure the job tools.
type JobsOptions struct {
	// Service owns every job action.
	Service Working
	// Log records that a tool ran — never a goal, never a ledger line. Nil
	// uses slog.Default().
	Log *slog.Logger
}

// NewJobs builds the four job tools over one service.
func NewJobs(opts JobsOptions) []Tool {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return []Tool{
		&JobsStart{svc: opts.Service, log: log},
		&JobsStatus{svc: opts.Service},
		&JobsStop{svc: opts.Service, log: log},
		&JobsAnswer{svc: opts.Service, log: log},
	}
}

// ---------------------------------------------------------------------------
// jobs.start
// ---------------------------------------------------------------------------

// JobsStart creates a job.
type JobsStart struct {
	svc Working
	log *slog.Logger
}

// startArgs is what the model proposes. The scope arrives as three lists
// because a scope has three enforceable faces and no others; there is
// deliberately no free-text "scope" field for the model to describe a boundary
// in, because a boundary described is a boundary unenforced.
type startArgs struct {
	Name        string   `json:"name"`
	Goal        string   `json:"goal"`
	Directories []string `json:"directories"`
	Apps        []string `json:"apps"`
	Tools       []string `json:"tools"`
}

// scope reads the arguments as a scope.
func (a startArgs) scope() jobs.Scope {
	return jobs.Scope{Tools: a.Tools, Roots: a.Directories, Apps: a.Apps}
}

// Name implements Tool.
func (t *JobsStart) Name() string { return JobsStartToolName }

// Description implements Tool.
//
// The last two sentences are the whole contract with the model, and they are
// worded as facts about the daemon rather than as instructions, because an
// instruction is something a model can decide to skip and a fact is not. It
// cannot widen the scope later; it cannot avoid the gate by being a job. Saying
// so plainly is what stops it proposing an over-wide scope "to be safe".
func (t *JobsStart) Description() string {
	return "Start a long-running job: work that carries on after this conversation ends. " +
		"Use it when the user gives a direction rather than a command — \"get the tests " +
		"passing\", \"tidy my downloads\" — not for anything you can simply do now. " +
		"name is a short handle the user will say to ask about it. goal must be the user's " +
		"own words, not your paraphrase. The scope is the boundary the job may act within: " +
		"directories are absolute paths it may touch, apps are window classes it may act on, " +
		"tools are the exact tool names it may use. Ask the user for whatever you do not know " +
		"rather than guessing; a scope is a grant of authority. The boundary is enforced by " +
		"the daemon before every single action and cannot be widened once the job has " +
		"started, so an over-wide scope is not caution, it is a larger grant. Anything the " +
		"job does that cannot be undone still stops and asks the user, however long it has " +
		"been running."
}

// Schema implements Tool.
func (t *JobsStart) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Short handle, up to four words, that the user will say to ask about this job."},
    "goal": {"type": "string", "description": "The direction in the user's own words, verbatim."},
    "directories": {"type": "array", "items": {"type": "string"}, "description": "Absolute directories the job may touch."},
    "apps": {"type": "array", "items": {"type": "string"}, "description": "Window classes the job may act on."},
    "tools": {"type": "array", "items": {"type": "string"}, "description": "Exact tool names the job may use."}
  },
  "required": ["name", "goal", "tools"]
}`)
}

// Refuse implements Refusing: a scope naming one of the tools that govern what
// Jarvix is allowed to do is refused structurally, before any policy is
// consulted — including the no-policy case.
//
// This is #109's wall at its full height. A job whose scope could include
// config.write_entry would be a job that could rewrite `[tools.policy]`, which
// is to say a job that could widen its own boundary; a gate that can be loosened
// on request is not a gate. Nothing a user writes in configuration softens it,
// which is the point, and it is refused HERE as well as in
// jobs.Scope.Validate so that the refusal is visible on the gate's own audit
// trail rather than only in the tool's reply.
func (t *JobsStart) Refuse(input json.RawMessage) (string, bool) {
	var args startArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return "", false
	}
	for _, want := range args.Tools {
		want = strings.TrimSpace(want)
		for _, banned := range jobs.Forbidden {
			if want == banned {
				return "a job's scope may never include " + banned +
					", which governs what I am allowed to do", true
			}
		}
	}
	return "", false
}

// Confirmation implements Confirmable: the scope is stated back before the job
// begins, in the words the user hears.
//
// It is the acceptance criterion — "the scope stated back for confirmation
// before it begins" — and it is met here rather than by the tool's reply for
// the reason Confirmable exists at all: the card is what the user judges, and a
// scope explained only in the answer would be explained after the decision.
func (t *JobsStart) Confirmation(input json.RawMessage) (command, summary string, ok bool) {
	var args startArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return "", "", false
	}
	scope, err := args.scope().Validate()
	if err != nil {
		return "", "", false
	}
	name, err := jobs.CleanName(args.Name)
	if err != nil {
		return "", "", false
	}
	goal := strings.TrimSpace(args.Goal)
	if goal == "" {
		return "", "", false
	}
	job := jobs.Job{Name: name, Goal: goal, Scope: scope}
	// The command is the ground truth a remembered approval would key on, so
	// it carries the boundary rather than the goal: two jobs with the same
	// wording and different scopes are two different grants.
	return name + ": " + scope.Stated(), job.Stated() + " Shall I start?", true
}

// Execute implements Tool.
func (t *JobsStart) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	if t.svc == nil {
		return "Jobs are not available on this daemon.", nil
	}
	var args startArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return "I could not read that job: " + err.Error(), nil
	}
	before := undo.Snapshot(ctx, t.svc.Path())
	job, err := t.svc.Start(args.Name, args.Goal, args.scope())
	if err != nil {
		// Every refusal here is a sentence the assistant can relay: a scope
		// that cannot be enforced, a name already in use, too many jobs at
		// once. err is reserved for a service that is not there at all.
		return "I have not started that job: " + err.Error() + ".", nil
	}
	before.Note(ctx, undo.Action{
		Tool: JobsStartToolName, Target: job.Name,
		Summary: fmt.Sprintf("started the job %q (%s)", job.Name, job.Goal),
	})
	t.log.Info("job started", "component", "jobs", "job", job.ID, "name", job.Name)
	return "Started. " + job.Stated() +
		" Ask me how it is getting on whenever you like, and say stop to end it.", nil
}

// ---------------------------------------------------------------------------
// jobs.status
// ---------------------------------------------------------------------------

// JobsStatus reads a job's state back.
type JobsStatus struct{ svc Working }

// Name implements Tool.
func (t *JobsStatus) Name() string { return JobsStatusToolName }

// Description implements Tool. The final sentence is the same
// anti-confabulation clause the situation tool carries, and for the same
// reason: the account arrives already composed from a ledger the daemon wrote,
// and a model that expanded it would be inventing progress — which is precisely
// the failure jobs exist under the shadow of.
func (t *JobsStatus) Description() string {
	return "Report where a long-running job has got to: what it is doing, what it has done, " +
		"and what it is waiting for. Give the job's name, or leave it out for all of them. " +
		"Read the result back as it is written: every claim in it was read from a record " +
		"written as each step finished, and you must not add to it, explain it, or guess at " +
		"anything it does not say."
}

// Schema implements Tool.
func (t *JobsStatus) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "The job's name. Leave out for every job."}
  }
}`)
}

// Execute implements Tool.
func (t *JobsStatus) Execute(_ context.Context, input json.RawMessage) (string, error) {
	if t.svc == nil {
		return "Jobs are not available on this daemon.", nil
	}
	var args struct {
		Name string `json:"name"`
	}
	if len(input) > 0 {
		_ = json.Unmarshal(input, &args)
	}
	return t.svc.Status(args.Name), nil
}

// ---------------------------------------------------------------------------
// jobs.stop
// ---------------------------------------------------------------------------

// JobsStop interrupts a job.
type JobsStop struct {
	svc Working
	log *slog.Logger
}

// Name implements Tool.
func (t *JobsStop) Name() string { return JobsStopToolName }

// Description implements Tool.
func (t *JobsStop) Description() string {
	return "Stop a long-running job. It stops promptly, and tells you what it had done and " +
		"what it had not. Use it when the user says to stop, cancel or abandon a job by name."
}

// Schema implements Tool.
func (t *JobsStop) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "The job's name."}
  },
  "required": ["name"]
}`)
}

// Confirmation implements Confirmable so the question names the job rather
// than the tool. It is the difference between "may I use the jobs.stop tool?"
// and "shall I stop the tidy job?", and only one of those is a question the
// user can answer.
func (t *JobsStop) Confirmation(input json.RawMessage) (command, summary string, ok bool) {
	name, ok := jobName(input)
	if !ok {
		return "", "", false
	}
	return "stop the " + name + " job",
		"Shall I stop the " + name + " job? It will tell you what it had done and what it had not.", true
}

// Execute implements Tool.
func (t *JobsStop) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	if t.svc == nil {
		return "Jobs are not available on this daemon.", nil
	}
	name, ok := jobName(input)
	if !ok {
		return "I could not tell which job to stop.", nil
	}
	before := undo.Snapshot(ctx, t.svc.Path())
	job, err := t.svc.Stop(name, "You asked me to stop.")
	if err != nil {
		return "I have not stopped that: " + err.Error() + ".", nil
	}
	before.Note(ctx, undo.Action{
		Tool: JobsStopToolName, Target: job.Name,
		Summary: fmt.Sprintf("stopped the job %q", job.Name),
	})
	t.log.Info("job stopped", "component", "jobs", "job", job.ID, "name", job.Name)
	return job.Spoken(), nil
}

// ---------------------------------------------------------------------------
// jobs.answer
// ---------------------------------------------------------------------------

// JobsAnswer settles what a parked job is waiting for.
type JobsAnswer struct {
	svc Working
	log *slog.Logger
}

// Name implements Tool.
func (t *JobsAnswer) Name() string { return JobsAnswerToolName }

// Description implements Tool.
func (t *JobsAnswer) Description() string {
	return "Answer what a parked job is waiting for. A job that reached something it cannot " +
		"undo, or a decision only the user can make, stops and waits rather than interrupting; " +
		"this is how the user's answer reaches it. Approving resumes the job at exactly the " +
		"step it stopped on. Declining stops the job — it does not look for another way round."
}

// Schema implements Tool.
func (t *JobsAnswer) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "The job's name."},
    "approved": {"type": "boolean", "description": "True to go ahead, false to stop the job."},
    "answer": {"type": "string", "description": "What the user said, for a job waiting on a decision rather than a yes or no."}
  },
  "required": ["name", "approved"]
}`)
}

// answerArgs is what the model proposes.
type answerArgs struct {
	Name     string `json:"name"`
	Approved bool   `json:"approved"`
	Answer   string `json:"answer"`
}

// Confirmation implements Confirmable.
//
// It exists because approving here is approving the thing the job parked on,
// which may be irreversible — that is why it parked. The card therefore names
// the job and says plainly that the answer will be acted on, rather than asking
// about a tool.
func (t *JobsAnswer) Confirmation(input json.RawMessage) (command, summary string, ok bool) {
	var args answerArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return "", "", false
	}
	name := strings.ToLower(strings.TrimSpace(args.Name))
	if name == "" {
		return "", "", false
	}
	if !args.Approved {
		return "stop the " + name + " job", "Shall I tell the " + name +
			" job no? That stops it where it is.", true
	}
	return "let the " + name + " job go ahead",
		"Shall I let the " + name + " job go ahead with what it stopped on and carry on?", true
}

// Execute implements Tool.
func (t *JobsAnswer) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	if t.svc == nil {
		return "Jobs are not available on this daemon.", nil
	}
	var args answerArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return "I could not read that answer: " + err.Error(), nil
	}
	if strings.TrimSpace(args.Name) == "" {
		return "I could not tell which job that was about.", nil
	}
	before := undo.Snapshot(ctx, t.svc.Path())
	job, err := t.svc.Answer(args.Name, args.Approved, args.Answer)
	if err != nil {
		return "I have not passed that on: " + err.Error() + ".", nil
	}
	word := "declined"
	if args.Approved {
		word = "approved"
	}
	before.Note(ctx, undo.Action{
		Tool: JobsAnswerToolName, Target: job.Name,
		Summary: fmt.Sprintf("%s what the job %q was waiting for", word, job.Name),
	})
	t.log.Info("job answered", "component", "jobs", "job", job.ID,
		"name", job.Name, "approved", args.Approved)
	return job.Spoken(), nil
}

// jobName reads the one argument the simple job verbs take.
func jobName(input json.RawMessage) (string, bool) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", false
	}
	name := strings.ToLower(strings.TrimSpace(args.Name))
	return name, name != ""
}
