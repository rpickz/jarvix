package daemon

// The account as a SURFACE reads it (#210, ADR 0066): the fields the window
// renders and the arrangement it draws, both composed here so that a client can
// only place them.
//
// These are unit tests of the report rather than socket round trips, and that
// is deliberate: every sentence the Account tab shows is decided in this file,
// and a vocabulary only reachable through a wired daemon is a vocabulary nobody
// writes cases for. The one end-to-end test at the bottom proves the new fields
// actually travel; everything above it is about what they say.

import (
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/jobs"
	"github.com/rpickz/jarvix/internal/undo"
)

// reportNow is the instant every fixture below is read at, so "4 minutes ago"
// is arithmetic rather than a wall-clock race.
var reportNow = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

// fileRecord is a reversible config write, as the store holds one.
func fileRecord(id, summary string, ago time.Duration) undo.Record {
	return undo.Record{
		ID: id, At: reportNow.Add(-ago),
		Action: undo.Action{
			Tool: "config.write_entry", Summary: summary,
			Target: "/home/u/.config/jarvix/config.toml",
			Restore: undo.Restore{Kind: undo.KindFile, File: &undo.FileRestore{
				Path: "/home/u/.config/jarvix/config.toml", Existed: true,
				Previous: "before\n", AfterDigest: "deadbeef"}},
		},
	}
}

// shellRecord is an action that cannot be taken back.
func shellRecord(id, summary string, ago time.Duration) undo.Record {
	return undo.Record{
		ID: id, At: reportNow.Add(-ago),
		Action: undo.Action{Tool: "shell.run", Summary: summary,
			Restore: undo.OneWay("shell.run")},
	}
}

// report renders one account, with an offer function standing in for the
// Undoer's own and a job lookup standing in for the store's.
func report(records []undo.Record, offer func(undo.Record) (bool, string),
	job func(string) (jobs.Job, bool)) map[string]any {
	if offer == nil {
		offer = func(r undo.Record) (bool, string) { return r.Reversible(), r.Why() }
	}
	return undoViewReport(undoAccount{
		view: undo.View{Records: records, Bound: undo.MaxActions, Forgotten: 3,
			Path: "/home/u/.local/state/jarvix/undo.toml", Now: reportNow},
		offer: offer,
		job:   job,
	})
}

// rows pulls the flat listing out of a report.
func rows(t *testing.T, v map[string]any) []map[string]any {
	t.Helper()
	out, ok := v["actions"].([]map[string]any)
	if !ok {
		t.Fatalf("the report carries no actions: %#v", v["actions"])
	}
	return out
}

// groups pulls the arrangement out of a report.
func groups(t *testing.T, v map[string]any) []map[string]any {
	t.Helper()
	out, ok := v["groups"].([]map[string]any)
	if !ok {
		t.Fatalf("the report carries no groups: %#v", v["groups"])
	}
	return out
}

// Every claim a row makes arrives as a string or a boolean the daemon worked
// out. The window has a clock it cannot trust and a policy it cannot read, so
// "when" and "can I put this back" are both answered here.
func TestEveryRowCarriesItsOwnWordsAndItsOwnVerdict(t *testing.T) {
	v := report([]undo.Record{
		shellRecord("a2", "ran rm -rf ./build", 20*time.Minute),
		fileRecord("a1", `saved the routine "morning"`, 4*time.Minute),
	}, nil, nil)

	got := rows(t, v)
	if len(got) != 2 {
		t.Fatalf("the report has %d rows, want 2", len(got))
	}

	if got[0]["when"] != "20 minutes ago" {
		t.Errorf("when = %v, want the daemon's own phrase for the elapsed time", got[0]["when"])
	}
	if got[0]["can_undo"] != false {
		t.Error("a shell command that has run is offered as reversible")
	}
	if state, _ := got[0]["state"].(string); !strings.Contains(state, "can't put this back") ||
		!strings.Contains(state, "a command that has run has run") {
		t.Errorf("state = %q, want the standing and the reason in one sentence", state)
	}

	if got[1]["when"] != "4 minutes ago" {
		t.Errorf("when = %v, want \"4 minutes ago\"", got[1]["when"])
	}
	if got[1]["can_undo"] != true {
		t.Error("a reversible config write is not offered")
	}
	if got[1]["state"] != "I can put this back." {
		t.Errorf("state = %v, want the sentence a reversible row shows", got[1]["state"])
	}
	if _, carried := got[1]["why"]; carried {
		t.Error("a row that can be put back carries a reason it cannot")
	}
}

