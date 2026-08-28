package session

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/tools"
)

// These tests pin issue #118 at the engine: a resolved tool confirmation is
// part of the conversation record — rendered by Conversation() at its
// position between the turns of its exchange, archived beside them under the
// same staged-before-acknowledged discipline (#116, #125), and distinct per
// outcome. The daemon socket tests cover the same promises end to end; here
// the corners live: visibility at the instant of resolution, the turn that
// dies without committing, and the adoption rebase under the context cap.

// recordGateHarness is a gate harness whose engine also archives, which is
// the combination every record test needs.
func recordGateHarness(t *testing.T, fake *conversations.Fake) (*harness, *namedTool) {
	t.Helper()
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, archiveOptions(fake, 8), rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "Build directory removed.")
	return h, rec
}

// confirmationTurns filters Conversation() down to its record entries.
func confirmationTurns(turns []Turn) []Turn {
	var recs []Turn
	for _, turn := range turns {
		if turn.Role == conversations.RoleConfirmation {
			recs = append(recs, turn)
		}
	}
	return recs
}

// The approved exchange, whole: the record sits between the question and the
// answer — in the snapshot the window rebuilds from and in the archive —
// carrying the verbatim command and the approved outcome.
func TestResolvedConfirmationIsRecordedBetweenItsTurns(t *testing.T) {
	fake := conversations.NewFake()
	h, rec := recordGateHarness(t, fake)

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	h.waitFor(t, "tool.confirmation_required")

	// A snapshot taken mid-wait carries no record turn: the pending question
	// is the snapshot's `confirmation` field (issue #76), and doubling it as
	// a turn is exactly what the reopen criterion forbids.
	if got := confirmationTurns(h.engine.Conversation()); len(got) != 0 {
		t.Fatalf("pending confirmation already appears as %d record turn(s)", len(got))
	}

	if err := h.engine.Confirm(true); err != nil {
		t.Fatal(err)
	}
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
	awaitAppend(t, fake)
	if rec.calls != 1 {
		t.Fatalf("approved tool ran %d times, want 1", rec.calls)
	}

	turns := h.engine.Conversation()
	if len(turns) != 3 {
		t.Fatalf("conversation has %d turns, want user/record/assistant", len(turns))
	}
	if turns[0].Role != "user" || turns[1].Role != conversations.RoleConfirmation || turns[2].Role != "assistant" {
		t.Fatalf("order = %s/%s/%s, want the record between the exchange's halves",
			turns[0].Role, turns[1].Role, turns[2].Role)
	}
	c := turns[1].Confirmation
	if c == nil {
		t.Fatal("record turn carries no payload")
	}
	if c.Command != "rm -rf ./build" || c.Outcome != conversations.ConfirmationApproved || c.Tool != "shell.run" {
		t.Errorf("payload = %+v, want the verbatim command approved", *c)
	}
	if !strings.Contains(turns[1].Text, "rm -rf ./build") {
		t.Errorf("record text %q does not quote the question", turns[1].Text)
	}

	archived := fake.Turns(h.engine.ActiveConversationID())
	if len(archived) != 3 {
		t.Fatalf("archive holds %d turns, want 3", len(archived))
	}
	if archived[1].Role != conversations.RoleConfirmation || archived[1].Confirmation == nil ||
		archived[1].Confirmation.Outcome != conversations.ConfirmationApproved {
		t.Errorf("archived record = %+v, want the approved confirmation between the halves", archived[1])
	}
	if archived[1].Time.IsZero() {
		t.Error("archived record carries no timestamp")
	}
}

// The record exists the instant the resolution is acknowledged: Confirm has
// returned, so a conversation.get racing the still-running turn must already
// see the declined record — the #116 read-your-acknowledged-writes guarantee
// extended to resolutions.
func TestConfirmationRecordAppearsTheMomentItResolves(t *testing.T) {
	fake := conversations.NewFake()
	h, rec := recordGateHarness(t, fake)

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	h.waitFor(t, "tool.confirmation_required")
	if err := h.engine.Confirm(false); err != nil {
		t.Fatal(err)
	}

	// No waiting: the turn is still running (the model is answering the
	// decline), and the record is already in the snapshot, after the
	// in-flight question.
	turns := h.engine.Conversation()
	recs := confirmationTurns(turns)
	if len(recs) != 1 {
		t.Fatalf("snapshot carries %d records immediately after Confirm, want 1", len(recs))
	}
	if recs[0].Confirmation.Outcome != conversations.ConfirmationDeclined {
		t.Errorf("outcome = %q, want declined", recs[0].Confirmation.Outcome)
	}
	if turns[0].Role != "user" || turns[1].Role != conversations.RoleConfirmation {
		t.Errorf("mid-turn order = %s/%s, want the record after the question", turns[0].Role, turns[1].Role)
	}

	h.countUntil(t, "session.finished")
	h.waitIdle(t)
	if rec.calls != 0 {
		t.Fatalf("declined tool ran %d times", rec.calls)
	}
}

// A timeout is its own outcome on the record, never conflated with a spoken
// no: the archive says timed_out with the timeout that applied.
func TestTimedOutConfirmationRecordsItsOwnOutcome(t *testing.T) {
	fake := conversations.NewFake()
	h, _ := recordGateHarness(t, fake)
	fire := make(chan time.Time)
	h.engine.timer = func(time.Duration) (<-chan time.Time, func()) {
		return fire, func() {}
	}

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	h.waitFor(t, "tool.confirmation_required")
	fire <- time.Time{} // the user never answered
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
	awaitAppend(t, fake)

	archived := fake.Turns(h.engine.ActiveConversationID())
	if len(archived) != 3 {
		t.Fatalf("archive holds %d turns, want 3", len(archived))
	}
	c := archived[1].Confirmation
	if c == nil || c.Outcome != conversations.ConfirmationTimedOut || c.Source != "timeout" {
		t.Errorf("archived record = %+v, want timed_out by timeout", archived[1])
	}
	if c != nil && c.TimeoutSec != int(DefaultConfirmTimeout.Seconds()) {
		t.Errorf("timeout_sec = %d, want the window that applied (%d)",
			c.TimeoutSec, int(DefaultConfirmTimeout.Seconds()))
	}
}

