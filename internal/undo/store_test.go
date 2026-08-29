package undo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheBoundIsDisclosedRatherThanSilentlyForgotten is the acceptance
// criterion the store's whole shape is built around: an account that quietly
// shortens its own memory is worse than one that says "I only keep the last
// N". Every surface that lists the account prints the daemon's own sentence,
// so what is under test here is that the sentence tells the truth.
func TestTheBoundIsDisclosedRatherThanSilentlyForgotten(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "undo.toml"),
		StoreOptions{MaxActions: 20, Now: fixedClock()}, nil)

	// Nothing has dropped off yet: the sentence states the bound and claims
	// nothing about forgetting.
	for i := range 20 {
		if _, err := store.Append(Action{Tool: "shell.run",
			Summary: "ran command " + itoa(i), Restore: OneWay("shell.run")}); err != nil {
			t.Fatal(err)
		}
	}
	view := store.List()
	if got, want := view.Disclosure(), "I keep the last 20 actions."; got != want {
		t.Errorf("disclosure = %q, want %q", got, want)
	}
	if len(view.Records) != 20 {
		t.Fatalf("the account holds %d rows at the cap, want 20", len(view.Records))
	}

	// Three more, and the oldest three go — counted, not quietly dropped.
	for i := range 3 {
		if _, err := store.Append(Action{Tool: "shell.run",
			Summary: "ran later command " + itoa(i), Restore: OneWay("shell.run")}); err != nil {
			t.Fatal(err)
		}
	}
	view = store.List()
	if len(view.Records) != 20 {
		t.Errorf("the account holds %d rows past the cap, want the bound to hold at 20",
			len(view.Records))
	}
	if view.Forgotten != 3 {
		t.Errorf("forgotten = %d, want the 3 that were evicted", view.Forgotten)
	}
	if got, want := view.Disclosure(),
		"I keep the last 20 actions; 3 older ones have dropped off."; got != want {
		t.Errorf("disclosure = %q, want %q", got, want)
	}

	// And the count survives a restart, because a bound whose arithmetic
	// resets is a bound that lies after the first daemon restart.
	reopened := NewStore(store.Path(), StoreOptions{MaxActions: 20, Now: fixedClock()}, nil)
	if got := reopened.List().Forgotten; got != 3 {
		t.Errorf("forgotten = %d after a reload, want the persisted 3", got)
	}
}

// TestTheBoundNeverRefusesToRecord pins the one way this store differs from
// every other bounded store in this repository. They refuse at the cap and
// name the fix; this one cannot, because refusing to record would leave an
// action that happened with nothing in the account — the single outcome the
// feature exists to prevent.
func TestTheBoundNeverRefusesToRecord(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "undo.toml"),
		StoreOptions{MaxActions: 2, Now: fixedClock()}, nil)
	for i := range 5 {
		if _, err := store.Append(Action{Tool: "shell.run",
			Summary: "ran " + itoa(i), Restore: OneWay("shell.run")}); err != nil {
			t.Fatalf("the account refused to record action %d: %v", i, err)
		}
	}
	view := store.List()
	if len(view.Records) != 2 {
		t.Fatalf("the account holds %d rows, want the cap of 2", len(view.Records))
	}
	if view.Records[0].Summary != "ran 4" {
		t.Errorf("the newest row is %q, want the last action recorded", view.Records[0].Summary)
	}
}

