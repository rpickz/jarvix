package daemon

// The jobs SURFACE (#221, ADR 0067): `jobs.list`, `jobs.stop` and
// `jobs.answer`, and the report all three are read through.
//
// #200 shipped the engine complete and every surface deferred. The only way to
// learn what a job was doing was to ask the model to call a tool, which is the
// wrong shape for this feature specifically: the premise of the operator
// direction (#195) is that the user is the MANAGER of this machine, and a
// manager's most basic act is to look at what is in flight. A job you can only
// observe by successfully talking to a model is a job you cannot manage when
// the model is the thing behaving oddly.
//
// Three properties of the shape, before the code.
//
//   - **Every word here is composed here.** ADR 0013 with no exceptions: the
//     window and the CLI place sentences and never write them. A surface that
//     worded a job's standing would be the second place in this feature where
//     two readers of one file could disagree about what a job is doing — and
//     the thing they would disagree about is unsupervised work.
//   - **Every control is offered here too.** A row's Answer, Say-no and Stop
//     controls come back as a daemon-composed list, and the same
//     `Job.StopOffer` / `Job.AnswerOffer` the runner refuses with decides
//     whether each one is in it. That is ADR 0066's lesson applied to work:
//     a control that is withheld and an action that is refused cannot explain
//     the same policy differently, because there is one sentence.
//   - **Nothing here is composed by a model.** The report is #200's honesty
//     rule read back out of the ledger — `Job.Report`, `Job.Progress`,
//     `Scope.Stated` — and an unverified step is shown as unverified. Not one
//     sentence on this surface has been near a provider.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/jobs"
	"github.com/rpickz/jarvix/internal/undo"
)

// jobsEmptySentence is what the listing says when there is no work at all.
//
// One sentence, in one place, like the account's (ADR 0066): "you have not
// given me anything to do" is a claim about the machine, and the CLI and the
// window saying it two ways would be two claims.
const jobsEmptySentence = "You haven't given me any work to do."

// jobsAccount is what the report is composed from: the jobs, a clock, and the
// one thing the store cannot answer — what verbatim detail a session's
// confirmation card would show for the step a job parked on.
//
// A struct of seams rather than a *Daemon, for undoAccount's reason: the whole
// vocabulary of this surface is decided below, and a vocabulary only reachable
// through a wired daemon on a socket is one nobody writes cases for.
type jobsAccount struct {
	jobs []jobs.Job
	// now is the daemon's clock, so how long ago something happened is phrased
	// against the clock the jobs were written with rather than against the
	// clock of whichever machine is rendering them.
	now time.Time
	// path is the file jobs live in, so every surface can say where to read it
	// by hand.
	path string
	// detail answers what a step's confirmation card showed underneath its
	// question. Nil means this daemon cannot say, which is honest and renders
	// as no detail rather than as a guess.
	detail func(jobs.Step) string
	// gate answers what the permission gate says about a kept step as the tier
	// stands right now, so a row withholds Approve on exactly the tier the
	// runner would refuse it on (#225). It is the runner's own gate rather than
	// a second reading of the policy. Nil means this daemon cannot ask, which
	// leaves the offer standing — the enforcement is the runner's, and a listing
	// that guessed would be the second place the rule lives.
	gate jobs.Gate
}

// jobsAccount assembles the report's inputs from the running daemon.
func (d *Daemon) jobsAccount() jobsAccount {
	a := jobsAccount{now: time.Now().UTC()}
	if d.jobStore != nil {
		a.jobs, a.path = d.jobStore.List(), d.jobStore.Path()
	}
	if d.registry != nil {
		a.detail = func(s jobs.Step) string {
			return d.registry.ConfirmationDetail(ai.ToolCall{Name: s.Tool, Arguments: s.Args})
		}
		// The runner's own actor, asked the runner's own question. Constructed
		// here rather than held on the daemon because jobActor is a stateless
		// wrapper over *Daemon; what matters is that this is jobActor.Judge and
		// not a second walk of the policy.
		actor := &jobActor{d: d}
		a.gate = func(s jobs.Step) jobs.Verdict { return actor.Judge(context.Background(), s) }
	}
	return a
}