// A turn that fails after the user approved still archives the record,
// standalone: the command already ran, and the failure of the turn around it
// must not erase what was authorised.
func TestFailedTurnStillArchivesItsConfirmationRecord(t *testing.T) {
	fake := conversations.NewFake()
	h, rec := recordGateHarness(t, fake)

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	h.waitFor(t, "tool.confirmation_required")
	// The model dies on the round after the tool runs. Setting Fail here is
	// ordered before the next Chat by the confirm below (channel-synchronised
	// through the engine), and round one has already streamed to completion.
	h.provider.Fail = errors.New("stream torn")
	if err := h.engine.Confirm(true); err != nil {
		t.Fatal(err)
	}
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
	awaitAppend(t, fake)
	if rec.calls != 1 {
		t.Fatalf("approved tool ran %d times, want 1", rec.calls)
	}

	// No exchange committed — the failed turn is not a turn of the record —
	// but the approval is: alone in the archive, and alone in the snapshot.
	archived := fake.Turns(h.engine.ActiveConversationID())
	if len(archived) != 1 {
		t.Fatalf("archive holds %d turns, want just the record", len(archived))
	}
	if archived[0].Role != conversations.RoleConfirmation || archived[0].Confirmation == nil ||
		archived[0].Confirmation.Outcome != conversations.ConfirmationApproved {
		t.Errorf("archived record = %+v, want the approved confirmation", archived[0])
	}
	if recs := confirmationTurns(h.engine.Conversation()); len(recs) != 1 {
		t.Errorf("snapshot carries %d records after the failure, want 1", len(recs))
	}
}

// An interruption mid-wait declines the question and keeps the record inside
// the interrupted exchange (#117 + #118 together): the archive holds the
// question, the declined record, and the interrupted assistant half.
func TestInterruptedTurnCarriesItsConfirmationRecord(t *testing.T) {
	fake := conversations.NewFake()
	h, rec := recordGateHarness(t, fake)

	first, _ := h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	h.waitFor(t, "tool.confirmation_required")
	second, err := h.engine.StartSession() // the user talks over the question
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("expected a fresh session")
	}
	h.waitFor(t, "session.cancelled")
	awaitAppend(t, fake)
	if rec.calls != 0 {
		t.Fatalf("abandoned tool ran %d times", rec.calls)
	}

	archived := fake.Turns(h.engine.ActiveConversationID())
	if len(archived) != 3 {
		t.Fatalf("archive holds %d turns, want 3", len(archived))
	}
	if archived[0].Role != "user" || !archived[0].Interrupted {
		t.Errorf("first turn = %+v, want the interrupted question", archived[0])
	}
	c := archived[1].Confirmation
	if archived[1].Role != conversations.RoleConfirmation || c == nil ||
		c.Outcome != conversations.ConfirmationDeclined || c.Source != "interrupted" {
		t.Errorf("archived record = %+v, want declined by interruption", archived[1])
	}
}

// AdoptConversation restores records beside the turns they sat between, and
// rebases their anchors under the context cap: a record whose exchange fell
// outside the budget falls away with it, one inside lands exactly where it
// was.
func TestAdoptRestoresConfirmationRecordsUnderTheCap(t *testing.T) {
	fake := conversations.NewFake()
	h := newHarness(t, archiveOptions(fake, 2)) // budget: two exchanges of the three
	base := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	rec := func(cmd, outcome string) conversations.Confirmation {
		return conversations.Confirmation{Tool: "shell.run", Command: cmd, Outcome: outcome, TimeoutSec: 30}
	}
	msgs := []ai.Message{
		{Role: ai.RoleUser, Content: "oldest question"},
		{Role: ai.RoleAssistant, Content: "oldest answer"},
		{Role: ai.RoleUser, Content: "middle question"},
		{Role: ai.RoleAssistant, Content: "middle answer"},
		{Role: ai.RoleUser, Content: "newest question"},
		{Role: ai.RoleAssistant, Content: "newest answer"},
	}
	confs := []AdoptedConfirmation{
		{Record: rec("rm -rf ./old", conversations.ConfirmationDeclined),
			Summary: "Run rm -rf ./old?", Time: base, AfterMessages: 1},
		{Record: rec("rm -rf ./new", conversations.ConfirmationApproved),
			Summary: "Run rm -rf ./new?", Time: base.Add(time.Minute), AfterMessages: 5},
	}
	h.engine.AdoptConversation("old-conv", msgs, confs, nil)

	turns := h.engine.Conversation()
	if len(turns) != 5 {
		t.Fatalf("adopted snapshot has %d turns, want 4 messages + 1 record", len(turns))
	}
	for i, turn := range turns {
		if turn.Role == conversations.RoleConfirmation && turn.Confirmation.Command == "rm -rf ./old" {
			t.Errorf("turn %d: a record from beyond the cap survived the adoption", i)
		}
	}
	if turns[3].Role != conversations.RoleConfirmation ||
		turns[3].Confirmation.Command != "rm -rf ./new" {
		t.Fatalf("turns[3] = %+v, want the newest record after its question", turns[3])
	}
	if turns[2].Text != "newest question" || turns[4].Text != "newest answer" {
		t.Errorf("record neighbours = %q / %q, want its own exchange", turns[2].Text, turns[4].Text)
	}
}