// TestAFileTooBigToKeepIsMarkedIrreversibleWhenItHappens is the other bound,
// and the criterion that irreversibility is marked AT THE TIME rather than
// discovered later. A half-kept copy would be worse than none — it would
// restore a truncated file over a whole one — so the record says so on the
// day, in the account the user can read.
func TestAFileTooBigToKeepIsMarkedIrreversibleWhenItHappens(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "undo.toml"),
		StoreOptions{Now: fixedClock()}, nil)
	path := filepath.Join(t.TempDir(), "enormous.toml")
	writeFile(t, path, strings.Repeat("x", MaxRestoreBytes+1))

	ctx := WithRecorder(context.Background(), store)
	before := Snapshot(ctx, path)
	writeFile(t, path, "small now\n")
	rec := before.Note(ctx, Action{Tool: "config.write_entry", Summary: "rewrote the big file"})

	if rec.Reversible() {
		t.Fatal("a file too big to copy was recorded as reversible")
	}
	if !strings.Contains(rec.Why(), "64 KB") {
		t.Errorf("the reason %q does not disclose the cap it hit", rec.Why())
	}
	// And the account, read back from disk, still says it.
	reopened := NewStore(store.Path(), StoreOptions{Now: fixedClock()}, nil)
	got, err := reopened.Get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reversible() || !strings.Contains(got.Why(), "64 KB") {
		t.Errorf("after a reload the row reads %+v, want it still marked irreversible with its reason",
			got.Restore)
	}
}

// TestAHandEditThatRemovesTheRestoreLeavesAnHonestRow: the file is the
// user's, and dropping an [action.file] stanza is a legitimate thing to do
// with a copy of something private. What must not happen is a row that
// still offers a reversal it can no longer perform.
func TestAHandEditThatRemovesTheRestoreLeavesAnHonestRow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "undo.toml")
	doc := `version = 1
next_id = 5

[[action]]
id = "a4"
at = 2026-08-29T09:00:00Z
tool = "config.write_entry"
summary = "saved the routine \"morning\""
kind = "file"
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path, StoreOptions{Now: fixedClock()}, nil)
	rec, err := store.Get("a4")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Reversible() {
		t.Error("a row whose restore was deleted by hand still offers to put something back")
	}
	if !strings.Contains(rec.Why(), "does not say which") {
		t.Errorf("the reason %q does not explain what is missing", rec.Why())
	}
}

// TestForgettingARowActuallyForgetsItAndTheIDIsNotReissued: the account's
// only deletion. It exists because a shell command is recorded verbatim and
// a user may have dictated a secret into one; it deletes rather than
// tombstones, exactly as the conversation archive does, and the id it held
// never names anything again.
func TestForgettingARowActuallyForgetsItAndTheIDIsNotReissued(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "undo.toml"),
		StoreOptions{Now: fixedClock()}, nil)
	secret, err := store.Append(Action{Tool: "shell.run",
		Summary: "ran mysql -p hunter2", Restore: OneWay("shell.run")})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Forget(secret.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(secret.ID); err == nil {
		t.Fatal("the forgotten row is still in the account")
	}
	if body := readFile(t, store.Path()); strings.Contains(body, "hunter2") {
		t.Error("the forgotten command is still in the file")
	}
	next, err := store.Append(Action{Tool: "shell.run",
		Summary: "ran ls", Restore: OneWay("shell.run")})
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == secret.ID {
		t.Errorf("id %q was reissued after the row holding it was forgotten", next.ID)
	}
}

// TestARecordWithNoSummaryIsRefused: a row nobody can read is not a record,
// and an account of what was done in your name that contains a blank line is
// worse than one that made the caller say what it did.
func TestARecordWithNoSummaryIsRefused(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "undo.toml"),
		StoreOptions{Now: fixedClock()}, nil)
	if _, err := store.Append(Action{Tool: "shell.run", Summary: "   "}); err == nil {
		t.Fatal("an action with no summary was recorded")
	}
}

// TestTheAccountIsPrivate: the file holds the previous contents of the user's
// configuration, which is where their api keys live. It is 0600 in a 0700
// directory, asserted on every write rather than hoped for.
func TestTheAccountIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store := NewStore(filepath.Join(dir, "undo.toml"), StoreOptions{Now: fixedClock()}, nil)
	if _, err := store.Append(Action{Tool: "shell.run",
		Summary: "ran ls", Restore: OneWay("shell.run")}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("the account is mode %v, want 0600", got)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("the state dir is mode %v, want 0700", got)
	}
}