// jobsViewReport renders every job for the wire.
//
// The ledger never travels, and that is the store's own rule (see Store.emit)
// applied to a read rather than to an event: a ledger line carries what a tool
// said, which for a job that read a file is the contents of the user's work.
// What travels is the account composed FROM it — how many steps, how many
// changed something, how many could not be confirmed — which is what a manager
// acts on and is not the file.
func jobsViewReport(a jobsAccount) map[string]any {
	rows := make([]map[string]any, 0, len(a.jobs))
	for _, job := range a.jobs {
		rows = append(rows, jobRowReport(job, a))
	}
	return map[string]any{
		"jobs":       rows,
		"empty":      jobsEmptySentence,
		"disclosure": jobsDisclosure(),
		"path":       a.path,
	}
}

// jobsDisclosure states the two bounds a listing is showing you the inside of.
//
// The account discloses its bound on every read (ADR 0064) and this is the same
// promise for work: a manager looking at four running jobs needs to know that
// four is the ceiling rather than the coincidence, because "nothing else is
// running" and "nothing else may run" are different facts.
func jobsDisclosure() string {
	return "I run at most " + strconv.Itoa(jobs.MaxLive) +
		" jobs at once and keep the last " + strconv.Itoa(jobs.MaxJobs) +
		", finished ones included."
}

// jobRowReport renders one job: what it is, where it stands, and what may be
// done about it.
//
// Every field is a finished sentence except `id`, `name` and the control list,
// and that is deliberate down to the goal: the goal travels inside a sentence
// the daemon wrote rather than as a bare string a client would have to
// introduce, because a lead-in is wording (ADR 0066). The goal itself is
// verbatim inside it and is never rewritten — it is the only record of what was
// actually asked for.
func jobRowReport(job jobs.Job, a jobsAccount) map[string]any {
	row := map[string]any{
		"id":       job.ID,
		"name":     job.Name,
		"title":    job.Title(),
		"state":    jobStandingSentence(job, a.now),
		"goal":     "You asked for “" + job.Goal + "”.",
		"scope":    "It may act " + job.Scope.Stated() + ".",
		"progress": "It has done " + job.Progress() + ".",
		"controls": jobControls(job, a.gate),
	}
	if job.State == jobs.Parked {
		if ask := strings.TrimSpace(job.Question.Ask); ask != "" {
			// Verbatim, in the words of whoever parked it — the gate's own
			// generated question for an approval, internal/jobs' wording for a
			// boundary. Re-wording it here would put a second sentence between
			// the user and the thing they are being asked about.
			row["question"] = ask
		}
		if detail := jobApprovalDetail(job, a); detail != "" {
			// The verbatim detail a session's confirmation card shows under its
			// question — the exact thing being approved, never the model's
			// account of it (#200's contract, ADR 0053's ground truth).
			row["detail"] = detail
		}
	}
	if !job.State.Live() {
		// The ledger-derived account, unverified steps first. Only for a job
		// that has ended: a running job's report would be a progress note
		// wearing a conclusion's clothes.
		row["report"] = job.Report()
	}
	if _, because := job.ApproveOffer(a.gate); because != "" && job.State == jobs.Parked {
		// Parked, and no yes will move it — a boundary, a denial, or an approval
		// whose tool the user has turned off since it was asked about (#225).
		// The row says so where the control would have been, in the sentence the
		// verb itself refuses with.
		row["why"] = because
	}
	return row
}

