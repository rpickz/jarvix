// Package jobs is work that outlives the conversation that asked for it
// (#200, ADR 0065, the centre of the operator direction #195).
//
// Every capability Jarvix had before this lived and died inside one exchange.
// That is what makes it an assistant you direct turn by turn, and it is why a
// *direction* — "get the CI green", "tidy my downloads" — could not be given
// at all: there was no object to hold one. A job is that object. It has a goal
// in the user's own words, a scope it may act within, a state you can ask
// about, a checkpoint it resumes from, an interrupt, and a report.
//
// Five stances shape everything below, and each is a decision the user took
// rather than an implementation detail.
//
// **A scope is enforced, never described.** The boundary is not a paragraph in
// a prompt asking the model to stay inside a directory. It is a Go function
// this package runs against a subject the DAEMON read out of the proposed call
// (Scope.Judge), before anything is dispatched. A model that proposes an
// action outside the boundary does not get refused politely and asked to try
// again — the job stops and parks with the reason, because a planner that has
// wandered out of its scope once has told you something about the rest of its
// plan.
//
// **The gate's floor does not move, however long a job has run.** An
// irreversible action inside the scope still asks. What changes is who waits:
// a session blocks on the question because a person is there, and a job cannot,
// because the whole point is that nobody may be. So a job PARKS on the question
// — the question becomes state on disk — and answering it later resumes from
// the checkpoint.
//
// **Parking is state, not a paused goroutine.** A parked job is a row in a TOML
// file with its scope, its ledger and its question intact. Nothing is holding a
// channel open, so a restart costs it nothing, and one job's parking cannot
// stall another because there is no shared thread to stall.
//
// **A report is gathered, not recalled.** This project's scar is #71 — a small
// model narrating actions it never performed — and long unsupervised work is
// exactly where that failure gets expensive. So no sentence in a job's report
// is composed by a model. Every line is read back out of the ledger the runner
// wrote as each step finished, and a step whose outcome the runner could not
// confirm is reported as unverified rather than as done. A job that cannot
// verify what it did says so.
//
// **A job is not a privilege escalation.** The tools that write the assistant's
// own configuration, its advisors and its credentials are refused to every
// scope, whatever the scope says (Forbidden, #109's wall). A boundary the
// bounded thing can widen is not a boundary.
package jobs

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// State is where a job is. Six, and the set is closed: every one of them is a
// different sentence to the user, and a seventh would be a state nobody could
// be told about.
type State string

const (
	// Ready is a job with work left and nobody doing it. It is what a restart
	// finds, what a resumed job becomes, and the only state the supervisor
	// picks up.
	Ready State = "ready"
	// Running is a job a runner is acting on right now. It is written to disk
	// while it runs so that a daemon which died mid-step can say so; a Running
	// job found at load is adopted back to Ready (see Store.load).
	Running State = "running"
	// Parked is a job stopped on something only the user can settle: a
	// question at the gate, a decision it needs, or a boundary it hit. It is
	// waiting, it is not running, and it is not finished.
	Parked State = "parked"
	// Done is a job whose planner said the goal was met.
	Done State = "done"
	// Stopped is a job the user stopped.
	Stopped State = "stopped"
	// Failed is a job that could not go on for a reason that is not the
	// user's to answer — the planner unreachable, the store unwritable.
	Failed State = "failed"
)

// Live reports whether a job still has somewhere to go. It is the one
// definition of "unfinished", so the supervisor, the situation source and the
// report cannot disagree about which jobs are news.
func (s State) Live() bool {
	switch s {
	case Ready, Running, Parked:
		return true
	default:
		return false
	}
}

// Why is a parking reason. It is a small closed vocabulary rather than free
// text because the reason decides what happens next — an approval can be
// answered yes, a boundary cannot — and because the situation report words
// each one differently.
type Why string