// `reversible` and `can_undo` are two facts, not one. The gate can withhold a
// reversal of a record that is perfectly reversible, and collapsing the two
// would lose the difference between "there is nothing to put back" and "you
// have told me not to".
func TestAGateDeniedReversalIsWithheldWithoutBeingCalledIrreversible(t *testing.T) {
	denied := func(r undo.Record) (bool, string) {
		return false, "putting it back means another " + r.Tool + ", and you have that turned off"
	}
	got := rows(t, report([]undo.Record{
		fileRecord("a1", `saved the routine "morning"`, time.Minute)}, denied, nil))

	if got[0]["reversible"] != true {
		t.Error("the record itself stopped being reversible; the fixture no longer tests the split")
	}
	if got[0]["can_undo"] != false {
		t.Error("a reversal the gate denies is still offered")
	}
	why, _ := got[0]["why"].(string)
	if !strings.Contains(why, "turned off") {
		t.Errorf("why = %q, want the gate's own clause", why)
	}
	if state, _ := got[0]["state"].(string); !strings.Contains(state, why) {
		t.Errorf("state = %q does not carry the reason %q the field states", state, why)
	}
}

// A row that has been reversed says so, and says by what — the reversal earns
// its own id and the account is read backwards by nobody.
func TestAReversedRowSaysSoAndNamesTheReversal(t *testing.T) {
	rec := fileRecord("a1", `saved the routine "morning"`, 10*time.Minute)
	rec.UndoneBy = "a4"
	rec.UndoneAt = reportNow.Add(-30 * time.Second)

	got := rows(t, report([]undo.Record{rec}, nil, nil))
	state, _ := got[0]["state"].(string)
	if !strings.Contains(state, "I put this back") || !strings.Contains(state, "a4") {
		t.Errorf("state = %q, want it to say it was reversed and by what", state)
	}
	if !strings.Contains(state, "just now") {
		t.Errorf("state = %q, want the reversal placed in time too", state)
	}
	if got[0]["can_undo"] != false {
		t.Error("a row already put back is offered again")
	}
}

// A hand-edit can leave a row marked undone with no time on the mark. The
// sentence still has to be true, so it drops the clause rather than dating the
// reversal from the zero time.
func TestAReversalWithNoTimeStillReadsHonestly(t *testing.T) {
	rec := fileRecord("a1", `saved the routine "morning"`, 10*time.Minute)
	rec.UndoneBy = "a4"

	got := rows(t, report([]undo.Record{rec}, nil, nil))
	state, _ := got[0]["state"].(string)
	if strings.Contains(state, "weeks ago") || strings.Contains(state, "years") {
		t.Errorf("state = %q dates a reversal from a time nobody wrote", state)
	}
	if !strings.Contains(state, "a4") {
		t.Errorf("state = %q no longer names the reversal", state)
	}
}

// The account stores a provenance reference as one "kind:ref" string, because
// that is what fits a hand-editable TOML line. It is split here so the window
// can hand the result straight to provenance.resolve without learning the
// encoding — and a malformed line is dropped rather than guessed at.
func TestProvenanceIsHandedOverInTheShapeTheResolverTakes(t *testing.T) {
	rec := fileRecord("a1", "remembered a fact", time.Minute)
	rec.Provenance = []string{"fact:f1", "artifact:/tmp/plot.png", "nonsense", "fact:"}

	got := rows(t, report([]undo.Record{rec}, nil, nil))
	sources, ok := got[0]["sources"].([]map[string]any)
	if !ok {
		t.Fatalf("the row carries no resolvable sources: %#v", got[0]["sources"])
	}
	if len(sources) != 2 {
		t.Fatalf("sources = %#v, want the two well-formed references only", sources)
	}
	if sources[0]["kind"] != "fact" || sources[0]["ref"] != "f1" {
		t.Errorf("sources[0] = %#v", sources[0])
	}
	// Split on the FIRST colon: a path is a perfectly good reference and
	// contains none, but a URL would.
	if sources[1]["kind"] != "artifact" || sources[1]["ref"] != "/tmp/plot.png" {
		t.Errorf("sources[1] = %#v", sources[1])
	}
	// The stored strings travel too, unchanged, because the CLI reads them.
	if _, carried := got[0]["provenance"]; !carried {
		t.Error("the stored references no longer travel")
	}
}