// jobApprovalDetail is the verbatim detail for a job parked at the gate.
//
// Only for WhyApproval, and that is the honest bound: the other five parking
// reasons are not questions about a pending call — a boundary, a denial, an
// unreadable subject and a stuck planner have nothing a card would show — and a
// planner's own question to the user is already the whole of what it is asking.
func jobApprovalDetail(job jobs.Job, a jobsAccount) string {
	if job.Question.Why != jobs.WhyApproval || a.detail == nil {
		return ""
	}
	if strings.TrimSpace(job.Question.Step.Tool) == "" {
		return ""
	}
	return strings.TrimSpace(a.detail(job.Question.Step))
}

// jobStandingSentence is where one job stands, in one sentence, including when.
//
// State in words rather than in a colour or a code, because a listing read by
// somebody who cannot tell two greys apart — or by a screen reader, which never
// sees the fill — must still answer "is this waiting for me?". The time is in
// it rather than beside it for the same reason the account puts `when` on the
// wire: a client rendering this has no clock it can measure the daemon's with,
// and "parked four minutes ago" and "parked yesterday" are different situations.
func jobStandingSentence(job jobs.Job, now time.Time) string {
	switch job.State {
	case jobs.Parked:
		since := undo.Ago(now, job.Question.At)
		if job.Question.Why.Answerable() {
			return job.Title() + " is waiting on you — parked " + since + "."
		}
		// A boundary or a denial. It is stopped, not waiting, and the
		// difference is what the user has to do next.
		return job.Title() + " has stopped and needs you — parked " + since + "."
	case jobs.Ready:
		// Work left and nobody on it: between steps, or waiting for the
		// supervisor to pick it up. "Running" would overstate it.
		return job.Title() + " is queued to carry on — started " +
			undo.Ago(now, job.Started) + "."
	case jobs.Running:
		return job.Title() + " is running — started " + undo.Ago(now, job.Started) + "."
	case jobs.Done:
		return job.Title() + " finished " + undo.Ago(now, job.Ended) + "."
	case jobs.Stopped:
		return job.Title() + " stopped " + undo.Ago(now, job.Ended) + "."
	case jobs.Failed:
		return job.Title() + " failed " + undo.Ago(now, job.Ended) + "."
	default:
		return job.Title() + " is in a state I can't describe, which is a fault of mine."
	}
}

// The control identities a surface sends back. Closed, and small, because each
// one is a different verb with a different shape — a client that invented a
// fourth would be inventing an action.
const (
	jobControlApprove = "approve"
	jobControlAnswer  = "answer"
	jobControlDecline = "decline"
	jobControlStop    = "stop"
)

