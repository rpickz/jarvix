package main

// `jarvix jobs` — work that outlives the conversation that asked for it, from
// the terminal (#221; the engine is #200 / ADR 0065, the surface ADR 0067).
//
// It prints the same account the window shows, because it is the same account:
// every sentence below arrives already composed from `jobs.list`, and this file
// chooses an order and a mark and nothing else. That is not tidiness — a job's
// report is the record of unsupervised work, and a CLI that phrased a job's
// standing its own way would be a second claim about what Jarvix has been doing
// while nobody watched.
//
// The pairing with `jarvix actions` is deliberate: that command answers "what
// did you change", this one answers "what are you still doing". Neither needs
// the daemon to be asked nicely, and neither goes through a model — which is
// the point of both, and matters most on the day the model is the thing
// behaving oddly.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
)

// jobControl is one thing the daemon says may be done with a job right now.
// The CLI reads `label` for the hint line and nothing else: whether a control
// is offered at all is the daemon's answer, and its absence is the refusal.
type jobControl struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// jobRow is the wire shape of one job from jobs.list. Every string on it is a
// finished sentence except the two identifiers.
type jobRow struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	State    string       `json:"state"`
	Goal     string       `json:"goal"`
	Scope    string       `json:"scope"`
	Progress string       `json:"progress"`
	Question string       `json:"question"`
	Detail   string       `json:"detail"`
	Report   string       `json:"report"`
	Why      string       `json:"why"`
	Controls []jobControl `json:"controls"`
}

// jobsView is the wire shape of jobs.list.
type jobsView struct {
	Jobs []jobRow `json:"jobs"`
	// Empty and Disclosure are the daemon's own sentences, printed verbatim
	// rather than reworded here for `jarvix actions`' reason: "you have given
	// me no work" and "I keep the last sixty" are promises, and a promise the
	// CLI phrased its own way would be a second promise (ADR 0013).
	Empty      string `json:"empty"`
	Disclosure string `json:"disclosure"`
	Path       string `json:"path"`
}

// jobOutcome is the wire shape of jobs.stop and jobs.answer.
type jobOutcome struct {
	Done    bool   `json:"done"`
	Refused bool   `json:"refused"`
	Spoken  string `json:"spoken"`
}

// cmdJobs prints every job: live ones first, newest first within each half,
// which is the order the store returns and the order the window shows.
func cmdJobs(paths config.Paths) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	var view jobsView
	if err := client.Call("jobs.list", nil, &view); err != nil {
		return err
	}
	if len(view.Jobs) == 0 {
		fmt.Println(view.Empty)
		fmt.Println(view.Disclosure)
		return nil
	}
	for _, job := range view.Jobs {
		fmt.Printf("%s%-8s %s\n", jobMark(job), job.Name, job.State)
		fmt.Println("         " + job.Goal)
		fmt.Println("         " + job.Scope)
		fmt.Println("         " + job.Progress)
		// The question, then the verbatim detail underneath it, in the same
		// order and with the same separation a confirmation card uses: the
		// question is what is being asked, the detail is what is being asked
		// ABOUT, and the second is the one a person actually judges.
		if job.Question != "" {
			fmt.Println("         " + job.Question)
		}
		if job.Detail != "" {
			fmt.Println("           " + job.Detail)
		}
		if job.Report != "" {
			fmt.Println("         " + job.Report)
		}
		if job.Why != "" {
			fmt.Println("         " + job.Why)
		}
		if hint := jobHint(job); hint != "" {
			fmt.Println("         " + hint)
		}
	}
	fmt.Println()
	fmt.Println(view.Disclosure)
	fmt.Println("the file is", view.Path)
	return nil
}

// jobMark is the one glyph a scanner reads down the left margin. It carries no
// information the row's own sentence does not already state in words — which is
// the rule, not an accident: a mark is a shortcut for the eye, never the only
// place a fact lives.
func jobMark(job jobRow) string {
	for _, c := range job.Controls {
		if c.ID == "approve" || c.ID == "answer" {
			return "? "
		}
	}
	for _, c := range job.Controls {
		if c.ID == "stop" {
			return "▸ "
		}
	}
	return "  "
}

// jobHint says what to type next, built from the controls the daemon offered
// rather than from the CLI's idea of what is possible. A job with nothing on
// offer gets no hint, which is the same withholding the window's missing button
// is: an instruction that would be refused is worse than none.
func jobHint(job jobRow) string {
	var parts []string
	for _, c := range job.Controls {
		switch c.ID {
		case "approve":
			parts = append(parts, "jarvix jobs answer "+job.Name+" yes")
		case "answer":
			parts = append(parts, "jarvix jobs answer "+job.Name+" <your answer>")
		case "decline":
			parts = append(parts, "jarvix jobs answer "+job.Name+" no")
		case "stop":
			parts = append(parts, "jarvix jobs stop "+job.Name)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "   —   ")
}

// cmdJobsStop stops one job.
//
// A refusal exits non-zero, for `jarvix undo`'s reason: "I won't, because it
// has already finished" is not a success, and a script that ran this and
// carried on as though the work had been halted would be exactly the quiet
// wrongness this feature exists to remove.
func cmdJobsStop(paths config.Paths, name string) error {
	return jobAction(paths, "jobs.stop", map[string]any{"name": name})
}

// cmdJobsAnswer settles what a parked job is waiting for.
//
// `yes` and `no` are the whole vocabulary for an approval, because an approval
// IS a yes or a no — the user is being asked about an action they have already
// been shown. Anything else is the answer to a decision the planner could not
// make, passed through verbatim and approved: a job that asked "which of these
// two directories did you mean?" is not answered by a boolean.
func cmdJobsAnswer(paths config.Paths, name string, words []string) error {
	return jobAction(paths, "jobs.answer", answerParams(name, words))
}

// answerParams reads what was typed as the job's answer. Split out so the
// reading is exercised without a daemon: it is the one decision this file makes
// about a user's words, and getting it wrong would send a yes to a question
// that was answered no.
func answerParams(name string, words []string) map[string]any {
	said := strings.TrimSpace(strings.Join(words, " "))
	params := map[string]any{"name": name, "approved": true}
	switch strings.ToLower(said) {
	case "yes":
	case "no":
		params["approved"] = false
	default:
		params["answer"] = said
	}
	return params
}

// jobAction calls one acting verb and prints what the daemon said about it.
func jobAction(paths config.Paths, method string, params map[string]any) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	var out jobOutcome
	if err := client.Call(method, params, &out); err != nil {
		return err
	}
	fmt.Println(out.Spoken)
	if out.Refused || !out.Done {
		// The reason is already printed; errChecksFailed is the CLI's "already
		// said, just exit 1".
		return errChecksFailed
	}
	return nil
}

// errJobsUsage is the shared refusal for a malformed invocation.
var errJobsUsage = errors.New(
	"usage: jarvix jobs | jarvix jobs stop <name> | jarvix jobs answer <name> yes|no|<your answer>")