const (
	// WhyApproval is the gate asking about an irreversible action inside the
	// scope. Answering yes resumes the job at exactly this step.
	WhyApproval Why = "approval"
	// WhyDecision is the planner asking the user something it cannot settle
	// itself.
	WhyDecision Why = "decision"
	// WhyOutOfScope is an attempt outside the boundary. Nothing was done. It
	// is not answerable — the way out is a new job with a scope that admits
	// the work, which is a decision the user makes deliberately rather than a
	// yes/no they nod through.
	WhyOutOfScope Why = "out_of_scope"
	// WhyRefused is the permission gate denying the action outright. Also not
	// answerable: a denied tool is denied by standing configuration, and a job
	// is not a way to ask again.
	WhyRefused Why = "refused"
	// WhyUnclear is the daemon being unable to read what a proposed step would
	// touch. Refusing to guess is the whole of the enforcement promise: a
	// subject nobody can name cannot be checked against a boundary.
	WhyUnclear Why = "unclear"
	// WhyStuck is the planner failing — unreachable, or proposing nothing this
	// package can act on.
	WhyStuck Why = "stuck"
)

// Answerable reports whether the user saying "yes" can resume the job. Only
// the two questions are: a boundary and a denial are not opinions.
func (w Why) Answerable() bool { return w == WhyApproval || w == WhyDecision }

// Question is what a parked job is waiting for.
type Question struct {
	// Why is the kind of parking, which decides whether an answer resumes it.
	Why Why
	// Ask is the sentence the user is asked or told, already worded. It is
	// composed by whoever parked the job — the gate's own generated question
	// for an approval, this package's wording for a boundary — because the
	// code that knows what happened is the only code that can say it.
	Ask string
	// At is when the job parked.
	At time.Time
	// Step is the step that was pending when it parked, kept whole so that
	// answering resumes THIS action rather than asking the planner again. That
	// is the difference between resuming from a checkpoint and restarting: a
	// planner asked twice may answer differently, and the user approved the
	// action they were shown.
	Step Step
}

// Step is one thing a job does: a tool call, or the planner saying it is
// finished.
//
// It carries the model's own one-line account of what it is doing (Intent)
// alongside the call, and the two are kept apart everywhere downstream. Intent
// is what the model SAID; Tool and Args are what the daemon will actually run,
// and every check in this package runs against the second.
type Step struct {
	// Intent is the model's one line about what this step is for. It appears
	// in the ledger next to what actually happened, never instead of it.
	Intent string
	// Tool is the gate identity to call. Empty with Finished set means the
	// planner is declaring the goal met.
	Tool string
	// Args is the tool's raw JSON argument object.
	Args string
	// Finished is the planner saying there is nothing left to do.
	Finished bool
	// Question is set when the planner needs the user to decide something. The
	// job parks on it rather than guessing.
	Question string
}

// Entry is one line of the ledger: what was attempted and what is known to
// have happened. The ledger is the whole factual basis of a job's report, and
// nothing else is.
type Entry struct {
	// At is when the step finished (or was abandoned), UTC.
	At time.Time
	// Intent is the model's line, kept for the record.
	Intent string
	// Tool is what was called.
	Tool string
	// Said is what the daemon observed: the tool's own result, trimmed. It is
	// the gathered fact.
	Said string
	// Verified is whether the runner saw this step through to a result it
	// read. False means the step was started and the outcome is genuinely
	// unknown — a stop mid-flight, a daemon that died between dispatch and
	// result — and the report says so in those words rather than assuming
	// either way.
	Verified bool
	// Failed is whether the tool reported a failure. Only meaningful when
	// Verified.
	Failed bool
	// Undo is the id of the account record this step produced, empty when the
	// step changed nothing recordable. It is the join to #201's account.
	Undo string
}

