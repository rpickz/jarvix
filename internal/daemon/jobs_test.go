package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/briefing"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/jobs"
	"github.com/rpickz/jarvix/internal/situation"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
)

// The job wiring at the daemon boundary (#200, ADR 0065). internal/jobs' own
// tests are hermetic and cover the model, the store and the runner; what is
// worth proving here is the wiring — that the subject table refuses what it
// cannot read, that a job is refused an unmanaged window, that the situation
// report gained a source rather than a second reporting mechanism, and that a
// running job cannot be undone underneath itself.

// jobsDaemon builds a daemon with a compositor holding the given windows.
func jobsDaemon(t *testing.T, windows ...desktop.Window) *Daemon {
	t.Helper()
	dir := t.TempDir()
	cfg := testConfig()
	d, err := New(cfg, config.Paths{Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock")}, nil, Deps{
		Provider:    &ai.Fake{},
		Transcriber: &stt.Fake{},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
		Compositor:  desktop.NewFakeCompositor(windows...),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// aJobScope is an enforceable scope over one directory.
func aJobScope(t *testing.T) jobs.Scope {
	t.Helper()
	return jobs.Scope{Tools: []string{tools.MemorySearchToolName}, Roots: []string{t.TempDir()}}
}

func TestTheJobVerbsAreRegistered(t *testing.T) {
	d := jobsDaemon(t)
	names := strings.Join(d.registry.Names(), ",")
	for _, want := range []string{tools.JobsStartToolName, tools.JobsStatusToolName,
		tools.JobsStopToolName, tools.JobsAnswerToolName} {
		if !strings.Contains(names, want) {
			t.Errorf("registered tools = %q, missing %q", names, want)
		}
	}
}

// TestAJobIsRefusedAnUnmanagedWindow is #197's seam used for exactly what it
// was built for: a job acts only in windows the user has handed over.
func TestAJobIsRefusedAnUnmanagedWindow(t *testing.T) {
	d := jobsDaemon(t, desktop.Window{
		Address: "0x1", Class: "Alacritty", Title: "builds", PID: 42, Workspace: 1,
	})
	actor := &jobActor{d: d}
	_, err := actor.Subject(context.Background(), jobs.Step{
		Tool: tools.MoveWindowToolName, Args: `{"window":"alacritty"}`})
	if err == nil {
		t.Fatal("a job was handed a window nobody had given it")
	}
	if !strings.Contains(err.Error(), "handed over") {
		t.Errorf("refusal = %q, want it to say what the user would have to do", err.Error())
	}
	if !errors.Is(err, tools.ErrNotManaged) {
		// The wrapped sentinel is what makes this a decision other code can key
		// on rather than a string it has to match.
		t.Errorf("refusal = %v, want it to wrap tools.ErrNotManaged", err)
	}
}

// TestAToolWhoseSubjectCannotBeReadIsRefused is the sharpest consequence of
// enforcing a scope rather than describing one: a shell command's files cannot
// be read out of its text, so it cannot be checked against a directory, so a
// job will not run one. See ADR 0065.
func TestAToolWhoseSubjectCannotBeReadIsRefused(t *testing.T) {
	d := jobsDaemon(t)
	actor := &jobActor{d: d}
	for _, tool := range []string{"shell.run", tools.LaunchAppToolName, "made.up"} {
		if _, err := actor.Subject(context.Background(),
			jobs.Step{Tool: tool, Args: `{"command":"rm -rf /"}`}); err == nil {
			t.Errorf("%s was given a readable subject; the boundary could not be checked against it", tool)
		}
	}
}

// TestAReadWithNoSubjectIsInScopeIfItsToolIs: there is nothing else to check.
func TestAReadWithNoSubjectIsInScopeIfItsToolIs(t *testing.T) {
	d := jobsDaemon(t)
	actor := &jobActor{d: d}
	got, err := actor.Subject(context.Background(), jobs.Step{Tool: tools.MemorySearchToolName})
	if err != nil {
		t.Fatalf("a read of Jarvix's own state was refused: %v", err)
	}
	if len(got.Paths) != 0 || got.App != "" {
		t.Errorf("attempt = %+v, want no subject beyond the tool itself", got)
	}
}

// TestAWriteToJarvixsOwnStoresNamesTheRealFile so a scope over the user's tree
// does not silently admit them.
func TestAWriteToJarvixsOwnStoresNamesTheRealFile(t *testing.T) {
	d := jobsDaemon(t)
	actor := &jobActor{d: d}
	got, err := actor.Subject(context.Background(), jobs.Step{Tool: tools.MemoryRememberToolName})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Paths) != 1 || got.Paths[0] != d.paths.MemoryFile() {
		t.Errorf("attempt = %+v, want the memory book's real path", got)
	}
}

// TestTheGateStillAsksInsideAJob: the floor does not move because the caller is
// a job, and the one-way warning rides the question — which matters most here,
// because the approval is read hours later and out of context.
func TestTheGateStillAsksInsideAJob(t *testing.T) {
	d := jobsDaemon(t)
	verdict := (&jobActor{d: d}).Judge(context.Background(),
		jobs.Step{Tool: "shell.run", Args: `{"command":"rm -rf /tmp/x"}`})
	if verdict.Decision != jobs.Ask {
		t.Fatalf("decision = %v, want the gate to ask about a shell command inside a job", verdict.Decision)
	}
	if !strings.Contains(verdict.Question, "can't be undone") {
		t.Errorf("question = %q, want the one-way warning on it", verdict.Question)
	}
}

// TestAParkedJobLeadsTheSituationReport is the acceptance criterion about
// reporting: it is a source plugged into #196's interface, and a parked job is
// work stopped until the user does something, which is the rank the report
// leads with.
func TestAParkedJobLeadsTheSituationReport(t *testing.T) {
	d := jobsDaemon(t)
	job, err := d.jobStore.Start("tidy", "tidy up my downloads", aJobScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.jobStore.Update(job.ID, func(j *jobs.Job) bool {
		j.State = jobs.Parked
		j.Question = jobs.Question{Why: jobs.WhyApproval,
			Ask: "Shall I delete the duplicates? This can't be undone."}
		return true
	}); err != nil {
		t.Fatal(err)
	}

	items, err := d.situationJobs(context.Background(),
		situation.Instant{Now: time.Now(), Since: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one line about the parked job", items)
	}
	if items[0].Rank != situation.NeedsYou {
		t.Errorf("rank = %v, want %v", items[0].Rank, situation.NeedsYou)
	}
	if !strings.Contains(items[0].Text, "delete the duplicates") {
		t.Errorf("line = %q, want the question the job is waiting on", items[0].Text)
	}
}

func TestARunningJobIsReportedAsInProgress(t *testing.T) {
	d := jobsDaemon(t)
	if _, err := d.jobStore.Start("tidy", "tidy up", aJobScope(t)); err != nil {
		t.Fatal(err)
	}
	items, err := d.situationJobs(context.Background(), situation.Instant{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Rank != situation.InProgress {
		t.Fatalf("items = %+v, want one in-progress line", items)
	}
}

// TestAFinishedJobIsOnlyNewsUntilYouHaveLooked is Instant's zero rule: a fresh
// daemon reading out every job it ever ran would be answering a question about
// now with an archive.
func TestAFinishedJobIsOnlyNewsUntilYouHaveLooked(t *testing.T) {
	d := jobsDaemon(t)
	job, err := d.jobStore.Start("tidy", "tidy up", aJobScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.jobStore.Update(job.ID, func(j *jobs.Job) bool {
		j.State = jobs.Done
		return true
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Hour)
	// Nobody has ever looked: nothing interval-shaped is news.
	items, err := d.situationJobs(context.Background(), situation.Instant{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("items = %+v, want nothing: with no record of a previous look there is no news", items)
	}
	// With a watermark before it finished, it is.
	items, err = d.situationJobs(context.Background(),
		situation.Instant{Now: now, Since: now.Add(-2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Rank != situation.Finished {
		t.Errorf("items = %+v, want the finished job reported once", items)
	}
}

// TestTheJobsSourceIsRegisteredWithTheReport pins the wiring itself: the report
// gained a source, not a second mechanism.
func TestTheJobsSourceIsRegisteredWithTheReport(t *testing.T) {
	d := jobsDaemon(t)
	if _, err := d.jobStore.Start("tidy", "tidy up my downloads", aJobScope(t)); err != nil {
		t.Fatal(err)
	}
	view, err := d.situation.View(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, section := range view.Sections {
		for _, line := range section.Lines {
			if strings.Contains(line.Text, "Tidy is running") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the report said nothing about the job; sections = %+v", view.Sections)
	}
}

// TestAParkedJobIsInTheReturnBriefing is the second surface, and the more
// consequential one: a blocked job never interrupts, so the morning after is
// when the user finds out, and it has to be there.
func TestAParkedJobIsInTheReturnBriefing(t *testing.T) {
	d := jobsDaemon(t)
	job, err := d.jobStore.Start("tidy", "tidy up my downloads", aJobScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.jobStore.Update(job.ID, func(j *jobs.Job) bool {
		j.State = jobs.Parked
		j.Question = jobs.Question{Why: jobs.WhyApproval, Ask: "Shall I delete them?"}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	lines, err := d.briefJobs(context.Background(), now.Add(-8*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("lines = %+v, want one", lines)
	}
	if lines[0].Category != briefing.Awaiting {
		t.Errorf("category = %v, want %v — a job stopped all night has been waiting longest",
			lines[0].Category, briefing.Awaiting)
	}
	if !strings.Contains(lines[0].Text, "needs you") {
		t.Errorf("line = %q, want it to say the job needs the user", lines[0].Text)
	}
}

// TestAJobThatFinishedWhileYouWereAwayIsBriefedOnce, and a job that finished
// before the absence began is not news at all.
func TestAJobThatFinishedWhileYouWereAwayIsBriefedOnce(t *testing.T) {
	d := jobsDaemon(t)
	job, err := d.jobStore.Start("tidy", "tidy up", aJobScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.jobStore.Update(job.ID, func(j *jobs.Job) bool {
		j.State = jobs.Done
		return true
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Minute)
	lines, err := d.briefJobs(context.Background(), now.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Category != briefing.Completed {
		t.Fatalf("lines = %+v, want one completed line", lines)
	}
	// The same job, against an absence that began after it ended.
	lines, err = d.briefJobs(context.Background(), now.Add(time.Hour), now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Errorf("lines = %+v, want nothing: it finished before the absence began", lines)
	}
}

// TestARunningJobCannotBeUndoneUnderneathItself is the first of the two
// questions ADR 0064 left to #200. A reversal racing a live runner would
// produce an account of a state the machine was never in.
func TestARunningJobCannotBeUndoneUnderneathItself(t *testing.T) {
	d := jobsDaemon(t)
	job, err := d.jobStore.Start("tidy", "tidy up", aJobScope(t))
	if err != nil {
		t.Fatal(err)
	}
	err = d.settleBeforeUndo(job.ID)
	if err == nil {
		t.Fatal("a running job was reversed underneath itself")
	}
	if !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("refusal = %q, want it to say what to do about it", err.Error())
	}
}

// TestUndoingAParkedJobStopsItFirst: resuming from a checkpoint whose effects
// have just been reversed would be resuming into a world the checkpoint does
// not describe.
func TestUndoingAParkedJobStopsItFirst(t *testing.T) {
	d := jobsDaemon(t)
	job, err := d.jobStore.Start("tidy", "tidy up", aJobScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.jobStore.Update(job.ID, func(j *jobs.Job) bool {
		j.State = jobs.Parked
		j.Question = jobs.Question{Why: jobs.WhyApproval, Ask: "Shall I?"}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.settleBeforeUndo(job.ID); err != nil {
		t.Fatalf("a parked job refused to be reversed: %v", err)
	}
	after, err := d.jobStore.Find(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != jobs.Stopped {
		t.Errorf("state = %q, want %q: undoing a job's work must stop it", after.State, jobs.Stopped)
	}
}

// TestUndoingAJobNobodyRecognisesIsNotAnError: the account is asked, and
// answers honestly that it holds nothing for it.
func TestUndoingAJobNobodyRecognisesIsNotAnError(t *testing.T) {
	d := jobsDaemon(t)
	if err := d.settleBeforeUndo("j99"); err != nil {
		t.Errorf("an unknown job id was refused rather than passed through: %v", err)
	}
}

// TestTheStatusAccountIsTheJobsOwnWords: the service chooses which jobs to say
// and joins them; it composes no sentence of its own about the work.
func TestTheStatusAccountIsTheJobsOwnWords(t *testing.T) {
	d := jobsDaemon(t)
	svc := &jobService{d: d}
	if got := svc.Status(""); !strings.Contains(got, "not working on anything") {
		t.Errorf("status = %q, want an honest empty answer", got)
	}
	job, err := d.jobStore.Start("tidy", "tidy up", aJobScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := svc.Status("tidy"); got != job.Spoken() {
		t.Errorf("status = %q, want the job's own account %q", got, job.Spoken())
	}
	if got := svc.Status("nothing-by-that-name"); !strings.Contains(got, "no job called") {
		t.Errorf("status = %q, want an honest miss", got)
	}
}

// TestThePlannerIsGivenOnlyTheToolsInScope, plus the two control verbs it needs
// to say something other than "do this".
func TestThePlannerIsGivenOnlyTheToolsInScope(t *testing.T) {
	d := jobsDaemon(t)
	defs, err := d.scopedDefs(jobs.Scope{Tools: []string{tools.MemorySearchToolName}})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, def := range defs {
		names[def.Name] = true
	}
	if !names[tools.MemorySearchToolName] {
		t.Error("the scope's own tool was withheld from the plan")
	}
	if names[tools.MemoryRememberToolName] {
		t.Error("a tool outside the scope was offered to the plan")
	}
	if !names[jobFinishVerb] || !names[jobAskVerb] {
		t.Error("the plan had no way to finish or to ask, so prose would be its only option")
	}
	if _, err := d.scopedDefs(jobs.Scope{Tools: []string{"nothing.real"}}); err == nil {
		t.Error("a scope naming nothing this daemon has produced a plannable job")
	}
}

// TestThePromptFeedsTheLedgerAsFactsAndMarksWhatIsUnknown is the #150
// discipline applied to a plan: what happened is fed, and a step nobody could
// confirm is not fed as one that succeeded.
func TestThePromptFeedsTheLedgerAsFactsAndMarksWhatIsUnknown(t *testing.T) {
	prompt := jobPrompt(jobs.View{
		Goal:  "tidy up my downloads",
		Scope: jobs.Scope{Tools: []string{tools.MemorySearchToolName}, Roots: []string{"/tmp"}},
		Ledger: []jobs.Entry{
			{Tool: tools.MemorySearchToolName, Said: "seven files", Verified: true},
			{Tool: "shell.run", Said: "I was stopped before I saw how this ended."},
		},
		Left: 12,
	})
	for _, want := range []string{"tidy up my downloads", "/tmp", "seven files",
		"STARTED, OUTCOME UNKNOWN", "12 steps left", jobFinishVerb, jobAskVerb} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
	if !strings.Contains(prompt, "enforced by the machine") {
		t.Error("the prompt does not say the boundary is not the model's to keep")
	}
}
