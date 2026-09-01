package daemon

// This file wires jobs (#200, ADR 0065) into jarvixd: the store and the
// supervisor, the planner that decides what a job does next, the actor that
// reads a step's subject and carries it out, the four job tools, and the
// situation source that reports where every job has got to.
//
// Three properties of the shape are worth naming before the code.
//
//   - **The subject table is the enforcement.** Subject below is the only code
//     that says what a proposed tool call would touch, and it says it from the
//     parsed arguments. A tool it does not know has NO readable subject, and
//     the runner parks the job rather than guessing.
//   - **A shell command is the one step whose subject is never read** (#222,
//     ADR 0068). It cannot be: a command's filesystem subject is not
//     recoverable from its text, and a check that is right most of the time is
//     worse than none because it will be trusted. So commandSubject reads
//     nothing and instead establishes that the KERNEL will hold the command
//     inside the job's roots — or refuses the step outright when this machine
//     cannot. There is no third branch. ADR 0065 said this could not be done
//     and was right about the parser; ADR 0068 is where it moved.
//   - **The planner proposes and nothing else.** It cannot dispatch, it cannot
//     widen a scope, and the two control verbs it is given — finishing and
//     asking — are synthetic definitions this file owns. Plain prose from the
//     model is not a finish: a job that ended because the model narrated an
//     ending would be #71 with a longer runway.
//   - **The report is not composed here.** Every sentence a job says comes out
//     of internal/jobs, from its ledger. This file feeds the ledger; it never
//     words it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/briefing"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/confine"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/jobs"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/situation"
	"github.com/rpickz/jarvix/internal/statehold"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/undo"
)

// The planner's model call. Pinned rather than configurable for the recap's
// reason (ADR 0043): the planner's whole output is one tool call, which fits
// far inside the cap, and a plan wants faithful rather than creative.
const (
	jobPlanMaxTokens   = 400
	jobPlanTemperature = 0.1
)

// jobsNamed is how many jobs one rank of the situation report will name before
// it stops counting them out loud — the report's own cap (situationNamed),
// restated for jobs because MaxLive is four and a report that read out four
// job ledgers would stop being an answer.
const jobsNamed = 2

// jobWindowTimeout bounds the one inventory read a window-shaped step makes
// while its subject is being read. Shorter than a step, because a wedged
// compositor must cost that step its answer and not the whole job's step
// budget.
const jobWindowTimeout = 3 * time.Second

// The two synthetic verbs a planner is given alongside the job's scoped tools.
// They are how a plan says something other than "do this", and they are tool
// calls rather than prose for the reason stated at the top of the file.
const (
	jobFinishVerb = "job_finished"
	jobAskVerb    = "job_ask_user"
)

// newJobsStore builds the jobs file. Always present, like the reminder store
// and the account: there is no configuration switch for "keep no record of the
// work you are doing for me". A user who wants it gone deletes the file, which
// the store reads as no jobs.
func newJobsStore(paths config.Paths, bus *session.Bus, gate *statehold.Gate,
	logger *slog.Logger) *jobs.Store {
	return jobs.NewStore(paths.JobsFile(), jobs.StoreOptions{
		Gate: gate,
		Publish: func(event string, data map[string]any) {
			bus.Publish(session.Event{Type: event, Data: data})
		},
	}, logger)
}

// bindJobs completes the supervisor once the daemon exists: the planner needs
// the live provider and the actor needs the registry, the account and the
// window seam, and none of those exists before the daemon does. The situation
// service's construction rule, for the same reason.
func (d *Daemon) bindJobs() {
	d.jobRunner.Bind(&jobPlanner{d: d}, &jobActor{d: d})
}

// ---------------------------------------------------------------------------
// The service the tools and the CLI talk to
// ---------------------------------------------------------------------------

// jobService is the daemon's implementation of tools.Working: the store and the
// supervisor behind one small surface, so nothing that starts or stops a job
// has to know which of the two owns which verb.
type jobService struct{ d *Daemon }