// Job is one piece of work.
type Job struct {
	// ID is "j3", minted by the store and never reused.
	ID string
	// Name is the user's short handle for it, so several jobs are
	// individually addressable by a word rather than by an id.
	Name string
	// Goal is the direction in the user's own words, stored verbatim. It is
	// never rewritten, summarised or normalised: it is the only record of what
	// was actually asked for, and a job that had paraphrased its own
	// instruction could not be audited against it.
	Goal string
	// Scope is the boundary. Fixed at creation and never widened — see
	// Store.widen's absence.
	Scope Scope
	// State is where it is.
	State State
	// Question is what it is parked on; the zero value when it is not parked.
	Question Question
	// InFlight is the step that has been dispatched and not yet finished. It
	// is written to disk BEFORE the action is taken and cleared when its
	// outcome is recorded, which is the whole of how a daemon that dies
	// mid-step can be honest about it: a job found with one is a job whose
	// action was started and whose end nobody saw, and the store adopts it
	// into the ledger as unverified rather than guessing either way.
	//
	// It is deliberately not folded into Question. A question is something a
	// person answers; this is something the daemon owes an account of, and one
	// field carrying both would make "waiting for you" and "I don't know what
	// happened" indistinguishable in the file the user reads.
	InFlight Step
	// Ledger is every step, oldest first. It is the checkpoint: a job resumes
	// by handing the planner what has already happened, not by replaying it.
	Ledger []Entry
	// Steps is how many steps it has been allowed. It is a bound, not a
	// counter — see MaxSteps.
	Steps int
	// Started, Ended are the edges. Ended is zero while the job is live.
	Started time.Time
	Ended   time.Time
	// Closing is the one sentence recorded when the job left the live states,
	// worded by whatever ended it.
	Closing string
}

// MaxSteps bounds one job.
//
// It is a ceiling on a runaway plan rather than a guess at how much work a
// direction needs, and it is deliberately not configurable. The failure it
// guards against is a planner that never says "finished" — a loop that reads
// the same file forty times, each read perfectly inside the scope and perfectly
// pointless — and the honest response to that is to stop and say so, which is
// what hitting the bound does: the job parks as stuck, with its ledger, and the
// user can see exactly what it spent the steps on.
const MaxSteps = 40

// Bounded reports whether the job has used its allowance.
func (j Job) Bounded() bool { return j.Steps >= MaxSteps }

// Acted reports how many ledger entries changed something, which is what the
// report counts. A step that read and reported is work but not a change, and
// conflating the two would let a job that looked at forty files describe itself
// as having done forty things.
func (j Job) Acted() int {
	n := 0
	for _, e := range j.Ledger {
		if e.Undo != "" {
			n++
		}
	}
	return n
}

