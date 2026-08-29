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
	clause := job.progressClause()
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
