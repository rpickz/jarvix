package undo

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// The job seam's tests (#200, ADR 0065). ADR 0064 built everything else about
// job-scoped reversal and left exactly one thing missing: the code that fills
// the field in. This is that code, and these are its assertions.

func jobStore(t *testing.T) *Store {
	t.Helper()
	at := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	return NewStore(filepath.Join(t.TempDir(), "undo.toml"), StoreOptions{
		Now: func() time.Time { at = at.Add(time.Second); return at },
	}, nil)
}

// TestAnActionTakenInsideAJobIsRecordedUnderIt is the whole of what #200 owed
// the account: one context value, read in Note, and every one of the twelve
// places that build an Action is grouped without any of them changing.
func TestAnActionTakenInsideAJobIsRecordedUnderIt(t *testing.T) {
	store := jobStore(t)
	ctx := WithJob(WithRecorder(context.Background(), store), "j7")
	rec := Note(ctx, Action{Tool: "memory.remember", Summary: "wrote down where the invoices went",
		Restore: OneWay("memory.remember")})
	if rec.Job != "j7" {
		t.Fatalf("job = %q, want the id the runner installed", rec.Job)
	}
	got := store.Job("j7")
	if len(got) != 1 || got[0].ID != rec.ID {
		t.Errorf("Store.Job(j7) = %+v, want the record just written", got)
	}
}

// TestAnOrdinaryTurnRecordsNoJob: an action the user asked for in a
// conversation was not part of a job, and saying it was would make the account
// lie about who was watching.
func TestAnOrdinaryTurnRecordsNoJob(t *testing.T) {
	store := jobStore(t)
	rec := Note(WithRecorder(context.Background(), store),
		Action{Tool: "shell.run", Summary: "ran df -h", Restore: OneWay("shell.run")})
	if rec.Job != "" {
		t.Errorf("job = %q, want empty for a turn nobody delegated", rec.Job)
	}
}

// TestACallerThatKnowsItsOwnJobKeepsIt: the ambient value fills a gap, it does
// not overwrite an answer.
func TestACallerThatKnowsItsOwnJobKeepsIt(t *testing.T) {
	store := jobStore(t)
	ctx := WithJob(WithRecorder(context.Background(), store), "j7")
	rec := Note(ctx, Action{Tool: "shell.run", Summary: "ran something", Job: "j9",
		Restore: OneWay("shell.run")})
	if rec.Job != "j9" {
		t.Errorf("job = %q, want the caller's own answer kept", rec.Job)
	}
}

// TestAnEmptyJobIDInstallsNothing keeps the seam free for every caller that has
// no job: the same context back, and no value to read.
func TestAnEmptyJobIDInstallsNothing(t *testing.T) {
	ctx := context.Background()
	if got := WithJob(ctx, "   "); got != ctx {
		t.Error("an empty job id wrapped the context anyway")
	}
	if got := JobFrom(ctx); got != "" {
		t.Errorf("JobFrom = %q, want empty", got)
	}
	//nolint:staticcheck // the nil case is exactly what a caller outside a turn hands in.
	if got := JobFrom(nil); got != "" {
		t.Errorf("JobFrom(nil) = %q, want empty", got)
	}
}

// TestASnapshottedWriteInsideAJobIsGroupedToo: the file-restore path notes
// through the same function, so it inherits the grouping rather than needing
// its own.
func TestASnapshottedWriteInsideAJobIsGroupedToo(t *testing.T) {
	store := jobStore(t)
	path := filepath.Join(t.TempDir(), "memory.toml")
	ctx := WithJob(WithRecorder(context.Background(), store), "j7")
	before := Snapshot(ctx, path)
	rec := before.Note(ctx, Action{Tool: "memory.remember", Summary: "wrote a fact down"})
	if rec.Job != "j7" {
		t.Errorf("job = %q, want the runner's id on a snapshotted write too", rec.Job)
	}
	if rec.Restore.Kind != KindFile {
		t.Errorf("restore = %v, want a file restore", rec.Restore.Kind)
	}
}