// Grouped by job where a job exists, chronological otherwise. A job's group
// sits where its newest action falls, so nothing on the page jumps backwards
// in time between groups.
func TestTheAccountIsArrangedAsWorkWithoutLosingItsOrder(t *testing.T) {
	step2 := fileRecord("a4", "tagged the build", 2*time.Minute)
	step2.Job = "j3"
	loose := shellRecord("a3", "ran a check", 5*time.Minute)
	step1 := fileRecord("a2", "wrote the release notes", 9*time.Minute)
	step1.Job = "j3"
	older := shellRecord("a1", "ran something else", 20*time.Minute)

	tidy := jobs.Job{ID: "j3", Name: "tidy", State: jobs.Done}
	got := groups(t, report([]undo.Record{step2, loose, step1, older}, nil,
		func(id string) (jobs.Job, bool) { return tidy, id == "j3" }))

	if len(got) != 3 {
		t.Fatalf("the account made %d groups, want the job plus two loose actions", len(got))
	}
	// The job leads, because its newest action is the newest thing here.
	if got[0]["job"] != "j3" {
		t.Fatalf("the first group is %v, want the job whose newest step is newest", got[0]["job"])
	}
	heading, _ := got[0]["heading"].(string)
	if !strings.Contains(heading, "tidy") || !strings.Contains(heading, "2 actions") {
		t.Errorf("heading = %q, want the job's name and how much of it is here", heading)
	}
	held, _ := got[0]["actions"].([]map[string]any)
	if len(held) != 2 || held[0]["id"] != "a4" || held[1]["id"] != "a2" {
		t.Errorf("the job's group holds %#v, want both steps newest first", held)
	}
	// The two loose actions stand alone, in order, with no heading of their
	// own — nothing to head them with, and a made-up one would be wording.
	for i, want := range []string{"a3", "a1"} {
		g := got[i+1]
		if g["job"] != "" || g["heading"] != "" {
			t.Errorf("group %d is headed though its action belonged to no job: %#v", i+1, g)
		}
		one, _ := g["actions"].([]map[string]any)
		if len(one) != 1 || one[0]["id"] != want {
			t.Errorf("group %d holds %#v, want just %s", i+1, one, want)
		}
	}
}

// A job the job store has since dropped is still nameable. The account's bound
// and the job list's bound are different bounds, and a group headed by nothing
// would hide which work it was.
func TestAJobTheStoreNoLongerHoldsIsStillNamedByItsID(t *testing.T) {
	step := fileRecord("a1", "wrote the release notes", time.Minute)
	step.Job = "j9"

	got := groups(t, report([]undo.Record{step}, nil, nil))
	heading, _ := got[0]["heading"].(string)
	if !strings.Contains(heading, "j9") {
		t.Errorf("heading = %q, want the job's id when its name has gone", heading)
	}
	if !strings.Contains(heading, "1 action") || strings.Contains(heading, "1 actions") {
		t.Errorf("heading = %q, want a count that reads as English", heading)
	}
}