// Unverified reports how many steps ended without the runner reading a result.
// It is the number the report leads its honesty caveat with.
func (j Job) Unverified() int {
	n := 0
	for _, e := range j.Ledger {
		if !e.Verified {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// The scope
// ---------------------------------------------------------------------------

// Scope is the boundary one job may act within, and it is the whole of the
// autonomy decision: inside it Jarvix acts without asking, outside it it stops.
//
// Three faces, because three is what the daemon can actually check:
//
//   - **Tools** is the set of gate identities the job may use at all. Exact
//     names, no patterns: a boundary you have to read a glob to understand is
//     a boundary nobody reads.
//   - **Roots** is the directories it may touch. A step whose subject is a
//     path outside every root is out of scope, and containment is decided
//     after symlinks are resolved, so a link out of the tree is not a way
//     through the wall.
//   - **Apps** is the window classes it may act on. A window action is
//     additionally refused unless Jarvix MANAGES the window (ADR 0062) — the
//     scope says which windows are in bounds, and management says whether the
//     window is Jarvix's to touch at all. Both must hold.
//
// What a scope may NOT contain is Forbidden, below. That list is not a default
// a scope overrides; it is checked after everything else and wins.
type Scope struct {
	// Tools are the gate identities in bounds, sorted and de-duplicated by
	// Validate.
	Tools []string
	// Roots are absolute directories in bounds.
	Roots []string
	// Apps are window classes in bounds, matched case-insensitively the way
	// every other class comparison in this repository is.
	Apps []string
}

// Forbidden are the tool identities no scope may contain, whatever it says.
//
// They are the assistant's own governance: the configuration that decides what
// Jarvix may do, the advisors it consults, and the credentials it holds. #109
// built a wall around exactly this space for the model; a job is a model with
// more time, so the wall is the same height here. A job that could write
// `[tools.policy]` would be a job that could widen its own scope, and a
// boundary the bounded thing can move is decoration.
//
// The names are string literals rather than imports of internal/tools's
// constants, because internal/tools imports THIS package for the job verbs and
// the cycle would be real. TestTheForbiddenNamesAreTheRealToolNames in
// internal/tools pins them against the constants, so the two cannot drift.
var Forbidden = []string{
	"config.write_entry",
	"config.delete_entry",
	"config.write_setting",
	"script.run",
	"intent.run",
	"advisor.ask",
	"desktop.manage_window",
}

// forbidden is Forbidden as a set, built once.
var forbidden = func() map[string]bool {
	m := make(map[string]bool, len(Forbidden))
	for _, t := range Forbidden {
		m[t] = true
	}
	return m
}()

// ErrUnenforceable is what a scope that cannot be checked becomes. A job may
// not start without a scope it can enforce, and "I could not enforce it" is
// therefore a refusal to create the job rather than a job created leniently.
type ErrUnenforceable struct{ Because string }

func (e *ErrUnenforceable) Error() string { return "that scope cannot be enforced: " + e.Because }

// Validate normalises a scope and refuses one that cannot be enforced.
//
// The refusals are all the same shape and all the same argument: a scope with
// nothing in it, or with a boundary this daemon has no way to check, would let
// a job act freely while wearing a boundary's name. Better to refuse to start.
func (s Scope) Validate() (Scope, error) {
	out := Scope{
		Tools: tidy(s.Tools, false),
		Roots: make([]string, 0, len(s.Roots)),
		Apps:  tidy(s.Apps, true),
	}
	if len(out.Tools) == 0 {
		return Scope{}, &ErrUnenforceable{Because: "it names no tools, so there is nothing it permits"}
	}
	for _, t := range out.Tools {
		if forbidden[t] {
			return Scope{}, &ErrUnenforceable{Because: t +
				" governs what I am allowed to do, and a job may never change that"}
		}
	}
	for _, raw := range tidy(s.Roots, false) {
		if !filepath.IsAbs(raw) {
			return Scope{}, &ErrUnenforceable{Because: raw +
				" is not an absolute path, and I cannot tell what it would mean later"}
		}
		out.Roots = append(out.Roots, resolved(filepath.Clean(raw)))
	}
	sort.Strings(out.Roots)
	if len(out.Roots) == 0 && len(out.Apps) == 0 {
		return Scope{}, &ErrUnenforceable{
			Because: "it names neither a directory nor an application, so it has no boundary at all"}
	}
	return out, nil
}

// tidy trims, drops blanks, lowercases when asked, sorts and de-duplicates.
func tidy(in []string, lower bool) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if lower {
			s = strings.ToLower(s)
		}
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Attempt is what a proposed step would actually touch, as the DAEMON read it
// out of the call — never as the model described it.
//
// The distinction is the feature. A model that says "I'll tidy ~/Downloads"
// while passing a path in /etc has told the user one thing and the machine
// another, and the only reading that can be enforced is the second.
type Attempt struct {
	// Tool is the gate identity.
	Tool string
	// Paths are every filesystem path the call would touch, absolute. Empty
	// for a call that touches none.
	Paths []string
	// App is the class of the window the call would act on, empty for a call
	// that acts on no window.
	App string
	// Window is how the window was named, for the wording of a refusal.
	Window string
}

// Ruling is the boundary's answer.
type Ruling struct {
	// OK is whether the attempt is inside the scope.
	OK bool
	// Because is the sentence the job parks with when it is not. Worded here,
	// daemon-side, and specific: "outside the scope" tells the user nothing
	// they can act on, and the thing they need to know is which boundary and
	// which subject.
	Because string
}

// Judge decides whether one attempt is inside this scope. It is the whole of
// daemon-side scope enforcement, and it is deliberately a pure function of a
// struct the caller filled in from parsed arguments: nothing it reads came from
// a sentence.
func (s Scope) Judge(a Attempt) Ruling {
	if forbidden[a.Tool] {
		// Re-checked here as well as at Validate, because a scope can arrive
		// off disk: a hand-edited jobs.toml naming config.write_entry must not
		// become a job that may write configuration.
		return Ruling{Because: a.Tool +
			" governs what I am allowed to do, and I will not change that inside a job"}
	}
	if !contains(s.Tools, a.Tool) {
		return Ruling{Because: "I would have had to use " + a.Tool +
			", which is not one of the tools you gave this job"}
	}
	for _, p := range a.Paths {
		if !s.holds(p) {
			return Ruling{Because: "it would have touched " + p +
				", which is outside " + joinNaturally(s.Roots, "and")}
		}
	}
	if a.App != "" && !contains(s.Apps, strings.ToLower(a.App)) {
		where := a.Window
		if strings.TrimSpace(where) == "" {
			where = a.App
		}
		return Ruling{Because: "it would have acted on " + where +
			", and this job is only for " + joinNaturally(s.Apps, "and")}
	}
	return Ruling{OK: true}
}

// holds reports whether one path is inside any root.
//
// Symlinks are resolved on both sides before the comparison, and for a path
// that does not exist yet the deepest ancestor that does is resolved instead —
// so creating a file through a link that points out of the tree is caught at
// the directory, which is where the escape actually is. A path that cannot be
// resolved at all is NOT held: refusing is the only safe direction when the
// question is "is this inside the wall".
func (s Scope) holds(path string) bool {
	if len(s.Roots) == 0 {
		return false
	}
	if !filepath.IsAbs(path) {
		return false
	}
	real := resolved(filepath.Clean(path))
	for _, root := range s.Roots {
		if real == root || strings.HasPrefix(real, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// contains is an exact membership test over a sorted, de-duplicated list.
func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// Stated is the scope read back to the user in one sentence, which is what the
// confirmation shows before a job begins.
//
// It says every face of the boundary, including the empty ones, because a
// listener told "inside ~/code" and not told which tools has been shown half a
// boundary and will assume the other half is narrower than it is.
func (s Scope) Stated() string {
	parts := make([]string, 0, 3)
	if len(s.Roots) > 0 {
		parts = append(parts, "inside "+joinNaturally(s.Roots, "and"))
	}
	if len(s.Apps) > 0 {
		parts = append(parts, "on "+joinNaturally(s.Apps, "and"))
	}
	parts = append(parts, "using only "+joinNaturally(s.Tools, "and"))
	return strings.Join(parts, ", ")
}

// joinNaturally words a list the way a person says it.
func joinNaturally(items []string, conj string) string {
	switch len(items) {
	case 0:
		return "nothing"
	case 1:
		return items[0]
	case 2:
		return items[0] + " " + conj + " " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " " + conj + " " + items[len(items)-1]
	}
}

// ---------------------------------------------------------------------------
// Naming
// ---------------------------------------------------------------------------

// MaxNameWords bounds a job's handle. A name is what the user says to address
// one job among several — "how's the tidy job" — so it has to be short enough
// to say and remember; anything longer is a goal wearing a name's slot.
const MaxNameWords = 4

// CleanName normalises a job's handle, or reports why it will not do.
func CleanName(raw string) (string, error) {
	name := strings.ToLower(strings.Join(strings.Fields(raw), " "))
	if name == "" {
		return "", fmt.Errorf("a job needs a short name so you can ask about it later")
	}
	if n := len(strings.Fields(name)); n > MaxNameWords {
		return "", fmt.Errorf("%q is too long for a name; %d words or fewer, so you can say it",
			raw, MaxNameWords)
	}
	return name, nil
}
