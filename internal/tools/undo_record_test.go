package tools

// The tools' half of the account (#201, ADR 0064): every tool that changes
// the machine writes its own row, with the previous bytes where it can and an
// honest refusal to promise where it cannot.
//
// These are integration tests over the real stores in temp directories rather
// than assertions about a fake recorder, because the thing worth proving is
// that the row a tool writes is one the reverser can actually act on — a
// record whose restore payload was captured a moment too late would pass any
// test that only checked a row appeared.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/undo"
)

// recordingCtx installs a real account over a temp file and returns both.
func recordingCtx(t *testing.T) (context.Context, *undo.Store) {
	t.Helper()
	at := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	store := undo.NewStore(filepath.Join(t.TempDir(), "undo.toml"), undo.StoreOptions{
		Now: func() time.Time { at = at.Add(time.Minute); return at },
	}, nil)
	return undo.WithRecorder(context.Background(), store), store
}

// onlyRecord returns the single row the account holds, failing when there is
// not exactly one — an extra row is as much a defect as a missing one.
func onlyRecord(t *testing.T, store *undo.Store) undo.Record {
	t.Helper()
	view := store.List()
	if len(view.Records) != 1 {
		t.Fatalf("the account holds %d rows, want exactly 1", len(view.Records))
	}
	return view.Records[0]
}

// TestRememberingAFactRecordsWhatWouldPutItBack. The memory book is one TOML
// document rewritten whole, so the previous bytes are a complete answer —
// which is the argument for the file kind covering every voice-written store
// rather than each one growing a bespoke reversal.
func TestRememberingAFactRecordsWhatWouldPutItBack(t *testing.T) {
	m, book, _ := testMemory(t)
	ctx, account := recordingCtx(t)

	tool := memoryTool(t, m, MemoryRememberToolName)
	if _, err := tool.Execute(ctx, json.RawMessage(`{"content":"the wifi password is swordfish"}`)); err != nil {
		t.Fatal(err)
	}

	rec := onlyRecord(t, account)
	if rec.Tool != MemoryRememberToolName {
		t.Errorf("the row names %q, want %q", rec.Tool, MemoryRememberToolName)
	}
	if !rec.Reversible() {
		t.Fatalf("remembering a fact was recorded as something Jarvix cannot put back: %s", rec.Why())
	}
	if rec.Restore.File == nil || rec.Restore.File.Path != book.Path() {
		t.Fatalf("the restore points at %+v, want the memory book's own file", rec.Restore.File)
	}
	// The captured bytes are the file BEFORE the write. The book did not
	// exist, so "put it back" means "there was no file", which is what
	// Existed false says.
	if rec.Restore.File.Existed {
		t.Error("the record claims the memory book existed before the first fact was stored")
	}
	if rec.Restore.File.AfterDigest == "" {
		t.Error("no clobber guard was recorded, so the undo could never tell it was safe")
	}
}

// TestForgettingAPhraseKeepsTheBytesThatWouldRestoreIt.
func TestForgettingAPhraseKeepsTheBytesThatWouldRestoreIt(t *testing.T) {
	v, store := testVocabulary(t)
	// Teach outside the recorder, so the account holds only the forget: what
	// is under test is the reversal of a deletion.
	if _, err := vocabularyTool(t, v, VocabularyTeachToolName).Execute(context.Background(),
		json.RawMessage(`{"phrase":"kubectl","meaning":"the kubernetes CLI"}`)); err != nil {
		t.Fatal(err)
	}
	taught, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}

	ctx, account := recordingCtx(t)
	if _, err := vocabularyTool(t, v, VocabularyForgetToolName).Execute(ctx,
		json.RawMessage(`{"phrase":"kubectl"}`)); err != nil {
		t.Fatal(err)
	}

	rec := onlyRecord(t, account)
	if !rec.Reversible() {
		t.Fatalf("forgetting a phrase was recorded as irreversible: %s", rec.Why())
	}
	if rec.Restore.File == nil || rec.Restore.File.Previous != string(taught) {
		t.Error("the kept bytes are not the file as it stood before the phrase was forgotten")
	}
	if !strings.Contains(rec.Summary, "kubectl") {
		t.Errorf("the row reads %q, which does not say what was forgotten", rec.Summary)
	}
}

// TestASetReminderIsRecordedAsSomethingToPutBack.
func TestASetReminderIsRecordedAsSomethingToPutBack(t *testing.T) {
	fam := newReminderTools(t)
	ctx, account := recordingCtx(t)

	var set Tool
	for _, tool := range fam.Tools() {
		if tool.Name() == ReminderSetToolName {
			set = tool
		}
	}
	if set == nil {
		t.Fatal("no reminder.set tool in the family")
	}
	if _, err := set.Execute(ctx,
		json.RawMessage(`{"when":"at three","text":"call the pharmacy"}`)); err != nil {
		t.Fatal(err)
	}
	rec := onlyRecord(t, account)
	if !rec.Reversible() {
		t.Fatalf("a set reminder was recorded as irreversible: %s", rec.Why())
	}
	if !strings.Contains(rec.Summary, "call the pharmacy") {
		t.Errorf("the row reads %q, which does not say what the reminder was for", rec.Summary)
	}
}

// TestAShellCommandIsRecordedVerbatimAndPromisedNothing. This is the
// distinction the ticket calls its spine, tested at the tool: the command is
// in the account exactly as it ran, and the row promises nothing.
func TestAShellCommandIsRecordedVerbatimAndPromisedNothing(t *testing.T) {
	ctx, account := recordingCtx(t)
	shell := &Shell{}
	if _, err := shell.Execute(ctx, json.RawMessage(`{"command":"echo hello"}`)); err != nil {
		t.Fatal(err)
	}
	rec := onlyRecord(t, account)
	if rec.Reversible() {
		t.Fatal("a shell command was recorded as something Jarvix could put back")
	}
	if rec.Summary != "ran echo hello" {
		t.Errorf("the row reads %q, want the command verbatim", rec.Summary)
	}
	if !strings.Contains(rec.Why(), "has run") {
		t.Errorf("the reason %q does not say why the command cannot be taken back", rec.Why())
	}
}

// TestAFailedCommandIsStillRecorded: a command that exited non-zero still
// ran, and half of it may well have landed. An account that only recorded
// successes would be an account that missed the runs worth reviewing.
func TestAFailedCommandIsStillRecorded(t *testing.T) {
	ctx, account := recordingCtx(t)
	shell := &Shell{}
	if _, err := shell.Execute(ctx, json.RawMessage(`{"command":"exit 3"}`)); err != nil {
		t.Fatal(err)
	}
	rec := onlyRecord(t, account)
	if rec.Summary != "ran exit 3" {
		t.Errorf("the row reads %q, want the failed command recorded like any other", rec.Summary)
	}
}

// TestToolsRecordNothingWithoutARecorder is the seam's promise at the tool
// layer: every one of these behaves exactly as it did before the account
// existed when it is run outside a turn.
func TestToolsRecordNothingWithoutARecorder(t *testing.T) {
	m, _, _ := testMemory(t)
	if _, err := memoryTool(t, m, MemoryRememberToolName).Execute(context.Background(),
		json.RawMessage(`{"content":"nobody is watching"}`)); err != nil {
		t.Fatal(err)
	}
	shell := &Shell{}
	if _, err := shell.Execute(context.Background(), json.RawMessage(`{"command":"echo quiet"}`)); err != nil {
		t.Fatal(err)
	}
	// Nothing to assert about a store, because no store was ever created —
	// which is the point. The assertion is that neither call panicked on a
	// nil recorder and neither needed one.
}
