package daemon

// The jobs surface as a CLIENT reads it (#221, ADR 0067): the fields the Jobs
// tab and `jarvix jobs` render, and the two verbs that steer a job by hand.
//
// Most of these are unit tests of the report rather than socket round trips,
// and that is deliberate — the undoaccount_test.go argument, which applies more
// sharply here: every sentence this surface shows is decided in
// jobsurface.go, and a vocabulary only reachable through a wired daemon on a
// socket is a vocabulary nobody writes cases for. The wired tests at the bottom
// prove the fields actually travel and that answering a parked job resumes the
// step it kept whole.

import (
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/jobs"
	"github.com/rpickz/jarvix/internal/tools"
)

// jobsNow is the instant every fixture below is read at, so "4 minutes ago" is
// arithmetic against an injected clock rather than a race with the wall.
var jobsNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// aRunningJob is one piece of work in flight, as the store holds it.
func aRunningJob() jobs.Job {
	return jobs.Job{
		ID: "j3", Name: "tidy", Goal: "tidy my downloads",
		Scope: jobs.Scope{
			Tools: []string{"memory.search"}, Roots: []string{"/home/u/Downloads"}},
		State:   jobs.Running,
		Started: jobsNow.Add(-4 * time.Minute),
		Ledger: []jobs.Entry{
			{At: jobsNow.Add(-3 * time.Minute), Intent: "list the folder",
				Tool: "memory.search", Said: "17 files", Verified: true},
			{At: jobsNow.Add(-2 * time.Minute), Intent: "archive the old installers",
				Tool: "memory.search", Said: "moved 4", Verified: true, Undo: "a9"},
		},
		Steps: 2,
	}
}

// parkedAt parks a job on one question, keeping the step it stopped on — which
// is what makes answering a resumption rather than a restart.
func parkedAt(job jobs.Job, why jobs.Why, ask string, step jobs.Step) jobs.Job {
	job.State = jobs.Parked
	job.Question = jobs.Question{Why: why, Ask: ask, At: jobsNow.Add(-4 * time.Minute), Step: step}
	return job
}

// jobsReport renders one listing, with a stand-in for the registry's answer
// about what a confirmation card would show.
func jobsReport(list []jobs.Job, detail func(jobs.Step) string) map[string]any {
	return jobsViewReport(jobsAccount{
		jobs: list, now: jobsNow,
		path:   "/home/u/.local/state/jarvix/jobs.toml",
		detail: detail,
	})
}

// jobsReportGated renders one listing through a permission gate, which is what
// decides whether a parked approval is still worth offering a yes for (#225).
func jobsReportGated(list []jobs.Job, gate jobs.Gate) map[string]any {
	return jobsViewReport(jobsAccount{
		jobs: list, now: jobsNow,
		path: "/home/u/.local/state/jarvix/jobs.toml",
		gate: gate,
	})
}

// retier installs a policy that says one thing about one tool, which is the
// user changing their standing instruction about a capability.
func retier(t *testing.T, d *Daemon, tool string, decision tools.PolicyDecision) {
	t.Helper()
	policy, err := tools.NewPolicy(tools.PolicyConfig{
		Default: tools.PolicyAsk,
		Tools:   map[string]tools.PolicyDecision{tool: decision},
	})
	if err != nil {
		t.Fatal(err)
	}
	d.registry.SetPolicy(policy)
}

// jobRows pulls the listing out of a report.
func jobRows(t *testing.T, v map[string]any) []map[string]any {
	t.Helper()
	out, ok := v["jobs"].([]map[string]any)
	if !ok {
		t.Fatalf("the report carries no jobs: %#v", v["jobs"])
	}
	return out
}

// says fails unless the field holds exactly the sentence expected of it.
func says(t *testing.T, row map[string]any, field, want string) {
	t.Helper()
	got, _ := row[field].(string)
	if got != want {
		t.Errorf("row %s = %q, want %q", field, got, want)
	}
}

// controlIDs is the ordered list of what a row offers.
func controlIDs(t *testing.T, row map[string]any) []string {
	t.Helper()
	held, ok := row["controls"].([]map[string]any)
	if !ok {
		t.Fatalf("the row carries no controls: %#v", row["controls"])
	}
	out := make([]string, 0, len(held))
	for _, c := range held {
		id, _ := c["id"].(string)
		out = append(out, id)
	}
	return out
}