// Start creates a job and wakes the supervisor.
func (s *jobService) Start(name, goal string, scope jobs.Scope) (jobs.Job, error) {
	job, err := s.d.jobStore.Start(name, goal, scope)
	if err != nil {
		return jobs.Job{}, err
	}
	s.d.jobRunner.Rearm()
	return job, nil
}

// Stop interrupts a job.
func (s *jobService) Stop(ref, why string) (jobs.Job, error) { return s.d.jobRunner.Stop(ref, why) }

// Answer settles what a parked job was waiting for.
func (s *jobService) Answer(ref string, approved bool, said string) (jobs.Job, error) {
	return s.d.jobRunner.Answer(ref, approved, said)
}

// Path is the file jobs live in.
func (s *jobService) Path() string { return s.d.jobStore.Path() }

// Status is the spoken account: one job, or all of them.
//
// The wording is entirely internal/jobs's, read from each job's ledger. This
// method chooses which jobs to say and joins them, and does not compose a
// single sentence of its own beyond the two that say there are none.
func (s *jobService) Status(ref string) string {
	if strings.TrimSpace(ref) != "" {
		job, err := s.d.jobStore.Find(ref)
		if err != nil {
			return "I have no job called " + strings.TrimSpace(ref) + "."
		}
		return job.Spoken()
	}
	all := s.d.jobStore.List()
	lines := make([]string, 0, len(all))
	for _, job := range all {
		if !job.State.Live() {
			continue
		}
		lines = append(lines, job.Spoken())
	}
	if len(lines) == 0 {
		return "I'm not working on anything for you at the moment."
	}
	return strings.Join(lines, " ")
}

// ---------------------------------------------------------------------------
// The planner
// ---------------------------------------------------------------------------

// jobPlanner asks the model for the next step.
type jobPlanner struct{ d *Daemon }

// Next implements jobs.Planner: one exchange, no history, no conversation.
//
// The prompt is the whole exchange, so nothing here can leak a conversation
// into a job or a job into a conversation — the briefing's shape (ADR 0050),
// which matters more here than there: a job runs while nobody is watching, and
// a plan that could see the user's last private exchange would be reading it
// without them present.
func (p *jobPlanner) Next(ctx context.Context, v jobs.View) (jobs.Step, error) {
	p.d.cfgMu.Lock()
	provider, model := p.d.provider, p.d.cfg.AI.Model
	p.d.cfgMu.Unlock()
	if provider == nil {
		return jobs.Step{}, fmt.Errorf("no assistant provider is configured")
	}
	defs, err := p.d.scopedDefs(v.Scope)
	if err != nil {
		return jobs.Step{}, err
	}
	events, err := provider.Chat(ctx, ai.ChatRequest{
		Model:       model,
		Messages:    []ai.Message{{Role: ai.RoleUser, Content: jobPrompt(v)}},
		MaxTokens:   jobPlanMaxTokens,
		Temperature: jobPlanTemperature,
		Tools:       defs,
	})
	if err != nil {
		return jobs.Step{}, err
	}
	var said strings.Builder
	var call *ai.ToolCall
	for ev := range events {
		switch ev.Type {
		case ai.EventDelta:
			said.WriteString(ev.Content)
		case ai.EventToolCall:
			if call == nil {
				// The FIRST call only. A round that proposed three actions is
				// a round that wanted three checkpoints, and taking them one
				// at a time is what makes every one of them scope-checked,
				// gated and recorded before the next is even planned.
				c := ev.Call
				call = &c
			}
		case ai.EventError:
			return jobs.Step{}, ev.Err
		}
	}
	if call == nil {
		// Prose with no call is not a finish and not a step. Saying so plainly
		// parks the job as stuck, which is the honest outcome: the model said
		// something and asked for nothing, and reading an ending into that is
		// the #71 failure with a longer runway.
		return jobs.Step{}, nil
	}
	intent := strings.TrimSpace(said.String())
	switch call.Name {
	case jobFinishVerb:
		var args struct {
			Because string `json:"because"`
		}
		_ = json.Unmarshal([]byte(call.Arguments), &args)
		because := strings.TrimSpace(args.Because)
		if because == "" {
			because = intent
		}
		return jobs.Step{Finished: true, Intent: because}, nil
	case jobAskVerb:
		var args struct {
			Question string `json:"question"`
		}
		_ = json.Unmarshal([]byte(call.Arguments), &args)
		question := strings.TrimSpace(args.Question)
		if question == "" {
			question = "I need you to decide something before I can carry on."
		}
		return jobs.Step{Question: question, Intent: intent}, nil
	default:
		return jobs.Step{Intent: intent, Tool: call.Name, Args: call.Arguments}, nil
	}
}

