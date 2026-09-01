package jobs

import (
	"strings"
	"testing"
	"time"
)

// The report's tests. They are all about one property: a job never claims more
// than its ledger holds, and it says what it could not confirm before it says
// what it did.

// ledger builds a job with the given entries.
func ledger(name string, state State, entries ...Entry) Job {
	return Job{Name: name, State: state, Ledger: entries, Started: time.Now()}
}

func did(intent string) Entry {
	return Entry{Intent: intent, Tool: "memory.remember", Said: "ok", Verified: true, Undo: "a1"}
}

func failed(intent string) Entry {
	return Entry{Intent: intent, Tool: "memory.remember", Said: "no", Verified: true, Failed: true}
}

func unknown(intent string) Entry {
	return Entry{Intent: intent, Tool: "shell.run", Said: "I was stopped before I saw how this ended."}
}

// TestWhatCouldNotBeConfirmedIsSaidFirst is the honesty rule's shape. "I did
// nine things and I can't tell you whether the tenth happened" is a different
// report from "I did ten things", and a listener has to be able to tell them
// apart without asking.
func TestWhatCouldNotBeConfirmedIsSaidFirst(t *testing.T) {
	job := ledger("tidy", Stopped, did("moved the old invoices"), unknown("deleted the duplicates"))
	report := job.Report()
	first := strings.Index(report, "never saw the end of")
	second := strings.Index(report, "moved the old invoices")
	if first < 0 {
		t.Fatalf("report = %q, want it to admit the step it could not confirm", report)
	}
	if second >= 0 && first > second {
		t.Errorf("report = %q, want the unconfirmed step said before the successes", report)
	}
}

// TestAReportNamesWhatItCouldNotDoAsWellAsWhatItDid: a report that named only
// the successes would read as if the direction had been carried out.
func TestAReportNamesWhatItCouldNotDoAsWellAsWhatItDid(t *testing.T) {
	job := ledger("tidy", Done, did("moved the old invoices"), failed("emptied the trash"))
	report := job.Report()
	if !strings.Contains(report, "moved the old invoices") {
		t.Errorf("report = %q, want what it did", report)
	}
	if !strings.Contains(report, "emptied the trash") {
		t.Errorf("report = %q, want what it could not do", report)
	}
}

func TestAJobThatDidNothingSaysSo(t *testing.T) {
	job := ledger("tidy", Done)
	if got := job.Report(); !strings.Contains(got, "nothing at all") {
		t.Errorf("report = %q, want a plain admission", got)
	}
}

func TestAJobWhereNothingWorkedDoesNotClaimOtherwise(t *testing.T) {
	job := ledger("tidy", Done, failed("emptied the trash"))
	got := job.Report()
	if !strings.Contains(got, "Nothing I tried worked") {
		t.Errorf("report = %q, want it to say nothing worked", got)
	}
}

// TestReadingIsNotChanging: a job that looked at forty files must not describe
// itself as having done forty things.
func TestReadingIsNotChanging(t *testing.T) {
	read := Entry{Intent: "looked at the folder", Tool: "memory.search", Said: "seven files", Verified: true}
	job := ledger("tidy", Running, read, read, did("moved one"))
	clause := job.Progress()
	if !strings.Contains(clause, "3 steps") {
		t.Errorf("clause = %q, want the step count", clause)
	}
	if !strings.Contains(clause, "1 of which changed something") {
		t.Errorf("clause = %q, want changes counted apart from reads", clause)
	}
}

func TestAParkedJobSaysWhatItIsWaitingFor(t *testing.T) {
	job := ledger("tidy", Parked, did("moved the old invoices"))
	job.Question = Question{Why: WhyApproval, Ask: "Shall I delete them? This can't be undone."}
	spoken := job.Spoken()
	if !strings.Contains(spoken, "waiting on you") {
		t.Errorf("spoken = %q, want it to say the job is waiting", spoken)
	}
	if !strings.Contains(spoken, "can't be undone") {
		t.Errorf("spoken = %q, want the question in the gate's own words", spoken)
	}
}

// TestABoundaryReadsAsAStopRatherThanAQuestion: those need different things
// from the user, and the lead-in has to distinguish them.
func TestABoundaryReadsAsAStopRatherThanAQuestion(t *testing.T) {
	job := ledger("tidy", Parked)
	job.Question = Question{Why: WhyOutOfScope, Ask: "I stopped without doing it: it would have touched /etc."}
	spoken := job.Spoken()
	if strings.Contains(spoken, "waiting on you") {
		t.Errorf("spoken = %q, want a boundary read as a stop, not as a question to answer", spoken)
	}
	if !strings.Contains(spoken, "needs you") {
		t.Errorf("spoken = %q, want it to say the user is needed", spoken)
	}
}