// The whole-job control is withheld exactly where the apply path would refuse
// it, in the same sentence — a control offered here and refused there is the
// dead affordance this surface exists to avoid.
func TestAJobStillWorkingIsWithheldInTheSentenceTheApplyPathUses(t *testing.T) {
	step := fileRecord("a1", "wrote the release notes", time.Minute)
	step.Job = "j3"
	deploy := jobs.Job{ID: "j3", Name: "deploy", State: jobs.Running}

	got := groups(t, report([]undo.Record{step}, nil,
		func(string) (jobs.Job, bool) { return deploy, true }))
	if got[0]["can_undo"] != false {
		t.Fatal("a job that is still working offers to be put back")
	}
	why, _ := got[0]["why"].(string)
	if why != undoJobBusy("deploy") {
		t.Errorf("why = %q, want the sentence settleBeforeUndo raises: %q",
			why, undoJobBusy("deploy"))
	}
}

// A job whose actions have all been reversed, or none of which could be, has
// nothing to offer and says so rather than showing a control that would report
// having done nothing.
func TestAJobWithNothingLeftToReverseSaysSoRatherThanOffering(t *testing.T) {
	step := shellRecord("a1", "ran ./deploy.sh", time.Minute)
	step.Job = "j3"

	got := groups(t, report([]undo.Record{step}, nil,
		func(string) (jobs.Job, bool) { return jobs.Job{ID: "j3", Name: "deploy", State: jobs.Done}, true }))
	if got[0]["can_undo"] != false {
		t.Fatal("a job with nothing reversible in it still offers to be put back")
	}
	if why, _ := got[0]["why"].(string); !strings.Contains(why, "nothing left") {
		t.Errorf("why = %q, want it to say there is nothing left to put back", why)
	}
}

// Undoing a parked job stops it. That is a consequence of pressing the
// control, so it is stated before the press — the confirmation card's own
// argument, applied to a control that has no card.
func TestAParkedJobsReversalDisclosesThatItWillStopIt(t *testing.T) {
	step := fileRecord("a1", "wrote the release notes", time.Minute)
	step.Job = "j3"
	parked := jobs.Job{ID: "j3", Name: "tidy", State: jobs.Parked}

	got := groups(t, report([]undo.Record{step}, nil,
		func(string) (jobs.Job, bool) { return parked, true }))
	if got[0]["can_undo"] != true {
		t.Fatal("a parked job cannot be put back, though it is not acting")
	}
	note, _ := got[0]["note"].(string)
	if !strings.Contains(note, "stops it") {
		t.Errorf("note = %q, want it to say the reversal also stops the job", note)
	}
}

// An account with nothing in it still has something to say, once, so the CLI
// and the window cannot say it differently.
func TestAnEmptyAccountCarriesItsOwnSentence(t *testing.T) {
	v := report(nil, nil, nil)
	if v["empty"] != undoEmptySentence {
		t.Errorf("empty = %v, want the daemon's own sentence", v["empty"])
	}
	if got := groups(t, v); len(got) != 0 {
		t.Errorf("an empty account produced %d groups", len(got))
	}
}

// And the whole thing travels. One socket round trip, because "the fields are
// composed correctly" and "the fields arrive" are two different claims.
func TestTheWindowsHalfOfTheAccountTravelsOverTheWire(t *testing.T) {
	h := startUndoDaemon(t)
	h.recordFileChange(t, "config.toml", "before\n", "after\n", `saved the routine "morning"`)

	var view struct {
		Actions []map[string]any `json:"actions"`
		Groups  []struct {
			Job     string           `json:"job"`
			Heading string           `json:"heading"`
			CanUndo bool             `json:"can_undo"`
			Actions []map[string]any `json:"actions"`
		} `json:"groups"`
		Empty string `json:"empty"`
	}
	if err := h.client.Call("undo.list", nil, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Actions) != 1 || len(view.Groups) != 1 {
		t.Fatalf("undo.list returned %d actions in %d groups, want one of each",
			len(view.Actions), len(view.Groups))
	}
	row := view.Groups[0].Actions[0]
	for _, field := range []string{"when", "state", "can_undo"} {
		if _, carried := row[field]; !carried {
			t.Errorf("the row does not carry %q; the window would have to work it out itself", field)
		}
	}
	if row["can_undo"] != true {
		t.Errorf("a reversible config write is not offered over the wire: %#v", row)
	}
	if view.Empty == "" {
		t.Error("the account's empty sentence does not travel")
	}
}