// Every claim a row makes arrives as a sentence the daemon wrote. The window
// has no clock it can trust, no idea what a scope is and no access to the
// ledger, so where a job stands, how long it has been there, what it may touch
// and how much it has done are all answered here.
func TestAJobRowCarriesItsOwnWordsForEveryClaimItMakes(t *testing.T) {
	rows := jobRows(t, jobsReport([]jobs.Job{aRunningJob()}, nil))
	if len(rows) != 1 {
		t.Fatalf("jobs.list returned %d rows, want 1", len(rows))
	}
	row := rows[0]

	says(t, row, "name", "tidy")
	says(t, row, "title", "Tidy")
	says(t, row, "state", "Tidy is running — started 4 minutes ago.")
	// The goal verbatim, inside the daemon's own sentence: a client that had to
	// supply a lead-in would be wording one (ADR 0066).
	says(t, row, "goal", "You asked for “tidy my downloads”.")
	says(t, row, "scope",
		"It may act inside /home/u/Downloads, using only memory.search.")
	says(t, row, "progress", "It has done 2 steps, 1 of which changed something.")

	// A running job has no report. One composed now would be a progress note
	// wearing a conclusion's clothes.
	if _, carried := row["report"]; carried {
		t.Errorf("a running job carries a report: %#v", row["report"])
	}
	// And the ledger never travels. A line of it can hold what a tool read out
	// of the user's files, and a listing carrying that would put their work on
	// every connected socket (the store's own rule, applied to a read).
	if strings.Contains(strings.ToLower(joinValues(row)), "17 files") {
		t.Errorf("a ledger line travelled on the listing: %#v", row)
	}
}

// Each of the six states is a different sentence, and each of them says in
// words — never in a colour or a code — what a reader has to do about it.
func TestEveryStateIsADifferentSentenceThatSaysWhatItMeans(t *testing.T) {
	base := aRunningJob()
	ended := func(state jobs.State) jobs.Job {
		j := base
		j.State, j.Ended = state, jobsNow.Add(-20*time.Minute)
		return j
	}
	ready := base
	ready.State = jobs.Ready

	for _, tc := range []struct {
		job  jobs.Job
		want string
	}{
		{base, "Tidy is running — started 4 minutes ago."},
		{ready, "Tidy is queued to carry on — started 4 minutes ago."},
		{parkedAt(base, jobs.WhyApproval, "Shall I?", jobs.Step{Tool: "memory.search"}),
			"Tidy is waiting on you — parked 4 minutes ago."},
		{parkedAt(base, jobs.WhyOutOfScope, "I stopped without doing it.", jobs.Step{}),
			"Tidy has stopped and needs you — parked 4 minutes ago."},
		{ended(jobs.Done), "Tidy finished 20 minutes ago."},
		{ended(jobs.Stopped), "Tidy stopped 20 minutes ago."},
		{ended(jobs.Failed), "Tidy failed 20 minutes ago."},
	} {
		row := jobRows(t, jobsReport([]jobs.Job{tc.job}, nil))[0]
		says(t, row, "state", tc.want)
	}
}

// A parked job says what it is waiting for in the words of whoever parked it —
// the gate's own generated question for an approval, internal/jobs' wording for
// a boundary — and the surface never re-words either.
func TestAParkedJobSaysWhatItIsWaitingForVerbatim(t *testing.T) {
	ask := "I'm about to delete /home/u/Downloads/old.iso. " +
		"This can't be undone: a deleted file is gone."
	row := jobRows(t, jobsReport([]jobs.Job{
		parkedAt(aRunningJob(), jobs.WhyApproval, ask, jobs.Step{Tool: "memory.search"}),
	}, nil))[0]

	says(t, row, "question", ask)
}