func TestARunningJobSaysHowFarItHasGot(t *testing.T) {
	job := ledger("tidy", Running, did("moved the old invoices"))
	if got := job.Spoken(); !strings.Contains(got, "is running") || !strings.Contains(got, "1 step") {
		t.Errorf("spoken = %q, want the job named as running with its progress", got)
	}
}

// TestTheStatedJobCarriesTheGoalAndTheWholeBoundary is what the confirmation
// card shows, and it is the only thing the user judges before a job begins.
func TestTheStatedJobCarriesTheGoalAndTheWholeBoundary(t *testing.T) {
	job := Job{Name: "tidy", Goal: "tidy up my downloads",
		Scope: Scope{Tools: []string{"memory.search"}, Roots: []string{"/home/rich/Downloads"}}}
	stated := job.Stated()
	for _, want := range []string{"tidy up my downloads", "tidy", "/home/rich/Downloads",
		"memory.search", "can't undo"} {
		if !strings.Contains(stated, want) {
			t.Errorf("Stated() = %q, want it to say %q", stated, want)
		}
	}
}

// The offers (#221). One function, two callers: the sentence a surface shows
// where a control would have been IS the sentence the verb refuses with, so a
// listing and a press cannot explain the same rule differently.

// TestStopIsOfferedExactlyWhileThereIsSomethingToStop.
func TestStopIsOfferedExactlyWhileThereIsSomethingToStop(t *testing.T) {
	for _, state := range []State{Ready, Running, Parked} {
		if ok, why := ledger("tidy", state).StopOffer(); !ok || why != "" {
			t.Errorf("a %s job is not offered a stop (%q); it still has somewhere to go",
				state, why)
		}
	}
	for _, tc := range []struct {
		state State
		word  string
	}{
		{Done, "finished"}, {Stopped, "been stopped"}, {Failed, "ended"},
	} {
		ok, why := ledger("tidy", tc.state).StopOffer()
		if ok {
			t.Errorf("a %s job is offered a stop that could only refuse", tc.state)
		}
		if !strings.Contains(why, tc.word) {
			t.Errorf("the refusal for a %s job = %q, want it to say %q", tc.state, why, tc.word)
		}
		if !strings.HasPrefix(why, "Tidy ") {
			t.Errorf("the refusal does not name the job as a sentence would: %q", why)
		}
	}
}

// TestOnlyTheTwoQuestionsCanBeAnswered. A boundary and a denial are not
// opinions: the way past them is a new job with a scope that admits the work,
// which is a thing the user does deliberately rather than a yes they nod
// through. The sentence has to say so, because the alternative is a button that
// looks like it would help.
func TestOnlyTheTwoQuestionsCanBeAnswered(t *testing.T) {
	parked := func(why Why, ask string) Job {
		j := ledger("tidy", Parked)
		j.Question = Question{Why: why, Ask: ask}
		return j
	}
	for _, why := range []Why{WhyApproval, WhyDecision} {
		if ok, because := parked(why, "Shall I?").AnswerOffer(); !ok {
			t.Errorf("a job parked on %s cannot be answered (%q)", why, because)
		}
	}
	for _, why := range []Why{WhyOutOfScope, WhyRefused, WhyUnclear, WhyStuck} {
		ok, because := parked(why, "I stopped without doing it.").AnswerOffer()
		if ok {
			t.Errorf("a job parked on %s is offered an answer that cannot resume it", why)
		}
		if !strings.Contains(because, "isn't something I can carry on from") {
			t.Errorf("the refusal for %s = %q, want it to say why an answer will not do",
				why, because)
		}
		if strings.Contains(because, "..") {
			t.Errorf("the refusal doubles its full stop: %q", because)
		}
	}
	if ok, because := ledger("tidy", Running).AnswerOffer(); ok ||
		!strings.Contains(because, "not waiting on anything") {
		t.Errorf("a running job = (%v, %q), want a refusal saying it is not waiting", ok, because)
	}
}