// scopedDefs is the tool vocabulary one job is given: the scope's tools, and
// the two control verbs.
//
// Handing the planner only the tools in scope is a courtesy rather than the
// control — every proposal is checked against the scope afterwards, whatever
// it was shown — but it is the courtesy that makes the difference between a
// job that works and a job that spends its whole step budget being refused.
//
// A scope naming a tool this daemon does not have is not an error: the job
// simply cannot use it, and will be told so the first time it tries. Refusing
// to plan at all because one name is unavailable would turn a partly-usable
// scope into a dead job.
func (d *Daemon) scopedDefs(scope jobs.Scope) ([]ai.ToolDef, error) {
	all := d.registry.Defs()
	byName := make(map[string]ai.ToolDef, len(all))
	for _, def := range all {
		byName[def.Name] = def
	}
	defs := make([]ai.ToolDef, 0, len(scope.Tools)+2)
	for _, name := range scope.Tools {
		if def, ok := byName[name]; ok {
			defs = append(defs, def)
		}
	}
	if len(defs) == 0 {
		return nil, fmt.Errorf("none of the tools this job was given are available on this daemon")
	}
	return append(defs,
		ai.ToolDef{
			Name: jobFinishVerb,
			Description: "Declare the job's goal met, or that it cannot be met and you are stopping. " +
				"Call this instead of writing a summary: nothing you write is read as an ending.",
			Schema: json.RawMessage(`{"type":"object","properties":{"because":{"type":"string",` +
				`"description":"One sentence saying what was achieved, or why you are stopping."}},` +
				`"required":["because"]}`),
		},
		ai.ToolDef{
			Name: jobAskVerb,
			Description: "Ask the user something you cannot decide yourself. The job stops and waits; " +
				"it does not interrupt them, and it carries on from here when they answer.",
			Schema: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string",` +
				`"description":"The one question, in plain words."}},"required":["question"]}`),
		}), nil
}