// #200's contract as a listing can keep it: a parked approval carries the same
// verbatim detail a session's confirmation card shows.
//
// The equality is asserted against the gate's own Command rather than against a
// literal, because the claim is not "this string" — it is "the same string the
// card would have shown", and a test naming the string itself would keep
// passing after the two had drifted apart.
func TestAParkedApprovalCarriesTheDetailAConfirmationCardWouldShow(t *testing.T) {
	d := jobsDaemon(t)
	call := ai.ToolCall{Name: tools.JobsStopToolName, Arguments: `{"name":"deploy"}`}

	card := d.registry.Check(call)
	if card.Decision != tools.PolicyAsk {
		t.Fatalf("the fixture no longer tests what it claims to: %s is %q, not ask",
			call.Name, card.Decision)
	}
	if card.Command == "" {
		t.Fatal("the confirmation card for this call carries no verbatim detail")
	}

	job := parkedAt(aRunningJob(), jobs.WhyApproval, card.Summary,
		jobs.Step{Tool: call.Name, Args: call.Arguments})
	row := jobRows(t, jobsReport([]jobs.Job{job}, d.jobsAccount().detail))[0]

	says(t, row, "detail", card.Command)
}

// The five other parking reasons are not questions about a pending call, so
// there is no card detail to show and none is invented.
func TestOnlyAGateApprovalCarriesACardsDetail(t *testing.T) {
	d := jobsDaemon(t)
	job := parkedAt(aRunningJob(), jobs.WhyDecision,
		"There are two folders called invoices. Which did you mean?",
		jobs.Step{Question: "which one?"})

	row := jobRows(t, jobsReport([]jobs.Job{job}, d.jobsAccount().detail))[0]
	if detail, carried := row["detail"]; carried {
		t.Errorf("a planner's own question carries a confirmation detail: %#v", detail)
	}
}