// TestApproveIsAnsweringPlusTheTierAsItStandsNow pins ApproveOffer's bounds
// (#225). It is AnswerOffer plus one clause, and the clause only speaks about a
// gate approval it can actually consult a tier for.
func TestApproveIsAnsweringPlusTheTierAsItStandsNow(t *testing.T) {
	approval := func() Job {
		j := ledger("tidy", Parked)
		j.Question = Question{Why: WhyApproval, Ask: "Shall I?",
			Step: Step{Tool: "memory.remember", Intent: "delete the old files"}}
		return j
	}
	deny := func(reason string) Gate {
		return func(Step) Verdict { return Verdict{Decision: Deny, Reason: reason} }
	}

	// A caller with no gate cannot ask, so the offer stands: the enforcement is
	// the runner's, and a listing that guessed would be a second policy.
	if ok, because := approval().ApproveOffer(nil); !ok {
		t.Errorf("a surface that cannot ask the gate withheld the yes anyway: %q", because)
	}
	// AnswerOffer's own refusals come through unchanged and first — a boundary
	// is not a tier, and saying "you turned that off" about one would be wrong.
	boundary := ledger("tidy", Parked)
	boundary.Question = Question{Why: WhyOutOfScope, Ask: "I stopped without doing it."}
	_, plain := boundary.AnswerOffer()
	if ok, because := boundary.ApproveOffer(deny("never mind the tier")); ok || because != plain {
		t.Errorf("a boundary = (%v, %q), want AnswerOffer's own sentence %q", ok, because, plain)
	}
	// A planner's decision is not a question about a pending call, so there is
	// no tier to consult and none is invented.
	decision := ledger("tidy", Parked)
	decision.Question = Question{Why: WhyDecision, Ask: "Which folder?", Step: Step{Question: "Which?"}}
	if ok, because := decision.ApproveOffer(deny("memory.remember is off")); !ok {
		t.Errorf("a planner's decision was refused on a tier it never uses: %q", because)
	}
	// A gate that only says no, with nothing to say about why, still names the
	// tool — a refusal a reader cannot act on is the shrug this replaces.
	ok, because := approval().ApproveOffer(deny(""))
	if ok {
		t.Fatal("a denied tool was still offered its yes")
	}
	if !strings.Contains(because, "memory.remember") {
		t.Errorf("the refusal = %q, want it to name the tool", because)
	}
	// The gate's reason is carried verbatim, without doubling its full stop.
	_, because = approval().ApproveOffer(
		deny("I'm not allowed to use memory.remember (you turned it off)."))
	if strings.Contains(because, ". —") {
		t.Errorf("the refusal punctuates twice: %q", because)
	}
	if !strings.Contains(because, "you turned it off") {
		t.Errorf("the refusal = %q, want the gate's own reason inside it", because)
	}
}

// TestTheRunnerRefusesInTheOfferOwnWords is the pairing itself. A second
// sentence for the same rule is the drift this arrangement exists to prevent.
func TestTheRunnerRefusesInTheOffersOwnWords(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir+"/jobs.toml", StoreOptions{}, nil)
	runner := NewRunner(RunnerOptions{Store: store}, nil)
	job, err := store.Start("tidy", "tidy up", Scope{
		Tools: []string{"memory.search"}, Roots: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	// Not parked, so neither an answer nor — once it has ended — a stop will do.
	if _, err := runner.Answer(job.Name, true, ""); err == nil {
		t.Fatal("a job that is not waiting accepted an answer")
	} else if _, because := job.AnswerOffer(); err.Error() != because {
		t.Errorf("the runner refuses with %q and the listing says %q", err, because)
	}
	stopped, err := runner.Stop(job.Name, "You stopped it.")
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Stop(job.Name, "You stopped it.")
	if err == nil {
		t.Fatal("a job that had already stopped was stopped again")
	}
	if _, because := stopped.StopOffer(); err.Error() != because {
		t.Errorf("the runner refuses with %q and the listing says %q", err, because)
	}
}

// TestTitleIsTheHandleAsAHeading. Every surface that leads a sentence with a
// job's name needs the same answer, and the user typed it in whatever case
// they liked.
func TestTitleIsTheHandleAsAHeading(t *testing.T) {
	if got := (Job{Name: "tidy downloads"}).Title(); got != "Tidy downloads" {
		t.Errorf("Title = %q", got)
	}
	if got := (Job{}).Title(); got != "" {
		t.Errorf("Title of a nameless job = %q, want nothing rather than a panic", got)
	}
}