// jobControls is what may be done with this job right now, worded here.
//
// The list IS the eligibility. A control that is not offered is absent rather
// than present-and-dead: the shared collection row skips an empty label in the
// focus chain, so withholding the offer withholds the tab stop too, and a
// keyboard user never lands on a button that could only refuse (ADR 0066).
//
// Approve and Answer are the same verb wearing two labels, and the split is the
// question's own. An approval the gate demanded is a yes/no about an action the
// user has already been shown, so pressing the button IS the whole answer. A
// decision the planner could not settle needs the user's own words, so that
// control carries a field — and the field's label is here too, because a label
// is wording.
//
// Approve is offered on the stricter question (Job.ApproveOffer, #225): a tool
// re-tiered to deny while the job sat parked is one the runner will refuse on
// resumption, so the button goes rather than becoming one that only apologises.
// Say-no survives it — stopping a job is not a use of the tool it was waiting
// on, and a user who has just turned that tool off is exactly the user who wants
// to end the job cleanly.
func jobControls(job jobs.Job, gate jobs.Gate) []map[string]any {
	out := make([]map[string]any, 0, 3)
	if ok, _ := job.AnswerOffer(); ok {
		approve, _ := job.ApproveOffer(gate)
		switch {
		case job.Question.Why == jobs.WhyApproval && approve:
			out = append(out, map[string]any{
				"id": jobControlApprove, "label": "Approve",
				"name": "Approve what " + job.Name + " is waiting for and let it carry on",
			})
		case job.Question.Why == jobs.WhyApproval:
			// Withheld, and the row's `why` says so. Absent rather than
			// present-and-dead: the collection row skips an empty label in the
			// focus chain, so no keyboard user lands on it either.
		default:
			out = append(out, map[string]any{
				"id": jobControlAnswer, "label": "Send your answer",
				"name":        "Answer what " + job.Name + " is waiting for and let it carry on",
				"words":       true,
				"field_label": "Your answer to " + job.Name,
			})
		}
		out = append(out, map[string]any{
			"id": jobControlDecline, "label": "Say no",
			"name": "Say no to what " + job.Name + " is waiting for, which stops it",
		})
	}
	if ok, _ := job.StopOffer(); ok {
		out = append(out, map[string]any{
			"id": jobControlStop, "label": "Stop",
			"name": "Stop the " + job.Name + " job",
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// The verbs
// ---------------------------------------------------------------------------

// registerJobMethods adds the jobs.* verbs: the listing, and the two things a
// manager does with a piece of work by hand.
//
// **No confirmation card in front of either action**, and that is ADR 0066's
// argument restated rather than a new one: the card exists for something the
// MODEL asked to do (ADR 0053). These are the manager's own instruction, given
// by hand, on a row that names the job and says what pressing it will do.
//
// The gate's floor is untouched by all three. `jobs.stop` and `jobs.answer` the
// TOOLS keep their tiers exactly — a model still asks before it stops a job or
// answers for the user — and approving here is not a way round the gate but the
// gate being answered: the job parked BECAUSE the gate demanded a confirmation,
// the question travels verbatim with the detail underneath it, and the runner
// then executes the step it kept whole rather than re-planning (#200).
func (d *Daemon) registerJobMethods() {
	d.server.Handle("jobs.list", func(json.RawMessage) (any, error) {
		return jobsViewReport(d.jobsAccount()), nil
	})

	d.server.Handle("jobs.stop", func(params json.RawMessage) (any, error) {
		p := struct {
			Name string `json:"name"`
		}{}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "jobs.stop params: %v", err)
			}
		}
		if strings.TrimSpace(p.Name) == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "jobs.stop needs the job's name")
		}
		if d.jobRunner == nil {
			return jobRefusal("Jobs are not available on this daemon."), nil
		}
		job, err := d.jobRunner.Stop(p.Name, "You stopped it from the window.")
		if err != nil {
			// A refusal is a normal reply carrying its own sentence, not an
			// error: a job that had already finished is not a fault, and a
			// surface that saw a -32602 would have to invent an explanation.
			return jobRefusal(err.Error()), nil
		}
		return map[string]any{"done": true, "refused": false,
			"name": job.Name, "spoken": job.Spoken()}, nil
	})

	d.server.Handle("jobs.answer", func(params json.RawMessage) (any, error) {
		p := struct {
			Name     string `json:"name"`
			Approved bool   `json:"approved"`
			Answer   string `json:"answer"`
		}{}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "jobs.answer params: %v", err)
			}
		}
		if strings.TrimSpace(p.Name) == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "jobs.answer needs the job's name")
		}
		if d.jobRunner == nil {
			return jobRefusal("Jobs are not available on this daemon."), nil
		}
		job, err := d.jobRunner.Answer(p.Name, p.Approved, p.Answer)
		if err != nil {
			return jobRefusal(err.Error()), nil
		}
		return map[string]any{"done": true, "refused": false,
			"name": job.Name, "spoken": job.Spoken()}, nil
	})
}

// jobRefusal is a declined action reported as one. The sentence is whoever
// declined it — the runner's own refusal, which is Job.StopOffer's or
// Job.AnswerOffer's, which is the same sentence the listing put on the row.
func jobRefusal(spoken string) map[string]any {
	return map[string]any{"done": false, "refused": true, "spoken": spoken}
}