// The controls are the eligibility. A job that may be approved offers exactly
// the three things that would work; a job that has ended offers none — and the
// absence is the refusal, because the shared row skips an empty label in the
// focus chain, so a keyboard user never lands on a control that could only
// decline.
func TestTheControlsOfferedAreTheOnesThatWouldActuallyWork(t *testing.T) {
	approval := parkedAt(aRunningJob(), jobs.WhyApproval, "Shall I?",
		jobs.Step{Tool: "memory.search"})
	decision := parkedAt(aRunningJob(), jobs.WhyDecision, "Which one?", jobs.Step{})
	boundary := parkedAt(aRunningJob(), jobs.WhyOutOfScope,
		"I stopped without doing it: it would have touched /etc/hosts.", jobs.Step{})
	done := aRunningJob()
	done.State, done.Ended = jobs.Done, jobsNow.Add(-time.Minute)

	for _, tc := range []struct {
		name string
		job  jobs.Job
		want []string
	}{
		{"a gate approval", approval, []string{"approve", "decline", "stop"}},
		{"a planner's decision", decision, []string{"answer", "decline", "stop"}},
		{"a boundary nothing can answer", boundary, []string{"stop"}},
		{"work in flight", aRunningJob(), []string{"stop"}},
		{"a job that has finished", done, nil},
	} {
		row := jobRows(t, jobsReport([]jobs.Job{tc.job}, nil))[0]
		got := controlIDs(t, row)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s offers %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestApproveGoesAwayWhenTheToolIsTurnedOffWhileTheJobWaits is AC 4 of #225,
// end to end through the real gate: the same job, the same question, read twice
// with only the user's standing instruction changed between the two reads.
//
// It goes through jobActor.Judge and a compiled tools.Policy rather than a
// stand-in, because the claim being made is that the row reads the RUNNER's
// gate. A second reading of the policy here would be the drift the pairing
// exists to prevent.
func TestApproveGoesAwayWhenTheToolIsTurnedOffWhileTheJobWaits(t *testing.T) {
	d := jobsDaemon(t)
	job := parkedAt(aRunningJob(), jobs.WhyApproval, "Shall I look that up?",
		jobs.Step{Tool: tools.MemorySearchToolName, Args: `{"query":"invoices"}`})

	retier(t, d, tools.MemorySearchToolName, tools.PolicyAsk)
	row := jobRows(t, jobsReportGated([]jobs.Job{job}, d.jobsAccount().gate))[0]
	if got := strings.Join(controlIDs(t, row), ","); got != "approve,decline,stop" {
		t.Fatalf("a job whose tier still asks offers %q, want the yes on the row", got)
	}
	if why, carried := row["why"]; carried {
		t.Errorf("a row that offers its yes also explains why it cannot: %#v", why)
	}

	// The user turns the tool off while the job waits.
	retier(t, d, tools.MemorySearchToolName, tools.PolicyDeny)
	gate := d.jobsAccount().gate
	row = jobRows(t, jobsReportGated([]jobs.Job{job}, gate))[0]

	if got := strings.Join(controlIDs(t, row), ","); got != "decline,stop" {
		t.Errorf("controls = %q, want the yes withheld and saying no still offered — "+
			"stopping a job is not a use of the tool it was waiting on", got)
	}
	// Withheld with a reason, in the sentence the verb itself refuses with.
	why, _ := row["why"].(string)
	_, refusal := job.ApproveOffer(gate)
	if why == "" || why != refusal {
		t.Errorf("the row says %q and the verb refuses with %q; two sentences for one rule",
			why, refusal)
	}
	if !strings.Contains(why, tools.MemorySearchToolName) {
		t.Errorf("the refusal = %q, want it to name the tool that was turned off", why)
	}

	// And it withholds rather than settles: turning the tool back on gives the
	// control back, because a standing instruction that changed twice has
	// changed twice.
	retier(t, d, tools.MemorySearchToolName, tools.PolicyAsk)
	row = jobRows(t, jobsReportGated([]jobs.Job{job}, d.jobsAccount().gate))[0]
	if got := strings.Join(controlIDs(t, row), ","); got != "approve,decline,stop" {
		t.Errorf("controls = %q after the tool was turned back on, want the yes back", got)
	}
}

// Only the control that needs the user's own words carries a field, and it
// carries the field's label too — because a label is wording.
func TestOnlyADecisionAsksForWords(t *testing.T) {
	decision := parkedAt(aRunningJob(), jobs.WhyDecision, "Which one?", jobs.Step{})
	row := jobRows(t, jobsReport([]jobs.Job{decision}, nil))[0]
	held, _ := row["controls"].([]map[string]any)
	if len(held) == 0 {
		t.Fatal("a job waiting on a decision offers nothing")
	}
	if held[0]["words"] != true {
		t.Errorf("the answer control is not marked as needing words: %#v", held[0])
	}
	if label, _ := held[0]["field_label"].(string); label == "" {
		t.Error("the answer control carries no label for its field, so a surface " +
			"would have to write one")
	}

	approval := parkedAt(aRunningJob(), jobs.WhyApproval, "Shall I?",
		jobs.Step{Tool: "memory.search"})
	held, _ = jobRows(t, jobsReport([]jobs.Job{approval}, nil))[0]["controls"].([]map[string]any)
	if held[0]["words"] == true {
		t.Error("a gate approval asks for words; it is a yes about an action " +
			"the user has already been shown")
	}
}

// A job no answer can move says why, in the sentence the verb itself refuses
// with. One function, two callers: the control that is withheld and the action
// that is refused cannot explain the same rule differently.
func TestAJobNoAnswerCanMoveSaysWhyInTheVerbsOwnSentence(t *testing.T) {
	job := parkedAt(aRunningJob(), jobs.WhyOutOfScope,
		"I stopped without doing it: it would have touched /etc/hosts, "+
			"which is outside /home/u/Downloads.", jobs.Step{})

	row := jobRows(t, jobsReport([]jobs.Job{job}, nil))[0]
	why, _ := row["why"].(string)
	if why == "" {
		t.Fatal("a job nothing can answer does not say why")
	}
	_, refusal := job.AnswerOffer()
	if why != refusal {
		t.Errorf("the row says %q and the verb refuses with %q; two sentences for "+
			"one rule", why, refusal)
	}
}

// A finished job's report is the ledger-derived account, and a step the runner
// never saw the end of leads it. #200's honesty rule, on the surface.
func TestAFinishedJobsReportLeadsWithWhatCouldNotBeConfirmed(t *testing.T) {
	job := aRunningJob()
	job.State, job.Ended = jobs.Done, jobsNow.Add(-time.Minute)
	job.Ledger = append(job.Ledger, jobs.Entry{
		At: jobsNow.Add(-90 * time.Second), Intent: "empty the trash",
		Tool: "memory.search", Said: "I was stopped before I saw how this ended."})
	job.Closing = "The downloads folder is tidy."

	row := jobRows(t, jobsReport([]jobs.Job{job}, nil))[0]
	report, _ := row["report"].(string)
	if report != job.Report() {
		t.Errorf("the row's report = %q, want the ledger's own account %q", report, job.Report())
	}
	if !strings.HasPrefix(report, "There is 1 step I started and never saw the end of") {
		t.Errorf("the report does not lead with the unverified step: %q", report)
	}
	says(t, row, "progress",
		"It has done 3 steps, 1 of which changed something, and 1 I can't confirm either way.")
}

// An empty listing still has something to say, and the bounds disclose
// themselves on every read — the account's two promises at either end of the
// range, restated for work.
func TestAnEmptyListingAndItsBoundsCarryTheirOwnSentences(t *testing.T) {
	v := jobsReport(nil, nil)

	if len(jobRows(t, v)) != 0 {
		t.Fatal("an empty store produced rows")
	}
	if v["empty"] != jobsEmptySentence {
		t.Errorf("empty = %q, want the daemon's one sentence", v["empty"])
	}
	disclosure, _ := v["disclosure"].(string)
	if !strings.Contains(disclosure, "4") || !strings.Contains(disclosure, "60") {
		t.Errorf("the disclosure does not state both bounds: %q", disclosure)
	}
	if v["path"] == "" {
		t.Error("the listing does not say where the jobs file is")
	}
}

// ---------------------------------------------------------------------------
// The tier
// ---------------------------------------------------------------------------

// Asking what the jobs are doing must not cost a confirmation, and nothing that
// ACTS on a job moved with it. This is the whole of the authorization change
// (#221): one read-only verb, argued on desktop.release_window's precedent, and
// the gate's floor unchanged everywhere else.
func TestOnlyTheReadOnlyJobVerbIsAllowedWithoutAsking(t *testing.T) {
	d := jobsDaemon(t)

	if got := d.registry.Check(ai.ToolCall{Name: tools.JobsStatusToolName,
		Arguments: `{}`}).Decision; got != tools.PolicyAllow {
		t.Errorf("jobs.status is %q — asking what my jobs are doing must not "+
			"produce a confirmation prompt before it produces an answer", got)
	}
	for name, args := range map[string]string{
		tools.JobsStartToolName: `{"name":"tidy","goal":"tidy up","tools":["memory.search"],` +
			`"directories":["/home/u/Downloads"]}`,
		tools.JobsStopToolName:   `{"name":"tidy"}`,
		tools.JobsAnswerToolName: `{"name":"tidy","approved":true}`,
	} {
		if got := d.registry.Check(ai.ToolCall{Name: name, Arguments: args}).Decision; got != tools.PolicyAsk {
			t.Errorf("%s is %q, want ask — only the read-only verb was re-tiered", name, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Over the wire
// ---------------------------------------------------------------------------

// startJobsDaemon serves a daemon whose store has already been seeded, so the
// supervisor never sees a Ready job it might start acting on. Seeding before
// Run rather than after is the hermetic half: nothing below shares a goroutine
// with a planner.
func startJobsDaemon(t *testing.T, seed func(*testing.T, *Daemon)) (*Daemon, *ipc.Client) {
	t.Helper()
	d := jobsDaemon(t)
	if seed != nil {
		seed(t, d)
	}
	serveDaemon(t, d)
	return d, dialDaemon(t, d.paths.Socket)
}

// parkOne creates one job and parks it on a gate approval, keeping the step.
func parkOne(t *testing.T, d *Daemon, step jobs.Step, ask string) jobs.Job {
	t.Helper()
	job, err := d.jobStore.Start("tidy", "tidy my downloads", jobs.Scope{
		Tools: []string{tools.MemorySearchToolName}, Roots: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	parked, err := d.jobStore.Update(job.ID, func(j *jobs.Job) bool {
		j.State = jobs.Parked
		j.Question = jobs.Question{Why: jobs.WhyApproval, Ask: ask, At: job.Started, Step: step}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return parked
}

// The window's half of the listing travels, and a client can render a parked
// job without working anything out.
func TestTheJobsListingTravelsOverTheWire(t *testing.T) {
	step := jobs.Step{Tool: tools.JobsStopToolName, Args: `{"name":"deploy"}`}
	_, client := startJobsDaemon(t, func(t *testing.T, d *Daemon) {
		parkOne(t, d, step, "Shall I stop the deploy job?")
	})

	var view struct {
		Jobs []struct {
			Name     string `json:"name"`
			State    string `json:"state"`
			Goal     string `json:"goal"`
			Scope    string `json:"scope"`
			Question string `json:"question"`
			Detail   string `json:"detail"`
			Controls []struct {
				ID    string `json:"id"`
				Label string `json:"label"`
				Name  string `json:"name"`
			} `json:"controls"`
		} `json:"jobs"`
		Empty      string `json:"empty"`
		Disclosure string `json:"disclosure"`
		Path       string `json:"path"`
	}
	if err := client.Call("jobs.list", nil, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Jobs) != 1 {
		t.Fatalf("jobs.list returned %d jobs, want 1", len(view.Jobs))
	}
	row := view.Jobs[0]
	if row.State == "" || row.Goal == "" || row.Scope == "" || row.Question == "" {
		t.Errorf("a parked job travelled without one of its sentences: %#v", row)
	}
	if row.Detail == "" {
		t.Error("the verbatim detail a confirmation card would show did not travel")
	}
	if len(row.Controls) == 0 || row.Controls[0].Label == "" || row.Controls[0].Name == "" {
		t.Errorf("the controls travelled without their words: %#v", row.Controls)
	}
	if view.Empty == "" || view.Disclosure == "" || view.Path == "" {
		t.Error("the listing's own sentences do not travel")
	}
}

// Approving from the window resumes the step the job kept whole — the whole of
// #200's "resumes from a checkpoint, not a re-plan".
//
// What is asserted is the state the runner's resumption branch keys on: Ready,
// with WhyApproval and the SAME step still on the question. A job put back to
// Ready with the step dropped would be re-planned, and a planner asked twice
// may answer differently — so the user would have approved one action and got
// another.
func TestApprovingFromTheWindowResumesTheStepTheJobKept(t *testing.T) {
	step := jobs.Step{Intent: "stop the deploy job",
		Tool: tools.JobsStopToolName, Args: `{"name":"deploy"}`}
	d := jobsDaemon(t)
	parked := parkOne(t, d, step, "Shall I stop the deploy job?")

	if _, err := d.jobRunner.Answer(parked.Name, true, ""); err != nil {
		t.Fatal(err)
	}

	resumed, err := d.jobStore.Find(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != jobs.Ready {
		t.Fatalf("an approved job is %q, want ready so the supervisor picks it up", resumed.State)
	}
	if resumed.Question.Why != jobs.WhyApproval {
		t.Errorf("the parking reason became %q; the runner resumes only on an "+
			"approval, so this job would be re-planned", resumed.Question.Why)
	}
	if resumed.Question.Step != step {
		t.Errorf("the kept step is now %#v, want %#v — approving must run the "+
			"action the user was shown, not one planned afresh", resumed.Question.Step, step)
	}
	if len(resumed.Ledger) != len(parked.Ledger) {
		t.Errorf("approving wrote %d ledger entries, want %d: nothing has happened yet",
			len(resumed.Ledger), len(parked.Ledger))
	}
}

// Stopping from the window halts the job and reports what it had done. A
// refusal comes back as a normal reply carrying its reason rather than as an
// error, because Jarvix declining is not a fault.
func TestStoppingOverTheWireHaltsTheJobAndRefusesTwiceInWords(t *testing.T) {
	_, client := startJobsDaemon(t, func(t *testing.T, d *Daemon) {
		parkOne(t, d, jobs.Step{Tool: tools.MemorySearchToolName}, "Shall I?")
	})

	var out struct {
		Done    bool   `json:"done"`
		Refused bool   `json:"refused"`
		Spoken  string `json:"spoken"`
	}
	if err := client.Call("jobs.stop", map[string]any{"name": "tidy"}, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Done || out.Refused || out.Spoken == "" {
		t.Fatalf("stopping a parked job = %#v, want done with an account of it", out)
	}

	// The store is written before the reply goes out (Store.Update saves and
	// only then returns), so the second call is ordered by the first's answer
	// and not by anything asynchronous.
	out.Done, out.Refused, out.Spoken = false, false, ""
	if err := client.Call("jobs.stop", map[string]any{"name": "tidy"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Done || !out.Refused {
		t.Fatalf("stopping an already-stopped job = %#v, want a refusal", out)
	}
	if !strings.Contains(out.Spoken, "already") {
		t.Errorf("the refusal does not say the job had already ended: %q", out.Spoken)
	}
}

// joinValues flattens a row's string values so a test can ask whether anything
// at all leaked into it.
func joinValues(row map[string]any) string {
	var b strings.Builder
	for _, v := range row {
		if s, ok := v.(string); ok {
			b.WriteString(s)
			b.WriteString(" ")
		}
	}
	return b.String()
}