// jobPrompt is the planning turn's whole exchange.
//
// The ledger is fed as facts, one line each, in the #150 discipline: the model
// is composing a decision from a record it did not write, and the record says
// what happened rather than what was intended. The unverified lines are marked
// as unverified for the same reason they are in the report — a plan that
// assumed a step it cannot confirm had succeeded is a plan that skips the work.
func jobPrompt(v jobs.View) string {
	var b strings.Builder
	b.WriteString("You are carrying out one piece of work on a computer, on behalf of its owner, ")
	b.WriteString("while they are not watching.\n\n")
	b.WriteString("THE GOAL, in their own words:\n")
	b.WriteString(v.Goal)
	b.WriteString("\n\nWHAT YOU MAY DO: ")
	b.WriteString(v.Scope.Stated())
	b.WriteString(".\nThis boundary is enforced by the machine before every action, not by you. ")
	b.WriteString("An action outside it stops the job; it is not refused and retried.\n\n")
	if len(v.Ledger) == 0 {
		b.WriteString("NOTHING HAS HAPPENED YET.\n")
	} else {
		b.WriteString("WHAT HAS ALREADY HAPPENED, oldest first. These are the machine's own ")
		b.WriteString("records, not your recollection:\n")
		for i, e := range v.Ledger {
			fmt.Fprintf(&b, "%d. %s — ", i+1, e.Tool)
			switch {
			case !e.Verified:
				b.WriteString("STARTED, OUTCOME UNKNOWN. ")
			case e.Failed:
				b.WriteString("FAILED. ")
			default:
				b.WriteString("done. ")
			}
			b.WriteString(strings.TrimSpace(e.Said))
			b.WriteString("\n")
		}
	}
	if answer := strings.TrimSpace(v.Answer); answer != "" {
		b.WriteString("\nTHE OWNER HAS JUST ANSWERED: ")
		b.WriteString(answer)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nYou have %d steps left.\n\n", v.Left)
	b.WriteString("Choose ONE next action and call exactly one tool. Do not describe what you ")
	b.WriteString("would do; nothing you write is acted on. If the goal is met, or you cannot ")
	b.WriteString("meet it, call " + jobFinishVerb + ". If you need the owner to decide something, ")
	b.WriteString("call " + jobAskVerb + " — they are not here, so the job will wait for them. ")
	b.WriteString("Never claim a step succeeded that the record above does not show succeeding.")
	return b.String()
}

// ---------------------------------------------------------------------------
// The actor
// ---------------------------------------------------------------------------

// jobActor reads a step's subject, judges it, and carries it out.
type jobActor struct{ d *Daemon }

// Subject implements jobs.Actor: what would this call ACTUALLY touch?
//
// It answers from the parsed arguments and from the live machine, never from
// the model's account of its own intention. An error is the honest "I cannot
// tell", and the runner parks on it — refusing to guess is the whole of the
// enforcement promise, because a subject nobody can name cannot be checked
// against a boundary.
func (a *jobActor) Subject(ctx context.Context, scope jobs.Scope, s jobs.Step) (jobs.Attempt, error) {
	if s.Tool == tools.ShellToolName {
		return a.commandSubject(scope, s)
	}
	if window, ok := windowShaped(s.Tool); ok {
		return a.windowSubject(ctx, s, window)
	}
	if path, ok := a.fileSubject(s.Tool); ok {
		return jobs.Attempt{Tool: s.Tool, Paths: []string{path}}, nil
	}
	if subjectlessReads[s.Tool] {
		// A read with no subject beyond itself. It is in scope if the scope
		// named it, and there is nothing else to check.
		return jobs.Attempt{Tool: s.Tool}, nil
	}
	return jobs.Attempt{}, fmt.Errorf(
		"I have no way to tell what %s would touch, so I can't check it against this job's boundary",
		s.Tool)
}

// commandSubject answers the question that used to have no answer (#222, ADR
// 0068): what would a shell command touch?
//
// It does not answer it. It establishes that the question does not need
// answering, and refuses when it cannot.
//
// The old refusal here was correct and is worth restating, because this
// function is what replaced it: a command's filesystem subject cannot be
// recovered from its text — quoting, variable expansion, `$(…)`, relative
// paths, `cd` and symlinks each defeat a reader on their own — and a check that
// is right most of the time is worse than none, because it will be trusted. So
// nothing here reads the command. **There is deliberately no code in this
// daemon that takes a command string and returns paths, and there must not be.**
//
// What there is instead is a precondition. If internal/confine can build a
// kernel-held boundary around this job's roots, then the command physically
// cannot reach outside them whatever its text says, and the attempt carries no
// paths because there are none to check — the check has moved from a parser to
// the kernel, and Scope.Judge is left with the question it can still answer,
// which is whether the job was given shell.run at all. If the boundary cannot
// be built — no Landlock, a Landlock too old to stop a file being emptied, a
// scope with no directory in it, or a scope that swallows Jarvix's own
// configuration — the step is refused with the reason, and the job parks on
// WhyUnconfined without anything having been dispatched.
//
// Refusing is the only other option, and it is the one the ticket makes
// non-negotiable: there is no third branch in which the command runs and the
// boundary is merely hoped for. A job that parks visibly is what this daemon
// did yesterday; a command that ran unconfined would be worse than that.
func (a *jobActor) commandSubject(scope jobs.Scope, s jobs.Step) (jobs.Attempt, error) {
	if err := a.confinement(scope).Check(confine.Available()); err != nil {
		var unconfinable *confine.ErrUnconfinable
		if errors.As(err, &unconfinable) {
			// Translated into the jobs package's own type rather than passed
			// through, so the runner's decision about how to park is keyed on a
			// kind and not on a sentence, and so a job's model never has to
			// import a kernel feature.
			return jobs.Attempt{}, &jobs.ErrUnconfinable{Because: unconfinable.Because}
		}
		return jobs.Attempt{}, err
	}
	return jobs.Attempt{Tool: s.Tool}, nil
}

// confinement is the boundary one job's scope asks the kernel for, and it is
// one function with two callers on purpose: commandSubject asks whether it can
// be built, and Do builds it. A daemon that computed the boundary twice would
// eventually check one thing and apply another, which is the shape of every
// interesting security bug.
//
// Reserved is the whole of Jarvix's own footprint — its configuration, its
// state, its data and its runtime directory. Naming them here rather than
// inside internal/confine keeps that package free of any opinion about this
// application's layout, and keeps the list next to the config.Paths it is read
// from, so a new Jarvix directory is added in the place somebody adding one is
// already looking.
//
// They are a refusal rather than a subtraction because Landlock cannot
// subtract: its rights are a union up the directory tree, so a rule granting
// less on a child does not narrow a rule granting more on its parent, and a
// rule with no rights at all is rejected. There is no ruleset meaning "all of
// ~ except ~/.config/jarvix". A scope that would need one is therefore declined
// — which keeps #109's wall standing against the new route a command opens: a
// job that cannot call config.write_entry must also not be able to run
// `sed -i` over config.toml.
func (a *jobActor) confinement(scope jobs.Scope) confine.Spec {
	return confine.Spec{
		Roots: scope.Roots,
		Reserved: []string{
			a.d.paths.Config, a.d.paths.State, a.d.paths.Data, a.d.paths.Runtime,
			filepath.Dir(a.d.paths.Socket),
		},
	}
}

// subjectlessReads are the tools whose subject is nothing outside Jarvix: they
// read the machine or Jarvix's own state and change neither.
//
// The list is explicit rather than derived from the undo classifier's read-only
// nature, and the difference matters: "changes nothing" and "touches nothing a
// scope could be about" are different claims, and only the second one is what
// makes a boundary check unnecessary. advisor.ask is read-only and is NOT here
// — it sends the user's words to another company's model, which is exactly the
// kind of reach a boundary should have an opinion about (and it is forbidden to
// every scope anyway).
var subjectlessReads = map[string]bool{
	tools.ListWindowsToolName:         true,
	tools.ListAppsToolName:            true,
	tools.ListManagedToolName:         true,
	tools.MemorySearchToolName:        true,
	tools.ConversationsSearchToolName: true,
	tools.ConfigListEntriesToolName:   true,
	tools.ConfigGetEntryToolName:      true,
	tools.ConfigReadSettingsToolName:  true,
	tools.ReminderListToolName:        true,
	tools.KnowledgeGetToolName:        true,
	tools.BriefingToolName:            true,
	tools.SituationToolName:           true,
	tools.JobsStatusToolName:          true,
}

// fileSubject reports the one file a tool rewrites, for the tools whose whole
// effect is one of Jarvix's own documents. The path is the real one, so a scope
// that admits the state directory admits them and a scope over the user's own
// tree does not.
func (a *jobActor) fileSubject(tool string) (string, bool) {
	switch tool {
	case tools.MemoryRememberToolName, tools.MemoryForgetToolName:
		return a.d.paths.MemoryFile(), true
	case tools.VocabularyTeachToolName, tools.VocabularyForgetToolName:
		return a.d.paths.VocabularyFile(), true
	case tools.ReminderSetToolName, tools.ReminderCancelToolName:
		return a.d.paths.RemindersFile(), true
	case tools.ArtifactToolName:
		// The artifact tool names its own file from its title, so the subject
		// is the directory it may create in — which is the honest boundary
		// question anyway: "may this job write artifacts at all".
		return filepath.Join(a.d.paths.State, "artifacts"), true
	default:
		return "", false
	}
}

// windowShaped reports whether a tool acts on one window, and how that window
// is named in its arguments.
func windowShaped(tool string) (bool, bool) {
	switch tool {
	case tools.FocusWindowToolName, tools.MoveWindowToolName,
		tools.CloseWindowToolName, tools.NameWindowToolName,
		tools.TypeTextToolName, tools.PressKeyToolName:
		return true, true
	default:
		return false, false
	}
}

// windowSubject resolves the window a step would act on, and refuses one Jarvix
// does not manage.
//
// This is #197's seam used for exactly what it was built for (ADR 0062): a job
// acts only in managed windows. The refusal is separate from the scope check
// and both must pass — the scope says which windows are in bounds, and
// management says whether the window is Jarvix's to touch at all. A window the
// user has not handed over is not made touchable by a scope that names its
// class.
func (a *jobActor) windowSubject(ctx context.Context, s jobs.Step, _ bool) (jobs.Attempt, error) {
	if a.d.windows == nil {
		return jobs.Attempt{}, fmt.Errorf("the window tools are switched off on this daemon")
	}
	var args struct {
		Window string `json:"window"`
		App    string `json:"app"`
		Match  string `json:"match"`
	}
	_ = json.Unmarshal([]byte(s.Args), &args)
	reference := firstNonEmpty(args.Window, args.App, args.Match)
	if reference == "" {
		return jobs.Attempt{}, fmt.Errorf("%s did not say which window it meant", s.Tool)
	}
	callCtx, cancel := context.WithTimeout(ctx, jobWindowTimeout)
	defer cancel()
	w, err := a.d.windows.RequireManaged(callCtx, reference)
	if err != nil {
		if errors.Is(err, tools.ErrNotManaged) {
			// Wrapped, not restated: the sentinel is what makes this refusal a
			// decision other code can key on rather than a string it matches.
			return jobs.Attempt{}, fmt.Errorf(
				"%w — a job only acts in windows you have handed over", err)
		}
		return jobs.Attempt{}, err
	}
	return jobs.Attempt{Tool: s.Tool, App: w.Class, Window: w.Describe()}, nil
}

// firstNonEmpty picks the first argument that says something.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// Judge implements jobs.Actor: the permission gate, exactly as a session
// applies it.
//
// The same registry, the same policy, the same generated question — the floor
// does not move because the caller is a job. What changes is only who waits for
// the answer, and that is the runner's business rather than the gate's.
//
// Conversation-scoped grants (#162) are deliberately NOT passed: a grant made
// in a conversation is scoped to that conversation, and a job outliving the
// conversation that created it must not inherit a permission the user gave for
// the length of a chat.
func (a *jobActor) Judge(_ context.Context, s jobs.Step) jobs.Verdict {
	verdict := a.d.registry.Check(ai.ToolCall{Name: s.Tool, Arguments: s.Args})
	switch verdict.Decision {
	case tools.PolicyDeny:
		reason := verdict.Rule
		if strings.TrimSpace(verdict.Reason) != "" {
			reason = verdict.Reason
		}
		return jobs.Verdict{Decision: jobs.Deny,
			Reason: "I'm not allowed to use " + s.Tool + " (" + reason + ")"}
	case tools.PolicyAsk:
		question := strings.TrimSpace(verdict.Summary)
		if question == "" {
			question = "Shall I " + desktop.ToolActionAsk(s.Tool) + "?"
		}
		if note := undo.CardNote(s.Tool); note != "" {
			// The one-way warning rides the question here exactly as it rides
			// a session's card (ADR 0064). A job's approval is asked hours
			// later, out of context, which is when knowing a decision is
			// one-way matters most.
			question = strings.TrimSuffix(question, " ") + " " + note
		}
		return jobs.Verdict{Decision: jobs.Ask, Question: question}
	default:
		return jobs.Verdict{Decision: jobs.Allow}
	}
}

// Do implements jobs.Actor: run the step with the job's id on the context, so
// every change it makes is recorded under that job.
//
// The undo record's id is read back out of the ACCOUNT rather than taken from
// the tool, which is the gathered-not-recalled rule applied to the runner's own
// bookkeeping: what went into the account is a fact the account holds, and a
// tool that said it recorded something is not evidence that it did.
func (a *jobActor) Do(ctx context.Context, job string, scope jobs.Scope,
	s jobs.Step) (jobs.Result, error) {
	before := len(a.d.account.Job(job))
	toolCtx := undo.WithJob(undo.WithRecorder(ctx, a.d.account), job)
	// The boundary rides the same context the account does, and for the same
	// reason: the registry dispatches thirty tools identically and only one of
	// them needs this. It is installed on EVERY step rather than only on the
	// ones that look like commands — a tool that grew a shell out of it later
	// would otherwise inherit an unconfined path by omission, and the shell
	// tool's own refusal (see internal/tools.Shell.Execute) is what turns a
	// missing boundary into a command that does not run rather than one that
	// runs unheld.
	toolCtx = confine.With(toolCtx, a.confinement(scope))
	said := a.d.registry.Execute(toolCtx, ai.ToolCall{Name: s.Tool, Arguments: s.Args})
	result := jobs.Result{Said: said, Failed: strings.HasPrefix(said, "error:")}
	if s.Tool == tools.ShellToolName && !result.Failed && !tools.CommandSucceeded(said) {
		// A command that ran and exited non-zero is not a tool that failed —
		// shell.run worked perfectly — but it IS a step whose work did not
		// happen, and the ledger's Failed flag is about the step (#222).
		//
		// Without this the report reads the entry as done and labels it with
		// the model's own line about what it was for, so a command the kernel
		// refused at the boundary would come back as "I did tidy the folder".
		// That is #71 with a shell behind it: a sentence about an effect
		// nobody observed, assembled out of an honest ledger that had been
		// told the wrong fact.
		result.Failed = true
	}
	if records := a.d.account.Job(job); len(records) > before {
		result.Undo = records[len(records)-1].ID
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// The situation source
// ---------------------------------------------------------------------------

// situationJobs is the jobs source for the situation report (#196, ADR 0061).
//
// It is an addition and not surgery, which is what that ADR promised and what
// its stub-source test proved: one Source value in bindSituation's list, one
// function here, and neither the composer, the ordering nor the speech budget
// changed. A parked job is the whole reason jobs need a source at all — it is
// work stopped until the user does something, which is the rank the report
// leads with.
//
// The lines carry no provenance reference. There is a window surface now (#221,
// ADR 0067), but no per-job reveal state to navigate to — the Jobs tab is one
// listing, not a listing plus a detail id — so a reference would open the tab
// and stop there. That is the Automations tab's weaker promise (see revealIn),
// and taking it on is a decision to make when there is a row worth landing on
// rather than a side effect of a tab existing.
func (d *Daemon) situationJobs(_ context.Context, at situation.Instant) ([]situation.Item, error) {
	if d.jobStore == nil {
		return nil, nil
	}
	var items []situation.Item
	counts := map[situation.Rank]int{}
	for _, job := range d.jobStore.List() {
		rank, report := jobRank(job, at)
		if !report {
			continue
		}
		counts[rank]++
		if counts[rank] > jobsNamed {
			continue
		}
		items = append(items, situation.Item{Rank: rank, Text: job.Spoken()})
	}
	for _, rank := range []situation.Rank{situation.NeedsYou, situation.InProgress,
		situation.Finished, situation.Failing} {
		if rest := counts[rank] - jobsNamed; rest > 0 {
			items = append(items, situation.Item{Rank: rank,
				Text: overflowSentence(rest, "jobs", jobRankVerb(rank))})
		}
	}
	return items, nil
}

// jobRank places one job in the report, and decides whether it is news at all.
//
// A finished job is news only until the user has looked — the report's
// interval-shaped half, so Instant's zero rule applies: with no record of a
// previous look, a fresh daemon reading out every job it ever ran would be
// answering a question about now with an archive.
func jobRank(job jobs.Job, at situation.Instant) (situation.Rank, bool) {
	switch job.State {
	case jobs.Parked:
		return situation.NeedsYou, true
	case jobs.Ready, jobs.Running:
		return situation.InProgress, true
	case jobs.Done, jobs.Stopped:
		return situation.Finished, !at.Since.IsZero() &&
			job.Ended.After(at.Since) && !job.Ended.After(at.Now)
	case jobs.Failed:
		return situation.Failing, true
	default:
		return situation.Housekeeping, false
	}
}

// briefJobs is the jobs source for the return briefing (ADR 0050).
//
// The second of the two places a parked job surfaces, and the more important
// one of the pair: a briefing is about a stretch the user was away for, which
// is exactly when a job will have stopped on something only they can settle.
// The user's binding decision was that a blocked job **never interrupts**, so
// the morning after is when they find out — and it must be there.
//
// One line per category at most, which is the briefing's own rule, so a
// machine with four jobs on it gets a counted sentence rather than four. That
// is the difference in shape from the situation source above: a briefing is a
// paragraph about a night, and a report is an answer about now.
func (d *Daemon) briefJobs(_ context.Context, since, now time.Time) ([]briefing.Line, error) {
	if d.jobStore == nil {
		return nil, nil
	}
	var waiting, finished, running []string
	for _, job := range d.jobStore.List() {
		switch {
		case job.State == jobs.Parked:
			waiting = append(waiting, job.Name)
		case job.State.Live():
			running = append(running, job.Name)
		case job.Ended.After(since) && !job.Ended.After(now):
			finished = append(finished, job.Name)
		}
	}
	var lines []briefing.Line
	if len(waiting) > 0 {
		lines = append(lines, briefing.Line{Category: briefing.Awaiting,
			Text: countedSentence(waiting, "job I'm working on has stopped and needs you",
				"jobs I'm working on have stopped and need you")})
	}
	if len(finished) > 0 {
		lines = append(lines, briefing.Line{Category: briefing.Completed,
			Text: countedSentence(finished, "job finished while you were away",
				"jobs finished while you were away")})
	}
	if len(running) > 0 {
		lines = append(lines, briefing.Line{Category: briefing.InProgress,
			Text: countedSentence(running, "job is still going", "jobs are still going")})
	}
	return lines, nil
}

// jobRankVerb words the overflow tail for a rank.
func jobRankVerb(rank situation.Rank) string {
	switch rank {
	case situation.NeedsYou:
		return "are waiting on you"
	case situation.InProgress:
		return "are running"
	case situation.Finished:
		return "have finished"
	default:
		return "have failed"
	}
}

// ---------------------------------------------------------------------------
// Shutdown
// ---------------------------------------------------------------------------

// jobsDrain stops the supervisor and waits for every step in flight.
func (d *Daemon) jobsDrain(ctx context.Context) error {
	if d.jobRunner == nil {
		return nil
	}
	return d.jobRunner.Drain(ctx)
}

// jobsInFlight reports what a give-up log should name.
func (d *Daemon) jobsInFlight() int {
	if d.jobRunner == nil {
		return 0
	}
	return d.jobRunner.InFlight()
}
